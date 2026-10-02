package runtimedb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/research"
)

func scanSearchProvider(row interface{ Scan(...any) error }) (research.Provider, error) {
	var p research.Provider
	var raw string
	err := row.Scan(&p.ID, &raw, &p.ConfigRevision, &p.State, &p.TestedConfigRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(raw), &p.Config)
	return p, err
}

const providerColumns = `id,config_json,config_revision,state,tested_config_revision`

func (db *DB) SearchProvider(id string) (research.Provider, error) {
	return scanSearchProvider(db.sql.QueryRow(`SELECT `+providerColumns+` FROM search_providers WHERE id=?`, id))
}
func (db *DB) SearchProviders() ([]research.Provider, error) {
	rows, err := db.sql.Query(`SELECT ` + providerColumns + ` FROM search_providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []research.Provider{}
	for rows.Next() {
		p, err := scanSearchProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApproveResearchPlan atomically compares the stored revision, changes the
// registry, and consumes the exact Plan. No host resource is provisioned here.
func (db *DB) ApproveResearchPlan(id, by string) error {
	seq, err := parsePlanID(id)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw, state string
	if err = tx.QueryRow(`SELECT plan_json,state FROM plans WHERE seq=?`, seq).Scan(&raw, &state); err != nil {
		return err
	}
	if state != string(PlanProposed) {
		return research.ErrConflict
	}
	var stored struct {
		Research *research.Change `json:"research"`
	}
	if err = json.Unmarshal([]byte(raw), &stored); err != nil {
		return err
	}
	if stored.Research == nil {
		return errors.New("not a research Plan")
	}
	c := stored.Research
	old, err := scanSearchProvider(tx.QueryRow(`SELECT `+providerColumns+` FROM search_providers WHERE id=?`, c.ProviderID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	switch c.Operation {
	case "create":
		if err == nil || c.ExpectedRevision != "" {
			return research.ErrConflict
		}
	case "update", "delete":
		if err != nil || c.ExpectedRevision == "" || old.ConfigRevision != c.ExpectedRevision {
			return research.ErrConflict
		}
	default:
		return errors.New("invalid research operation")
	}
	if c.Operation == "delete" {
		_, err = tx.Exec(`DELETE FROM search_providers WHERE id=?`, c.ProviderID)
	} else {
		if c.Config == nil {
			return errors.New("missing research configuration")
		}
		config, e := c.Config.Normalize()
		if e != nil {
			return e
		}
		data, e := json.Marshal(config)
		if e != nil {
			return e
		}
		readiness := "untested"
		if config.EndpointRef == "" {
			readiness = "unconfigured"
		}
		_, err = tx.Exec(`INSERT INTO search_providers(id,config_json,config_revision,state) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET config_json=excluded.config_json,config_revision=excluded.config_revision,state=excluded.state,tested_config_revision='',test_revision=''`, c.ProviderID, string(data), research.NewID(), readiness)
	}
	if err != nil {
		return err
	}
	// Registry-only Plans are complete now, not authorization for a reconciler
	// infrastructure tick. Record the terminal state in the same transaction.
	_, err = tx.Exec(`UPDATE plans SET state='applied',resolved_by=?,resolved_at=? WHERE seq=?`, by, now(), seq)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RecordSearchProviderTest uses a compare-and-swap: an in-flight test cannot
// bless edited or removed configuration. Detail from upstream is never stored.
func (db *DB) BeginSearchProviderTest(id, revision string) (string, error) {
	token := research.NewID()
	result, err := db.sql.Exec(`UPDATE search_providers SET state='untested',tested_config_revision='',test_revision=? WHERE id=? AND config_revision=? AND state!='unconfigured'`, token, id, revision)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return "", research.ErrConflict
	}
	return token, err
}
func (db *DB) RecordSearchProviderTest(id, revision, probe string, ready bool) error {
	if probe == "" {
		return research.ErrConflict
	}
	state := "unavailable"
	if ready {
		state = "ready"
	}
	result, err := db.sql.Exec(`UPDATE search_providers SET state=?,tested_config_revision=?,test_revision='' WHERE id=? AND config_revision=? AND test_revision=? AND state!='unconfigured'`, state, revision, id, revision, probe)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return research.ErrConflict
	}
	return err
}

// InvalidateSearchProvider is the adapter/service hook for observed dependency
// loss, credential rotation or endpoint revision changes. It also fences probes.
func (db *DB) InvalidateSearchProvider(id, revision string) error {
	result, err := db.sql.Exec(`UPDATE search_providers SET state='unavailable',tested_config_revision='',test_revision='' WHERE id=? AND config_revision=?`, id, revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return research.ErrConflict
	}
	return err
}
func researchScope(agent int64) string {
	if agent == 0 {
		return "household"
	}
	return "agent/" + strconv.FormatInt(agent, 10)
}
func (db *DB) ResearchSelection(agent int64) (research.Selection, error) {
	var s research.Selection
	err := db.sql.QueryRow(`SELECT provider_id,config_revision FROM research_selections WHERE scope=?`, researchScope(agent)).Scan(&s.ProviderID, &s.ConfigRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	return s, err
}

// SetResearchSelection never changes assignments/bundles. Agent uses its stable
// database ID, not a display name. Missing selections have the empty revision.
func (db *DB) SetResearchSelection(agent int64, provider *string, expected string) error {
	if agent < 0 {
		return errors.New("invalid Agent")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if agent != 0 {
		var n int
		if err = tx.QueryRow(`SELECT count(*) FROM butlers WHERE seq=?`, agent).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrNotFound
		}
	}
	if provider != nil {
		var n int
		if err = tx.QueryRow(`SELECT count(*) FROM search_providers WHERE id=?`, *provider).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrNotFound
		}
	}
	var revision string
	err = tx.QueryRow(`SELECT config_revision FROM research_selections WHERE scope=?`, researchScope(agent)).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if revision != expected {
		return research.ErrConflict
	}
	_, err = tx.Exec(`INSERT INTO research_selections(scope,provider_id,config_revision) VALUES(?,?,?) ON CONFLICT(scope) DO UPDATE SET provider_id=excluded.provider_id,config_revision=excluded.config_revision`, researchScope(agent), provider, research.NewID())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveSearchProvider only resolves configuration readiness. Callers must
// independently enforce tool assignment, current identity and egress policy.
// One query observes selection/default/provider in a consistent snapshot.
func (db *DB) ResolveSearchProvider(agent int64) (research.Provider, string, error) {
	if agent <= 0 {
		return research.Provider{}, "provider_unavailable: Agent missing", nil
	}
	var exists int
	if err := db.sql.QueryRow(`SELECT count(*) FROM butlers WHERE seq=?`, agent).Scan(&exists); err != nil {
		return research.Provider{}, "", err
	}
	if exists != 1 {
		return research.Provider{}, "provider_unavailable: Agent missing", nil
	}
	p, err := scanSearchProvider(db.sql.QueryRow(`SELECT `+providerColumns+` FROM search_providers WHERE id=COALESCE((SELECT provider_id FROM research_selections WHERE scope=?),(SELECT provider_id FROM research_selections WHERE scope='household'))`, researchScope(agent)))
	if errors.Is(err, ErrNotFound) {
		return p, "provider_unavailable: selected provider missing or no default configured", nil
	}
	if err != nil {
		return p, "", err
	}
	if p.State != "ready" || p.TestedConfigRevision != p.ConfigRevision {
		return p, fmt.Sprintf("provider_unavailable: selected provider is %s", p.State), nil
	}
	return p, "", nil
}
