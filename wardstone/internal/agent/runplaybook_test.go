package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/Wildstone-Systems/wildstone-homelab/wardstone/internal/playbook"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func signedRunPlaybookSet(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"ansible.cfg":          "[defaults]\nlocalhost_warning=false\n",
		"os-update.yml":        "---\n- hosts: localhost\n",
		"k3s-upgrade.yml":      "---\n- hosts: localhost\n",
		"wardstone-update.yml": "---\n- hosts: localhost\n",
		playbook.CatalogFile:   playbook.CatalogHeader + "\nos-update 1 os-update-v1 os-update.yml\nk3s-patch-update 1 k3s-patch-update-v1 k3s-upgrade.yml\nwardstone-update 1 wardstone-update-v1 wardstone-update.yml\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := playbook.Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, playbook.ManifestFile), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, playbook.SignatureFile), playbook.Sign(priv, manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, pub
}

func runPlaybookAgent(t *testing.T) (*Agent, *int) {
	t.Helper()
	dir, pub := signedRunPlaybookSet(t)
	calls := 0
	a := &Agent{cfg: Config{DataDir: t.TempDir(), PlaybookDir: dir, PlaybookPublicKey: pub, AnsiblePath: "/usr/bin/ansible-playbook", RebootRequiredPath: filepath.Join(t.TempDir(), "reboot-required")}}
	probes := 0
	a.playbookVersion = func(context.Context, wsv1.PlaybookIdentity) string {
		probes++
		if probes%2 == 1 {
			return "before"
		}
		return "after"
	}
	a.playbookCommand = func(_ context.Context, name string, args, env []string) error {
		calls++
		if name != "/usr/bin/ansible-playbook" || !reflect.DeepEqual(args[:4], []string{"-c", "local", "-i", "localhost,"}) {
			t.Fatalf("command = %q %q", name, args)
		}
		if len(args) != 7 || !strings.HasPrefix(args[6], "@"+a.cfg.DataDir) {
			t.Fatalf("argv is not fixed/private: %q", args)
		}
		b, err := os.ReadFile(strings.TrimPrefix(args[6], "@"))
		if err != nil {
			t.Fatal(err)
		}
		var input map[string]any
		if err := json.Unmarshal(b, &input); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"command", "argv", "environment", "path", "yaml", "payload", "extra_vars"} {
			if _, ok := input[forbidden]; ok {
				t.Fatalf("generated input contains escape key %q", forbidden)
			}
		}
		if len(env) != 4 || !strings.HasPrefix(env[0], "ANSIBLE_CONFIG="+dir) {
			t.Fatalf("env = %q", env)
		}
		return nil
	}
	return a, &calls
}

func validK3sRun() *wsv1.RunPlaybook {
	return &wsv1.RunPlaybook{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_K3S_PATCH_UPDATE, Version: "1", Parameters: &wsv1.RunPlaybook_K3SPatchUpdate{K3SPatchUpdate: &wsv1.K3SPatchUpdateParams{Release: "v1.37.1+k3s1", Sha256Hex: strings.Repeat("a", 64)}}}
}

func validOSRun() *wsv1.RunPlaybook {
	return &wsv1.RunPlaybook{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE, Version: "1", Parameters: &wsv1.RunPlaybook_OsUpdate{OsUpdate: &wsv1.OsUpdateParams{RebootIfRequired: true}}}
}

func validWardstoneRun() *wsv1.RunPlaybook {
	return &wsv1.RunPlaybook{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE, Version: "1", Parameters: &wsv1.RunPlaybook_WardstoneUpdate{WardstoneUpdate: &wsv1.WardstoneUpdateParams{Release: "v1.2.3", Sha256Hex: strings.Repeat("b", 64)}}}
}

func TestRunPlaybookUsesOnlySignedAllowlistedFixedInvocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   *wsv1.RunPlaybook
	}{
		{"os", validOSRun()},
		{"k3s", validK3sRun()},
		{"wardstone", validWardstoneRun()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := runPlaybookAgent(t)
			r, oerr := a.runPlaybook(t.Context(), tc.op)
			if oerr != nil {
				t.Fatalf("runPlaybook: %v", oerr)
			}
			if *calls != 1 || r.RunPlaybook.GetPhase() != wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE || r.RunPlaybook.GetBeforeVersion() != "before" || r.RunPlaybook.GetAfterVersion() != "after" {
				t.Fatalf("result=%+v calls=%d", r.RunPlaybook, *calls)
			}
		})
	}
}

