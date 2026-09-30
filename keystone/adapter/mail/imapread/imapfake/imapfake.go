// Package imapfake is a deterministic in-process IMAP server for tests
// of the read-only mail reader (epic #326, slice S04), kept as
// production test infrastructure so later slices' end-to-end proofs
// run against the same fixture.
//
// It speaks real IMAP over a real (TLS) socket through go-imap's server
// package, so the reader under test uses its production client and
// dialer. Its store is deliberately small and fully controllable:
// deliver a message, expunge one, reset a folder's UIDVALIDITY, drop
// the connection in the middle of a batch, answer a FETCH twice.
//
// It is also the witness for the read-only guarantee. Every command
// that could change provider mail — STORE, COPY, MOVE, EXPUNGE, APPEND,
// CREATE, DELETE, RENAME, (UN)SUBSCRIBE, a read-write SELECT, a FETCH
// without PEEK (which sets \Seen) — is refused and recorded as a
// violation, which tests assert is empty.
package imapfake

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// Faults are injected failures. Zero means none.
type Faults struct {
	// DropOnBodyFetch closes the connection when the server is about to
	// answer the Nth (1-based) full-body FETCH item across the server's
	// lifetime since the faults were set.
	DropOnBodyFetch int
	// DuplicateFetch answers every FETCH message twice, as some servers
	// do after unsolicited flag updates.
	DuplicateFetch bool
	// OmitUIDNext leaves UIDNEXT out of SELECT/EXAMINE responses.
	OmitUIDNext bool
	// LoginUnavailable refuses LOGIN with a non-authentication NO
	// (UNAVAILABLE), as a provider in maintenance does.
	LoginUnavailable bool
}

// Options configure the listener.
type Options struct {
	// TLS, when set, serves implicit TLS (the "ssl" mode, port 993).
	TLS *tls.Config
	// StartTLS, when set, serves plaintext with STARTTLS (the "tls"
	// mode, port 143). Exactly one of TLS and StartTLS is expected.
	StartTLS *tls.Config
}

// Server is one fake provider.
type Server struct {
	mu           sync.Mutex
	users        map[string]*user
	nextValidity uint32
	violations   []string
	commands     []string
	faults       Faults
	bodyFetches  int
	clock        time.Time

	srv  *imapserver.Server
	ln   net.Listener
	addr string
}

type user struct {
	password  string
	mailboxes map[string]*mailbox
}

type mailbox struct {
	validity uint32
	nextUID  uint32
	msgs     []*message // ascending UID
}

type message struct {
	uid  uint32
	raw  []byte
	date time.Time
}

// Start listens on 127.0.0.1 and serves until Close.
func Start(opts Options) (*Server, error) {
	s := &Server{users: map[string]*user{}, nextValidity: 1000, clock: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	// MOVE is advertised so a client that tries it sends a real MOVE
	// (and is caught) rather than emulating it.
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}}
	s.srv = imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{s: s, conn: c}, &imapserver.GreetingData{}, nil
		},
		Caps:      caps,
		TLSConfig: opts.StartTLS,
		Logger:    log.New(io.Discard, "", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.addr = ln.Addr().String()
	if opts.TLS != nil {
		ln = tls.NewListener(ln, opts.TLS)
	}
	s.ln = ln
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Addr is host:port.
func (s *Server) Addr() string { return s.addr }

// Close stops the server.
func (s *Server) Close() error { return s.srv.Close() }

// AddUser creates an account with an INBOX.
func (s *Server) AddUser(name, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[name] = &user{password: password, mailboxes: map[string]*mailbox{}}
	s.createLocked(name, "INBOX")
}

// CreateMailbox adds a folder (server-side; not an IMAP CREATE).
func (s *Server) CreateMailbox(userName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createLocked(userName, name)
}

func (s *Server) createLocked(userName, name string) {
	s.nextValidity++
	s.users[userName].mailboxes[name] = &mailbox{validity: s.nextValidity, nextUID: 1}
}

// Deliver appends a message server-side and returns its UID.
func (s *Server) Deliver(userName, folder string, raw []byte) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.users[userName].mailboxes[folder]
	uid := mb.nextUID
	mb.nextUID++
	s.clock = s.clock.Add(time.Minute)
	mb.msgs = append(mb.msgs, &message{uid: uid, raw: append([]byte(nil), raw...), date: s.clock})
	return uid
}

