package runtimedb

// Household secrets are a separate domain from ADR-W012's node credentials.
// Ciphertext alone is persisted. The caller, Agent, resource grant and surface
// are intersected again at resolution, never inferred from possession of an ID.
import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var ErrSecretDenied = errors.New("secret unavailable or permission denied")
var secretLabel = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 ._-]{0,63}$`)

type SecretVault struct {
	db      *DB
	aead    cipher.AEAD
	keyPath string
}
type SecretInput struct {
	OriginConnection int64  `json:"-"`
	Scope            string `json:"scope"`
	Label            string `json:"label"`
	Value            string `json:"value"`
	Submit           bool   `json:"submit"`
}
type SecretObject struct {
	ID      string `json:"id"`
	Scope   string `json:"scope"`
	Owner   string `json:"ownerPersonId"`
	Label   string `json:"label"`
	State   string `json:"state"`
	Version int    `json:"version"`
}
type SecretGrant struct {
	PersonID string `json:"personId,omitempty"`
	GroupID  string `json:"groupId,omitempty"`
	Verb     string `json:"verb"`
}

func (db *DB) OpenSecretVault(keyPath string) (*SecretVault, error) {
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		var n int
		if err = db.sql.QueryRow(`SELECT (SELECT COUNT(*) FROM household_secrets) + (SELECT COUNT(*) FROM integration_pairings)`).Scan(&n); err != nil {
			return nil, err
		}
		if n != 0 {
			return nil, errors.New("household secret key missing; restore the original key")
		}
		if err = os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid household secret key")
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	v := &SecretVault{db: db, aead: a, keyPath: keyPath}
	rows, err := db.sql.Query(`SELECT id,scope,owner_person_id,label,state,version,ciphertext FROM household_secrets WHERE length(ciphertext)>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s SecretObject
		var b []byte
		if err = rows.Scan(&s.ID, &s.Scope, &s.Owner, &s.Label, &s.State, &s.Version, &b); err != nil {
			return nil, err
		}
		raw, e := v.unseal(s, b)
		clear(raw)
		if e != nil {
			return nil, errors.New("household secret key or ciphertext invalid; refusing to open store")
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = v.checkPairingsOpen(); err != nil {
		return nil, err
	}
	return v, nil
}
func secretAAD(s SecretObject) []byte {
	return []byte(fmt.Sprintf("household-secret/v1/%s/%s/%s/%d", s.ID, s.Scope, s.Owner, s.Version))
}
func (v *SecretVault) seal(s SecretObject, value string) ([]byte, error) {
	if strings.Contains(s.Label, value) {
		return nil, errors.New("choose a safe label that does not contain the value")
	}
	if len(value) == 0 || len(value) > 8192 {
		return nil, errors.New("secret value must contain 1 to 8192 bytes")
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, []byte(value), secretAAD(s)), nil
}
func (v *SecretVault) unseal(s SecretObject, b []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(b) < n {
		return nil, ErrSecretDenied
	}
	raw, err := v.aead.Open(nil, b[:n], b[n:], secretAAD(s))
	if err != nil {
		return nil, ErrSecretDenied
	}
	return raw, nil
}
func secretRow(tx *sql.Tx, id string) (SecretObject, []byte, error) {
	var s SecretObject
	var b []byte
	err := tx.QueryRow(`SELECT id,scope,owner_person_id,label,state,version,ciphertext FROM household_secrets WHERE id=?`, id).Scan(&s.ID, &s.Scope, &s.Owner, &s.Label, &s.State, &s.Version, &b)
	if err != nil {
		return s, nil, ErrSecretDenied
	}
	return s, b, nil
}
func secretAudit(tx *sql.Tx, id, actor, action string) error {
	_, err := tx.Exec(`INSERT INTO household_secret_audit(secret_id,actor_person_id,action,at) VALUES(?,?,?,?)`, id, actor, action, now())
	return err
}
func (v *SecretVault) Create(actor string, in SecretInput) (SecretObject, error) {
	var s SecretObject
	if err := v.db.gate(GateIngestion); err != nil {
		return s, err
	}
	if !secretLabel.MatchString(in.Label) || (in.Scope != "personal" && in.Scope != "household") || (in.Submit && in.Scope != "household") {
		return s, errors.New("choose a safe label and Personal or Household scope")
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return s, err
	}
	defer tx.Rollback()
	p, err := activeMemoryActor(tx, actor, in.Scope == "household" && !in.Submit)
	if err != nil {
		return s, ErrSecretDenied
	}
	// 96 random bits; this is a non-bearer identifier, kept below the
	// generic secret-value redactor's token length without adding exemptions.
	r := make([]byte, 12)
	if _, err = rand.Read(r); err != nil {
		return s, err
	}
	s = SecretObject{ID: "hs_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(r), Scope: in.Scope, Owner: p, Label: in.Label, State: "active", Version: 1}
	if in.Submit {
		s.State = "pending"
	}
	b, err := v.seal(s, in.Value)
	if err != nil {
		return s, err
	}
	_, err = tx.Exec(`INSERT INTO household_secrets(id,scope,owner_person_id,label,state,version,ciphertext,created_at) VALUES(?,?,?,?,?,?,?,?)`, s.ID, s.Scope, s.Owner, s.Label, s.State, s.Version, b, now())
	if err != nil {
		return s, err
	}
	if _, err = tx.Exec(`INSERT INTO household_secret_origins VALUES(?,?,?,?)`, s.ID, in.OriginConnection, in.OriginConnection != 0, now()); err != nil {
		return s, err
	}
	if err = secretAudit(tx, s.ID, p, "create-"+s.State); err != nil {
		return s, err
	}
	return s, tx.Commit()
}
func secretManager(tx *sql.Tx, s SecretObject, actor string) (string, error) {
	p, err := activeMemoryActor(tx, actor, s.Scope == "household")
	if err != nil || (s.Scope == "personal" && s.Owner != p) {
		return "", ErrSecretDenied
	}
	return p, nil
}
func (v *SecretVault) Rotate(actor, id, value string) error {
	if err := v.db.gate(GateIngestion); err != nil {
		return err
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s, _, err := secretRow(tx, id)
	if err != nil {
		return err
	}
	// Direct Personal editing remains deferred. Intake does not grant a UI CRUD surface.
	if s.Scope != "household" || s.State != "active" {
		return ErrSecretDenied
	}
	p, err := secretManager(tx, s, actor)
	if err != nil {
		return err
	}
	s.Version++
	b, err := v.seal(s, value)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE household_secrets SET ciphertext=?,version=? WHERE id=?`, b, s.Version, id); err != nil {
		return err
	}
	if err = secretJournal(tx, id, actor, false); err != nil {
		return err
	}
	if err = secretAudit(tx, id, p, "rotate"); err != nil {
		return err
	}
	return tx.Commit()
}
func (v *SecretVault) Grant(actor, id string, g SecretGrant, allow bool) error {
	if allow {
		if err := v.db.gate(GateIngestion); err != nil {
			return err
		}
	}
	if (g.PersonID == "") == (g.GroupID == "") || (g.Verb != "use" && g.Verb != "reveal") {
		return ErrSecretDenied
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s, _, err := secretRow(tx, id)
	if err != nil {
		return err
	}
	if s.Scope != "household" || s.State != "active" {
		return ErrSecretDenied
	}
	p, err := secretManager(tx, s, actor)
	if err != nil {
		return err
	}
	var person, group any
	if g.PersonID != "" {
		person = g.PersonID
	} else {
		group = g.GroupID
	}
	if allow {
		_, err = tx.Exec(`INSERT OR IGNORE INTO household_secret_grants(secret_id,person_id,group_id,verb) VALUES(?,?,?,?)`, id, person, group, g.Verb)
	} else {
		_, err = tx.Exec(`DELETE FROM household_secret_grants WHERE secret_id=? AND person_id IS ? AND group_id IS ? AND verb=?`, id, person, group, g.Verb)
	}
	if err != nil {
		return ErrSecretDenied
	}
	if !allow {
		if err = secretJournal(tx, id, actor, false); err != nil {
			return err
		}
	}
	if err = secretAudit(tx, id, p, "grant-change"); err != nil {
		return err
	}
	return tx.Commit()
}
func (v *SecretVault) AgentGrant(actor, id, agent, purpose string, allow bool) error {
	if allow {
		if err := v.db.gate(GateIngestion); err != nil {
			return err
		}
	}
	if purpose != "calendar.get_agenda" && purpose != "reveal" {
		return ErrSecretDenied
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s, _, err := secretRow(tx, id)
	if err != nil {
		return err
	}
	if s.State != "active" {
		return ErrSecretDenied
	}
	p, err := secretManager(tx, s, actor)
	if err != nil {
		return err
	}
	if s.Scope == "personal" && purpose == "reveal" {
		return ErrSecretDenied
	}
	var aid int64
	if err = tx.QueryRow(`SELECT b.seq FROM butlers b JOIN butler_access a ON a.butler=b.seq WHERE b.name=? AND b.enabled=1 AND b.retired_at='' AND a.person_id=? AND a.revoked_at=''`, agent, p).Scan(&aid); err != nil {
		return ErrSecretDenied
	}
	if allow {
		_, err = tx.Exec(`INSERT OR IGNORE INTO household_secret_agents VALUES(?,?,?)`, id, aid, purpose)
	} else {
		_, err = tx.Exec(`DELETE FROM household_secret_agents WHERE secret_id=? AND agent=? AND purpose=?`, id, aid, purpose)
	}
	if err != nil {
		return err
	}
	if !allow {
		if err = secretJournal(tx, id, actor, false); err != nil {
			return err
		}
	}
	if err = secretAudit(tx, id, p, "agent-grant-change"); err != nil {
		return err
	}
	return tx.Commit()
}
func (v *SecretVault) authorize(tx *sql.Tx, s SecretObject, c *MemoryReadContext, purpose string) error {
	if c == nil || c.person == "" || c.agent == 0 || c.destination.Group || !c.destination.BuiltIn || c.destination.ConnectionRef != 0 {
		return ErrSecretDenied
	}
	// Secret use starts on trusted surfaces. External disclosure policy is slice 16.
	var n int
	var role, class string
	err := tx.QueryRow(`SELECT p.role,b.agent_class FROM butler_members p JOIN butlers b ON b.seq=? WHERE p.person_id=? AND p.admitted=1 AND p.disabled_at='' AND p.offboarded_at='' AND b.enabled=1 AND b.retired_at='' AND (b.agent_class='member' OR p.role='owner') AND (EXISTS(SELECT 1 FROM butler_access a WHERE a.butler=b.seq AND a.person_id=p.person_id AND a.revoked_at='') OR (? AND p.role='owner'))`, c.agent, c.person, c.destination.ConsoleOwner).Scan(&role, &class)
	if err != nil || s.State != "active" {
		return ErrSecretDenied
	}
	if err = tx.QueryRow(`SELECT COUNT(*) FROM household_secret_agents WHERE secret_id=? AND agent=? AND purpose=?`, s.ID, c.agent, purpose).Scan(&n); err != nil || n != 1 {
		return ErrSecretDenied
	}
	if s.Scope == "personal" {
		if s.Owner != c.person || purpose == "reveal" {
			return ErrSecretDenied
		}
		return nil
	}
	verb := "use"
	if purpose == "reveal" {
		verb = "reveal"
	}
	return secretPrincipalGrant(tx, s.ID, c.person, verb)
}
func secretPrincipalGrant(tx *sql.Tx, id, person, verb string) error {
	return secretGeneralGrant(tx, id, person, 0, verb)
}

// secretGeneralGrant is the principal/object leg shared by attended Person
// use and unattended exact-version use. A routine has no Person/Group identity
// and the schema gives it only the use verb.
func secretGeneralGrant(tx *sql.Tx, id, person string, routinePlan int64, verb string) error {
	if routinePlan != 0 {
		if person != "" || verb != "use" {
			return ErrSecretDenied
		}
		var n int
		err := tx.QueryRow(`SELECT count(*) FROM household_secret_routine_grants WHERE secret_id=? AND plan=? AND verb='use'`, id, routinePlan).Scan(&n)
		if err != nil || n != 1 {
			return ErrSecretDenied
		}
		return nil
	}
	if person == "" {
		return ErrSecretDenied
	}
	// Group membership uses the shared resolver in the same transaction.
	rows, err := tx.Query(`SELECT person_id,group_id FROM household_secret_grants WHERE secret_id=? AND verb=?`, id, verb)
	if err != nil {
		return err
	}
	var grants []SecretGrant
	for rows.Next() {
		var p, g sql.NullString
		if err = rows.Scan(&p, &g); err != nil {
			rows.Close()
			return err
		}
		grants = append(grants, SecretGrant{PersonID: p.String, GroupID: g.String})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, g := range grants {
		if g.PersonID == person {
			return nil
		}
		if g.GroupID != "" {
			people, e := resolveGroupMembers(tx, g.GroupID)
			if e != nil {
				return e
			}
			for _, p := range people {
				if p == person {
					return nil
				}
			}
		}
	}
	return ErrSecretDenied
}

func (v *SecretVault) routineUseAuthorized(tx *sql.Tx, id string, p *RoutinePrincipal, purpose string) (SecretObject, []byte, error) {
	routine, err := v.db.routineVersion(tx, p)
	if err != nil || routine.agent == 0 {
		return SecretObject{}, nil, ErrSecretDenied
	}
	s, ciphertext, err := secretRow(tx, id)
	if err != nil || s.Scope != "household" || s.State != "active" {
		return SecretObject{}, nil, ErrSecretDenied
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM household_secret_agents WHERE secret_id=? AND agent=? AND purpose=?", id, routine.agent, purpose).Scan(&n); err != nil || n != 1 {
		return SecretObject{}, nil, ErrSecretDenied
	}
	if err = secretGeneralGrant(tx, id, "", p.plan, "use"); err != nil {
		return SecretObject{}, nil, err
	}
	return s, ciphertext, nil
}

func (v *SecretVault) CheckRoutineUse(id string, p *RoutinePrincipal, purpose string) error {
	if purpose != "calendar.get_agenda" {
		return ErrSecretDenied
	}
	if err := v.db.gate(GateDispatch); err != nil {
		return err
	}
	tx, err := v.db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, _, err = v.routineUseAuthorized(tx, id, p, purpose)
	return err
}

// UseRoutine resolves a Household credential below inference. The exact Plan
// principal is rechecked with Agent and resource grants on every call.
func (v *SecretVault) UseRoutine(id string, p *RoutinePrincipal, purpose string, consume func([]byte) error) error {
	if purpose != "calendar.get_agenda" || consume == nil {
		return ErrSecretDenied
	}
	if err := v.db.gate(GateDispatch); err != nil {
		return err
	}
	tx, err := v.db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s, ciphertext, err := v.routineUseAuthorized(tx, id, p, purpose)
	if err != nil {
		return err
	}
	raw, err := v.unseal(s, ciphertext)
	if err != nil {
		return err
	}
	defer clear(raw)
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = consume(raw); err != nil {
		return errors.New("secret-backed operation failed")
	}
	_, err = v.db.sql.Exec("INSERT INTO household_secret_routine_audit(secret_id,plan,action,at) VALUES(?,?,'use',?)", id, p.plan, now())
	return err
}

// RevealRoutine deliberately has no raw-value path for unattended work.
func (v *SecretVault) RevealRoutine(_ string, _ *RoutinePrincipal) ([]byte, error) {
	return nil, ErrSecretDenied
}

func (v *SecretVault) CalendarBindingRoutine(p *RoutinePrincipal) (string, error) {
	tx, err := v.db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	routine, err := v.db.routineVersion(tx, p)
	if err != nil || routine.agent == 0 {
		return "", ErrSecretDenied
	}
	var id string
	err = tx.QueryRow("SELECT secret_id FROM household_secret_bindings WHERE purpose='calendar.get_agenda' AND principal='household'").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Use's callback is trusted runtime code, not a model-selected function. The
// read snapshot fixes authorization before consumption and closes before network
// I/O. Concurrent revocation fences subsequent uses; completion is audited separately.
func (v *SecretVault) Use(id string, c *MemoryReadContext, purpose string, consume func([]byte) error) error {
	if purpose != "calendar.get_agenda" || consume == nil {
		return ErrSecretDenied
	}
	if err := v.db.gate(GateDispatch); err != nil {
		return err
	}
	tx, err := v.db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s, b, err := secretRow(tx, id)
	if err != nil {
		return err
	}
	if err = v.authorize(tx, s, c, purpose); err != nil {
		return err
	}
	raw, err := v.unseal(s, b)
	if err != nil {
		return err
	}
	defer clear(raw)
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = consume(raw); err != nil {
		return errors.New("secret-backed operation failed")
	}
	audit, err := v.db.sql.Begin()
	if err != nil {
		return err
	}
	defer audit.Rollback()
	if err = secretAudit(audit, id, c.person, "use"); err != nil {
		return err
	}
	return audit.Commit()
}
func (v *SecretVault) Reveal(id string, c *MemoryReadContext, surface string) ([]byte, error) {
	if err := v.db.gate(GateRetrieval); err != nil {
		return nil, err
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s, b, err := secretRow(tx, id)
	if err != nil {
		return nil, err
	}
	if s.Scope != "household" {
		return nil, ErrSecretDenied
	}
	if err = v.authorize(tx, s, c, "reveal"); err != nil {
		return nil, err
	}
	var class string
	if err = tx.QueryRow(`SELECT agent_class FROM butlers WHERE seq=?`, c.agent).Scan(&class); err != nil {
		return nil, err
	}
	if (class == "member" && surface != "homepage") || (class == "owner" && (surface != "console" || !c.destination.ConsoleOwner)) {
		return nil, ErrSecretDenied
	}
	raw, err := v.unseal(s, b)
	if err != nil {
		return nil, err
	}
	if err = secretAudit(tx, id, c.person, "reveal"); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		clear(raw)
		return nil, err
	}
	return raw, nil
}
func (v *SecretVault) Lifecycle(actor, id, action string) error {
	if action == "approve" {
		if err := v.db.gate(GateIngestion); err != nil {
			return err
		}
	}
	state := map[string]string{"quarantine": "quarantine", "disable": "disabled", "delete": "deleted", "offboard": "quarantine", "approve": "active", "reject": "deleted"}[action]
	if state == "" {
		return ErrSecretDenied
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := activeMemoryActor(tx, actor, true)
	if err != nil {
		return ErrSecretDenied
	}
	s, _, err := secretRow(tx, id)
	if err != nil {
		return err
	}
	if action == "approve" && (s.Scope != "household" || s.State != "pending") {
		return ErrSecretDenied
	}
	if action == "reject" && s.State != "pending" {
		return ErrSecretDenied
	}
	if s.State == "deleted" {
		return nil
	}
	_, err = tx.Exec(`UPDATE household_secrets SET state=?,ciphertext=CASE WHEN ?='deleted' THEN X'' ELSE ciphertext END WHERE id=?`, state, state, id)
	if err != nil {
		return err
	}
	if state != "active" {
		if _, err = tx.Exec(`DELETE FROM household_secret_agents WHERE secret_id=?`, id); err != nil {
			return err
		}
	}
	if state != "active" {
		if err = secretJournal(tx, id, actor, state == "deleted"); err != nil {
			return err
		}
	}
	if err = secretAudit(tx, id, p, action); err != nil {
		return err
	}
	return tx.Commit()
}

// Review only decrypts explicitly submitted Household copies, never Personal.
func (v *SecretVault) Review(actor, id string) ([]byte, error) {
	if err := v.db.gate(GateRetrieval); err != nil {
		return nil, err
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := activeMemoryActor(tx, actor, true)
	if err != nil {
		return nil, ErrSecretDenied
	}
	s, b, err := secretRow(tx, id)
	if err != nil || s.Scope != "household" || s.State != "pending" {
		return nil, ErrSecretDenied
	}
	raw, err := v.unseal(s, b)
	if err != nil {
		return nil, err
	}
	if err = secretAudit(tx, id, p, "review-submission"); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		clear(raw)
		return nil, err
	}
	return raw, nil
}
func (v *SecretVault) List(actor string, admin bool) ([]SecretObject, error) {
	if err := v.db.gate(GateRetrieval); err != nil {
		return nil, err
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := activeMemoryActor(tx, actor, admin)
	if err != nil {
		return nil, ErrSecretDenied
	}
	rows, err := tx.Query(`SELECT id,scope,owner_person_id,label,state,version FROM household_secrets WHERE owner_person_id=? OR ? OR (scope='household' AND state='active') ORDER BY created_at DESC`, p, admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretObject{}
	for rows.Next() {
		var s SecretObject
		if err = rows.Scan(&s.ID, &s.Scope, &s.Owner, &s.Label, &s.State, &s.Version); err != nil {
			return nil, err
		}
		if admin && s.Scope == "personal" && s.Owner != p {
			s.Label = "Personal item"
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	visible := []SecretObject{}
	for _, o := range out {
		if admin || o.Owner == p || secretPrincipalGrant(tx, o.ID, p, "use") == nil || secretPrincipalGrant(tx, o.ID, p, "reveal") == nil {
			visible = append(visible, o)
		}
	}
	return visible, nil
}
func (v *SecretVault) BindCalendar(actor, id string) error {
	if err := v.db.gate(GateIngestion); err != nil {
		return err
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s, _, err := secretRow(tx, id)
	if err != nil || s.State != "active" {
		return ErrSecretDenied
	}
	p, err := secretManager(tx, s, actor)
	if err != nil {
		return err
	}
	binding := p
	if s.Scope == "household" {
		binding = "household"
	}
	if _, err = tx.Exec(`INSERT INTO household_secret_bindings VALUES('calendar.get_agenda',?,?) ON CONFLICT(purpose,principal) DO UPDATE SET secret_id=excluded.secret_id`, binding, id); err != nil {
		return err
	}
	if err = secretAudit(tx, id, p, "bind-calendar"); err != nil {
		return err
	}
	return tx.Commit()
}
func (v *SecretVault) CalendarBinding(c *MemoryReadContext) (string, error) {
	person := ""
	if c != nil {
		person = c.person
	}
	var id string
	err := v.db.sql.QueryRow(`SELECT secret_id FROM household_secret_bindings WHERE purpose='calendar.get_agenda' AND principal IN (?, 'household') ORDER BY principal='household' LIMIT 1`, person).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Context is stamped by the per-turn runtime closure, never by tool arguments.
type secretUseContextKey struct{}
type routineSecretUseContextKey struct{}

func WithSecretUseContext(ctx context.Context, c *MemoryReadContext) context.Context {
	return context.WithValue(ctx, secretUseContextKey{}, c)
}
func SecretUseContext(ctx context.Context) *MemoryReadContext {
	c, _ := ctx.Value(secretUseContextKey{}).(*MemoryReadContext)
	return c
}

// WithRoutineSecretUseContext is set only by trusted unattended execution.
// It replaces an interactive Person context rather than borrowing one.
func WithRoutineSecretUseContext(ctx context.Context, p *RoutinePrincipal) context.Context {
	return context.WithValue(ctx, routineSecretUseContextKey{}, p)
}
func RoutineSecretUseContext(ctx context.Context) *RoutinePrincipal {
	p, _ := ctx.Value(routineSecretUseContextKey{}).(*RoutinePrincipal)
	return p
}

func secretJournal(tx *sql.Tx, id, actor string, deleted bool) error {
	action := "revoked"
	if deleted {
		action = "deleted"
	}
	_, err := tx.Exec(`INSERT INTO deletion_journal(subject_kind,subject_id,action,at,actor,detail) VALUES('secret',?,?,?,?, 'household domain; restore must not resurrect authority')`, "household-secret/"+id, action, now(), actor)
	return err
}
func (v *SecretVault) RecordDisclosure(actor string, connection int64) error {
	if connection <= 0 {
		return nil
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := activeMemoryActor(tx, actor, false)
	if err != nil {
		return ErrSecretDenied
	}
	_, err = tx.Exec(`INSERT INTO household_secret_disclosures(person_id,connection_ref,recorded_at) VALUES(?,?,?)`, p, connection, now())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SubmitPersonalCopy is explicit owning-Person disclosure consent, not Owner
// access to a Personal object. The original is neither changed nor returned.
func (v *SecretVault) SubmitPersonalCopy(actor, id string) (SecretObject, error) {
	var out SecretObject
	if err := v.db.gate(GateIngestion); err != nil {
		return out, err
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	p, err := activeMemoryActor(tx, actor, false)
	if err != nil {
		return out, ErrSecretDenied
	}
	source, b, err := secretRow(tx, id)
	if err != nil || source.Scope != "personal" || source.Owner != p || source.State != "active" {
		return out, ErrSecretDenied
	}
	raw, err := v.unseal(source, b)
	if err != nil {
		return out, err
	}
	defer clear(raw)
	r := make([]byte, 12)
	if _, err = rand.Read(r); err != nil {
		return out, err
	}
	out = SecretObject{ID: "hs_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(r), Scope: "household", Owner: p, Label: source.Label, State: "pending", Version: 1}
	encrypted, err := v.seal(out, string(raw))
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(`INSERT INTO household_secrets(id,scope,owner_person_id,label,state,version,ciphertext,created_at) VALUES(?,?,?,?,?,?,?,?)`, out.ID, out.Scope, out.Owner, out.Label, out.State, out.Version, encrypted, now()); err != nil {
		return out, err
	}
	if _, err = tx.Exec(`INSERT INTO household_secret_origins SELECT ?,connection_ref,disclosed_externally,? FROM household_secret_origins WHERE secret_id=?`, out.ID, now(), source.ID); err != nil {
		return out, err
	}
	if err = secretAudit(tx, out.ID, p, "submit-personal-copy"); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
