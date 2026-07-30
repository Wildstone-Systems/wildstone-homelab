package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Unit tests — always run.

func TestUserDataContainsKeyAndUser(t *testing.T) {
	kp, err := NewKeypair()
	if err != nil {
		t.Fatal(err)
	}
	ud := UserData("wildstone", kp.Authorized)
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Errorf("user-data must start with #cloud-config, got: %q", ud[:20])
	}
	if !strings.Contains(ud, kp.Authorized) {
		t.Error("user-data missing the authorized key")
	}
	if !strings.Contains(ud, "name: wildstone") {
		t.Error("user-data missing the login user")
	}
	if !strings.Contains(ud, "ssh_pwauth: false") {
		t.Error("password auth must stay disabled")
	}
}

func TestSeedISORoundTrip(t *testing.T) {
	path := t.TempDir() + "/seed.iso"
	if err := WriteSeedISO(path, "vm-test", "#cloud-config\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Error("seed ISO is empty")
	}
}

func TestQEMUArgs(t *testing.T) {
	args := strings.Join(QEMUArgs("/x/disk.qcow2", "/x/seed.iso", 2222, 2048, 2, false), " ")
	for _, want := range []string{
		"hostfwd=tcp:127.0.0.1:2222-:22",
		"file=/x/disk.qcow2,format=qcow2",
		"file=/x/seed.iso,format=raw",
		"-accel tcg",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("qemu args missing %q in: %s", want, args)
		}
	}
	if kvm := strings.Join(QEMUArgs("d", "s", 1, 1024, 1, true), " "); !strings.Contains(kvm, "-accel kvm") {
		t.Error("kvm=true must select the kvm accelerator")
	}
}

func TestFirstIPv4(t *testing.T) {
	data := []byte(`{"result":[
		{"name":"lo","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"127.0.0.1"}]},
		{"name":"eth0","ip-addresses":[
			{"ip-address-type":"ipv6","ip-address":"fe80::1"},
			{"ip-address-type":"ipv4","ip-address":"192.0.2.10"}]}]}`)
	if got := firstIPv4(data); got != "192.0.2.10" {
		t.Errorf("firstIPv4 = %q, want 192.0.2.10 (loopback must be skipped)", got)
	}
}

// Smoke test — boots a real VM. Opt-in via environment:
//
//	WILDSTONE_E2E_PROVIDER=local WILDSTONE_E2E_IMAGE=/path/to/noble-server-cloudimg-amd64.img \
//	  go test -run TestSmoke -timeout 30m ./...
func TestSmoke(t *testing.T) {
	if os.Getenv("WILDSTONE_E2E_PROVIDER") != "local" {
		t.Skip("set WILDSTONE_E2E_PROVIDER=local to run the boot smoke test")
	}
	image := os.Getenv("WILDSTONE_E2E_IMAGE")
	if image == "" {
		t.Fatal("WILDSTONE_E2E_IMAGE must point at a qcow2 Ubuntu cloud image")
	}
	kp, err := NewKeypair()
	if err != nil {
		t.Fatal(err)
	}
	p := &LocalProvider{ImagePath: image, WorkDir: t.TempDir(), Keys: kp}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	vm, err := p.Create(ctx, "smoke-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		if err := p.Destroy(context.Background(), vm); err != nil {
			t.Errorf("destroy: %v", err)
		}
	}()

	out, err := Run(vm, kp.Signer, "cloud-init status --wait && uname -a")
	if err != nil {
		t.Fatalf("command failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Linux") {
		t.Errorf("unexpected uname output: %s", out)
	}
	t.Logf("smoke output:\n%s", out)
}