// Remove expunges one message server-side (as another client would).
func (s *Server) Remove(userName, folder string, uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.users[userName].mailboxes[folder]
	for i, m := range mb.msgs {
		if m.uid == uid {
			mb.msgs = append(mb.msgs[:i], mb.msgs[i+1:]...)
			return
		}
	}
}

// ResetUIDValidity renumbers a folder under a new UIDVALIDITY, as a
// provider does after a mailbox rebuild. Messages keep their order and
// content; their UIDs restart at 1.
func (s *Server) ResetUIDValidity(userName, folder string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.users[userName].mailboxes[folder]
	s.nextValidity++
	mb.validity = s.nextValidity
	mb.nextUID = 1
	for _, m := range mb.msgs {
		m.uid = mb.nextUID
		mb.nextUID++
	}
	return mb.validity
}

// UIDValidity reports a folder's current UIDVALIDITY.
func (s *Server) UIDValidity(userName, folder string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.users[userName].mailboxes[folder].validity
}

// SetFaults replaces the injected faults and resets their counters.
func (s *Server) SetFaults(f Faults) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = f
	s.bodyFetches = 0
}

// Violations lists every mutating command a client attempted.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

// Commands lists every command that reached a session, in order, as
// "VERB" or "UID VERB" (no arguments, so no credentials).
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *Server) record(cmd string) {
	s.mu.Lock()
	s.commands = append(s.commands, cmd)
	s.mu.Unlock()
}

var errReadOnly = &imap.Error{Type: imap.StatusResponseTypeNo, Text: "imapfake: this fixture is read-only"}

func (s *Server) violate(cmd string) error {
	s.mu.Lock()
	s.commands = append(s.commands, cmd)
	s.violations = append(s.violations, cmd)
	s.mu.Unlock()
	return errReadOnly
}

// session is one connection. It snapshots the selected folder's
// sequence-number mapping at EXAMINE time; UID commands resolve against
// the live store, so a message expunged since then simply is not
// answered — exactly what a client sees on a real server that defers
// EXPUNGE notifications.
type session struct {
	s        *Server
	conn     *imapserver.Conn
	user     string
	selected string
	seqUIDs  []uint32
}

var _ imapserver.SessionMove = (*session)(nil)

func (ss *session) Close() error { return nil }

func (ss *session) Login(username, password string) error {
	ss.s.record("LOGIN")
	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()
	if ss.s.faults.LoginUnavailable {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable, Text: "Try again later"}
	}
	u := ss.s.users[username]
	if u == nil || u.password != password {
		return imapserver.ErrAuthFailed
	}
	ss.user = username
	return nil
}

func (ss *session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if options == nil || !options.ReadOnly {
		return nil, ss.s.violate("SELECT")
	}
	ss.s.record("EXAMINE")
	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()
	mb := ss.s.users[ss.user].mailboxes[name]
	if mb == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
	}
	ss.selected = name
	ss.seqUIDs = ss.seqUIDs[:0]
	for _, m := range mb.msgs {
		ss.seqUIDs = append(ss.seqUIDs, m.uid)
	}
	data := &imap.SelectData{
		Flags:          []imap.Flag{imap.FlagSeen},
		PermanentFlags: nil,
		NumMessages:    uint32(len(mb.msgs)),
		UIDValidity:    mb.validity,
	}
	if !ss.s.faults.OmitUIDNext {
		data.UIDNext = imap.UID(mb.nextUID)
	}
	return data, nil
}

