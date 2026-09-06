package runtimedb

// The Butler's provider configuration, held as runtime state so it can
// be changed without restarting Keystone (ADR-W017 A9).

import (
	"database/sql"
	"errors"
	"time"
)

// ButlerConfig is which provider answers, and what the probe made of it.
//
// It carries no API key and never will: ADR-W017 A4 keeps credentials in
// the process environment, and A9 made the CHOICE editable, not the
// secret.
type ButlerConfig struct {
	Provider    string    `json:"provider"`
	Model       string    `json:"model,omitempty"`
	Endpoint    string    `json:"endpoint,omitempty"`
	ProbeMode   string    `json:"probeMode,omitempty"`
	ProbeDetail string    `json:"probeDetail,omitempty"`
	ProbedAt    time.Time `json:"probedAt,omitzero"`
	UpdatedAt   time.Time `json:"updatedAt,omitzero"`
	UpdatedBy   string    `json:"updatedBy,omitempty"`
}

// GetButlerConfig returns the stored configuration, or ok=false when
// none has been set.
//
// Absent is not an error and must not be reported as one: a deployment
// that has never opened the Console has no stored config and is
// correctly running on its startup flags (A9 keeps them as the
// bootstrap).
func (d *DB) GetButlerConfig() (ButlerConfig, bool, error) {
	var c ButlerConfig
	var probedAt, updatedAt sql.NullTime
	err := d.sql.QueryRow(`SELECT provider, model, endpoint, probe_mode, probe_detail, probed_at, updated_at, updated_by
	                        FROM butler_config WHERE id = 1`).
		Scan(&c.Provider, &c.Model, &c.Endpoint, &c.ProbeMode, &c.ProbeDetail, &probedAt, &updatedAt, &c.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerConfig{}, false, nil
	}
	if err != nil {
		return ButlerConfig{}, false, err
	}
	c.ProbedAt, c.UpdatedAt = probedAt.Time, updatedAt.Time
	return c, true, nil
}

// SetButlerConfig records a choice, replacing whatever was there.
//
// The probe verdict is cleared, not carried over: it described the
// PREVIOUS provider and model, and keeping it would let a model inherit
// a pass it never earned — which is the failure A9 exists to stop.
func (d *DB) SetButlerConfig(c ButlerConfig) error {
	_, err := d.sql.Exec(`INSERT INTO butler_config (id, provider, model, endpoint, probe_mode, probe_detail, probed_at, updated_at, updated_by)
	                     VALUES (1, ?, ?, ?, '', '', NULL, ?, ?)
	                     ON CONFLICT(id) DO UPDATE SET
	                       provider = excluded.provider,
	                       model = excluded.model,
	                       endpoint = excluded.endpoint,
	                       probe_mode = '',
	                       probe_detail = '',
	                       probed_at = NULL,
	                       updated_at = excluded.updated_at,
	                       updated_by = excluded.updated_by`,
		c.Provider, c.Model, c.Endpoint, time.Now().UTC(), c.UpdatedBy)
	return err
}

// RecordButlerProbe stores what the probe made of the CURRENT config.
func (d *DB) RecordButlerProbe(mode, detail string) error {
	_, err := d.sql.Exec(`UPDATE butler_config SET probe_mode = ?, probe_detail = ?, probed_at = ? WHERE id = 1`,
		mode, detail, time.Now().UTC())
	return err
}
