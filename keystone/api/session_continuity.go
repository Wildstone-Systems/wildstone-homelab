package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const resumeOfferLifetime = 10 * time.Minute

const trustedSurfaceRedirect = "Please use the household dashboard."

func (s *Server) sessionRecencyWindow() time.Duration {
	settings, _, err := s.DB.GetButlerSettings()
	if err != nil || settings.SessionRecency < 60 {
		return 30 * time.Minute
	}
	return time.Duration(settings.SessionRecency) * time.Second
}

type safeSessionCandidate struct {
	runtimedb.SessionCandidate
	ShowSubject bool
	ShowTime    bool
}

type continuationConnection struct {
	Ref        int64
	Name       string
	Disclosure runtimedb.Disclosure
	Trusted    bool
}

func externalContinuationConnection(c runtimedb.Connection) continuationConnection {
	return continuationConnection{Ref: c.Seq, Name: c.Name(), Disclosure: c.Disclosure}
}

func trustedContinuationConnection(name string) continuationConnection {
	return continuationConnection{Ref: 0, Name: name, Trusted: true}
}

func resumeTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newResumeToken() (string, error) {
	var raw [9]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func trustedBuiltInConnection(name string) bool {
	return name == butlerlive.ConsoleConnection || name == portal.Connection
}

// Built-in replies leave synchronously with the recorded turn. External
// transports record outbound provenance later, when their reply is queued.
func (s *Server) noteBuiltInSessionReply(sessionID, connection string) {
	if !trustedBuiltInConnection(connection) {
		return
	}
	if err := s.DB.NoteButlerSessionConnection(sessionID, "outbound", connection, 0); err != nil {
		s.Log.Error("recording built-in session outbound connection", "session", sessionID, "err", err)
	}
}

// candidateOnConnection applies disclosure before selection. Returning false
// hides the candidate's existence; callers must not let count or ordering from
// the unfiltered set escape.
func candidateOnConnection(c runtimedb.SessionCandidate, current continuationConnection) (safeSessionCandidate, bool) {
	if current.Trusted {
		return safeSessionCandidate{SessionCandidate: c, ShowSubject: true, ShowTime: true}, true
	}
	shown := safeSessionCandidate{SessionCandidate: c, ShowSubject: true, ShowTime: true}
	if len(c.Provenance) == 0 {
		return safeSessionCandidate{}, false
	}
	for _, source := range c.Provenance {
		if source.ConnectionRef == 0 && trustedBuiltInConnection(source.ConnectionName) {
			// A trusted-UI conversation is not announced over external
			// messaging. Even a neutral placeholder would disclose existence.
			return safeSessionCandidate{}, false
		}
		if source.Connection == nil {
			return safeSessionCandidate{}, false
		}
		a, b := source.Connection.Disclosure, current.Disclosure
		resolved := a.Reach != runtimedb.ReachUnresolved && a.Surface != runtimedb.TrustUnresolved &&
			a.Encryption != runtimedb.EncryptionUnresolved && a.Retention != runtimedb.RetentionUnresolved &&
			b.Reach != runtimedb.ReachUnresolved && b.Surface != runtimedb.TrustUnresolved &&
			b.Encryption != runtimedb.EncryptionUnresolved && b.Retention != runtimedb.RetentionUnresolved
		// Existence, subject and activity are three disclosures. A common reach
		// and surface permits the neutral candidate; encryption permits its
		// subject; retention permits its activity timestamp.
		existence := resolved && a.Reach == b.Reach && a.Surface == b.Surface
		if !existence {
			return safeSessionCandidate{}, false
		}
		shown.ShowSubject = shown.ShowSubject && a.Encryption == b.Encryption
		shown.ShowTime = shown.ShowTime && a.Retention == b.Retention
	}
	return shown, true
}

func (s *Server) chatSessionCandidates(c chatCommandCtx) ([]safeSessionCandidate, bool, error) {
	broken, err := s.DB.ChatThreadBrokenAt(c.At, c.Conversation, c.Identity)
	if err != nil {
		return nil, false, err
	}
	return s.sessionCandidates(c.Identity, c.At.Butler, externalContinuationConnection(c.At), broken)
}

func (s *Server) sessionCandidates(identity, agent string, current continuationConnection, after time.Time) ([]safeSessionCandidate, bool, error) {
	all, err := s.DB.RecentSessionCandidates(identity, agent,
		time.Now().Add(-s.sessionRecencyWindow()), after)
	if err != nil {
		return nil, false, err
	}
	safe := make([]safeSessionCandidate, 0, len(all))
	for _, candidate := range all {
		if shown, ok := candidateOnConnection(candidate, current); ok {
			safe = append(safe, shown)
		}
	}
	return safe, len(safe) != len(all), nil
}

// trustedContinuation is the trusted-UI resolver. Homepage uses it for
// automatic continuation; Console already presents the same sessions in its
// conversation rail and continues the one the Owner explicitly opens.
func (s *Server) trustedContinuation(identity, agent, connection string) (*turnContext, int, error) {
	candidates, _, err := s.sessionCandidates(identity, agent, trustedContinuationConnection(connection), time.Time{})
	if err != nil {
		return nil, 0, err
	}
	if len(candidates) != 1 {
		return nil, len(candidates), nil
	}
	sess, err := s.DB.GetButlerSession(candidates[0].SessionID)
	if err != nil {
		return nil, 0, err
	}
	return &turnContext{SessionID: sess.ID, History: historyFrom(sess.Turns)}, 1, nil
}

func continuationChoiceError(n int) error {
	return fmt.Errorf("your message was not processed: %d recent conversations match; choose one from conversation history and send it again, or start a new conversation", n)
}

func renderCandidate(c safeSessionCandidate, token string) string {
	label := "Recent conversation"
	if c.ShowSubject && strings.TrimSpace(c.Subject) != "" {
		label = strings.TrimSpace(c.Subject)
		if len([]rune(label)) > 72 {
			label = string([]rune(label)[:72]) + "…"
		}
	}
	if c.ShowTime {
		label += " (" + c.LastTurnAt.UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	return fmt.Sprintf("%s — `/resume %s`", label, token)
}

// Eight subjects of at most 72 four-byte runes, their timestamps and tokens,
// plus the prompt and navigation, fit within runtimedb.MaxChatBodyBytes.
const sessionChoicesPerPage = 8

func (s *Server) offerChatSessions(c chatCommandCtx, candidates []safeSessionCandidate, page int) (string, error) {
	if len(candidates) == 0 {
		return "There are no recent conversations I can show here. Send a message to start one, or use /new.", nil
	}
	// Only the already-filtered candidates contribute to page boundaries
	// or counts. Validate before multiplying so arbitrary page input cannot
	// overflow the slice bounds.
	pages := (len(candidates)-1)/sessionChoicesPerPage + 1
	if page < 1 || page > pages {
		return "That page is no longer available. Use /sessions for the current choices.", nil
	}
	start := (page - 1) * sessionChoicesPerPage
	end := min(start+sessionChoicesPerPage, len(candidates))
	lines := []string{"Your message was not processed. Choose a conversation below, then send it again — or use /new:"}
	for _, candidate := range candidates[start:end] {
		token, err := newResumeToken()
		if err != nil {
			return "", err
		}
		if err := s.DB.CreateSessionResumeOffer(resumeTokenHash(token), c.Identity, c.At.Butler,
			c.At.Seq, candidate.SessionID, time.Now().Add(resumeOfferLifetime)); err != nil {
			return "", err
		}
		lines = append(lines, renderCandidate(candidate, token))
	}
	if page > 1 {
		lines = append(lines, fmt.Sprintf("Previous page: `/sessions %d`.", page-1))
	}
	if page < pages {
		lines = append(lines, fmt.Sprintf("Next page: `/sessions %d`.", page+1))
	}
	return strings.Join(lines, "\n"), nil
}

// resolveChatContinuation decides without loading any candidate transcript.
// Context is fetched only after the candidate set has become exactly one.
func (s *Server) resolveChatContinuation(c chatCommandCtx) (*turnContext, string, error) {
	if prior := s.chatThread(c.At, c.Conversation, c.Identity); prior != nil {
		return prior, "", nil
	}
	candidates, protected, err := s.chatSessionCandidates(c)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, "", nil
		}
		return nil, "", err
	}
	switch len(candidates) {
	case 0:
		if protected {
			return nil, trustedSurfaceRedirect, nil
		}
		return nil, "", nil
	case 1:
		sess, err := s.DB.GetButlerSession(candidates[0].SessionID)
		if err != nil {
			return nil, "", err
		}
		return &turnContext{SessionID: sess.ID, History: historyFrom(sess.Turns)}, "", nil
	default:
		body, err := s.offerChatSessions(c, candidates, 1)
		return nil, body, err
	}
}
