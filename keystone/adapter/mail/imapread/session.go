package imapread

// This is the ONLY file in the package that may import imapclient.
// Everything else talks to the provider through readSession, whose
// method set is the whole of what the reader can ask an IMAP server:
// examine a folder read-only, search it by UID, and fetch with PEEK.
// There is no method that could store a flag, copy, move, expunge,
// append, create, delete or rename — so no code path above this file
// can issue one, and TestReadSessionSurfaceIsClosed pins that set.
// TestOnlySessionFileTalksIMAP pins that nothing else imports the
// client, and the fake server's violation log proves it on the wire.

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// readSession is the closed, read-only command surface.
type readSession interface {
	// examine opens a folder with EXAMINE (never SELECT).
	examine(folder string) (folderState, error)
	// searchUIDs runs UID SEARCH.
	searchUIDs(q searchQuery) ([]uint32, error)
	// fetchHeaders runs UID FETCH (UID RFC822.SIZE BODY.PEEK[HEADER]<0.n>).
	fetchHeaders(uids []uint32, maxHeader int) (map[uint32]fetchedHeader, error)
	// fetchBody runs UID FETCH (UID BODY.PEEK[]<0.n>) for one message.
	fetchBody(uid uint32, maxBytes int64) (raw []byte, found bool, err error)
	// logout ends the session.
	logout()
}

type folderState struct {
	UIDValidity uint32
	UIDNext     uint32
	Messages    uint32
}

// searchQuery is the subset of SEARCH the reader uses.
type searchQuery struct {
	// AfterUID matches UIDs strictly greater than it (UID n+1:*).
	AfterUID uint32
	UseUID   bool
	// Highest asks for the highest UID only (UID *).
	Highest bool
	Since   time.Time
	Before  time.Time
	From    string
	Subject string
}

type fetchedHeader struct {
	Size   int64
	Header []byte
}

var (
	errAuth          = errors.New("imapread: authentication failed")
	errFolderMissing = errors.New("imapread: folder does not exist")
)

// dialSession connects, verifies TLS, and logs in.
func dialSession(ctx context.Context, cfg *Config) (readSession, error) {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		ServerName: host,
		RootCAs:    cfg.RootCAs,
		MinVersion: tls.VersionTLS12,
	}
	if cfg.ServerName != "" {
		tlsConfig.ServerName = cfg.ServerName
	}
	d := &net.Dialer{Timeout: cfg.dialTimeout()}
	raw, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	// A cancelled context closes the socket, which unblocks every
	// pending command with an error.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })

	options := &imapclient.Options{TLSConfig: tlsConfig}
	var c *imapclient.Client
	switch cfg.Security {
	case SecurityStartTLS:
		c, err = imapclient.NewStartTLS(raw, options)
		if err != nil {
			stop()
			_ = raw.Close()
			return nil, err
		}
	default:
		conn := tls.Client(raw, tlsConfig)
		if err := conn.HandshakeContext(ctx); err != nil {
			stop()
			_ = raw.Close()
			return nil, err
		}
		c = imapclient.New(conn, options)
	}
	s := &clientSession{c: c, stop: stop}
	if err := c.WaitGreeting(); err != nil {
		s.close()
		return nil, err
	}
	if err := c.Login(cfg.loginName(), cfg.Password).Wait(); err != nil {
		s.close()
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo {
			switch ie.Code {
			case imap.ResponseCodeUnavailable, imap.ResponseCodeServerBug, imap.ResponseCodeInUse:
				return nil, err
			}
			return nil, errAuth
		}
		return nil, err
	}
	return s, nil
}

type clientSession struct {
	c    *imapclient.Client
	stop func() bool
}

var _ readSession = (*clientSession)(nil)

func (s *clientSession) close() {
	s.stop()
	_ = s.c.Close()
}

func (s *clientSession) logout() {
	_ = s.c.Logout().Wait()
	s.close()
}

func (s *clientSession) examine(folder string) (folderState, error) {
	// ReadOnly makes this EXAMINE. It is the only Select call in the
	// package and it is never anything else.
	data, err := s.c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo {
			return folderState{}, errFolderMissing
		}
		return folderState{}, err
	}
	return folderState{UIDValidity: data.UIDValidity, UIDNext: uint32(data.UIDNext), Messages: data.NumMessages}, nil
}

func (s *clientSession) searchUIDs(q searchQuery) ([]uint32, error) {
	var criteria imap.SearchCriteria
	switch {
	case q.Highest:
		criteria.UID = []imap.UIDSet{{imap.UIDRange{Start: 0, Stop: 0}}}
	case q.UseUID:
		criteria.UID = []imap.UIDSet{{imap.UIDRange{Start: imap.UID(q.AfterUID + 1), Stop: 0}}}
	}
	criteria.Since = q.Since
	criteria.Before = q.Before
	if q.From != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: q.From})
	}
	if q.Subject != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "Subject", Value: q.Subject})
	}
	data, err := s.c.UIDSearch(&criteria, nil).Wait()
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, u := range data.AllUIDs() {
		out = append(out, uint32(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func uidSet(uids []uint32) imap.UIDSet {
	var set imap.UIDSet
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	return set
}

func (s *clientSession) fetchHeaders(uids []uint32, maxHeader int) (map[uint32]fetchedHeader, error) {
	out := map[uint32]fetchedHeader{}
	if len(uids) == 0 {
		return out, nil
	}
	cmd := s.c.Fetch(uidSet(uids), &imap.FetchOptions{
		UID:        true,
		RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{{
			Specifier: imap.PartSpecifierHeader,
			Peek:      true,
			Partial:   &imap.SectionPartial{Offset: 0, Size: int64(maxHeader) + 1},
		}},
	})
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		var uid uint32
		var h fetchedHeader
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = uint32(item.UID)
			case imapclient.FetchItemDataRFC822Size:
				h.Size = item.Size
			case imapclient.FetchItemDataBodySection:
				if item.Literal != nil {
					// Bounded even if the server ignores the partial.
					b, err := io.ReadAll(io.LimitReader(item.Literal, int64(maxHeader)+1))
					if err != nil {
						_ = cmd.Close()
						return nil, err
					}
					h.Header = b
				}
			}
		}
		if uid == 0 {
			continue
		}
		if _, dup := out[uid]; dup {
			// A server may answer the same UID twice (unsolicited
			// updates); the first complete answer stands.
			continue
		}
		out[uid] = h
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *clientSession) fetchBody(uid uint32, maxBytes int64) ([]byte, bool, error) {
	cmd := s.c.Fetch(uidSet([]uint32{uid}), &imap.FetchOptions{
		UID: true,
		BodySection: []*imap.FetchItemBodySection{{
			Peek:    true,
			Partial: &imap.SectionPartial{Offset: 0, Size: maxBytes + 1},
		}},
	})
	var raw []byte
	found := false
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		var got uint32
		var body []byte
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				got = uint32(item.UID)
			case imapclient.FetchItemDataBodySection:
				if item.Literal != nil {
					b, err := io.ReadAll(io.LimitReader(item.Literal, maxBytes+1))
					if err != nil {
						_ = cmd.Close()
						return nil, false, err
					}
					body = b
				}
			}
		}
		if got == uid && !found && body != nil {
			raw, found = body, true
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, false, err
	}
	return raw, found, nil
}
