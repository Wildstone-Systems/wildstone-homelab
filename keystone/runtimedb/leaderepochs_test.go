package runtimedb

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestLeaderEpochsOnlyGrow(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, ok, err := db.CurrentLeaderEpoch(); err != nil || ok {
		t.Fatalf("fresh database: ok=%v err=%v, want no epoch", ok, err)
	}
	if _, err := db.IssueLeaderEpoch("", 0); err == nil {
		t.Fatal("an empty leader was accepted")
	}
	for want := uint64(1); want <= 3; want++ {
		got, err := db.IssueLeaderEpoch("system:serviceaccount:wildstone-system:wardstone-node-a", 0)
		if err != nil || got != want {
			t.Fatalf("epoch = %d, %v; want %d", got, err, want)
		}
	}
	// A cluster ahead of this database (a restore from an older backup)
	// moves the next epoch past it.
	if got, err := db.IssueLeaderEpoch("system:serviceaccount:wildstone-system:wardstone-node-b", 10); err != nil || got != 11 {
		t.Fatalf("epoch above floor 10 = %d, %v; want 11", got, err)
	}
	if got, err := db.IssueLeaderEpoch("system:serviceaccount:wildstone-system:wardstone-node-a", 2); err != nil || got != 12 {
		t.Fatalf("a floor below the newest epoch = %d, %v; want 12", got, err)
	}
	cur, ok, err := db.CurrentLeaderEpoch()
	if err != nil || !ok || cur.Epoch != 12 || cur.Leader == "" || cur.IssuedAt.IsZero() {
		t.Fatalf("current = %+v ok=%v err=%v", cur, ok, err)
	}
}

func TestLeaderEpochsAreNeverIssuedTwice(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 20
	var wg sync.WaitGroup
	got := make(chan uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := db.IssueLeaderEpoch("system:serviceaccount:wildstone-system:wardstone-node-a", 0)
			if err != nil {
				t.Error(err)
				return
			}
			got <- e
		}()
	}
	wg.Wait()
	close(got)
	seen := map[uint64]bool{}
	for e := range got {
		if seen[e] {
			t.Fatalf("epoch %d issued twice", e)
		}
		seen[e] = true
	}
	if len(seen) != n {
		t.Fatalf("issued %d distinct epochs, want %d", len(seen), n)
	}
}
