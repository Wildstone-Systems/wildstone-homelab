package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/Wildstone-Systems/wildstone-homelab/wardstone/internal/playbook"
)

var (
	playbookVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	k3sReleaseRE      = regexp.MustCompile(`^v1\.(3[0-9]|[4-9][0-9])\.[0-9]+\+k3s[0-9]+$`)
	hexDigestRE       = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type runPlaybookSpec struct {
	identity string
	schema   string
	input    any
}

func validateRunPlaybook(op *wsv1.RunPlaybook) (runPlaybookSpec, *wsv1.OpError) {
	if op == nil || !playbookVersionRE.MatchString(op.GetVersion()) {
		return runPlaybookSpec{}, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "invalid playbook-set version")
	}
	switch p := op.GetParameters().(type) {
	case *wsv1.RunPlaybook_OsUpdate:
		if op.GetIdentity() != wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE || p.OsUpdate == nil {
			return runPlaybookSpec{}, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "identity does not match os_update parameters")
		}
		return runPlaybookSpec{identity: "os-update", schema: "os-update-v1", input: map[string]any{"reboot_if_required": p.OsUpdate.GetRebootIfRequired()}}, nil
	case *wsv1.RunPlaybook_K3SPatchUpdate:
		if op.GetIdentity() != wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_K3S_PATCH_UPDATE || p.K3SPatchUpdate == nil || !k3sReleaseRE.MatchString(p.K3SPatchUpdate.GetRelease()) || !hexDigestRE.MatchString(p.K3SPatchUpdate.GetSha256Hex()) {
			return runPlaybookSpec{}, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "invalid pinned k3s patch identity, release, or digest")
		}
		return runPlaybookSpec{identity: "k3s-patch-update", schema: "k3s-patch-update-v1", input: map[string]any{"k3s_release": p.K3SPatchUpdate.GetRelease(), "k3s_sha256": p.K3SPatchUpdate.GetSha256Hex()}}, nil
	case *wsv1.RunPlaybook_WardstoneUpdate:
		if op.GetIdentity() != wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE || p.WardstoneUpdate == nil || !playbookVersionRE.MatchString(p.WardstoneUpdate.GetRelease()) || !hexDigestRE.MatchString(p.WardstoneUpdate.GetSha256Hex()) {
			return runPlaybookSpec{}, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "invalid pinned Wardstone update identity, release, or digest")
		}
		return runPlaybookSpec{identity: "wardstone-update", schema: "wardstone-update-v1", input: map[string]any{"wardstone_release": p.WardstoneUpdate.GetRelease(), "wardstone_sha256": p.WardstoneUpdate.GetSha256Hex()}}, nil
	default:
		return runPlaybookSpec{}, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "typed playbook parameters are required")
	}
}

func (a *Agent) runPlaybook(ctx context.Context, op *wsv1.RunPlaybook) (*wsv1.OpResult_RunPlaybook, *wsv1.OpError) {
	spec, oerr := validateRunPlaybook(op)
	if oerr != nil {
		return nil, oerr
	}
	if len(a.cfg.PlaybookPublicKey) != ed25519.PublicKeySize {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_VERIFICATION, "playbook verification key is not configured")
	}
	entry, err := playbook.VerifyEntry(a.cfg.PlaybookDir, a.cfg.PlaybookPublicKey, spec.identity, op.GetVersion(), spec.schema)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_VERIFICATION, "signed playbook set refused")
	}
	configPath := filepath.Join(a.cfg.PlaybookDir, "ansible.cfg")
	if info, err := os.Stat(configPath); err != nil || !info.Mode().IsRegular() {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_VERIFICATION, "signed playbook configuration refused")
	}
	before := ""
	if a.playbookVersion != nil {
		before = a.playbookVersion(ctx, op.GetIdentity())
	}
	b, err := json.Marshal(spec.input)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "typed playbook input could not be encoded")
	}
	if err := os.MkdirAll(a.cfg.DataDir, 0o700); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "create private playbook input directory")
	}
	f, err := os.CreateTemp(a.cfg.DataDir, "runplaybook-*.json")
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "create private playbook input")
	}
	inputPath := f.Name()
	defer os.Remove(inputPath)
	if err := f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "write private playbook input")
	}
	args := []string{"-c", "local", "-i", "localhost,", filepath.Join(a.cfg.PlaybookDir, entry.File), "-e", "@" + inputPath}
	env := []string{"ANSIBLE_CONFIG=" + configPath, "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "HOME=/root"}
	if err := a.playbookCommand(ctx, a.cfg.AnsiblePath, args, env); err != nil {
		if ctx.Err() != nil {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "playbook execution cancelled")
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "playbook execution failed")
	}
	_, rebootErr := os.Stat(a.cfg.RebootRequiredPath)
	after := ""
	if a.playbookVersion != nil {
		after = a.playbookVersion(ctx, op.GetIdentity())
	}
	if after == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "updated version could not be observed")
	}
	if op.GetIdentity() == wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE {
		// The fixed playbook stages BinaryPath-next. Probe it before promotion,
		// then use ADR-W003's existing A/B reconnect gate rather than replacing
		// the sole known-good generation in place.
		if err := a.promoteUpdateCandidate(op.GetWardstoneUpdate().GetRelease()); err != nil {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "Wardstone candidate promotion failed")
		}
	}
	return &wsv1.OpResult_RunPlaybook{RunPlaybook: &wsv1.RunPlaybookResult{
		Identity: op.GetIdentity(), Version: op.GetVersion(), Phase: wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE,
		RebootRequired: rebootErr == nil, BeforeVersion: before, AfterVersion: after,
	}}, nil
}

