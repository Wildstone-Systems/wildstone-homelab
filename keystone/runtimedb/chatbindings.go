package runtimedb

// Chat bindings and claim codes (ADR-W026 A4): how a chat account
// becomes a person Wildstone will talk to.
//
// The rule this file exists to enforce is that an adapter never asserts
// an identity. It presents a transport and an account id; a binding row
// — written by a human redeeming a code — is what turns that into a
// member or a principal. There is no path here that creates a binding
// from a message, an allow-list, or a config file.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HasLivePersonChatBinding checks current Person and Connection admission and
// the existing human-established binding. Source provenance does not substitute
// for this check when queued private work returns to its originating chat.
func (db *DB) HasLivePersonChatBinding(connection int64, person string) (bool, error) {
	return hasLivePersonChatBinding(db.sql, connection, person)
}

func hasLivePersonChatBinding(q privacyReader, connection int64, person string) (bool, error) {
	return hasLivePersonChatConversation(q, connection, person, "")
}

func hasLivePersonChatConversation(q privacyReader, connection int64, person, conversation string) (bool, error) {
	var role, surface, admits, subject string
	var at Connection
	// Every input to Admits belongs to this one statement's snapshot. Separate
	// role and Connection reads can admit a combination that never existed.
	err := q.QueryRow(`SELECT p.role,b.surface,b.agent_class,
 COALESCE(ca.admits,''),COALESCE(ca.subject_kind,'')
 FROM chat_bindings cb JOIN butler_members p ON p.person_id=cb.person_id
 JOIN butler_addresses a ON a.seq=cb.connection
 JOIN butlers b ON b.seq=a.butler
 LEFT JOIN connection_admission ca ON ca.connection=a.seq
 WHERE cb.connection=? AND p.member=? AND cb.revoked_at=''
 AND p.admitted=1 AND p.disabled_at='' AND p.offboarded_at=''
 AND a.revoked_at='' AND (?='' OR cb.conversation=?)
 LIMIT 1`, connection, person, conversation, conversation).Scan(&role, &surface, &at.AgentClass, &admits, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at.Surface = ButlerSurface(surface)
	at.Admission = parseAdmission(admits)
	at.AdmissionSubject = parseAdmissionSubject(subject)
	// Use the canonical closed-set admission check, including subject kind,
	// current Agent class/surface and current Person role. Missing or unknown
	// policy never grants authority merely because a binding survives.
	admitted, _ := at.Admits(role)
	return admitted, nil
}

// ChatBinding is one chat account admitted to one Connection — so to
// one Agent, on one transport (ADR-W033 B7, B9).
//
// It used to name a SURFACE, which is a class of Agents rather than an
// Agent: an account admitted to `admin` reached every Owner Agent,
// including ones made afterwards. Butler and Transport are read from
// the Connection rather than stored beside it, so they cannot disagree
// with it.
type ChatBinding struct {
	ID         int64 `json:"id"`
	Connection int64 `json:"connection"`
	// Butler is the Agent this Connection belongs to.
	Butler    string `json:"butler"`
	Transport string `json:"transport"`
	Account   string `json:"account"`
	Identity  string `json:"identity"`
	// Role is the bound Person's role as it stands NOW, not as it
	// stood when the binding was made. Admission is re-read on every
	// message from it, so a demotion takes effect at the next message
	// rather than at the next re-binding.
	Role        string `json:"role"`
	DisplayName string `json:"displayName,omitempty"`
	// Conversation is where to reach this account when nobody asked — a
	// rule's question rather than a reply. On Telegram it equals the
	// account; on Matrix it is the direct room, which the account (an
	// MXID) is not (ADR-W026 A8).
	Conversation string    `json:"conversation,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	CreatedBy    string    `json:"createdBy"`
	LastUsedAt   time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt    time.Time `json:"revokedAt,omitempty"`
}

// Revoked reports whether this binding has been withdrawn.
func (b ChatBinding) Revoked() bool { return !b.RevokedAt.IsZero() }

// ChatClaimCode is an issued, not-yet-redeemed invitation for one
// Person to be admitted to one Connection. The secret itself is
// never stored — only its hash — so this struct describes a code, it
// does not contain one.
type ChatClaimCode struct {
	// Connection is the one this code admits somebody to. There is no
	// "any transport" any more: a code that could be redeemed anywhere
	// cannot name the Agent it admits them to.
	Connection int64     `json:"connection"`
	Identity   string    `json:"identity"`
	CreatedAt  time.Time `json:"createdAt"`
	CreatedBy  string    `json:"createdBy"`
	ExpiresAt  time.Time `json:"expiresAt"`
	RedeemedAt time.Time `json:"redeemedAt,omitempty"`
	BindingID  int64     `json:"bindingId,omitempty"`
}

// ErrClaimCode is returned when a presented code is not redeemable. Its
// reasons deliberately collapse into one error: unknown, expired,
// already redeemed and wrong-transport are all "this code does not work"
// to whoever is holding it, and distinguishing them for the caller is
// free help for someone guessing.
var ErrClaimCode = errors.New("runtimedb: claim code is not redeemable")

// InsertChatClaimCode records an issued code by its hash.
func (db *DB) InsertChatClaimCode(hash string, c ChatClaimCode) error {
	if hash == "" || c.Identity == "" || c.CreatedBy == "" {
		return fmt.Errorf("runtimedb: claim code requires a hash, identity and issuer")
	}
	if c.Connection == 0 {
		return fmt.Errorf("runtimedb: a claim code names the connection it admits somebody to (ADR-W033 B7)")
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Errorf("runtimedb: claim code requires an expiry (ADR-W026 A4: short-lived, single-use)")
	}
	// A code names the Person it will admit (ADR-W033 B9), so the
	// binding it produces cannot name anybody else.
	person, err := resolvePersonID(db.sql, c.Identity)
	if err != nil {
		return fmt.Errorf("runtimedb: inserting claim code: %w", err)
	}
	_, err = db.sql.Exec(
		`INSERT INTO chat_claim_codes (hash, connection, person_id, created_at, created_by, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hash, c.Connection, person, now(), c.CreatedBy,
		c.ExpiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("runtimedb: inserting claim code: %w", err)
	}
	return nil
}

// RedeemChatClaimCode turns a presented code hash into an admission, in
// one transaction: the code must be unredeemed, unexpired, and for the
// Connection the redeeming bot IS; the binding is written; the code is
// burned. Redeeming twice fails the second time, which is what
// single-use means.
//
// An existing live binding for the same (connection, account) is revoked
// first, so re-binding an account — a new phone, a re-installed app — is
// a normal act rather than a unique-constraint failure.
//
// at is the Connection the redeeming bot serves. A code for a different
// Agent is refused without being burned: a code meant for Alfred sent to
// Robert's bot by mistake should be usable on the right one a minute
// later, not spent on an admission that bot can never resolve. It used
// to compare SURFACES, which could not tell Alfred from Robert at all.
func (db *DB) RedeemChatClaimCode(hash string, at Connection, account, displayName, conversation string) (ChatBinding, error) {
	if at.Seq == 0 || account == "" {
		return ChatBinding{}, fmt.Errorf("runtimedb: redeeming requires the connection it arrived at and an account")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ChatBinding{}, err
	}
	defer tx.Rollback() //nolint:errcheck // rolled back unless committed

	var connection int64
	var person, identity, role, redeemedAt, expiresAt, createdBy string
	err = tx.QueryRow(
		`SELECT c.connection, c.person_id, m.member, m.role, c.redeemed_at, c.expires_at, c.created_by
		 FROM chat_claim_codes c JOIN butler_members m ON m.person_id = c.person_id
		 WHERE c.hash = ?`, hash).
		Scan(&connection, &person, &identity, &role, &redeemedAt, &expiresAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatBinding{}, ErrClaimCode
	}
	if err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: reading claim code: %w", err)
	}
	if redeemedAt != "" {
		return ChatBinding{}, ErrClaimCode
	}
	// Checked BEFORE the burn, and reported as the same refusal every
	// other bad code gets: a bot relaying codes for strangers must not
	// learn that this one was real and merely for another Agent.
	if connection != at.Seq {
		return ChatBinding{}, ErrClaimCode
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: parsing claim code expiry: %w", err)
	}
	if time.Now().After(exp) {
		return ChatBinding{}, ErrClaimCode
	}

	stamp := now()
	if _, err := tx.Exec(
		`UPDATE chat_bindings SET revoked_at = ?
		 WHERE connection = ? AND account = ? AND revoked_at = ''`,
		stamp, at.Seq, account); err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: replacing binding: %w", err)
	}
	// The binding carries the code's Person across unchanged rather than
	// resolving the handle again: they are the same Person by
	// construction, and a second lookup could disagree with the first if
	// a rename landed between them.
	res, err := tx.Exec(
		`INSERT INTO chat_bindings (connection, account, person_id, display_name, conversation, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		at.Seq, account, person, displayName, conversation, stamp, createdBy)
	if err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: inserting binding: %w", err)
	}
	// The invite this fulfils is done: the Person is on the Connection
	// (ADR-W033 B7.1). Same transaction, so a binding never leaves an
	// invite open behind it.
	if err := closeInvitesOnBinding(tx, at.Seq, person); err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: closing the invite: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ChatBinding{}, err
	}
	if _, err := tx.Exec(
		`UPDATE chat_claim_codes SET redeemed_at = ?, binding_id = ? WHERE hash = ? AND redeemed_at = ''`,
		stamp, id, hash); err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: burning claim code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ChatBinding{}, err
	}
	created, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return ChatBinding{}, err
	}
	// Role comes from the claim's own joined member row, so a freshly
	// redeemed binding describes itself exactly as a later read of it
	// will. A review of #289 spotted this returning "" from
	// POST /chat/bindings/redeem while a list of the same binding
	// reported the real role -- the kind of disagreement that teaches
	// whoever is reading the API that one of the two is lying.
	return ChatBinding{
		ID: id, Connection: at.Seq, Butler: at.Butler, Transport: at.Transport,
		Account: account, Identity: identity, Role: role, DisplayName: displayName,
		Conversation: conversation, CreatedAt: created, CreatedBy: createdBy,
	}, nil
}

// LookupChatBinding resolves a live admission for one account on one
// Connection. A revoked binding is not found, which is what makes
// revoking take effect on the next message.
//
// It also re-checks the admitted Person, which is the runtime half of
// ADR-W033 B9: "Owner-Agent Connections admit Owners only.
// Configuration and runtime both enforce this." Checking only at issue
// would mean a Person demoted, disabled or offboarded after binding
// kept the reach the binding gave them — the same hole A6 closes for
// Console, where demotion is immediate rather than effective at the
// next re-enrolment.
//
// The refusal is ErrNotFound, exactly as a revoked binding is, so the
// message path treats "you may no longer speak here" the same way it
// treats "you never could" and tells a prober nothing about which.
func (db *DB) LookupChatBinding(at Connection, account string) (ChatBinding, error) {
	// The SQL asks only whether a LIVE BINDING FOR AN ACTIVE PERSON
	// exists. It used to carry admission too, as
	// `(agent.surface <> 'admin' OR m.role = 'owner')`, and that clause
	// has moved to Connection.Admits for two reasons.
	//
	// It failed open. A surface value that was not exactly 'admin'
	// satisfied `<> 'admin'` and admitted the household, so an
	// unreadable ceiling and the widest ceiling gave the same answer.
	//
	// And it could not see the Connection's own policy (design §8.1,
	// §8.3; migration 0063). Admission derived from the Agent cannot
	// express "this bot admits owners only" about a Member Agent,
	// which is the configuration §8.1 says every Connection carries.
	//
	// The two questions are now answered in the order they are asked:
	// is there a binding, and does this Connection admit that Person.
	row := db.sql.QueryRow(
		bindingColumns+` WHERE b.connection = ? AND b.account = ? AND b.revoked_at = ''
		   AND m.admitted = 1 AND m.disabled_at = '' AND m.offboarded_at = ''`,
		at.Seq, account)
	b, err := scanChatBinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatBinding{}, ErrNotFound
	}
	if err != nil {
		return ChatBinding{}, err
	}
	// ErrNotAdmitted, not ErrNotFound, and the difference is not
	// cosmetic: a stranger and a household member their Connection
	// turns away are both refused before inference, but they are not
	// the same event. One is somebody nobody introduced; the other is
	// a person on record who has been demoted, or a bot somebody
	// narrowed, and reporting it as "no such account" sends whoever
	// investigates looking for a binding that is sitting right there.
	if ok, reason := at.Admits(b.Role); !ok {
		return ChatBinding{}, fmt.Errorf("%w: %s", ErrNotAdmitted, reason)
	}
	return b, nil
}

// ListChatBindings returns bindings newest first. Revoked ones are
// included only when withRevoked is set: an operator auditing who can
// talk to their homelab wants the live list, and an operator asking what
// happened wants the history.
func (db *DB) ListChatBindings(withRevoked bool) ([]ChatBinding, error) {
	where := ""
	if !withRevoked {
		where = " WHERE b.revoked_at = ''"
	}
	rows, err := db.sql.Query(bindingColumns + where + ` ORDER BY b.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing chat bindings: %w", err)
	}
	defer rows.Close()
	var out []ChatBinding
	for rows.Next() {
		b, err := scanChatBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RevokeChatBinding withdraws one binding. Revoking an already-revoked
// or unknown binding reports ErrNotFound rather than pretending to have
// done something.
func (db *DB) RevokeChatBinding(id int64) error {
	res, err := db.sql.Exec(`UPDATE chat_bindings SET revoked_at = ? WHERE id = ? AND revoked_at = ''`, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking chat binding: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchChatBinding stamps last use, so the operator's list shows which
// bindings are alive and which are forgotten phones. A non-empty
// conversation refreshes where a rule should reach this account: the
// direct room a person messages from is the one to answer in, and it can
// change (a new device, a recreated room).
func (db *DB) TouchChatBinding(id int64, conversation string) error {
	if conversation == "" {
		_, err := db.sql.Exec(`UPDATE chat_bindings SET last_used_at = ? WHERE id = ?`, now(), id)
		return err
	}
	_, err := db.sql.Exec(
		`UPDATE chat_bindings SET last_used_at = ?, conversation = ? WHERE id = ?`, now(), conversation, id)
	return err
}

// Identity reads back as the handle the admitted Person carries now, so
// a Console listing and an inbound message both see today's name after
// a rename — which is the point of the Connection naming an identity
// rather than a label (ADR-W033 A2).
// The Connection carries the transport and the Agent, so both are read
// through it rather than stored beside it.
const bindingColumns = `SELECT b.id, b.connection, agent.name, conn.transport, b.account, m.member,
	 m.role, b.display_name, b.conversation, b.created_at, b.created_by, b.last_used_at, b.revoked_at
	 FROM chat_bindings b
	 JOIN butler_members m ON m.person_id = b.person_id
	 JOIN butler_addresses conn ON conn.seq = b.connection
	 JOIN butlers agent ON agent.seq = conn.butler`

func scanChatBinding(r rowScanner) (ChatBinding, error) {
	var b ChatBinding
	var created, lastUsed, revoked string
	if err := r.Scan(&b.ID, &b.Connection, &b.Butler, &b.Transport, &b.Account, &b.Identity,
		&b.Role, &b.DisplayName, &b.Conversation, &created, &b.CreatedBy, &lastUsed, &revoked); err != nil {
		return ChatBinding{}, err
	}
	var err error
	if b.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: parsing binding created_at: %w", err)
	}
	if lastUsed != "" {
		if b.LastUsedAt, err = time.Parse(time.RFC3339Nano, lastUsed); err != nil {
			return ChatBinding{}, err
		}
	}
	if revoked != "" {
		if b.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
			return ChatBinding{}, err
		}
	}
	return b, nil
}

// refreshTaskChatBinding refreshes only the human-established binding that
// trusted ingress verified. The transaction also inserts its admitted task;
// neither a failed insert nor a policy change may leave half of that decision.
func refreshTaskChatBinding(tx *sql.Tx, task ButlerTask, owner, stamp string) error {
	if task.Kind != ButlerTaskConversation || task.Audience != ButlerAudiencePrivate || task.Author != task.Owner || task.OriginConnectionRef == 0 || task.OriginConversation == "" {
		return ErrNotAdmitted
	}
	var valid bool
	err := tx.QueryRow(`SELECT EXISTS (
 SELECT 1 FROM chat_bindings cb
 JOIN butler_addresses c ON c.seq=cb.connection
 JOIN butlers agent ON agent.seq=c.butler
 WHERE cb.id=? AND cb.person_id=? AND cb.connection=? AND cb.revoked_at=''
 AND c.revoked_at='' AND c.transport||':'||c.external_id=?
 AND agent.enabled=1 AND agent.retired_at='' AND agent.agent_class=? AND agent.surface=?
 AND EXISTS (SELECT 1 FROM butler_access access WHERE access.butler=agent.seq AND access.person_id=cb.person_id AND access.revoked_at='')
 AND NOT EXISTS (SELECT 1 FROM chat_rooms room WHERE room.connection=c.seq AND room.conversation=?)
 )`, task.OriginBindingID, owner, task.OriginConnectionRef, task.OriginConnection, task.AgentClass, task.Surface, task.OriginConversation).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrNotAdmitted
	}
	admitted, err := hasLivePersonChatBinding(tx, task.OriginConnectionRef, task.Owner)
	if err != nil {
		return err
	}
	if !admitted {
		return ErrNotAdmitted
	}
	result, err := tx.Exec(`UPDATE chat_bindings SET conversation=?,last_used_at=? WHERE id=? AND revoked_at=''`, task.OriginConversation, stamp, task.OriginBindingID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotAdmitted
	}
	return nil
}
