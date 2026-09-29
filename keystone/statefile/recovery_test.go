package statefile

import (
	"path/filepath"
	"testing"
)

func TestBundleRestorePreservesHistoryAndResources(t *testing.T) {
	root := filepath.Join(t.TempDir(), "statefile")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(volume("photos")); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit("owner", "recovery-test", "add photos"); err != nil {
		t.Fatal(err)
	}
	wantHead, err := store.Head()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "statefile.bundle")
	if err := store.Bundle(bundle); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBundle(bundle, filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	gotHead, err := restored.Head()
	if err != nil || gotHead != wantHead {
		t.Fatalf("restored HEAD = %q, %v; want %q", gotHead, err, wantHead)
	}
	refs, err := restored.List()
	if err != nil || len(refs) != 1 || refs[0].Name != "photos" {
		t.Fatalf("restored refs = %+v, %v", refs, err)
	}
}

func TestBundleRefusesRepositoryDestinationAndOverwrite(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bundle(filepath.Join(store.Root, "unsafe.bundle")); err == nil {
		t.Fatal("bundle inside Statefile repository was accepted")
	}
}