func TestRunPlaybookRefusalsHaveZeroExecutionSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Agent, *wsv1.RunPlaybook)
	}{
		{"unknown identity", func(_ *Agent, op *wsv1.RunPlaybook) { op.Identity = wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE }},
		{"wrong set version", func(_ *Agent, op *wsv1.RunPlaybook) { op.Version = "2" }},
		{"unsupported release", func(_ *Agent, op *wsv1.RunPlaybook) { op.GetK3SPatchUpdate().Release = "latest" }},
		{"missing digest", func(_ *Agent, op *wsv1.RunPlaybook) { op.GetK3SPatchUpdate().Sha256Hex = "" }},
		{"unsupported old release", func(_ *Agent, op *wsv1.RunPlaybook) { op.GetK3SPatchUpdate().Release = "v1.29.9+k3s1" }},
		{"noncanonical digest", func(_ *Agent, op *wsv1.RunPlaybook) { op.GetK3SPatchUpdate().Sha256Hex = strings.Repeat("A", 64) }},
		{"missing verification key", func(a *Agent, _ *wsv1.RunPlaybook) { a.cfg.PlaybookPublicKey = nil }},
		{"unsigned", func(a *Agent, _ *wsv1.RunPlaybook) { a.cfg.PlaybookPublicKey = make([]byte, ed25519.PublicKeySize) }},
		{"tampered", func(a *Agent, _ *wsv1.RunPlaybook) {
			_ = os.WriteFile(filepath.Join(a.cfg.PlaybookDir, "k3s-upgrade.yml"), []byte("changed"), 0o644)
		}},
		{"missing fixed config", func(a *Agent, _ *wsv1.RunPlaybook) {
			_ = os.Remove(filepath.Join(a.cfg.PlaybookDir, "ansible.cfg"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := runPlaybookAgent(t)
			op := validK3sRun()
			tc.mutate(a, op)
			if _, oerr := a.runPlaybook(t.Context(), op); oerr == nil {
				t.Fatal("refusal unexpectedly succeeded")
			}
			if *calls != 0 {
				t.Fatalf("runner called %d times", *calls)
			}
			entries, err := os.ReadDir(a.cfg.DataDir)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("refusal left generated inputs: %v", entries)
			}
		})
	}
}

func TestRunPlaybookRemovesPrivateInputAfterRunnerFailure(t *testing.T) {
	a, calls := runPlaybookAgent(t)
	a.playbookCommand = func(context.Context, string, []string, []string) error {
		*calls++
		return errors.New("synthetic runner failure")
	}
	if _, oerr := a.runPlaybook(t.Context(), validWardstoneRun()); oerr == nil {
		t.Fatal("runner failure unexpectedly succeeded")
	}
	entries, err := os.ReadDir(a.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("runner failure left generated inputs: %v", entries)
	}
}

func TestRunPlaybookCancellationIsTypedAndRedacted(t *testing.T) {
	a, _ := runPlaybookAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.playbookCommand = func(ctx context.Context, _ string, _, _ []string) error {
		if ctx.Err() == nil {
			return errors.New("secret stdout value")
		}
		return ctx.Err()
	}
	_, oerr := a.runPlaybook(ctx, validK3sRun())
	if oerr.GetCode() != wsv1.ErrorCode_ERROR_CODE_RUNTIME || strings.Contains(oerr.GetMessage(), "secret") {
		t.Fatalf("error was not typed/redacted: %+v", oerr)
	}
}

func TestRunPlaybookWireSchemaIsClosedAndRoundTrips(t *testing.T) {
	in := &wsv1.Operation{Op: &wsv1.Operation_RunPlaybook{RunPlaybook: validK3sRun()}}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out wsv1.Operation
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, &out) {
		t.Fatalf("round trip changed message")
	}
	fields := (&wsv1.RunPlaybook{}).ProtoReflect().Descriptor().Fields()
	for _, forbidden := range []string{"command", "argv", "environment", "path", "yaml", "payload", "extra_vars"} {
		if fields.ByName(protoreflect.Name(forbidden)) != nil {
			t.Fatalf("raw escape field %q exists", forbidden)
		}
	}
}

func TestRunPlaybookGatewayEndToEnd(t *testing.T) {
	dir, pub := signedRunPlaybookSet(t)
	h := startGateway(t, []string{"runplaybook-token"}, "")
	a, _ := newTestAgent(t, h, "node-runplaybook", "runplaybook-token", func(c *Config) {
		c.PlaybookDir = dir
		c.PlaybookPublicKey = pub
		c.AnsiblePath = "/usr/bin/ansible-playbook"
	})
	a.playbookVersion = func(context.Context, wsv1.PlaybookIdentity) string { return "observed" }
	a.playbookCommand = func(context.Context, string, []string, []string) error { return nil }
	if !h.gw.WaitConnected("node-runplaybook", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-runplaybook", &wsv1.Operation{Op: &wsv1.Operation_RunPlaybook{RunPlaybook: validK3sRun()}})
	if res.GetRunPlaybook().GetAfterVersion() != "observed" || res.GetError() != nil {
		t.Fatalf("dispatch result = %+v", res)
	}
}

func TestRunPlaybookProductionPathsMustBeAbsolute(t *testing.T) {
	base := Config{NodeID: "node-a", KeystoneAddr: "127.0.0.1:1", DataDir: t.TempDir(), BinaryPath: filepath.Join(t.TempDir(), "wardstoned")}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.PlaybookDir = "relative/playbooks" },
		func(c *Config) { c.AnsiblePath = "ansible-playbook" },
		func(c *Config) { c.RebootRequiredPath = "reboot-required" },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatal("relative execution path was accepted")
		}
	}
}
