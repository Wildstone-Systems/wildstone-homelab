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
		got.Mode != ButlerModeFull || got.FinalText != "all healthy" || !got.Capped {
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
			Mode: ButlerModeQAOnly, TranscriptJSON: `{"big":"transcript"}`,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	list, err := db.ListButlerSessions(2)
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
	if _, err := db.InsertButlerSession(ButlerSession{Principal: "butler"}); err == nil {
		t.Fatalf("insert without acting_for succeeded")
	}
	if _, err := db.InsertButlerSession(ButlerSession{ActingFor: "root-admin"}); err == nil {
		t.Fatalf("insert without principal succeeded")
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
