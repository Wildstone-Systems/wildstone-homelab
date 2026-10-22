package runtimedb

// Conversation identity and cross-bot handoff (ADR-W030 A1/A2, P6).
//
// "One assistant, explicit profiles, explicit handoffs" (A1) means a
// person can be talking to the Household bot, say "take this to the
// operator side", and continue — while the bot they are speaking through
// remains a firm authority boundary.
//
// Two rules do all the work here, and they pull in opposite directions
// on purpose:
//
//   Continuity: a conversation has an identity of its own that outlives
//   the session it started in, so the destination knows what was being
//   discussed. Sessions are untouched (0003/0014 keep their attribution
//   exactly); this joins them.
//
//   Containment: what CROSSES is filtered by what the destination may
//   see, and the destination's authority is recomputed from scratch. A
//   handoff continues context; it does not carry privilege, and it does
//   not execute the pending operation. Tool traffic never crosses at
//   all — a reading taken under one profile's permissions is not a fact
//   the other profile was allowed to obtain.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ButlerConversation is the identity a conversation keeps across bots.
type ButlerConversation struct {
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"` // "bc-<seq>"
	Member    string    `json:"member"`
	Subject   string    `json:"subject,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func conversationID(seq int64) string { return fmt.Sprintf("bc-%d", seq) }

func parseConversationID(id string) (int64, error) {
	var seq int64
	if _, err := fmt.Sscanf(id, "bc-%d", &seq); err != nil || conversationID(seq) != id {
		return 0, fmt.Errorf("runtimedb: malformed conversation id %q", id)
	}
	return seq, nil
}

// StartButlerConversation opens a conversation identity for an admitted
// member. It refuses anyone who is not currently admitted, so a
// conversation cannot outlive the admission that justified it.
func (db *DB) StartButlerConversation(member, subject string) (string, error) {
	m, err := db.GetButlerMember(member)
	if err != nil {
		return "", err
	}
	if !m.Active() {
		return "", ErrNotAdmitted
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_conversations (person_id, created_at, subject) VALUES (?, ?, ?)`,
		m.PersonID, now(), subject)
	if err != nil {
		return "", fmt.Errorf("runtimedb: starting conversation: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	return conversationID(seq), nil
}

// AttachSessionToConversation joins one recorded session to a
// conversation. Attaching twice is not an error: an adapter that retries
// converges on one row rather than failing a delivered reply.
func (db *DB) AttachSessionToConversation(conversationID, sessionID string) error {
	conv, err := parseConversationID(conversationID)
	if err != nil {
		return err
	}
	var sess int64
	if _, err := fmt.Sscanf(sessionID, "bs-%d", &sess); err != nil {
		return fmt.Errorf("runtimedb: malformed session id %q", sessionID)
	}
	_, err = db.sql.Exec(
		`INSERT INTO butler_conversation_sessions (conversation, session, attached_at)
		 VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, conv, sess, now())
	if err != nil {
		return fmt.Errorf("runtimedb: attaching session to conversation: %w", err)
	}
	return nil
}

// GetButlerConversation returns one conversation identity.
func (db *DB) GetButlerConversation(id string) (ButlerConversation, error) {
	seq, err := parseConversationID(id)
	if err != nil {
		return ButlerConversation{}, err
	}
	var c ButlerConversation
	var created string
	err = db.sql.QueryRow(
		`SELECT c.seq, m.member, c.subject, c.created_at
		 FROM butler_conversations c JOIN butler_members m ON m.person_id = c.person_id
		 WHERE c.seq = ?`, seq).
		Scan(&c.Seq, &c.Member, &c.Subject, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerConversation{}, ErrNotFound
	}
	if err != nil {
		return ButlerConversation{}, fmt.Errorf("runtimedb: reading conversation: %w", err)
	}
	c.ID = conversationID(c.Seq)
	if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ButlerConversation{}, err
	}
	return c, nil
}

// MaxHandoffTurns bounds what a handoff carries. A handoff continues a
// conversation; it does not import a history, and a destination handed
// forty turns is being handed a context window rather than a thread.
const MaxHandoffTurns = 12

// HandoffContext is what a destination surface is given, plus the
// accounting that makes "no privileged context crossed" checkable.
type HandoffContext struct {
	ConversationID string        `json:"conversationId"`
	Member         string        `json:"member"`
	FromSurface    ButlerSurface `json:"fromSurface"`
	ToSurface      ButlerSurface `json:"toSurface"`
	// Turns is the filtered text of the conversation so far, oldest
	// first. Tool traffic is stripped: TranscriptJSON and PlanIDs are
	// empty on every turn here, and that is not tidying — a reading
	// taken under the operator's permissions is not a fact the household
	// surface was allowed to obtain.
	Turns []ButlerTurn `json:"turns"`
	// Withheld counts what did not cross, so the claim has a number
	// behind it.
	Withheld int `json:"withheld"`
}

// HandoffConversation moves a conversation between the two bots for the
// member who owns it.
//
// It recomputes authority at the destination rather than carrying it:
// the member must be admitted NOW, and moving to the operator surface
// requires the operator profile. Household admission does not grant
// operator access (A2.1), and a handoff is not a way around that — a
// household member who asks to be handed to the operator Butler is
// refused here, at the boundary, rather than arriving with a smaller
// tool set and a confusing explanation.
func (db *DB) HandoffConversation(conversationID string, from, to ButlerSurface, fromSessionID string) (HandoffContext, error) {
	conv, err := db.GetButlerConversation(conversationID)
	if err != nil {
		return HandoffContext{}, err
	}
	if !ValidButlerSurface(from) || !ValidButlerSurface(to) {
		return HandoffContext{}, fmt.Errorf("runtimedb: handoff surfaces must be admin|family")
	}
	if from == to {
		return HandoffContext{}, fmt.Errorf("runtimedb: a handoff moves between the two bots; %q is both", from)
	}
	member, err := db.GetButlerMember(conv.Member)
	if err != nil {
		return HandoffContext{}, err
	}
	if !member.Active() {
		return HandoffContext{}, ErrNotAdmitted
	}
	// The admin surface is the Owner surface. Before 0039 this asked
	// whether the person's profile was 'operator'; that column now says
	// role = 'owner' and means the same thing, because the migration
	// mapped the one to the other. Members do not reach it (§3.2, and
	// §18.1: "A Member cannot access Console").
	if to == ButlerSurfaceAdmin && member.Role != RoleOwner {
		return HandoffContext{}, fmt.Errorf(
			"%w: %s is a member of this household, and member admission does not grant owner access (ADR-W030 A2.1, design §3.2)",
			ErrNotAdmitted, conv.Member)
	}

	turns, withheld, err := db.conversationTurnsFor(conv.Seq, to)
	if err != nil {
		return HandoffContext{}, err
	}

	var fromSession any
	if fromSessionID != "" {
		var s int64
		if _, err := fmt.Sscanf(fromSessionID, "bs-%d", &s); err == nil {
			fromSession = s
		}
	}
	// The handoff names the same person the conversation does, taken
	// from the conversation row rather than resolved again: they are by
	// construction the same person, and two lookups could disagree.
	if _, err := db.sql.Exec(
		`INSERT INTO butler_handoffs (conversation, person_id, from_surface, to_surface, from_session, at, carried, withheld)
		 SELECT ?, person_id, ?, ?, ?, ?, ?, ? FROM butler_conversations WHERE seq = ?`,
		conv.Seq, string(from), string(to), fromSession, now(), len(turns), withheld, conv.Seq,
	); err != nil {
		return HandoffContext{}, fmt.Errorf("runtimedb: recording handoff: %w", err)
	}
	return HandoffContext{
		ConversationID: conv.ID, Member: conv.Member,
		FromSurface: from, ToSurface: to, Turns: turns, Withheld: withheld,
	}, nil
}

// conversationTurnsFor returns the conversation's text as the
// destination surface may see it, newest MaxHandoffTurns, oldest first.
//
// The filter is by the surface each session ran on, not by a label on
// the turn: what an operator session said may quote the Statefile, name
// nodes, or explain a plan, and none of that belongs on the family
// surface. Turns from the destination's own surface cross; turns from
// the other one are counted and left behind.
func (db *DB) conversationTurnsFor(conv int64, to ButlerSurface) ([]ButlerTurn, int, error) {
	rows, err := db.sql.Query(
		`SELECT s.seq, s.surface FROM butler_conversation_sessions cs
		 JOIN butler_sessions s ON s.seq = cs.session
		 WHERE cs.conversation = ? ORDER BY s.seq`, conv)
	if err != nil {
		return nil, 0, fmt.Errorf("runtimedb: reading conversation sessions: %w", err)
	}
	type sessionRef struct {
		seq     int64
		surface ButlerSurface
	}
	var sessions []sessionRef
	for rows.Next() {
		var ref sessionRef
		var surface string
		if err := rows.Scan(&ref.seq, &surface); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ref.surface = ButlerSurface(surface)
		sessions = append(sessions, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var carried []ButlerTurn
	withheld := 0
	for _, ref := range sessions {
		turns, err := db.ListButlerTurns(ref.seq)
		if err != nil {
			return nil, 0, err
		}
		if ref.surface != to {
			withheld += len(turns)
			continue
		}
		for _, t := range turns {
			// Text only. Tool traffic and proposed plan ids do not
			// cross: the destination re-reads whatever it needs under
			// its own permissions, which is the whole point of
			// recomputing authority rather than carrying it.
			carried = append(carried, ButlerTurn{
				Ordinal: t.Ordinal, Role: t.Role, Text: t.Text, CreatedAt: t.CreatedAt,
				Source: t.Source,
			})
		}
	}
	if len(carried) > MaxHandoffTurns {
		withheld += len(carried) - MaxHandoffTurns
		carried = carried[len(carried)-MaxHandoffTurns:]
	}
	return carried, withheld, nil
}

// ButlerHandoff is one recorded move between bots.
type ButlerHandoff struct {
	Seq         int64         `json:"seq"`
	Member      string        `json:"member"`
	FromSurface ButlerSurface `json:"fromSurface"`
	ToSurface   ButlerSurface `json:"toSurface"`
	At          time.Time     `json:"at"`
	Carried     int           `json:"carried"`
	Withheld    int           `json:"withheld"`
}

// ListButlerHandoffs returns a conversation's handoffs, oldest first.
func (db *DB) ListButlerHandoffs(conversationID string) ([]ButlerHandoff, error) {
	conv, err := parseConversationID(conversationID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT h.seq, m.member, h.from_surface, h.to_surface, h.at, h.carried, h.withheld
		 FROM butler_handoffs h JOIN butler_members m ON m.person_id = h.person_id
		 WHERE h.conversation = ? ORDER BY h.seq`, conv)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing handoffs: %w", err)
	}
	defer rows.Close()
	var out []ButlerHandoff
	for rows.Next() {
		var h ButlerHandoff
		var from, to, at string
		if err := rows.Scan(&h.Seq, &h.Member, &from, &to, &at, &h.Carried, &h.Withheld); err != nil {
			return nil, err
		}
		h.FromSurface, h.ToSurface = ButlerSurface(from), ButlerSurface(to)
		if h.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
