package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestEngine is deliberately opt-in. It installs k3s and Longhorn in a
// disposable QEMU guest, so it belongs in the manual e2e workflow rather
// than the normal pull-request test matrix. The separate owner VM test covers
// live Wardstone leadership until that installer path is added here.
func TestEngine(t *testing.T) {
	if os.Getenv("WILDSTONE_E2E_ENGINE") != "local" {
		t.Skip("set WILDSTONE_E2E_ENGINE=local to run the engine test")
	}
	image := os.Getenv("WILDSTONE_E2E_IMAGE")
	binary := os.Getenv("WILDSTONE_E2E_PLAYBOOK_BIN")
	if image == "" || binary == "" {
		t.Fatal("WILDSTONE_E2E_IMAGE and WILDSTONE_E2E_PLAYBOOK_BIN are required")
	}
	kp, err := NewKeypair()
	if err != nil {
		t.Fatal(err)
	}
	p := &LocalProvider{ImagePath: image, WorkDir: t.TempDir(), Keys: kp, MemoryMB: 5120, CPUs: 2, DiskGB: 32}
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()
	vm, err := p.Create(ctx, "engine-1")
	if err != nil {
		t.Fatalf("create QEMU guest: %v", err)
	}
	defer func() {
		if err := p.Destroy(context.Background(), vm); err != nil {
			t.Errorf("destroy QEMU guest: %v", err)
		}
	}()
	if out, err := Run(vm, kp.Signer, "cloud-init status --wait"); err != nil {
		t.Fatalf("cloud-init: %v: %s", err, out)
	}

	archive := filepath.Join(t.TempDir(), "engine-source.tar")
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}
	cmd := exec.Command("git", "archive", "--format=tar", "--output="+archive, "HEAD",
		"deploy/ansible", "scripts/engine-try.sh", "scripts/engine-storage-check.sh",
		"scripts/engine-fence-check.sh", "testing/e2e/engine-guest.sh")
	cmd.Dir = strings.TrimSpace(string(root))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("archive exact checkout: %v: %s", err, out)
	}
	if err := upload(vm, kp.Signer, archive, "mkdir -p /tmp/wildstone-src && tar -xf - -C /tmp/wildstone-src"); err != nil {
		t.Fatal(err)
	}
	if err := upload(vm, kp.Signer, binary, "cat > /tmp/wild-playbooks && chmod 0755 /tmp/wild-playbooks"); err != nil {
		t.Fatal(err)
	}
	out, err := Run(vm, kp.Signer, "timeout 75m bash /tmp/wildstone-src/testing/e2e/engine-guest.sh")
	t.Logf("engine guest output:\n%s", out)
	if err != nil {
		t.Fatalf("engine guest: %v", err)
	}
	if !strings.Contains(out, "QEMU engine bootstrap, repeatability, storage, fence, and tamper checks passed") {
		t.Fatal("engine guest did not report completion")
	}
}

func upload(vm *VM, signer ssh.Signer, localPath, remoteCommand string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	client, err := dialSSH(vm, signer, 15*time.Second)
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.Stdin = f
	if out, err := session.CombinedOutput(remoteCommand); err != nil {
		return fmt.Errorf("upload %s: %w: %s", filepath.Base(localPath), err, out)
	}
	return nil
}
