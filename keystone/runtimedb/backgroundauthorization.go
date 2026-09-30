package runtimedb

// A Person's authorization for UNATTENDED use of their own Personal
// credential, and the evaluator that intersects it with the Owner's
// approval of the exact subscription version (epic #326 S02; ADR-W033
// D1 amendment, ADR-W030 A5.4).
//
// ADR-W033 D1 says an unattended run cannot use a Personal secret, and
// that the only way that could ever change is "explicit Person-authorized
// exact-version delegation with its own revocation, audit and interface
// semantics". This is that delegation, and it is deliberately narrow:
//
//   - Only the Person who owns the Personal connection can authorize it.
//     Not an Owner, not another Person, not an Agent holding a grant, not
//     a shared-Agent relationship. D1's exhaustive list of stand-ins
//     stays exhaustive; this adds one legitimate authority, the owner.
//
//   - The authorization binds every material field: connection,
//     credential version, source scope, processing location, result
//     audience, Agent, target calendar and adapter executable. They are
//     digested, and the digest is what an Owner-approved subscription
//     names. The Person's row names that subscription and its content
//     digest in turn. Neither artifact alone authorizes anything.
//
//   - Rows are immutable (migration 0098 trigger). A material edit is a
//     new authorization with a new digest, which no approved
//     subscription names — so the edit invalidates the Owner's approval
//     too, without anybody having to remember to.
//
//   - AuthorizeBackgroundUse evaluates the intersection LIVE, on every
//     dispatch. Nothing about it is cached on a task, a lease or a
//     subscription.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Processing locations a Person may authorize. Local is the default and
// the only one a connection allows until its owner widens cloud routing
// (ADR-W030 A5).
const (
	ProcessingLocal = "local"
	ProcessingCloud = "cloud"
)

// The keys an Owner-approved subscription's match_json carries to name
// the Personal connection and the exact authorization it runs under.
// S03's scheduled subscriptions and S07's Watch-this-mailbox flow file
// subscriptions with these; see BackgroundSubscriptionMatch.
const (
	SubscriptionMatchConnection    = "connection"
	SubscriptionMatchAuthorization = "authorization"
)

// ErrBackgroundUseDenied is returned by AuthorizeBackgroundUse for every
// refusal. The wrapped reason says which half of the intersection, or
// which bound field, failed.
var ErrBackgroundUseDenied = errors.New("runtimedb: unattended use of this Personal credential is not authorized")

// Bounds on the source scope. Generous for a household mailbox, tight
// enough that a scope cannot become a way to smuggle a document.
const (
	maxScopeFolders  = 20
	maxScopeFilters  = 50
	maxScopeEntryLen = 255
)

// BackgroundScope is which mail a monitor may look at. Folders default
// to INBOX; senders and subjects are optional narrowing filters.
type BackgroundScope struct {
	Folders  []string `json:"folders"`
	Senders  []string `json:"senders,omitempty"`
	Subjects []string `json:"subjects,omitempty"`
}

// BackgroundAuthorizationRequest is what a Person authorizes.
type BackgroundAuthorizationRequest struct {
	Connection string `json:"connection"`
	// Need names the credential; empty means the mailbox app password.
	Need               string          `json:"need,omitempty"`
	Scope              BackgroundScope `json:"scope"`
	ProcessingLocation string          `json:"processingLocation"`
	// Agent is the butlers.seq of the Agent that runs the monitor.
	Agent          int64  `json:"agent"`
	TargetCalendar string `json:"targetCalendar"`
	// Subscription is the agent_subscriptions.id this is for. It may
	// still be awaiting Owner approval; the evaluator requires both.
	// Zero asks PrepareBackgroundAuthorization for the digest only.
	Subscription int64 `json:"subscription"`
}

