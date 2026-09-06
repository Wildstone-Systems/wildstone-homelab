package runtimedb

import (
	"errors"
	"path/filepath"
	"testing"
)

func butlerTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestButlerSessionRoundTrip(t *testing.T) {
	db := butlerTestDB(t)

	id, err := db.InsertButlerSession(ButlerSession{
		Principal:      "butler",
		ActingFor:      "root-admin",
		Provider:       "scripted",
		Mode:           ButlerModeFull,
		Surface:        ButlerSurfaceAdmin,
		FinalText:      "all healthy",
		Capped:         true,
		TranscriptJSON: `{"provider":"scripted"}`,
	})
	if err != nil {
		t.Fatalf("InsertButlerSession: %v", err)
	}
	if id != "bs-1" {
		t.Fatalf("id = %q, want bs-1", id)
	}

	got, err := db.GetButlerSession(id)
	if err != nil {
		t.Fatalf("GetButlerSession: %v", err)
	}
	if got.Principal != "butler" || got.ActingFor != "root-admin" || got.Provider != "scripted" ||
		got.Mode != ButlerModeFull || got.Surface != ButlerSurfaceAdmin || got.FinalText != "all healthy" || !got.Capped {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.TranscriptJSON != `{"provider":"scripted"}` {
		t.Fatalf("transcript = %q", got.TranscriptJSON)
	}
	if got.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt not set")
	}
}

func TestButlerSessionListOmitsTranscript(t *testing.T) {
	db := butlerTestDB(t)
	for i := 0; i < 3; i++ {
		if _, err := db.InsertButlerSession(ButlerSession{
			Principal: "butler", ActingFor: "op", Provider: "scripted",
			Mode: ButlerModeQAOnly, Surface: ButlerSurfaceAdmin, TranscriptJSON: `{"big":"transcript"}`,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	list, err := db.ListButlerSessions(2, "")
	if err != nil {
		t.Fatalf("ListButlerSessions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2 (limit)", len(list))
	}
	if list[0].ID != "bs-3" || list[1].ID != "bs-2" {
		t.Fatalf("order = %s,%s; want newest first bs-3,bs-2", list[0].ID, list[1].ID)
	}
	for _, s := range list {
		if s.TranscriptJSON != "" {
			t.Fatalf("list leaked transcript for %s", s.ID)
		}
	}
}

func TestButlerSessionRequiresPrincipalBinding(t *testing.T) {
	db := butlerTestDB(t)
	// ADR-W001: no anonymous session record, ever.
	if _, err := db.InsertButlerSession(ButlerSession{Principal: "butler", Surface: ButlerSurfaceAdmin}); err == nil {
		t.Fatalf("insert without acting_for succeeded")
	}
	if _, err := db.InsertButlerSession(ButlerSession{ActingFor: "root-admin", Surface: ButlerSurfaceAdmin}); err == nil {
		t.Fatalf("insert without principal succeeded")
	}
}

// ADR-W026 A1: the surface is stored, not inferred, and a session that
// does not name one is refused. Both defaults would be wrong — admin
// files a household conversation under the operator's ceiling, family
// hides an operator session from an audit of the Statefile — so there is
// no default at all.
func TestButlerSessionRequiresSurface(t *testing.T) {
	db := butlerTestDB(t)
	if _, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "op"}); err == nil {
		t.Fatalf("insert without a surface succeeded")
	}
	if _, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "op", Surface: ButlerSurface("operator"),
	}); err == nil {
		t.Fatalf("insert with an invented surface succeeded")
	}
}

// The two surfaces are separable in the audit record: an operator asking
// what the household's butler has been asked to do gets exactly that,
// and the reverse (ADR-W026 A1).
func TestButlerSessionListBySurface(t *testing.T) {
	db := butlerTestDB(t)
	for _, sfc := range []ButlerSurface{ButlerSurfaceAdmin, ButlerSurfaceFamily, ButlerSurfaceAdmin} {
		if _, err := db.InsertButlerSession(ButlerSession{
			Principal: "butler", ActingFor: "op", Provider: "scripted",
			Mode: ButlerModeFull, Surface: sfc,
		}); err != nil {
			t.Fatalf("insert %s: %v", sfc, err)
		}
	}
	family, err := db.ListButlerSessions(0, ButlerSurfaceFamily)
	if err != nil {
		t.Fatalf("ListButlerSessions(family): %v", err)
	}
	if len(family) != 1 || family[0].Surface != ButlerSurfaceFamily {
		t.Fatalf("family list = %+v, want exactly the one family session", family)
	}
	admin, err := db.ListButlerSessions(0, ButlerSurfaceAdmin)
	if err != nil {
		t.Fatalf("ListButlerSessions(admin): %v", err)
	}
	if len(admin) != 2 {
		t.Fatalf("admin list = %d sessions, want 2", len(admin))
	}
	both, err := db.ListButlerSessions(0, "")
	if err != nil {
		t.Fatalf("ListButlerSessions(all): %v", err)
	}
	if len(both) != 3 {
		t.Fatalf("unfiltered list = %d sessions, want 3", len(both))
	}
}

func TestButlerSessionNotFound(t *testing.T) {
	db := butlerTestDB(t)
	if _, err := db.GetButlerSession("bs-99"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetButlerSession("plan-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id err = %v, want ErrNotFound", err)
	}
}