// afterRunPlaybookResultSent activates only lifecycle changes that would
// terminate the operation executor. Playbooks stage the host change and return;
// the session goroutine calls this only after Keystone has received the typed
// result. This is the same send-before-reexec boundary used by SelfUpdate.
func (a *Agent) afterRunPlaybookResultSent(op *wsv1.Operation, res *wsv1.OpResult) {
	result := res.GetRunPlaybook()
	request := op.GetRunPlaybook()
	if result == nil || request == nil || result.GetPhase() != wsv1.PlaybookPhase_PLAYBOOK_PHASE_COMPLETE || result.GetIdentity() != request.GetIdentity() {
		return
	}
	switch request.GetIdentity() {
	case wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE:
		if request.GetWardstoneUpdate() != nil {
			if _, err := os.Stat(a.updateStatePath()); err != nil {
				return
			}
			a.execSelf()
		}
	case wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE:
		if request.GetOsUpdate().GetRebootIfRequired() && result.GetRebootRequired() {
			a.rebootHost()
		}
	}
}

// defaultRebootHost hands the fixed local reboot to systemd without waiting
// for shutdown. Ansible's reboot action intentionally refuses connection=local;
// this runs only from the post-result session path above, with no request data
// in its executable or argv.
func (a *Agent) defaultRebootHost() {
	cmd := exec.Command("/usr/bin/systemctl", "--no-block", "reboot")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	sink := &boundedDiscard{remaining: 4096}
	cmd.Stdout, cmd.Stderr = sink, sink
	if err := cmd.Run(); err != nil {
		a.log.Error("post-result host reboot handoff failed", "error", err)
	}
}

func (a *Agent) probePlaybookVersion(ctx context.Context, identity wsv1.PlaybookIdentity) string {
	if identity == wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_OS_UPDATE {
		b, err := os.ReadFile("/etc/os-release")
		if err != nil || len(b) > 64<<10 {
			return ""
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
				return strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		return ""
	}
	name := "/usr/local/bin/k3s"
	if identity == wsv1.PlaybookIdentity_PLAYBOOK_IDENTITY_WARDSTONE_UPDATE {
		name = a.cfg.BinaryPath
		if _, err := os.Stat(name + "-next"); err == nil {
			name += "-next"
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, name, "--version")
	out := &boundedCapture{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, &boundedDiscard{remaining: 4096}
	if err := cmd.Run(); err != nil {
		return ""
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	return strings.TrimSpace(line)
}

func (a *Agent) runAnsible(ctx context.Context, name string, args, env []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = a.cfg.PlaybookDir
	cmd.Env = env
	// Command output is never returned or logged. Keep a small tail only so a
	// noisy child cannot grow memory without bound; even it is discarded.
	sink := &boundedDiscard{remaining: 64 << 10}
	cmd.Stdout, cmd.Stderr = sink, sink
	return cmd.Run()
}

type boundedDiscard struct{ remaining int64 }

func (w *boundedDiscard) Write(p []byte) (int, error) {
	n := len(p)
	if w.remaining > 0 {
		kept := int64(len(p))
		if kept > w.remaining {
			kept = w.remaining
		}
		w.remaining -= kept
	}
	return n, nil
}

type boundedCapture struct {
	limit int
	b     []byte
}

func (w *boundedCapture) Write(p []byte) (int, error) {
	n := len(p)
	left := w.limit - len(w.b)
	if left > len(p) {
		left = len(p)
	}
	if left > 0 {
		w.b = append(w.b, p[:left]...)
	}
	return n, nil
}

func (w *boundedCapture) String() string { return string(w.b) }