// BackgroundAuthorization is one stored authorization.
type BackgroundAuthorization struct {
	Seq                int64           `json:"seq"`
	Connection         string          `json:"connection"`
	Person             string          `json:"person"`
	Credential         int64           `json:"credential"`
	CredentialVersion  int             `json:"credentialVersion"`
	Scope              BackgroundScope `json:"scope"`
	ProcessingLocation string          `json:"processingLocation"`
	ResultAudience     string          `json:"resultAudience"`
	Agent              int64           `json:"agent"`
	TargetCalendar     string          `json:"targetCalendar"`
	Executable         string          `json:"executable"`
	Subscription       int64           `json:"subscription"`
	SubscriptionDigest string          `json:"subscriptionDigest"`
	Digest             string          `json:"digest"`
	CreatedAt          time.Time       `json:"createdAt"`
	RevokedAt          time.Time       `json:"revokedAt,omitzero"`
	RevokedBy          string          `json:"revokedBy,omitempty"`
	RevokedReason      string          `json:"revokedReason,omitempty"`
}

// BackgroundGrant is what a dispatcher may act on after
// AuthorizeBackgroundUse succeeds: the exact scope, and the fence it was
// evaluated under. It carries no credential value — the adapter reads
// that on its node — only the reference and version.
type BackgroundGrant struct {
	Connection          string          `json:"connection"`
	Person              string          `json:"person"`
	Authorization       int64           `json:"authorization"`
	AuthorizationDigest string          `json:"authorizationDigest"`
	Credential          int64           `json:"credential"`
	CredentialVersion   int             `json:"credentialVersion"`
	SecretRef           string          `json:"secretRef"`
	Scope               BackgroundScope `json:"scope"`
	ProcessingLocation  string          `json:"processingLocation"`
	ResultAudience      string          `json:"resultAudience"`
	Agent               int64           `json:"agent"`
	TargetCalendar      string          `json:"targetCalendar"`
	Executable          string          `json:"executable"`
	Subscription        int64           `json:"subscription"`
	SubscriptionVersion int             `json:"subscriptionVersion"`
	// LeaseEpoch is the connection epoch at evaluation. Work queued under
	// it is refused if the epoch moves (a revocation, a replacement, a
	// failover) before the adapter answers.
	LeaseEpoch int64 `json:"leaseEpoch"`
}

// BackgroundSubscriptionMatch returns the match_json fields a
// subscription must carry to run under a Personal authorization: the
// connection and the authorization digest. A subscription filer merges
// them into its own match parameters.
func BackgroundSubscriptionMatch(connection, digest string) map[string]string {
	return map[string]string{SubscriptionMatchConnection: connection, SubscriptionMatchAuthorization: digest}
}