func (ss *session) Create(string, *imap.CreateOptions) error { return ss.s.violate("CREATE") }
func (ss *session) Delete(string) error                      { return ss.s.violate("DELETE") }
func (ss *session) Rename(string, string, *imap.RenameOptions) error {
	return ss.s.violate("RENAME")
}
func (ss *session) Subscribe(string) error   { return ss.s.violate("SUBSCRIBE") }
func (ss *session) Unsubscribe(string) error { return ss.s.violate("UNSUBSCRIBE") }
func (ss *session) Append(string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, ss.s.violate("APPEND")
}
func (ss *session) Expunge(*imapserver.ExpungeWriter, *imap.UIDSet) error {
	return ss.s.violate("EXPUNGE")
}
func (ss *session) Store(*imapserver.FetchWriter, imap.NumSet, *imap.StoreFlags, *imap.StoreOptions) error {
	return ss.s.violate("STORE")
}
func (ss *session) Copy(imap.NumSet, string) (*imap.CopyData, error) {
	return nil, ss.s.violate("COPY")
}
func (ss *session) Move(*imapserver.MoveWriter, imap.NumSet, string) error {
	return ss.s.violate("MOVE")
}

func (ss *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	ss.s.record("LIST")
	ss.s.mu.Lock()
	var names []string
	for name := range ss.s.users[ss.user].mailboxes {
		names = append(names, name)
	}
	ss.s.mu.Unlock()
	sort.Strings(names)
	for _, name := range names {
		if err := w.WriteList(&imap.ListData{Delim: '/', Mailbox: name}); err != nil {
			return err
		}
	}
	return nil
}

func (ss *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	ss.s.record("STATUS")
	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()
	mb := ss.s.users[ss.user].mailboxes[name]
	if mb == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
	}
	n := uint32(len(mb.msgs))
	return &imap.StatusData{Mailbox: name, NumMessages: &n, UIDNext: imap.UID(mb.nextUID), UIDValidity: mb.validity}, nil
}

func (ss *session) Poll(*imapserver.UpdateWriter, bool) error { return nil }

func (ss *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	<-stop
	return nil
}

func (ss *session) Unselect() error {
	ss.selected = ""
	return nil
}

// live returns the selected folder's messages (under s.mu).
func (ss *session) liveLocked() (*mailbox, error) {
	mb := ss.s.users[ss.user].mailboxes[ss.selected]
	if mb == nil {
		return nil, errors.New("imapfake: selected mailbox vanished")
	}
	return mb, nil
}

func (ss *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if kind == imapserver.NumKindUID {
		ss.s.record("UID SEARCH")
	} else {
		ss.s.record("SEARCH")
	}
	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()
	mb, err := ss.liveLocked()
	if err != nil {
		return nil, err
	}
	var maxUID uint32
	if len(mb.msgs) > 0 {
		maxUID = mb.msgs[len(mb.msgs)-1].uid
	}
	var uids imap.UIDSet
	var seqs imap.SeqSet
	for i, m := range mb.msgs {
		if !matches(m, uint32(i+1), maxUID, uint32(len(mb.msgs)), criteria) {
			continue
		}
		uids.AddNum(imap.UID(m.uid))
		seqs.AddNum(uint32(i + 1))
	}
	if kind == imapserver.NumKindUID {
		return &imap.SearchData{All: uids}, nil
	}
	return &imap.SearchData{All: seqs}, nil
}

// inRanges resolves "*" (zero) to max, as RFC 3501 requires — including
// the quirk that "n:*" with n above the highest UID still matches the
// highest UID. Readers must cope with that; the fake reproduces it.
func inRanges(q, max uint32, ranges [][2]uint32) bool {
	for _, r := range ranges {
		a, b := r[0], r[1]
		if a == 0 {
			a = max
		}
		if b == 0 {
			b = max
		}
		if a > b {
			a, b = b, a
		}
		if q >= a && q <= b {
			return true
		}
	}
	return false
}

func uidRanges(set imap.UIDSet) [][2]uint32 {
	var out [][2]uint32
	for _, r := range set {
		out = append(out, [2]uint32{uint32(r.Start), uint32(r.Stop)})
	}
	return out
}

func seqRanges(set imap.SeqSet) [][2]uint32 {
	var out [][2]uint32
	for _, r := range set {
		out = append(out, [2]uint32{r.Start, r.Stop})
	}
	return out
}

