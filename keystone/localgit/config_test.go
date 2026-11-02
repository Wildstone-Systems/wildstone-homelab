package localgit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
	"gopkg.in/yaml.v3"
)

func fixture(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{ListenAddress: "127.0.0.1", Port: 22222, ClientCIDR: "127.0.0.1/32", ServiceUser: "example-source", Repository: filepath.Join(root, "deployment.git"), MirrorRepository: filepath.Join(root, "source.git"), InstallDirectory: filepath.Join(root, "source"), Namespace: "example-app", SourceName: "example-local", CredentialSecret: "example-source-key"}
}

func TestRejectsUnsafeConfiguration(t *testing.T) {
	for _, change := range []string{"wildcard", "public", "hostname", "public-clients", "broad-clients", "unaligned-clients", "root", "nobody", "user-injection", "relative", "path-injection", "path-parent", "same-path", "nested-keys", "same-repository", "mirror-key-directory", "missing-mirror", "platform-namespace", "bad-reference", "privileged-port"} {
		t.Run(change, func(t *testing.T) {
			c := fixture(t)
			switch change {
			case "wildcard":
				c.ListenAddress = "0.0.0.0"
			case "public":
				c.ListenAddress = "192.0.2.1"
			case "hostname":
				c.ListenAddress = "example.internal"
			case "public-clients":
				c.ClientCIDR = "192.0.2.0/24"
			case "broad-clients":
				c.ClientCIDR = "0.0.0.0/0"
			case "unaligned-clients":
				c.ClientCIDR = "10.42.0.1/16"
			case "root":
				c.ServiceUser = "root"
			case "nobody":
				c.ServiceUser = "nobody"
			case "user-injection":
				c.ServiceUser = "example\nPermitRootLogin yes"
			case "relative":
				c.Repository = "deployment.git"
			case "path-injection":
				c.Repository = "/example';id;'"
			case "path-parent":
				c.Repository = "/example/../deployment.git"
			case "same-path":
				c.InstallDirectory = c.Repository
			case "nested-keys":
				c.InstallDirectory = filepath.Join(c.Repository, "keys")
			case "same-repository":
				c.MirrorRepository = c.Repository
			case "mirror-key-directory":
				c.MirrorRepository = filepath.Join(c.InstallDirectory, "mirror.git")
			case "missing-mirror":
				c.MirrorRepository = ""
			case "platform-namespace":
				c.Namespace = "flux-system"
			case "bad-reference":
				c.SourceName = "example\nother"
			case "privileged-port":
				c.Port = 22
			}
			if _, err := Render(c); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestRendersIndependentReadOnlySource(t *testing.T) {
	c := fixture(t)
	files, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Fatal("unexpected artifacts")
	}
	var source struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Spec       struct {
			URL string `yaml:"url"`
			Ref struct {
				Branch string `yaml:"branch"`
			} `yaml:"ref"`
			SecretRef struct {
				Name string `yaml:"name"`
			} `yaml:"secretRef"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(files["gitrepository.yaml"], &source); err != nil {
		t.Fatal(err)
	}
	if source.APIVersion != "source.toolkit.fluxcd.io/v1" || source.Kind != "GitRepository" || source.Spec.Ref.Branch != "deploy" || source.Spec.SecretRef.Name != c.CredentialSecret || source.Spec.URL != "ssh://example-source@127.0.0.1:22222/deployment.git" {
		t.Fatal("incorrect source contract")
	}
	unit := string(files["wildstone-git-source.service"])
	if strings.Contains(strings.ToLower(unit), "keystone") || !strings.Contains(unit, "/usr/sbin/sshd -D -e") {
		t.Fatal("source must be supervised without Keystone")
	}
	cfg := string(files["sshd_config"])
	for _, required := range []string{"ForceCommand /bin/sh ", "DisableForwarding yes", "PermitTTY no", "PermitUserRC no", "PermitUserEnvironment no", "PasswordAuthentication no", "PermitRootLogin no"} {
		if !strings.Contains(cfg, required) {
			t.Fatal("missing restriction", required)
		}
	}
	if strings.Contains(cfg, "\nInclude ") || strings.Contains(cfg, "\nAcceptEnv ") {
		t.Fatal("host/client configuration inheritance")
	}
	wrapper := filepath.Join(t.TempDir(), "upload-pack")
	if err := os.WriteFile(wrapper, files["upload-pack"], 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", wrapper).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	for _, request := range []string{"", "id", "git-receive-pack '/deployment.git'", "git-upload-pack '/other.git'", "git-upload-pack '/deployment.git'; id", "git-upload-pack '/deployment.git'\nid"} {
		cmd := exec.Command("sh", wrapper)
		cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+request)
		if err := cmd.Run(); err == nil {
			t.Fatal("unexpected SSH command permitted", request)
		}
	}
	c.ListenAddress = "fd00::1"
	c.ClientCIDR = "fd00::/64"
	if files, err := Render(c); err != nil || !strings.Contains(string(files["gitrepository.yaml"]), "@[fd00::1]:22222/") {
		t.Fatal("IPv6 source URL", err)
	}
}

func TestPrivateBundleRejectsCheckoutSymlinkAndOverwrite(t *testing.T) {
	c := fixture(t)
	if _, err := statefile.OpenLocalDeployment(c.Repository); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "review")
	if err := WriteBundle(c, out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("bundle is not private")
	}
	for _, name := range []string{"sshd_config", "upload-pack", "wildstone-git-source.service", "gitrepository.yaml"} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("bundle file permissions", name)
		}
	}
	if err := WriteBundle(c, out); err == nil {
		t.Fatal("overwrote existing review bundle")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteBundle(c, filepath.Join(root, "review")); err == nil {
		t.Fatal("source checkout output accepted")
	}
	c.Repository = filepath.Join(root, "deployment.git")
	if err := WriteBundle(c, filepath.Join(t.TempDir(), "review")); err == nil {
		t.Fatal("source checkout repository accepted")
	}
	c = fixture(t)
	if _, err := statefile.OpenLocalDeployment(c.Repository); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := WriteBundle(c, filepath.Join(link, "review")); err == nil {
		t.Fatal("symlink output accepted")
	}
}

func TestUploadPackAdvertisesOnlyApprovedRef(t *testing.T) {
	c := fixture(t)
	repo, err := statefile.OpenLocalDeployment(c.Repository)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"source.yaml": []byte("example source"), "release.yaml": []byte("example release"), "kustomization.yaml": []byte("example resources")}
	candidate, err := repo.Build("", "example-prepared", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Retain("example-prepared", candidate); err != nil {
		t.Fatal(err)
	}
	// Similar names must not escape the exact served-ref policy.
	for _, ref := range []string{"refs/heads/deploy-other", "refs/tags/deploy"} {
		if out, err := exec.Command("git", "--git-dir="+c.Repository, "update-ref", ref, candidate).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	if out, err := exec.Command("git", "--git-dir="+c.Repository, "symbolic-ref", "HEAD", "refs/wildstone/promotions/example-prepared").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	rendered, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.InstallDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"init-mirror", "mirror"} {
		if err := os.WriteFile(filepath.Join(c.InstallDirectory, name), rendered[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("sh", filepath.Join(c.InstallDirectory, "init-mirror")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	syncMirror := func() error { cmd := exec.Command("sh", filepath.Join(c.InstallDirectory, "mirror")); return cmd.Run() }
	wrapper := filepath.Join(t.TempDir(), "upload-pack")
	if err := os.WriteFile(wrapper, rendered["upload-pack"], 0600); err != nil {
		t.Fatal(err)
	}
	advertise := func() string {
		cmd := exec.Command("sh", wrapper)
		cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND=git-upload-pack '/deployment.git'", "GIT_DIR=/not-the-repo", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=uploadpack.allowAnySHA1InWant", "GIT_CONFIG_VALUE_0=true")
		// An empty client request exits after advertisement without serving a pack.
		out, err := cmd.Output()
		if err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatal(err)
			}
		}
		return string(out)
	}
	if before := advertise(); strings.Contains(before, candidate) || strings.Contains(before, "refs/heads/") {
		t.Fatal("unapproved or unrelated ref advertised", before)
	}
	if err := repo.Promote("", candidate); err != nil {
		t.Fatal(err)
	}
	if err := syncMirror(); err != nil {
		t.Fatal(err)
	}
	after := advertise()
	// Capabilities include upstream Git's HEAD symref metadata. Check advertised
	// refs separately from capabilities; neither grants hidden-object access.
	var refs strings.Builder
	for _, line := range strings.Split(after, "\n") {
		refs.WriteString(strings.SplitN(line, "\x00", 2)[0])
		refs.WriteByte('\n')
	}
	if !strings.Contains(after, "refs/heads/deploy\x00") || strings.Contains(refs.String(), "deploy-other") || strings.Contains(refs.String(), "refs/tags/") || strings.Contains(refs.String(), "refs/wildstone/") {
		t.Fatal("served-ref allowlist failed", after)
	}
	files["release.yaml"] = []byte("example hidden future release")
	hidden, err := repo.Build(candidate, "example-future", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Retain("example-future", hidden); err != nil {
		t.Fatal(err)
	}
	if err := syncMirror(); err != nil {
		t.Fatal(err)
	}
	blob, err := exec.Command("git", "--git-dir="+c.Repository, "rev-parse", hidden+":release.yaml").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []string{hidden, strings.TrimSpace(string(blob))} {
		if err := exec.Command("git", "--git-dir="+c.MirrorRepository, "cat-file", "-e", object).Run(); err == nil {
			t.Fatal("private prepared object copied into serving database")
		}
	}
	if out, err := exec.Command("git", "--git-dir="+c.MirrorRepository, "remote").Output(); err != nil || len(out) != 0 {
		t.Fatal("mirror inherited a remote", err)
	}
	if _, err := os.Stat(filepath.Join(c.MirrorRepository, "objects/info/alternates")); !os.IsNotExist(err) {
		t.Fatal("mirror uses shared objects")
	}
	for _, want := range []struct {
		id      string
		allowed bool
	}{{candidate, true}, {hidden, false}} {
		payload := "want " + want.id + "\n"
		request := fmt.Sprintf("%04x%s00000009done\n", len(payload)+4, payload)
		cmd := exec.Command("sh", wrapper)
		cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND=git-upload-pack '/deployment.git'")
		cmd.Stdin = strings.NewReader(request)
		out, err := cmd.Output()
		if want.allowed {
			if err != nil || !strings.Contains(string(out), "PACK") {
				t.Fatal("approved pack retrieval failed", err)
			}
		} else if err == nil || strings.Contains(string(out), "PACK") {
			t.Fatal("hidden candidate pack retrieved", err)
		}
	}
}

func TestMirrorRefConflictNeverForcesAnOwnerEdit(t *testing.T) {
	c := fixture(t)
	repo, err := statefile.OpenLocalDeployment(c.Repository)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"source.yaml": []byte("example source"), "release.yaml": []byte("example release"), "kustomization.yaml": []byte("example resources")}
	approved, err := repo.Build("", "example-approved", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Promote("", approved); err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.InstallDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"init-mirror", "mirror"} {
		if err := os.WriteFile(filepath.Join(c.InstallDirectory, name), rendered[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string) error { return exec.Command("sh", filepath.Join(c.InstallDirectory, name)).Run() }
	if err := run("init-mirror"); err != nil {
		t.Fatal(err)
	}
	if err := run("init-mirror"); err == nil {
		t.Fatal("existing mirror was adopted")
	}
	if err := run("mirror"); err != nil {
		t.Fatal(err)
	}
	tree, err := exec.Command("git", "--git-dir="+c.MirrorRepository, "rev-parse", approved+"^{tree}").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir="+c.MirrorRepository, "-c", "user.name=example", "-c", "user.email=example@example.invalid", "commit-tree", strings.TrimSpace(string(tree)), "-p", approved)
	cmd.Stdin = strings.NewReader("Example owner mirror edit\n")
	owner, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	ownerID := strings.TrimSpace(string(owner))
	if err := exec.Command("git", "--git-dir="+c.MirrorRepository, "update-ref", "refs/heads/deploy", ownerID, approved).Run(); err != nil {
		t.Fatal(err)
	}
	if err := run("mirror"); err == nil {
		t.Fatal("conflicting mirror ref was overwritten")
	}
	got, err := exec.Command("git", "--git-dir="+c.MirrorRepository, "rev-parse", "refs/heads/deploy").Output()
	if err != nil || strings.TrimSpace(string(got)) != ownerID {
		t.Fatal("owner conflict did not preserve actual ref", err)
	}
}
