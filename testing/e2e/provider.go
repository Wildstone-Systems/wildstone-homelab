// Package e2e boots disposable Ubuntu VMs for end-to-end tests.
//
// Two providers exist behind the Provider interface: "local" (QEMU on the
// current machine — the CI path, per ADR-W002) and "proxmox" (the platform
// owner's Proxmox host, reachable only from inside the tailnet).
package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// VM is a running disposable test machine.
type VM struct {
	Name string
	Addr string // SSH endpoint, host:port
	User string

	// providerData lets a provider stash whatever it needs for Destroy.
	providerData any
}

// Provider creates and destroys disposable VMs.
type Provider interface {
	// Create boots a fresh VM and returns once SSH is reachable.
	Create(ctx context.Context, name string) (*VM, error)
	// Destroy tears the VM down and releases its resources.
	Destroy(ctx context.Context, vm *VM) error
}

// Keypair is the per-run SSH identity injected into VMs via cloud-init.
type Keypair struct {
	Signer     ssh.Signer
	Authorized string // authorized_keys line, no trailing newline
}

// NewKeypair generates an ephemeral ed25519 SSH keypair.
func NewKeypair() (*Keypair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	auth := string(ssh.MarshalAuthorizedKey(sshPub))
	return &Keypair{Signer: signer, Authorized: auth[:len(auth)-1]}, nil
}

// Run executes a command on the VM over SSH and returns combined output.
func Run(vm *VM, signer ssh.Signer, cmd string) (string, error) {
	client, err := dialSSH(vm, signer, 15*time.Second)
	if err != nil {
		return "", err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// WaitSSH blocks until the VM accepts an authenticated SSH connection.
func WaitSSH(ctx context.Context, vm *VM, signer ssh.Signer) error {
	for {
		client, err := dialSSH(vm, signer, 5*time.Second)
		if err == nil {
			client.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ssh to %s (%s) never came up: last error: %w", vm.Name, vm.Addr, err)
		case <-time.After(3 * time.Second):
		}
	}
}

func dialSSH(vm *VM, signer ssh.Signer, timeout time.Duration) (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", vm.Addr, timeout)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User: vm.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// Disposable VMs mint fresh host keys on every boot; there is no
		// meaningful identity to pin.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, vm.Addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}
