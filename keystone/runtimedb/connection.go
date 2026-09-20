package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Connections (ADR-W033 B7, design §8.1).
//
// A Connection is one way a Person reaches one Agent: a bot on a
// transport, with its own credential, admission and lifecycle. It
// belongs to exactly one Agent, and an Agent may have many, including
// several on the same transport.
//
// The row is butler_addresses. That table predates the name — it was
// written as "where a Butler answers" (ADR-W031 A7, "the bot a message
// arrives at IS the address") — and it is the same thing: one live
// address per (transport, external_id), naming one Butler. So this file
// adds the vocabulary and the lookups rather than a second table.
//
// Not to be confused with integration_connections, which is an account
// on somebody else's service — an IMAP mailbox, a CalDAV calendar. The
// design keeps the two apart (§12.2 separates the household secret
// store from the infrastructure one) and the word is unfortunately the
// same. This one is how a Person reaches an Agent.
//
// What replaced what: admission, claim codes, threads and the outbound
// queue used to key on a SURFACE, and so did the question ledger. A
// surface is a class of Agents, not an Agent, so admitting an account
// to `admin` admitted it to every Owner Agent at once — including ones
// created later. Migration 0045 pointed all five at the Connection
// instead.

// ErrNoConnection is returned when no live Connection answers at an
// address. A message arriving at a bot nothing is assigned to has no
// Agent to run as, and inventing one would be answering as whichever
// Agent happened to sort first.
var ErrNoConnection = errors.New("runtimedb: no connection answers at that address")

// Connection is one Agent's reach on one transport.
//
// The accessors below are Chat-prefixed because this package already
// has ListConnections for integration_connections — an account on
// somebody else's service, which is a different thing wearing the same
// word. The type keeps the ADR's name; the methods say which kind.
type Connection struct {
	Seq        int64  `json:"seq"`
	Butler     string `json:"butler"`
	Transport  string `json:"transport"`
	ExternalID string `json:"externalId"`
	Label      string `json:"label,omitempty"`
	// Surface is the Agent's class ceiling, carried here because
	// admission depends on it: an Owner Agent's Connection admits
	// Owners only (B9). It is the Agent's property, not the
	// Connection's — slice 9 (#210) renames it to the agent class the
	// design uses.
	Surface ButlerSurface `json:"surface"`
	// AgentClass is the same ceiling under slice 9's vocabulary, read
	// alongside Surface rather than instead of it so that OwnerAgent
	// can take the narrower of the two. 0058 makes them agree; this
	// does not depend on their agreeing.
	AgentClass string `json:"agentClass"`

	// Admission is the Connection's OWN admission policy (design §8.1,
	// §8.3; migration 0063) — who may address the Agent here.
	//
	// It is not the Agent class restated. The Agent class is a ceiling
	// this cannot rise above, and within that ceiling a deployment may
	// narrow a Connection by hand: a Member Agent's public bot can be
	// set to admit Owners only, and nothing about the Agent changes.
	// Admits() is where the two meet.
	Admission Admission `json:"admission"`
	// AdmissionSubject is which KIND of subject that policy admits.
	// MVP is Person; Group is a seam that admits nobody (§8.3).
	AdmissionSubject AdmissionSubject `json:"admissionSubject"`
}

// Name identifies this Connection in an authority check and in a
// refusal (design §10.1 carries it on every person-facing query).
//
// Transport and external id together, because that pair is what makes
// one chat Connection distinct from another and it is what an inbound
// message already has to hand. The label is deliberately not used: a
// person can rename one, and an identifier that changes when somebody
// edits a display name is not an identifier.
func (c Connection) Name() string { return c.Transport + ":" + c.ExternalID }

// ChatConnectionAt resolves the live Connection at one address — which is
// what an inbound message has to hand: the bot it arrived at.
//
// BOTH halves are required, and a lookup on external_id alone would be
// wrong: butler_addresses_live is unique on (transport, external_id),
// not on external_id, and AddButlerAddress takes transport-local ids on
// purpose. Two live addresses on different transports may share one, so
// an external id alone can resolve an adapter to the wrong Agent — and
// therefore to another Agent's queue.
func (db *DB) ChatConnectionAt(transport, externalID string) (Connection, error) {
	return scanChatConnection(db.sql.QueryRow(chatConnectionColumns+`
		 WHERE a.transport = ? AND a.external_id = ? AND a.revoked_at = ''`,
		transport, externalID))
}

