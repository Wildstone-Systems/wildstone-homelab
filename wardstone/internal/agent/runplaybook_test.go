package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
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
	binPath := filepath.Join(t.TempDir(), "wardstoned")
	if err := os.WriteFile(binPath, []byte("working-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{DataDir: t.TempDir(), BinaryPath: binPath, PlaybookDir: dir, PlaybookPublicKey: pub, AnsiblePath: "/usr/bin/ansible-playbook", RebootRequiredPath: filepath.Join(t.TempDir(), "reboot-required")}}
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
		if strings.HasSuffix(args[4], "wardstone-update.yml") {
			if err := os.WriteFile(a.cfg.BinaryPath+"-next", []byte("candidate-binary"), 0o755); err != nil {
				t.Fatal(err)
			}
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

func TestRunPlaybookWardstoneUpdateUsesABReconnectGate(t *testing.T) {
	a, _ := runPlaybookAgent(t)
	oldBinary, err := os.ReadFile(a.cfg.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	res, oerr := a.runPlaybook(t.Context(), validWardstoneRun())
	if oerr != nil || res.RunPlaybook.GetPhase() != wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE {
		t.Fatalf("runPlaybook = %+v, %v", res, oerr)
	}
	if got, err := os.ReadFile(a.cfg.BinaryPath + "-prev"); err != nil || !reflect.DeepEqual(got, oldBinary) {
		t.Fatalf("rollback generation = %q, %v", got, err)
	}
	if got, err := os.ReadFile(a.cfg.BinaryPath); err != nil || string(got) != "candidate-binary" {
		t.Fatalf("promoted candidate = %q, %v", got, err)
	}
	st, ok, err := readUpdateState(a.cfg.DataDir)
	if err != nil || !ok || st.Pending != "v1.2.3" || !st.Deadline.After(time.Now()) {
		t.Fatalf("update state = %+v, ok=%v, err=%v", st, ok, err)
	}
	st.Deadline = time.Now().Add(-time.Minute)
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.updateStatePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	reexecuted := false
	if err := CheckUpdateState(Config{BinaryPath: a.cfg.BinaryPath, DataDir: a.cfg.DataDir, ExecSelf: func() { reexecuted = true }}, nil); err != nil {
		t.Fatal(err)
	}
	if !reexecuted {
		t.Fatal("expired candidate did not re-exec the restored generation")
	}
	if got, err := os.ReadFile(a.cfg.BinaryPath); err != nil || !reflect.DeepEqual(got, oldBinary) {
		t.Fatalf("expired candidate did not restore working binary: %q, %v", got, err)
	}
}

func TestRunPlaybookWardstoneUpdateRefusesMissingCandidateWithoutReplacingCurrent(t *testing.T) {
	a, calls := runPlaybookAgent(t)
	a.playbookCommand = func(context.Context, string, []string, []string) error {
		*calls++
		return nil
	}
	a.playbookVersion = func(context.Context, wsv1.PlaybookIdentity) string { return "observed" }
	want, err := os.ReadFile(a.cfg.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, oerr := a.runPlaybook(t.Context(), validWardstoneRun()); oerr == nil {
		t.Fatal("missing staged candidate unexpectedly succeeded")
	}
	if got, err := os.ReadFile(a.cfg.BinaryPath); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("working binary changed = %q, %v", got, err)
	}
	if _, err := os.Stat(a.updateStatePath()); !os.IsNotExist(err) {
		t.Fatalf("rollback gate armed without a candidate: %v", err)
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

func TestRunPlaybookDisruptiveActionsFollowSuccessfulResultSend(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   *wsv1.RunPlaybook
		mark bool
	}{
		{name: "os reboot", op: validOSRun(), mark: true},
		{name: "wardstone reexec", op: validWardstoneRun()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, pub := signedRunPlaybookSet(t)
			h := startGateway(t, []string{"runplaybook-lifecycle-token"}, "")
			a, _ := newTestAgent(t, h, "node-runplaybook-lifecycle", "runplaybook-lifecycle-token", func(c *Config) {
				c.PlaybookDir = dir
				c.PlaybookPublicKey = pub
				c.AnsiblePath = "/usr/bin/ansible-playbook"
				c.RebootRequiredPath = filepath.Join(t.TempDir(), "reboot-required")
			})
			a.playbookVersion = func(context.Context, wsv1.PlaybookIdentity) string { return "observed" }
			a.playbookCommand = func(context.Context, string, []string, []string) error {
				if tc.op.GetIdentity() == wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE {
					if err := os.WriteFile(a.cfg.BinaryPath+"-next", []byte("candidate-binary"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			if tc.mark {
				if err := os.WriteFile(a.cfg.RebootRequiredPath, []byte("required"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			activated := make(chan string, 1)
			a.rebootHost = func() { activated <- "reboot" }
			a.execSelf = func() { activated <- "reexec" }
			if !h.gw.WaitConnected("node-runplaybook-lifecycle", 10*time.Second) {
				t.Fatal("agent never connected")
			}
			res := dispatch(t, h, "node-runplaybook-lifecycle", &wsv1.Operation{Op: &wsv1.Operation_RunPlaybook{RunPlaybook: tc.op}})
			if res.GetRunPlaybook().GetPhase() != wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE {
				t.Fatalf("dispatch result = %+v", res)
			}
			want := "reexec"
			if tc.mark {
				want = "reboot"
			}
			select {
			case got := <-activated:
				if got != want {
					t.Fatalf("activation = %q, want %q", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s was not activated after result delivery", want)
			}
			entries, err := os.ReadDir(a.cfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "runplaybook-") {
					t.Fatalf("private input remained at activation: %s", entry.Name())
				}
			}
		})
	}
}

func TestWardstoneRunPlaybookSerializesWithSelfUpdateThroughActivation(t *testing.T) {
	dir, playbookPub := signedRunPlaybookSet(t)
	updatePub, updatePriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := startGateway(t, []string{"runplaybook-update-lock-token"}, "")
	a, _ := newTestAgent(t, h, "node-runplaybook-update-lock", "runplaybook-update-lock-token", func(c *Config) {
		c.PlaybookDir = dir
		c.PlaybookPublicKey = playbookPub
		c.UpdatePublicKey = updatePub
		c.AnsiblePath = "/usr/bin/ansible-playbook"
		c.ExecSelf = func() {}
	})
	a.playbookVersion = func(context.Context, wsv1.PlaybookIdentity) string { return "observed" }
	playbookStarted := make(chan struct{})
	releasePlaybook := make(chan struct{})
	a.playbookCommand = func(context.Context, string, []string, []string) error {
		if err := os.WriteFile(a.cfg.BinaryPath+"-next", []byte("playbook-candidate"), 0o755); err != nil {
			t.Fatal(err)
		}
		close(playbookStarted)
		<-releasePlaybook
		return nil
	}
	if !h.gw.WaitConnected("node-runplaybook-update-lock", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	type dispatchResult struct {
		res *wsv1.OpResult
		err error
	}
	playbookDone := make(chan dispatchResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := h.gw.Dispatch(ctx, "node-runplaybook-update-lock", &wsv1.Operation{Op: &wsv1.Operation_RunPlaybook{RunPlaybook: validWardstoneRun()}})
		playbookDone <- dispatchResult{res: res, err: err}
	}()
	<-playbookStarted

	selfBinary := []byte("self-update-candidate")
	selfSum := sha256.Sum256(selfBinary)
	selfDone := make(chan dispatchResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := h.gw.Dispatch(ctx, "node-runplaybook-update-lock", &wsv1.Operation{Op: &wsv1.Operation_SelfUpdate{SelfUpdate: &wsv1.SelfUpdate{
			Binary: selfBinary, Signature: ed25519.Sign(updatePriv, selfSum[:]), Version: "v2", Sha256Hex: hex.EncodeToString(selfSum[:]),
		}}})
		selfDone <- dispatchResult{res: res, err: err}
	}()

	select {
	case got := <-selfDone:
		t.Fatalf("SelfUpdate bypassed active RunPlaybook update: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	close(releasePlaybook)
	if got := <-playbookDone; got.err != nil || got.res.GetRunPlaybook() == nil {
		t.Fatalf("RunPlaybook result = %+v, %v", got.res, got.err)
	}
	if got := <-selfDone; got.err != nil || !got.res.GetSelfUpdate().GetPromoted() {
		t.Fatalf("SelfUpdate result = %+v, %v", got.res, got.err)
	}
	if got, err := os.ReadFile(a.cfg.BinaryPath + "-prev"); err != nil || string(got) != "playbook-candidate" {
		t.Fatalf("serialized rollback generation = %q, %v", got, err)
	}
	if got, err := os.ReadFile(a.cfg.BinaryPath); err != nil || string(got) != string(selfBinary) {
		t.Fatalf("serialized current generation = %q, %v", got, err)
	}
}

func TestRunPlaybookLifecycleActionsRequireSuccessfulCompleteResult(t *testing.T) {
	a, _ := runPlaybookAgent(t)
	activated := 0
	a.rebootHost = func() { activated++ }
	a.execSelf = func() { activated++ }
	request := validOSRun()
	for _, tc := range []struct {
		op  *wsv1.RunPlaybook
		res *wsv1.OpResult
	}{
		{op: request, res: &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME}}}},
		{op: request, res: &wsv1.OpResult{Result: &wsv1.OpResult_RunPlaybook{RunPlaybook: &wsv1.RunPlaybookResult{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE, Phase: wsv1.PlaybookPhase_PLAYBOOK_PHASE_EXECUTING, RebootRequired: true}}}},
		{op: request, res: &wsv1.OpResult{Result: &wsv1.OpResult_RunPlaybook{RunPlaybook: &wsv1.RunPlaybookResult{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE, Phase: wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE}}}},
		{op: &wsv1.RunPlaybook{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE, Parameters: &wsv1.RunPlaybook_OsUpdate{OsUpdate: &wsv1.OsUpdateParams{}}}, res: &wsv1.OpResult{Result: &wsv1.OpResult_RunPlaybook{RunPlaybook: &wsv1.RunPlaybookResult{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE, Phase: wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE, RebootRequired: true}}}},
		{op: request, res: &wsv1.OpResult{Result: &wsv1.OpResult_RunPlaybook{RunPlaybook: &wsv1.RunPlaybookResult{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE, Phase: wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE, RebootRequired: true}}}},
	} {
		a.afterRunPlaybookResultSent(&wsv1.Operation{Op: &wsv1.Operation_RunPlaybook{RunPlaybook: tc.op}}, tc.res)
	}
	wardstoneRequest := validWardstoneRun()
	wardstoneComplete := &wsv1.OpResult{Result: &wsv1.OpResult_RunPlaybook{RunPlaybook: &wsv1.RunPlaybookResult{Identity: wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE, Phase: wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE}}}
	a.afterRunPlaybookResultSent(&wsv1.Operation{Op: &wsv1.Operation_RunPlaybook{RunPlaybook: wardstoneRequest}}, wardstoneComplete)
	if activated != 0 {
		t.Fatalf("failed or incomplete result activated %d lifecycle actions", activated)
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
