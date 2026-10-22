package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// seedHarness wires an agent to a fake Docker whose volume "app-data" is
// backed by a real directory.
func seedHarness(t *testing.T) (*harness, string) {
	t.Helper()
	dir := t.TempDir()
	fake := newFakeDocker(t)
	fake.addOwnedVolumeAt("app-data", dir)
	fake.addForeignVolume("someone-elses")
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	return h, dir
}

func seedOp(path, content string) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_SeedVolumeFile{
		SeedVolumeFile: &wsv1.SeedVolumeFile{Volume: "app-data", Path: path, Content: []byte(content)},
	}}
}

// The core promise: a seed creates a file that was not there, and NEVER
// overwrites one that was. The second write must report written=false
// rather than an error — after the first tick that is the permanent
// steady state, and a reconciler treating it as failure would set the
// resource aside forever.
func TestSeedWritesOnceAndNeverOverwrites(t *testing.T) {
	h, dir := seedHarness(t)

	res := dispatch(t, h, "node-a", seedOp("configuration.yaml", "first\n"))
	if oe := res.GetError(); oe != nil {
		t.Fatalf("first seed failed: %s", oe.GetMessage())
	}
	if !res.GetSeedVolumeFile().GetWritten() {
		t.Error("first seed should report written=true")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "configuration.yaml")); string(b) != "first\n" {
		t.Errorf("content not written, got %q", b)
	}

	// The household edits its own file.
	if err := os.WriteFile(filepath.Join(dir, "configuration.yaml"), []byte("mine now\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res = dispatch(t, h, "node-a", seedOp("configuration.yaml", "first\n"))
	if oe := res.GetError(); oe != nil {
		t.Fatalf("re-seed should be a clean no-op, got error: %s", oe.GetMessage())
	}
	if res.GetSeedVolumeFile().GetWritten() {
		t.Error("re-seed must report written=false, not clobber the file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "configuration.yaml")); string(b) != "mine now\n" {
		t.Errorf("the household's edit was destroyed: %q", b)
	}
}

func TestSeedCreatesParentDirectories(t *testing.T) {
	h, dir := seedHarness(t)
	res := dispatch(t, h, "node-a", seedOp(".storage/http", `{"k":1}`))
	if oe := res.GetError(); oe != nil {
		t.Fatalf("seeding a dot-directory failed: %s", oe.GetMessage())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".storage", "http")); string(b) != `{"k":1}` {
		t.Errorf("not written under .storage: %q", b)
	}
}

// Traversal is refused, and so is an absolute path. This is the whole
// reason the op takes (volume, relative path) rather than a path.
func TestSeedRefusesEscapingTheVolume(t *testing.T) {
	h, dir := seedHarness(t)
	for _, path := range []string{
		"../escaped.yaml",
		"a/../../escaped.yaml",
		"/etc/passwd",
		"..",
	} {
		res := dispatch(t, h, "node-a", seedOp(path, "x"))
		if res.GetError() == nil {
			t.Errorf("path %q was accepted; it must be refused", path)
		}
	}
	// Nothing landed beside the volume.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.yaml")); err == nil {
		t.Error("a file escaped the volume")
	}
}

// A symlink planted inside the volume must not redirect a write onto the
// host. The name check alone cannot catch this: the path has no "..".
func TestSeedRefusesASymlinkOutOfTheVolume(t *testing.T) {
	h, dir := seedHarness(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	res := dispatch(t, h, "node-a", seedOp("escape/pwned.yaml", "x"))
	if res.GetError() == nil {
		t.Fatal("a seed followed a symlink out of the volume")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.yaml")); err == nil {
		t.Error("the write landed outside the volume")
	}
}

// Seeding writes into data Wildstone would otherwise never touch, so a
// volume it did not create is off limits.
func TestSeedRefusesAVolumeWildstoneDoesNotOwn(t *testing.T) {
	h, _ := seedHarness(t)
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SeedVolumeFile{
		SeedVolumeFile: &wsv1.SeedVolumeFile{Volume: "someone-elses", Path: "f.yaml", Content: []byte("x")},
	}})
	if res.GetError() == nil {
		t.Fatal("seeded a volume Wildstone does not own")
	}
	if got := res.GetError().GetCode(); got != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Errorf("want FORBIDDEN, got %s", got)
	}
}

// Stat reports existence and never content: after seeding the file is
// the app's, and a caller that could read it back would grow a
// comparison against the original.
func TestStatVolumeFileReportsExistenceOnly(t *testing.T) {
	h, _ := seedHarness(t)
	stat := func(path string) *wsv1.OpResult {
		return dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_StatVolumeFile{
			StatVolumeFile: &wsv1.StatVolumeFile{Volume: "app-data", Path: path},
		}})
	}
	if res := stat("configuration.yaml"); res.GetError() != nil || res.GetStatVolumeFile().GetExists() {
		t.Error("an absent file should stat as exists=false with no error")
	}
	if oe := dispatch(t, h, "node-a", seedOp("configuration.yaml", "x")).GetError(); oe != nil {
		t.Fatalf("seed failed: %s", oe.GetMessage())
	}
	if res := stat("configuration.yaml"); res.GetError() != nil || !res.GetStatVolumeFile().GetExists() {
		t.Error("a seeded file should stat as exists=true")
	}
}

// A volume that does not exist yet is the normal first-tick case: the
// Volume create is ordered ahead of the seed in the same plan.
func TestSeedOnMissingVolumeIsNotFound(t *testing.T) {
	h, _ := seedHarness(t)
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_StatVolumeFile{
		StatVolumeFile: &wsv1.StatVolumeFile{Volume: "not-created-yet", Path: "f.yaml"},
	}})
	if res.GetError() == nil || res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Errorf("want NOT_FOUND for a missing volume, got %v", res.GetError())
	}
}

// Regression: a symlink EARLIER in the path, with a later component
// missing. EvalSymlinks on the parent returns ENOENT, so a fallback that
// trusts the unresolved parent lets MkdirAll follow the symlink and
// write outside the volume — with host privileges.
func TestSeedRefusesASymlinkWithAMissingLaterComponent(t *testing.T) {
	h, dir := seedHarness(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	res := dispatch(t, h, "node-a", seedOp("escape/newdir/pwned.yaml", "x"))
	if res.GetError() == nil {
		t.Error("a seed followed a symlink out of the volume via a missing subdirectory")
	}
	if _, err := os.Stat(filepath.Join(outside, "newdir", "pwned.yaml")); err == nil {
		t.Error("the write landed outside the volume")
	}
}