// normalizeScope trims, bounds, de-duplicates and sorts a scope so the
// same choice always digests the same way.
func normalizeScope(s BackgroundScope) (BackgroundScope, error) {
	clean := func(what string, in []string, limit int) ([]string, error) {
		if len(in) > limit {
			return nil, fmt.Errorf("runtimedb: at most %d %s", limit, what)
		}
		seen := map[string]bool{}
		var out []string
		for _, v := range in {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if len(v) > maxScopeEntryLen {
				return nil, fmt.Errorf("runtimedb: a %s entry is at most %d characters", what, maxScopeEntryLen)
			}
			if strings.IndexFunc(v, unicode.IsControl) >= 0 {
				return nil, fmt.Errorf("runtimedb: a %s entry cannot contain control characters", what)
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	var err error
	var out BackgroundScope
	if out.Folders, err = clean("folders", s.Folders, maxScopeFolders); err != nil {
		return BackgroundScope{}, err
	}
	if len(out.Folders) == 0 {
		out.Folders = []string{"INBOX"}
	}
	if out.Senders, err = clean("sender filters", s.Senders, maxScopeFilters); err != nil {
		return BackgroundScope{}, err
	}
	if out.Subjects, err = clean("subject filters", s.Subjects, maxScopeFilters); err != nil {
		return BackgroundScope{}, err
	}
	return out, nil
}

// authorizationDigest digests every material field. The subscription
// binding is NOT in it: the subscription names this digest, so including
// the subscription here would make the two artifacts name each other.
func authorizationDigest(a BackgroundAuthorization, connSeq int64) (string, error) {
	scope, err := json.Marshal(a.Scope)
	if err != nil {
		return "", err
	}
	material, err := json.Marshal([]any{
		"wildstone-background-authorization-v1",
		a.Connection, connSeq, a.Person, a.Credential, a.CredentialVersion, string(scope),
		a.ProcessingLocation, a.ResultAudience, a.Agent, a.TargetCalendar, a.Executable,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(material)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SubscriptionContentDigest digests the parts of a subscription an Owner
// approves as its behaviour — the Agent, the matcher and its parameters,
// the audience, the capability set and the budget — and not the approval
// fields themselves, which change when the Owner approves. An in-place
// change to any of it no longer matches what the Person authorized.
func SubscriptionContentDigest(sub AgentSubscription) (string, error) {
	caps := append([]string(nil), sub.CapabilitySet...)
	sort.Strings(caps)
	material, err := json.Marshal([]any{
		"wildstone-subscription-content-v1",
		sub.ID, sub.Butler, sub.MatchKind, sub.MatchJSON, sub.Audience, caps, sub.BudgetJSON,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(material)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// subscriptionNames reads the connection and authorization digest a
// subscription's match_json names, if any.
func subscriptionNames(sub AgentSubscription) (connection, digest string) {
	var m map[string]any
	if json.Unmarshal([]byte(sub.MatchJSON), &m) != nil {
		return "", ""
	}
	c, _ := m[SubscriptionMatchConnection].(string)
	d, _ := m[SubscriptionMatchAuthorization].(string)
	return c, d
}

// PrepareBackgroundAuthorization validates what a Person would authorize
// and returns it with its digest, without storing anything. A
// subscription filer uses the digest to name this exact authorization.
func (db *DB) PrepareBackgroundAuthorization(actor string, req BackgroundAuthorizationRequest) (BackgroundAuthorization, error) {
	conn, err := db.personalConnection(req.Connection)
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	if err := db.requireCredentialOwner(conn, actor); err != nil {
		return BackgroundAuthorization{}, err
	}
	need := req.Need
	if need == "" {
		need = MailboxCredentialNeed
	}
	cred, err := db.ActivePersonalCredential(req.Connection, need)
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	if cred.Member != conn.Owner {
		return BackgroundAuthorization{}, fmt.Errorf("%w: the active credential is not %s's", ErrNotCredentialOwner, conn.Owner)
	}
	scope, err := normalizeScope(req.Scope)
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	location := req.ProcessingLocation
	if location == "" {
		location = ProcessingLocal
	}
	switch location {
	case ProcessingLocal:
	case ProcessingCloud:
		if !conn.CloudAllowed {
			return BackgroundAuthorization{}, fmt.Errorf("runtimedb: %q is local-only; widen its cloud routing "+
				"before authorizing cloud processing (ADR-W030 A5)", req.Connection)
		}
	default:
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: processing location must be local|cloud, got %q", location)
	}
	calendar := strings.TrimSpace(req.TargetCalendar)
	if calendar == "" || len(calendar) > maxScopeEntryLen || strings.IndexFunc(calendar, unicode.IsControl) >= 0 {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: a background authorization needs one target calendar")
	}
	if conn.Executable == "" {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: %q has no adapter executable recorded yet; "+
			"its placement has to be rendered before it can be authorized", req.Connection)
	}
	if err := db.personMayUseAgent(actor, req.Agent); err != nil {
		return BackgroundAuthorization{}, err
	}
	a := BackgroundAuthorization{
		Connection: conn.Name, Person: actor, Credential: cred.Seq, CredentialVersion: cred.Version,
		Scope: scope, ProcessingLocation: location, ResultAudience: AudienceOwner, Agent: req.Agent,
		TargetCalendar: calendar, Executable: conn.Executable,
	}
	if a.Digest, err = authorizationDigest(a, conn.Seq); err != nil {
		return BackgroundAuthorization{}, err
	}
	// With no subscription yet this is the preview a subscription filer
	// needs: the digest to name. The subscription half is checked once
	// there is one to check.
	if req.Subscription == 0 {
		return a, nil
	}
	sub, err := db.GetAgentSubscription(req.Subscription)
	if err != nil {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: no subscription %d: %w", req.Subscription, ErrNotFound)
	}
	if sub.Butler != req.Agent {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: subscription %d runs Agent %d, not %d", sub.ID, sub.Butler, req.Agent)
	}
	if sub.Audience != AudienceOwner {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: subscription %d must disclose its results to the "+
			"mailbox owner alone (audience %s), not %q", sub.ID, AudienceOwner, sub.Audience)
	}
	if a.SubscriptionDigest, err = SubscriptionContentDigest(sub); err != nil {
		return BackgroundAuthorization{}, err
	}
	a.Subscription = sub.ID
	return a, nil
}

// personMayUseAgent: the Agent must exist, be enabled and not retired,
// and be one this Person has been granted access to. An Agent the
// Person cannot talk to cannot act on their mailbox.
func (db *DB) personMayUseAgent(person string, agent int64) error {
	butlers, err := db.ButlersFor(person)
	if err != nil {
		return err
	}
	for _, b := range butlers {
		if b.Seq == agent {
			return nil
		}
	}
	return fmt.Errorf("runtimedb: Agent %d is not one %s may use", agent, person)
}

// AuthorizeBackgroundUseAsPerson stores a Person's authorization. The
// subscription it names must already name this authorization's digest
// and connection (see BackgroundSubscriptionMatch), so what the Owner
// approves and what the Person authorized are provably the same thing.
// A previous live authorization for the connection is revoked in the
// same transaction: a material change supersedes, it never edits.
func (db *DB) AuthorizeBackgroundUseAsPerson(actor string, req BackgroundAuthorizationRequest) (BackgroundAuthorization, error) {
	if req.Subscription == 0 {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: a background authorization is for one subscription; name it")
	}
	a, err := db.PrepareBackgroundAuthorization(actor, req)
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	sub, err := db.GetAgentSubscription(a.Subscription)
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	if c, d := subscriptionNames(sub); c != a.Connection || d != a.Digest {
		return a, fmt.Errorf("%w: subscription %d does not name this exact authorization; file it with "+
			"match %s=%q and %s=%q", ErrDigestMismatch, sub.ID, SubscriptionMatchConnection, a.Connection,
			SubscriptionMatchAuthorization, a.Digest)
	}
	conn, err := db.GetConnection(a.Connection)
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	scope, err := json.Marshal(a.Scope)
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: authorizing background use: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	superseded, err := revokeAuthorizationsTx(tx, conn.Seq, "", nil, actor, "superseded by a new authorization")
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	res, err := tx.Exec(
		`INSERT INTO personal_background_authorizations (connection, person, credential, credential_version,
		     source_scope, processing_location, result_audience, agent, target_calendar, executable,
		     subscription, subscription_digest, digest, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conn.Seq, a.Person, a.Credential, a.CredentialVersion, string(scope), a.ProcessingLocation,
		a.ResultAudience, a.Agent, a.TargetCalendar, a.Executable, a.Subscription, a.SubscriptionDigest,
		a.Digest, now())
	if err != nil {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: authorizing background use: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return BackgroundAuthorization{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: authorizing background use: %w", err)
	}
	for _, s := range superseded {
		if err := db.journal("grant", backgroundAuthorizationJournalID(s), "revoked", actor, "superseded"); err != nil {
			return BackgroundAuthorization{}, err
		}
	}
	return db.GetBackgroundAuthorization(seq)
}

// backgroundAuthorizationJournalPrefix is how a revoked authorization is
// named in the deletion journal (subject kind "grant"), so a restore
// replays the revocation over a snapshot that still has it live.
const backgroundAuthorizationJournalPrefix = "background-authorization/"

func backgroundAuthorizationJournalID(seq int64) string {
	return fmt.Sprintf("%s%d", backgroundAuthorizationJournalPrefix, seq)
}

// ErrDigestMismatch is returned when a subscription does not name the
// authorization being stored.
var ErrDigestMismatch = errors.New("runtimedb: the subscription and the authorization do not name each other")

// RevokeBackgroundAuthorization revokes the connection's live
// authorization. The Person may; so may an Owner administering the
// household — stopping only removes authority (ADR-W033 D1, E6.1).
func (db *DB) RevokeBackgroundAuthorization(connection, actor, reason string) error {
	if actor == "" || reason == "" {
		return fmt.Errorf("runtimedb: revoking an authorization needs who and why")
	}
	conn, err := db.personalConnection(connection)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: revoking authorization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	revoked, err := revokeAuthorizationsTx(tx, conn.Seq, "", nil, actor, reason)
	if err != nil {
		return err
	}
	if len(revoked) == 0 {
		return ErrNotFound
	}
	// Revocation fences new reads now: the epoch moves, so work already
	// queued or claimed under the authorization is refused, and queued
	// work is cancelled rather than left for a stale answer.
	if _, err := tx.Exec(`UPDATE integration_connections SET lease_epoch = lease_epoch + 1, updated_at = ? WHERE seq = ?`,
		now(), conn.Seq); err != nil {
		return fmt.Errorf("runtimedb: fencing on revocation: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE integration_work SET state = ?, finished_at = ?, error = ?
		 WHERE connection = ? AND state IN (?, ?)`,
		WorkCancelled, now(), "the background authorization was revoked", conn.Seq, WorkQueued, WorkClaimed); err != nil {
		return fmt.Errorf("runtimedb: cancelling work on revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: revoking authorization: %w", err)
	}
	for _, s := range revoked {
		if err := db.journal("grant", backgroundAuthorizationJournalID(s), "revoked", actor, reason); err != nil {
			return err
		}
	}
	return nil
}

// revokeAuthorizationsTx revokes live authorizations for a connection.
// With field "credential" it revokes only those bound to one of ids.
func revokeAuthorizationsTx(tx *sql.Tx, connSeq int64, field string, ids []int64, actor, reason string) ([]int64, error) {
	if field == "credential" && len(ids) == 0 {
		return nil, nil
	}
	query := `SELECT seq FROM personal_background_authorizations WHERE connection = ? AND revoked_at = ''`
	args := []any{connSeq}
	if field == "credential" {
		query += ` AND credential IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: finding authorizations: %w", err)
	}
	var seqs []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	for _, s := range seqs {
		if _, err := tx.Exec(
			`UPDATE personal_background_authorizations SET revoked_at = ?, revoked_by = ?, revoked_reason = ?
			 WHERE seq = ? AND revoked_at = ''`, now(), actor, reason, s); err != nil {
			return nil, fmt.Errorf("runtimedb: revoking authorization: %w", err)
		}
	}
	return seqs, nil
}

// RevokeBackgroundAuthorizationSeq revokes one authorization by id if it
// is still live — the restore replay of a journalled revocation.
func (db *DB) RevokeBackgroundAuthorizationSeq(seq int64, actor, reason string) error {
	res, err := db.sql.Exec(
		`UPDATE personal_background_authorizations SET revoked_at = ?, revoked_by = ?, revoked_reason = ?
		 WHERE seq = ? AND revoked_at = ''`, now(), actor, reason, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking authorization %d: %w", seq, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const backgroundAuthorizationColumns = `SELECT a.seq, c.name, a.person, a.credential, a.credential_version,
	a.source_scope, a.processing_location, a.result_audience, a.agent, a.target_calendar, a.executable,
	a.subscription, a.subscription_digest, a.digest, a.created_at, a.revoked_at, a.revoked_by, a.revoked_reason
	FROM personal_background_authorizations a JOIN integration_connections c ON c.seq = a.connection`

func scanBackgroundAuthorization(row rowScanner) (BackgroundAuthorization, error) {
	var a BackgroundAuthorization
	var scope, created, revoked string
	err := row.Scan(&a.Seq, &a.Connection, &a.Person, &a.Credential, &a.CredentialVersion, &scope,
		&a.ProcessingLocation, &a.ResultAudience, &a.Agent, &a.TargetCalendar, &a.Executable,
		&a.Subscription, &a.SubscriptionDigest, &a.Digest, &created, &revoked, &a.RevokedBy, &a.RevokedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundAuthorization{}, ErrNotFound
	}
	if err != nil {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: reading authorization: %w", err)
	}
	if err := json.Unmarshal([]byte(scope), &a.Scope); err != nil {
		return BackgroundAuthorization{}, fmt.Errorf("runtimedb: bad authorization scope: %w", err)
	}
	if a.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return BackgroundAuthorization{}, err
	}
	if revoked != "" {
		if a.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
			return BackgroundAuthorization{}, err
		}
	}
	return a, nil
}

// GetBackgroundAuthorization returns one authorization by id.
func (db *DB) GetBackgroundAuthorization(seq int64) (BackgroundAuthorization, error) {
	return scanBackgroundAuthorization(db.sql.QueryRow(backgroundAuthorizationColumns+` WHERE a.seq = ?`, seq))
}

// LiveBackgroundAuthorization returns the connection's live
// authorization, or ErrNotFound.
func (db *DB) LiveBackgroundAuthorization(connection string) (BackgroundAuthorization, error) {
	return scanBackgroundAuthorization(db.sql.QueryRow(backgroundAuthorizationColumns+
		` WHERE c.name = ? AND a.revoked_at = ''`, connection))
}

// ListBackgroundAuthorizations returns every authorization a connection
// has had, newest first — the audit of who authorized what, and why each
// one ended.
func (db *DB) ListBackgroundAuthorizations(connection string) ([]BackgroundAuthorization, error) {
	rows, err := db.sql.Query(backgroundAuthorizationColumns+` WHERE c.name = ? ORDER BY a.seq DESC`, connection)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing authorizations: %w", err)
	}
	defer rows.Close()
	var out []BackgroundAuthorization
	for rows.Next() {
		a, err := scanBackgroundAuthorization(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetConnectionExecutable records the adapter executable a connection's
// placements run ("<package>@<version>/<program>"). A change is a
// material edit: the live authorization is revoked, and with it the
// Owner's approval stops applying (the subscription names the old
// digest).
func (db *DB) SetConnectionExecutable(connection, executable, actor string) error {
	if executable == "" || actor == "" {
		return fmt.Errorf("runtimedb: recording an adapter executable needs the executable and who")
	}
	conn, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	if conn.Executable == executable {
		return nil
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: recording executable: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE integration_connections SET executable = ?, updated_at = ? WHERE seq = ?`,
		executable, now(), conn.Seq); err != nil {
		return fmt.Errorf("runtimedb: recording executable: %w", err)
	}
	revoked, err := revokeAuthorizationsTx(tx, conn.Seq, "", nil, actor,
		fmt.Sprintf("the adapter executable changed from %q to %q", conn.Executable, executable))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: recording executable: %w", err)
	}
	for _, s := range revoked {
		if err := db.journal("grant", backgroundAuthorizationJournalID(s), "revoked", actor, "adapter executable changed"); err != nil {
			return err
		}
	}
	return nil
}

// AuthorizeBackgroundUse is the evaluator a dispatcher calls before
// every unattended use of a Personal credential — before queuing a
// scheduled read, and again before acting on its result.
//
// It returns a grant only when BOTH artifacts hold, live, for the exact
// same thing:
//
//   - the owning Person's authorization for this connection, unrevoked,
//     bound to the active credential version, the connection's current
//     adapter executable, and this subscription's current content; and
//   - the Owner's approval of this exact subscription version, enabled,
//     for the same Agent and audience, naming this exact authorization.
//
// Owner approval alone, an authorization by anybody but the owner, an
// Agent's grants, a shared relationship, or a previous interactive
// login never substitute; none of them is consulted. It also refuses
// while the database is quarantined or execution is stopped: a restore
// cannot revive use before the journal replay has run.
func (db *DB) AuthorizeBackgroundUse(ctx context.Context, connection string, subscription int64, approvedVersion int) (BackgroundGrant, error) {
	deny := func(format string, args ...any) (BackgroundGrant, error) {
		return BackgroundGrant{}, fmt.Errorf("%w: %s", ErrBackgroundUseDenied, fmt.Sprintf(format, args...))
	}
	if err := ctx.Err(); err != nil {
		return BackgroundGrant{}, err
	}
	if err := db.gate(GateDispatch); err != nil {
		return BackgroundGrant{}, fmt.Errorf("%w: %w", ErrBackgroundUseDenied, err)
	}
	conn, err := db.GetConnection(connection)
	if err != nil {
		return deny("no connection %q", connection)
	}
	if conn.Ownership != OwnershipPersonal {
		return deny("%q is not a Personal connection", connection)
	}
	if conn.State == ConnectionUnavailable {
		return deny("%q is unavailable: %s", connection, conn.DegradedReason)
	}
	if m, err := db.GetButlerMember(conn.Owner); err != nil || !m.Active() {
		return deny("%s, whose connection this is, is not an active member", conn.Owner)
	}

	// The Person's half.
	a, err := db.LiveBackgroundAuthorization(connection)
	if err != nil {
		return deny("%s has not authorized unattended use of %q", conn.Owner, connection)
	}
	if a.Person != conn.Owner {
		return deny("the authorization was not given by %s, whose credential it is", conn.Owner)
	}
	if digest, err := authorizationDigest(a, conn.Seq); err != nil || digest != a.Digest {
		return deny("the stored authorization does not match its own digest")
	}
	cred, err := db.GetPersonalCredential(a.Credential)
	if err != nil || cred.State != CredentialActive || cred.Version != a.CredentialVersion || cred.Member != conn.Owner {
		return deny("the credential the authorization names is no longer the active one")
	}
	if conn.Executable == "" || conn.Executable != a.Executable {
		return deny("the adapter executable changed since it was authorized (%q, now %q)", a.Executable, conn.Executable)
	}
	if a.ProcessingLocation == ProcessingCloud && !conn.CloudAllowed {
		return deny("cloud processing was authorized but %q is now local-only", connection)
	}
	if a.Subscription != subscription {
		return deny("the authorization is for subscription %d, not %d", a.Subscription, subscription)
	}

	// The Owner's half.
	sub, err := db.GetAgentSubscription(subscription)
	if err != nil {
		return deny("no subscription %d", subscription)
	}
	if sub.ApprovedVersion == 0 || sub.ApprovedBy == "" {
		return deny("subscription %d has not been approved by an Owner", subscription)
	}
	if sub.ApprovedVersion != approvedVersion {
		return deny("subscription %d is at approved version %d, not %d", subscription, sub.ApprovedVersion, approvedVersion)
	}
	if !sub.Enabled {
		return deny("subscription %d is disabled", subscription)
	}
	if digest, err := SubscriptionContentDigest(sub); err != nil || digest != a.SubscriptionDigest {
		return deny("subscription %d changed after %s authorized it", subscription, a.Person)
	}
	if c, d := subscriptionNames(sub); c != connection || d != a.Digest {
		return deny("subscription %d does not name this connection's live authorization", subscription)
	}

	// The intersection: the same Agent and audience on both sides, and
	// the Agent still one the Person may use.
	if sub.Butler != a.Agent {
		return deny("the subscription's Agent is not the one the Person authorized")
	}
	if sub.Audience != a.ResultAudience {
		return deny("the subscription's audience is not the one the Person authorized")
	}
	if err := db.personMayUseAgent(conn.Owner, a.Agent); err != nil {
		return deny("%v", err)
	}
	return BackgroundGrant{
		Connection: connection, Person: conn.Owner, Authorization: a.Seq, AuthorizationDigest: a.Digest,
		Credential: cred.Seq, CredentialVersion: cred.Version, SecretRef: cred.SecretRef, Scope: a.Scope,
		ProcessingLocation: a.ProcessingLocation, ResultAudience: a.ResultAudience, Agent: a.Agent,
		TargetCalendar: a.TargetCalendar, Executable: a.Executable, Subscription: sub.ID,
		SubscriptionVersion: sub.ApprovedVersion, LeaseEpoch: conn.LeaseEpoch,
	}, nil
}