// ChatConnectionBySeq reads one by its seq, for a caller holding a
// stored reference rather than an address.
//
// Live only, like its siblings. A binding, a queued message and a
// question all keep the seq they were written with, and revoking an
// address does not rewrite them — so without this a household rule
// would resolve a recipient onto a bot nobody polls any more, queue the
// message, and report success. The message would sit there until it
// expired, with nothing anywhere saying it had not been sent. Refusing
// is the honest answer: there is no longer a way to reach that person
// at that Agent.
func (db *DB) ChatConnectionBySeq(seq int64) (Connection, error) {
	return scanChatConnection(db.sql.QueryRow(chatConnectionColumns+
		` WHERE a.seq = ? AND a.revoked_at = ''`, seq))
}

// LEFT JOIN, not JOIN. A Connection with no admission row still has to
// RESOLVE, because the refusal it earns is one this package writes --
// naming the Connection and saying no policy is on record -- and a row
// that vanishes from the result set instead comes back as
// ErrNoConnection, which says the address does not exist. Those are
// different facts and a household chasing the wrong one finds nothing
// wrong with the address it was told about.
const chatConnectionColumns = `SELECT a.seq, b.name, a.transport, a.external_id, a.label,
	        b.surface, b.agent_class,
	        COALESCE(ca.admits, ''), COALESCE(ca.subject_kind, '')
	 FROM butler_addresses a
	 JOIN butlers b ON b.seq = a.butler
	 LEFT JOIN connection_admission ca ON ca.connection = a.seq`

func scanChatConnection(row rowScanner) (Connection, error) {
	var c Connection
	var surface, agentClass, admits, subject string
	err := row.Scan(&c.Seq, &c.Butler, &c.Transport, &c.ExternalID, &c.Label,
		&surface, &agentClass, &admits, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNoConnection
	}
	if err != nil {
		return Connection{}, fmt.Errorf("runtimedb: reading a connection: %w", err)
	}
	c.Surface = ButlerSurface(surface)
	c.AgentClass = agentClass
	c.Admission = parseAdmission(admits)
	c.AdmissionSubject = parseAdmissionSubject(subject)
	return c, nil
}

// Admission is what a Connection admits: which Person role may address
// the Agent over it (design 8.1, 8.3; migration 0063).
//
// It is a CLOSED SET WITH A NAMED UNRESOLVED MEMBER, and that is the
// whole point of the type. The value it replaces was a bool derived
// from a string comparison, so every value that was not exactly
// "admin" meant "the household may talk here" -- an unreadable policy
// and the most permissive policy gave the same answer. Unresolved is a
// third answer, it is the one an absent or unrecognised value produces,
// and Admits refuses it.
type Admission string

const (
	// AdmissionUnresolved is no policy on record, or one this build
	// does not recognise. It admits nobody.
	AdmissionUnresolved Admission = ""
	// AdmitOwners admits Owners and nobody else (design 8.3).
	AdmitOwners Admission = "owners"
	// AdmitHousehold admits any Person on record, Owner or Member.
	AdmitHousehold Admission = "household"
)

// AdmissionSubject is the KIND of subject a policy admits (design 8.3:
// "The admission schema supports subjects of type Person and Group").
//
// MVP admits Person subjects. A Group subject is recognised, refused,
// and refused for a stated reason rather than being absent from the
// vocabulary -- 8.3 makes group support a seam, and a seam that
// resolves to "unrecognised value" cannot be told apart from a typo.
type AdmissionSubject string

const (
	// AdmissionSubjectUnresolved is no subject kind on record, or one
	// this build does not recognise. It admits nobody.
	AdmissionSubjectUnresolved AdmissionSubject = ""
	// AdmissionSubjectPerson is MVP: the policy admits named People.
	AdmissionSubjectPerson AdmissionSubject = "person"
	// AdmissionSubjectGroup is the seam. Nothing is admitted through
	// it until audience admission exists (design 8.3).
	AdmissionSubjectGroup AdmissionSubject = "group"
)

// parseAdmission reads a stored value as one of the closed set.
//
// Anything else -- including the empty string a LEFT JOIN produces for
// a Connection with no policy row -- is Unresolved. No trimming, no
// case folding: a value that needs cleaning up before it matches is a
// value somebody wrote by hand into a column with a CHECK on it, and
// guessing what they meant is how a ceiling gets widened by a stray
// space.
func parseAdmission(raw string) Admission {
	switch Admission(raw) {
	case AdmitOwners:
		return AdmitOwners
	case AdmitHousehold:
		return AdmitHousehold
	default:
		return AdmissionUnresolved
	}
}

func parseAdmissionSubject(raw string) AdmissionSubject {
	switch AdmissionSubject(raw) {
	case AdmissionSubjectPerson:
		return AdmissionSubjectPerson
	case AdmissionSubjectGroup:
		return AdmissionSubjectGroup
	default:
		return AdmissionSubjectUnresolved
	}
}

