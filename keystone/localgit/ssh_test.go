package localgit

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// This is a real temporary OpenSSH server and standard Git client, with keys
// generated at runtime. No Keystone process, host account, installed SSH config
// or service manager is involved. Bootstrap identity/filesystem deployment and
// actual source-controller reachability remain #258's live proof.
func TestOpenSSHReadOnlyRetrieval(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("OpenSSH server prerequisite unavailable")
	}
	for _, name := range []string{"ssh", "ssh-keygen", "git"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip("OpenSSH/Git prerequisite unavailable")
		}
	}
	if os.Geteuid() == 0 {
		// Root inside an unprivileged hosted sandbox cannot run OpenSSH's mandatory
		// privilege separation. Detect this prerequisite rather than disabling it.
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			t.Skip("cannot verify root OpenSSH privilege-separation prerequisites")
		}
		capable := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "CapEff:") {
				v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
				required := uint64(1<<18 | 1<<7 | 1<<6)
				capable = err == nil && v&required == required
			}
		}
		if !capable {
			t.Skip("root OpenSSH privilege separation requires chroot/setuid/setgid capabilities")
		}
		if _, err := os.Stat("/run/sshd"); err != nil {
			t.Skip("root OpenSSH needs the package-provided /run/sshd runtime directory")
		}
	}
	c := fixture(t)
	// StrictModes checks authorized-key ancestors. A world-writable /tmp is
	// unsuitable; use a private disposable directory below this test identity's
	// home, without changing or reading any existing SSH configuration.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	secure, err := os.MkdirTemp(home, "wildstone-localgit-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(secure) })
	c.InstallDirectory = filepath.Join(secure, "source")
	if err := os.Mkdir(c.InstallDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.Port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	rendered, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	keygen := func(name string) string {
		t.Helper()
		path := filepath.Join(c.InstallDirectory, name)
		cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "REPLACED-test-fixture", "-f", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal("generate ephemeral test key", err, string(out))
		}
		return path
	}
	hostKey := keygen("ssh_host_ed25519_key")
	identity := keygen("client")
	untrusted := keygen("untrusted")
	public, err := os.ReadFile(identity + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.InstallDirectory, "authorized_keys"), public, 0600); err != nil {
		t.Fatal(err)
	}
	hostPublic, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(c.InstallDirectory, "known_hosts")
	if err := os.WriteFile(known, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", c.Port, hostPublic)), 0600); err != nil {
		t.Fatal(err)
	}
	// Production forbids root. A hosted test may already run as root; substitute
	// only this fixture's existing identity, without creating or altering users.
	cfg := strings.ReplaceAll(string(rendered["sshd_config"]), "AllowUsers example-source@", "AllowUsers "+u.Username+"@")
	if os.Geteuid() == 0 {
		cfg = strings.ReplaceAll(cfg, "PermitRootLogin no", "PermitRootLogin prohibit-password")
	} else {
		cfg = strings.ReplaceAll(cfg, "UsePAM yes", "UsePAM no")
	}
	configPath := filepath.Join(c.InstallDirectory, "sshd_config")
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"init-mirror", "mirror"} {
		if err := os.WriteFile(filepath.Join(c.InstallDirectory, name), rendered[name], 0700); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("sh", filepath.Join(c.InstallDirectory, "init-mirror")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	syncMirror := func() {
		t.Helper()
		if out, err := exec.Command("sh", filepath.Join(c.InstallDirectory, "mirror")).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	wrapper := filepath.Join(c.InstallDirectory, "upload-pack")
	if err := os.WriteFile(wrapper, rendered["upload-pack"], 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(c.InstallDirectory, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(sshd, "-D", "-e", "-f", configPath)
	server.Stdout = logFile
	server.Stderr = logFile
	if err := server.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }()
	t.Cleanup(func() {
		server.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("temporary SSH server did not exit")
		}
		logFile.Close()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.Port)), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("temporary SSH listener unavailable")
		}
		time.Sleep(20 * time.Millisecond)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	sshArgs := []string{"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=3", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UserKnownHostsFile=" + known, "-i", identity}
	sshCommand := []string{"ssh"}
	for _, arg := range sshArgs {
		sshCommand = append(sshCommand, quote(arg))
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + strings.Join(sshCommand, " "), "GIT_SSH_VARIANT=ssh"}
	url := fmt.Sprintf("ssh://%s@127.0.0.1:%d/deployment.git", u.Username, c.Port)
	git := func(args ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	repo, err := statefile.OpenLocalDeployment(c.Repository)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"source.yaml": []byte("example source"), "release.yaml": []byte("example approved release"), "kustomization.yaml": []byte("example resources")}
	approved, err := repo.Build("", "example-approved", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Retain("example-approved", approved); err != nil {
		t.Fatal(err)
	}
	if out, err := git("ls-remote", url); err != nil || bytes.Contains(out, []byte(approved)) {
		logs, _ := os.ReadFile(logPath)
		t.Fatal("prepared source retrieval", err, string(out), string(logs))
	}
	if err := repo.Promote("", approved); err != nil {
		t.Fatal(err)
	}
	syncMirror()
	clone := filepath.Join(t.TempDir(), "checkout")
	if out, err := git("clone", "--branch", "deploy", "--single-branch", url, clone); err != nil {
		logs, _ := os.ReadFile(logPath)
		t.Fatal("read approved source", err, string(out), string(logs))
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(clone, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("retrieved bytes differ", name, err)
		}
	}
	files["release.yaml"] = []byte("example unapproved release")
	hidden, err := repo.Build(approved, "example-hidden", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Retain("example-hidden", hidden); err != nil {
		t.Fatal(err)
	}
	if out, err := git("-C", clone, "fetch", "origin", hidden); err == nil {
		t.Fatal("hidden candidate fetched by object ID", string(out))
	}
	for _, request := range []string{"id", "git-receive-pack '/deployment.git'", "git-upload-pack '/other.git'", ""} {
		args := append(append([]string{}, sshArgs...), "-p", strconv.Itoa(c.Port), u.Username+"@127.0.0.1")
		if request != "" {
			args = append(args, request)
		}
		if out, err := exec.Command("ssh", args...).CombinedOutput(); err == nil {
			t.Fatal("disallowed SSH request succeeded", request, string(out))
		}
	}
	if out, err := git("-C", clone, "push", "origin", "HEAD:refs/heads/example-write-attempt"); err == nil {
		t.Fatal("Git push succeeded", string(out))
	}
	// Source-address policy and forwarding restrictions are enforced by OpenSSH.
	for _, extra := range [][]string{{"-b", "127.0.0.2"}, {"-N", "-o", "ExitOnForwardFailure=yes", "-R", "127.0.0.1:0:127.0.0.1:9"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		args := append(append([]string{}, sshArgs...), extra...)
		args = append(args, "-p", strconv.Itoa(c.Port), u.Username+"@127.0.0.1")
		if extra[0] == "-b" {
			args = append(args, "git-upload-pack '/deployment.git'")
		}
		out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		if err == nil || timedOut {
			t.Fatal("OpenSSH restriction did not refuse request", extra, err, string(out))
		}
	}
	// Wrong client keys and wrong host keys must both fail closed.
	badArgs := append([]string{}, sshArgs...)
	badArgs[len(badArgs)-1] = untrusted
	badArgs = append(badArgs, "-p", strconv.Itoa(c.Port), u.Username+"@127.0.0.1", "git-upload-pack '/deployment.git'")
	if err := exec.Command("ssh", badArgs...).Run(); err == nil {
		t.Fatal("untrusted client key accepted")
	}
	if err := os.WriteFile(known, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := git("ls-remote", url); err == nil {
		t.Fatal("unpinned server host key accepted")
	}
	head, err := repo.Head()
	if err != nil || head != approved {
		t.Fatal("read-only session changed served ref", err)
	}
}
