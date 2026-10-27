package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// LocalProvider boots VMs with QEMU on the current machine — the CI path
// per ADR-W002. It needs qemu-system-x86_64 and qemu-img on PATH and a
// qcow2 Ubuntu cloud image to use as the backing file. KVM is used when
// /dev/kvm exists; otherwise it falls back to TCG (slow but functional).
type LocalProvider struct {
	ImagePath string        // base qcow2 cloud image (never written to)
	WorkDir   string        // scratch space; one subdirectory per VM
	Keys      *Keypair      // injected via cloud-init
	MemoryMB  int           // default 2048
	CPUs      int           // default 2
	DiskGB    int           // overlay size, default 10
	BootWait  time.Duration // max wait for SSH, default 10m (TCG is slow)
}

type localVM struct {
	dir string
	cmd *exec.Cmd
}

func (p *LocalProvider) Create(ctx context.Context, name string) (*VM, error) {
	if p.ImagePath == "" || p.Keys == nil {
		return nil, fmt.Errorf("LocalProvider needs ImagePath and Keys")
	}
	dir := filepath.Join(p.WorkDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	overlay := filepath.Join(dir, "disk.qcow2")
	img, err := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", "-F", "qcow2", "-b", mustAbs(p.ImagePath), overlay,
		fmt.Sprintf("%dG", orDefault(p.DiskGB, 10))).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("qemu-img: %v: %s", err, img)
	}

	seed := filepath.Join(dir, "seed.iso")
	if err := WriteSeedISO(seed, name, UserData("wildstone", p.Keys.Authorized)); err != nil {
		return nil, fmt.Errorf("seed iso: %w", err)
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}

	args := QEMUArgs(overlay, seed, port, orDefault(p.MemoryMB, 2048), orDefault(p.CPUs, 2), kvmAvailable())
	cmd := exec.Command("qemu-system-x86_64", args...)
	logf, err := os.Create(filepath.Join(dir, "qemu.log"))
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("qemu start: %w", err)
	}

	vm := &VM{
		Name:         name,
		Addr:         fmt.Sprintf("127.0.0.1:%d", port),
		User:         "wildstone",
		providerData: &localVM{dir: dir, cmd: cmd},
	}
	waitCtx, cancel := context.WithTimeout(ctx, orDefaultDur(p.BootWait, 10*time.Minute))
	defer cancel()
	if err := WaitSSH(waitCtx, vm, p.Keys.Signer); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	ok = true
	return vm, nil
}

func (p *LocalProvider) Destroy(_ context.Context, vm *VM) error {
	lv, _ := vm.providerData.(*localVM)
	if lv == nil {
		return fmt.Errorf("%s was not created by LocalProvider", vm.Name)
	}
	if lv.cmd.Process != nil {
		lv.cmd.Process.Kill()
		lv.cmd.Wait()
	}
	return os.RemoveAll(lv.dir)
}

// QEMUArgs builds the qemu-system-x86_64 argument list. Split out for
// testability.
func QEMUArgs(overlay, seedISO string, sshPort, memMB, cpus int, kvm bool) []string {
	args := []string{
		"-m", fmt.Sprintf("%d", memMB),
		"-smp", fmt.Sprintf("%d", cpus),
		"-drive", "file=" + overlay + ",format=qcow2,if=virtio",
		"-drive", "file=" + seedISO + ",format=raw,if=virtio,readonly=on",
		"-nic", fmt.Sprintf("user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:%d-:22", sshPort),
		"-display", "none",
		"-serial", "file:/dev/null",
	}
	if kvm {
		args = append(args, "-accel", "kvm", "-cpu", "host")
	} else {
		args = append(args, "-accel", "tcg", "-cpu", "max")
	}
	return args
}

func kvmAvailable() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func orDefaultDur(v, d time.Duration) time.Duration {
	if v == 0 {
		return d
	}
	return v
}