// Admits answers whether a Person of this role may address the Agent
// over this Connection, and says why not when the answer is no.
//
// IT IS THE INTERSECTION OF TWO CEILINGS, NOT THE POLICY ALONE. The
// Connection's own policy is checked, and so is the Agent class behind
// it, and the narrower wins. An Owner Agent admits Owners whatever its
// Connection row says (design 8.3), so a row that has drifted -- or
// been written around the trigger that guards it -- cannot be the way
// a Member reaches an Owner Agent. Migration 0063 enforces the same
// pairing in SQL; this is not that check repeated for tidiness, it is
// the check that still holds on the day somebody relaxes the trigger.
//
// Every unrecognised input on either side is a denial. That is the one
// property worth mutation-testing: the previous implementation
// answered this question with `surface == "admin"`, where every
// unreadable value fell through to the permissive side.
func (c Connection) Admits(role string) (bool, string) {
	who := "a " + role
	if role == "" {
		who = "somebody whose role is not on record"
	}
	switch c.AdmissionSubject {
	case AdmissionSubjectPerson:
	case AdmissionSubjectGroup:
		// Legal to store, never admitted. 8.3: person admission
		// authenticates the speaker, not the audience.
		return false, fmt.Sprintf(
			"%s admits a group, and group admission is not available yet: "+
				"knowing who is speaking is not knowing who is listening", c.Name())
	default:
		return false, fmt.Sprintf(
			"%s does not record what kind of subject it admits, so it admits nobody", c.Name())
	}

	// The Agent's own ceiling first, so that the reason a Member is
	// refused at an Owner Agent names the Agent rather than the policy
	// row -- the policy row is not what they would have to change.
	if c.OwnerAgent() {
		if role != RoleOwner {
			return false, fmt.Sprintf(
				"%s answers to owners only, and %s is not one", c.Butler, who)
		}
	}

	switch c.Admission {
	case AdmitOwners:
		if role != RoleOwner {
			return false, fmt.Sprintf(
				"%s admits owners only, and %s is not one", c.Name(), who)
		}
		return true, ""
	case AdmitHousehold:
		// A KNOWN role, not any non-empty string. "household" is the
		// widest policy there is, and the widest policy is still not a
		// reason to admit a role this build cannot name.
		if role != RoleOwner && role != RoleMember {
			return false, fmt.Sprintf(
				"%s admits the household, and %s is not a role on record", c.Name(), who)
		}
		return true, ""
	default:
		return false, fmt.Sprintf(
			"nothing on this deployment says who %s admits, so it admits nobody", c.Name())
	}
}

// OwnerAgent reports whether the Agent answering on this Connection is
// an Owner Agent.
//
// BOTH COLUMNS, EITHER ONE ENOUGH TO MAKE IT TRUE. Migration 0058's
// CHECK makes agent_class and surface agree by construction today, and
// portalMayReach already argues the case for not resting on that: a
// defence correct only because of an invariant elsewhere stops being a
// defence when the invariant is relaxed. Here the OR does more than
// belt and braces -- it means a row where the two disagree resolves to
// the NARROWER reading, which is the direction a disagreement should
// fail in.
func (c Connection) OwnerAgent() bool {
	return c.Surface == ButlerSurfaceAdmin || c.AgentClass == AgentClassOwner
}

// AdmitsOwnersOnly reports whether this Connection admits Owners and
// nobody else (ADR-W033 B9, design 8.3).
//
// It is retained for the SURFACE BEHAVIOUR that reads it -- which
// greeting a bot gives, whether it answers in a group at all, which
// slash commands it offers -- where the question really is "is this
// the operator's bot". It is NOT the admission check: use Admits,
// which is total over roles and answers with a reason. A bool cannot
// distinguish "admits the household" from "admits nobody, because
// nothing says", and the difference is the whole of this slice.
func (c Connection) AdmitsOwnersOnly() bool {
	return c.OwnerAgent() || c.Admission == AdmitOwners
}

// ErrAmbiguousConnection is returned when an Agent has more than one
// Connection on a transport and the caller named neither.
//
// B7 allows exactly this — "multiple Connections of the same transport
// type" — so the answer is to ask rather than to take the first. A code
// issued for the wrong one of somebody's two Telegram bots is an invite
// that works and admits them somewhere they did not mean to go.
var ErrAmbiguousConnection = errors.New("runtimedb: that agent has more than one connection on that transport")

