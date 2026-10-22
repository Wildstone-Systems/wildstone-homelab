//go:build linux

package reads

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/workspace/storage"
)

func TestReadCommittedBytesUsesConfinedVerifiedStorage(t *testing.T) {
	f := newFixture(t)
	store, err := storage.New(f.settings, f.w.PersonID, strconv.FormatInt(f.w.AgentID, 10))
	must(t, err)
	obj, err := store.Stage("report.txt", strings.NewReader("synthetic committed bytes"))
	must(t, err)
	must(t, store.Publish(obj, func() error { return nil }))
	r := runtimedb.WorkspaceRevision{ID: obj.ID, ArtifactID: "artifact", TaskID: 1, Hash: obj.SHA256, SizeBytes: obj.Size}
	data, err := ReadCommittedBytes(f.settings, f.w, r)
	must(t, err)
	if string(data) != "synthetic committed bytes" {
		t.Fatal("wrong bytes")
	}
	r.Hash = strings.Repeat("0", 64)
	if _, err = ReadCommittedBytes(f.settings, f.w, r); err != storage.ErrIntegrity {
		t.Fatalf("corrupt metadata accepted: %v", err)
	}
	r.ID = strings.Repeat("0", 32)
	if _, err = ReadCommittedBytes(f.settings, f.w, r); err == nil {
		t.Fatal("missing bytes accepted")
	}
}