func matches(m *message, seq, maxUID, maxSeq uint32, c *imap.SearchCriteria) bool {
	for _, set := range c.UID {
		if !inRanges(m.uid, maxUID, uidRanges(set)) {
			return false
		}
	}
	for _, set := range c.SeqNum {
		if !inRanges(seq, maxSeq, seqRanges(set)) {
			return false
		}
	}
	day := time.Date(m.date.Year(), m.date.Month(), m.date.Day(), 0, 0, 0, 0, time.UTC)
	if !c.Since.IsZero() && day.Before(c.Since) {
		return false
	}
	if !c.Before.IsZero() && !day.Before(c.Before) {
		return false
	}
	for _, h := range c.Header {
		if !headerContains(m.raw, h.Key, h.Value) {
			return false
		}
	}
	for _, not := range c.Not {
		if matches(m, seq, maxUID, maxSeq, &not) {
			return false
		}
	}
	for _, or := range c.Or {
		if !matches(m, seq, maxUID, maxSeq, &or[0]) && !matches(m, seq, maxUID, maxSeq, &or[1]) {
			return false
		}
	}
	return true
}

func headerContains(raw []byte, key, value string) bool {
	head := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		head = raw[:i]
	}
	prefix := strings.ToLower(key) + ":"
	for _, line := range strings.Split(string(head), "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(l, prefix) && strings.Contains(l, strings.ToLower(value)) {
			return true
		}
	}
	return false
}

func (ss *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	uidMode := false
	if _, ok := numSet.(imap.UIDSet); ok {
		uidMode = true
		ss.s.record("UID FETCH")
	} else {
		ss.s.record("FETCH")
	}
	for _, bs := range options.BodySection {
		if !bs.Peek {
			return ss.s.violate("FETCH BODY[] without PEEK")
		}
	}
	for _, bs := range options.BinarySection {
		if !bs.Peek {
			return ss.s.violate("FETCH BINARY[] without PEEK")
		}
	}

	type hit struct {
		seq uint32
		msg *message
	}
	ss.s.mu.Lock()
	mb, err := ss.liveLocked()
	if err != nil {
		ss.s.mu.Unlock()
		return err
	}
	var maxUID uint32
	if len(mb.msgs) > 0 {
		maxUID = mb.msgs[len(mb.msgs)-1].uid
	}
	var hits []hit
	for i, m := range mb.msgs {
		seq := uint32(i + 1)
		if uidMode {
			if !inRanges(m.uid, maxUID, uidRanges(numSet.(imap.UIDSet))) {
				continue
			}
		} else if !inRanges(seq, uint32(len(mb.msgs)), seqRanges(numSet.(imap.SeqSet))) {
			continue
		}
		hits = append(hits, hit{seq, m})
	}
	faults := ss.s.faults
	ss.s.mu.Unlock()

	repeat := 1
	if faults.DuplicateFetch {
		repeat = 2
	}
	for _, h := range hits {
		for r := 0; r < repeat; r++ {
			if err := ss.writeMessage(w, h.seq, h.msg, options); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ss *session) writeMessage(w *imapserver.FetchWriter, seq uint32, m *message, options *imap.FetchOptions) error {
	for _, bs := range options.BodySection {
		if bs.Specifier == imap.PartSpecifierNone && len(bs.Part) == 0 {
			ss.s.mu.Lock()
			ss.s.bodyFetches++
			drop := ss.s.faults.DropOnBodyFetch != 0 && ss.s.bodyFetches == ss.s.faults.DropOnBodyFetch
			ss.s.mu.Unlock()
			if drop {
				_ = ss.conn.NetConn().Close()
				return fmt.Errorf("imapfake: injected connection drop")
			}
		}
	}
	rw := w.CreateMessage(seq)
	rw.WriteUID(imap.UID(m.uid))
	if options.RFC822Size {
		rw.WriteRFC822Size(int64(len(m.raw)))
	}
	if options.InternalDate {
		rw.WriteInternalDate(m.date)
	}
	if options.Flags {
		rw.WriteFlags(nil)
	}
	for _, bs := range options.BodySection {
		buf := imapserver.ExtractBodySection(bytes.NewReader(m.raw), bs)
		wc := rw.WriteBodySection(bs, int64(len(buf)))
		if _, err := wc.Write(buf); err != nil {
			return err
		}
		if err := wc.Close(); err != nil {
			return err
		}
	}
	return rw.Close()
}