// ChatConnectionsForButler lists the live Connections an Agent answers
// on, for one transport or — with transport "" — for all of them.
//
// An empty transport means ANY rather than a transport named "": an
// Agent with a single bot should not have to say which, and
// `a.transport = ”` matches nothing, so the literal reading turned
// "I did not need to say" into "no connection".
func (db *DB) ChatConnectionsForButler(butler, transport string) ([]Connection, error) {
	where := chatConnectionColumns + ` WHERE b.name = ? AND a.revoked_at = ''`
	args := []any{butler}
	if transport != "" {
		where += ` AND a.transport = ?`
		args = append(args, transport)
	}
	rows, err := db.sql.Query(where+` ORDER BY a.transport, a.seq`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading connections for %q: %w", butler, err)
	}
	defer rows.Close()
	var found []Connection
	for rows.Next() {
		c, err := scanChatConnection(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, c)
	}
	return found, rows.Err()
}

// ChatConnectionForButler resolves the ONE live Connection an Agent
// answers on, optionally narrowed to a transport and to a single bot by
// its external id. An empty transport or id does not narrow.
func (db *DB) ChatConnectionForButler(butler, transport, externalID string) (Connection, error) {
	found, err := db.ChatConnectionsForButler(butler, transport)
	if err != nil {
		return Connection{}, err
	}
	if externalID != "" {
		// Narrowing by id rather than looking the address up directly,
		// because the answer has to be one of THIS Agent's: a bare
		// (transport, external_id) lookup would resolve somebody else's
		// bot and then need a second check to notice.
		var narrowed []Connection
		for _, c := range found {
			if c.ExternalID == externalID {
				narrowed = append(narrowed, c)
			}
		}
		found = narrowed
	}
	switch len(found) {
	case 0:
		return Connection{}, ErrNoConnection
	case 1:
		return found[0], nil
	default:
		// The error NAMES the choices, because a caller that cannot see
		// them cannot resolve the ambiguity: an operator told only
		// "there is more than one" has been refused with no way
		// forward. Each choice carries its transport too, since without
		// one named they may differ.
		ids := make([]string, 0, len(found))
		for _, c := range found {
			ids = append(ids, c.Transport+"/"+c.ExternalID)
		}
		return Connection{}, fmt.Errorf("%w: %s answers on %d bots (%s) -- name which one",
			ErrAmbiguousConnection, butler, len(found), strings.Join(ids, ", "))
	}
}

// SetConnectionAdmission narrows or restores a Connection's admission
// policy (design §8.1; migration 0063).
//
// NARROWS OR RESTORES, never widens past the Agent. The Agent class is
// the ceiling — an Owner Agent's Connection admits Owners only (§8.3)
// — and 0063's trigger refuses the write that would cross it. The
// error that comes back is turned into something an operator can act
// on here rather than left as a raw constraint failure, because
// "constraint failed" does not say which rule or what to do instead.
//
// There is no create: a Connection has a policy from the moment it
// exists, written by the trigger on butler_addresses. This only
// changes one, so a capability nobody has given to an Agent reports
// ErrNoConnection rather than quietly inserting a policy for a bot
// that answers nobody.
func (db *DB) SetConnectionAdmission(transport, externalID string, admits Admission, by string) (Connection, error) {
	if by == "" {
		return Connection{}, fmt.Errorf("runtimedb: setting connection admission needs who set it")
	}
	// The vocabulary, checked before the database sees it. Unresolved
	// is a READING, not a value somebody may store: "admits nobody" is
	// what an unreadable row means, and offering it as a setting would
	// make a Connection nobody can use and nobody can explain.
	switch admits {
	case AdmitOwners, AdmitHousehold:
	default:
		return Connection{}, fmt.Errorf(
			"runtimedb: %q is not an admission policy; it is %q or %q",
			string(admits), string(AdmitOwners), string(AdmitHousehold))
	}
	at, err := db.ChatConnectionAt(transport, externalID)
	if err != nil {
		return Connection{}, err
	}
	res, err := db.sql.Exec(
		`UPDATE connection_admission SET admits = ?, set_at = ?, set_by = ? WHERE connection = ?`,
		string(admits), now(), by, at.Seq)
	if err != nil {
		if strings.Contains(err.Error(), "owners only") {
			return Connection{}, fmt.Errorf(
				"%s answers on %s, and an owner agent's connections admit owners only -- "+
					"change the agent's class, or give the household a member agent instead (ADR-W033 B9)",
				at.Butler, at.Name())
		}
		return Connection{}, fmt.Errorf("runtimedb: setting admission on %s: %w", at.Name(), err)
	}
	// A Connection with no policy row. The trigger gives every new one
	// a policy, so this is a row that was deleted or a database
	// restored from before 0063 — and INSERTing one here would be this
	// function deciding a policy nobody chose. Refusing says what is
	// wrong while Admits keeps refusing everybody, which is the safe
	// order for the two to fail in.
	if n, _ := res.RowsAffected(); n == 0 {
		return Connection{}, fmt.Errorf(
			"runtimedb: %s has no admission policy to change", at.Name())
	}
	return db.ChatConnectionAt(transport, externalID)
}
