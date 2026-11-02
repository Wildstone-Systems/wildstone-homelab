package statefile

// Local deployment Git plumbing deliberately uses a separate bare repository:
// serving a Statefile repository would expose unrelated household history.
// These operations are not authorization; publish's approved dispatcher must
// bind review material before calling them.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const DeploymentRef = "refs/heads/deploy"
const deploymentMarker = "wildstone-local-deployment-v1"

type LocalDeployment struct{ root string }

var deploymentSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var deploymentFiles = map[string]bool{"source.yaml": true, "release.yaml": true, "kustomization.yaml": true}

// OpenLocalDeployment creates a private dedicated bare repository, or reopens
// one previously created here. It refuses source checkouts and foreign repos.
// It does not install Git or start any network listener.
func OpenLocalDeployment(root string) (*LocalDeployment, error) {
	d, err := localDeploymentPath(root, true)
	if err != nil {
		return nil, err
	}
	root = d.root
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		if _, err := d.git(nil, "init", "--bare", "--object-format=sha1", root); err != nil {
			return nil, err
		}
		for _, kv := range [][2]string{{"transfer.hideRefs", "refs/wildstone/"}, {"uploadpack.allowTipSHA1InWant", "false"}, {"uploadpack.allowReachableSHA1InWant", "false"}, {"uploadpack.allowAnySHA1InWant", "false"}} {
			if _, err := d.git(nil, "config", kv[0], kv[1]); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(filepath.Join(root, deploymentMarker), []byte("1\n"), 0600); err != nil {
			return nil, err
		}
	}
	if err := d.check(); err != nil {
		return nil, err
	}
	return d, nil
}

// InspectLocalDeployment validates an already initialized private deployment
// repository without creating directories, objects, refs, remotes or a listener.
func InspectLocalDeployment(root string) (*LocalDeployment, error) {
	d, err := localDeploymentPath(root, false)
	if err != nil {
		return nil, err
	}
	if err := d.check(); err != nil {
		return nil, err
	}
	return d, nil
}

func localDeploymentPath(root string, create bool) (*LocalDeployment, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("deployment: repository path must be absolute")
	}
	root = filepath.Clean(root)
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return nil, fmt.Errorf("deployment: repository cannot be inside a source checkout")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	if create {
		if err := os.MkdirAll(root, 0700); err != nil {
			return nil, err
		}
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root {
		return nil, fmt.Errorf("deployment: symlinked repository paths refused")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("deployment: repository must be private (0700)")
	}
	return &LocalDeployment{root: root}, nil
}

func (d *LocalDeployment) git(stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30e9)
	defer cancel()
	argv := append([]string{"--git-dir=" + d.root, "-c", "gc.auto=0", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	// Do not inherit a caller's GIT_DIR, alternate objects, config injection,
	// tracing or global/system configuration. No command here contacts a remote.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("deployment: local git %s failed: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

func (d *LocalDeployment) check() error {
	b, err := os.ReadFile(filepath.Join(d.root, deploymentMarker))
	if err != nil || string(b) != "1\n" {
		return fmt.Errorf("deployment: dedicated repository marker missing")
	}
	bare, err := d.git(nil, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return fmt.Errorf("deployment: bare repository required")
	}
	remotes, err := d.git(nil, "remote")
	if err != nil {
		return err
	}
	if remotes != "" {
		return fmt.Errorf("deployment: repositories with remotes refused")
	}
	for _, p := range []string{"objects/info/alternates", "objects/info/http-alternates", "shallow"} {
		if _, err := os.Lstat(filepath.Join(d.root, p)); err == nil {
			return fmt.Errorf("deployment: shared/partial repositories refused")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (d *LocalDeployment) Head() (string, error) {
	if err := d.check(); err != nil {
		return "", err
	}
	out, err := d.git(nil, "for-each-ref", "--format=%(refname) %(objectname)", DeploymentRef)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, DeploymentRef+" ") {
			return strings.TrimPrefix(line, DeploymentRef+" "), nil
		}
	}
	return "", nil
}

// Identity binds a journal to this local repository location without returning
// a private filesystem path in status. A missing candidate after disk loss is
// an error, never an invitation to regenerate/replace the installed history.
func (d *LocalDeployment) Identity() string {
	h := sha256.Sum256([]byte(d.root))
	return hex.EncodeToString(h[:])
}

func (d *LocalDeployment) Tree(rev string) (string, error) {
	if !deploymentSHA.MatchString(rev) {
		return "", fmt.Errorf("deployment: invalid candidate")
	}
	return d.git(nil, "rev-parse", "--verify", rev+"^{tree}")
}

// Build prepares complete snapshot bytes without changing the served ref.
// Operation IDs are bound in the durable promotion journal before visibility.
func (d *LocalDeployment) Build(expected, operation string, files map[string][]byte) (string, error) {
	if err := d.check(); err != nil {
		return "", err
	}
	if expected != "" && !deploymentSHA.MatchString(expected) {
		return "", fmt.Errorf("deployment: invalid parent")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,80}$`).MatchString(operation) {
		return "", fmt.Errorf("deployment: invalid operation id")
	}
	if len(files) != len(deploymentFiles) {
		return "", fmt.Errorf("deployment: exact projection file set required")
	}
	var names []string
	total := 0
	for name, data := range files {
		if !deploymentFiles[name] || len(data) == 0 {
			return "", fmt.Errorf("deployment: invalid projection file")
		}
		total += len(data)
		names = append(names, name)
	}
	if total > 1<<20 {
		return "", fmt.Errorf("deployment: projection exceeds 1 MiB")
	}
	sort.Strings(names)
	// Construct the complete tree directly; neither working-tree files nor a
	// shared index can ride into the deployment. Fixed root filenames only.
	var treeInput bytes.Buffer
	for _, name := range names {
		blob, err := d.git(files[name], "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&treeInput, "100644 blob %s\t%s\n", blob, name)
	}
	tree, err := d.git(treeInput.Bytes(), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=wildstone-keystone", "-c", "user.email=keystone@localhost", "commit-tree", tree}
	if expected != "" {
		args = append(args, "-p", expected)
	}
	return d.git([]byte("Approved deployment operation "+operation+"\n"), args...)
}

// Retain keeps a prepared approved candidate reachable without advertising it.
func (d *LocalDeployment) Retain(operation, candidate string) error {
	if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,80}$`).MatchString(operation) || !deploymentSHA.MatchString(candidate) {
		return fmt.Errorf("deployment: invalid candidate identity")
	}
	ref := "refs/wildstone/promotions/" + operation
	_, err := d.git(nil, "update-ref", ref, candidate, zeroID)
	if err != nil {
		out, rerr := d.git(nil, "rev-parse", "--verify", ref)
		if rerr == nil && out == candidate {
			return nil
		}
	}
	return err
}

// Promote is atomic compare-and-swap; no force push, checkout, hook or remote.
func (d *LocalDeployment) Promote(expected, candidate string) error {
	if err := d.check(); err != nil {
		return err
	}
	if !deploymentSHA.MatchString(candidate) || (expected != "" && !deploymentSHA.MatchString(expected)) {
		return fmt.Errorf("deployment: invalid revision")
	}
	if expected == "" {
		expected = zeroID
	}
	_, err := d.git(nil, "update-ref", DeploymentRef, candidate, expected)
	if err != nil {
		return errors.Join(ErrDiverged, err)
	}
	return nil
}
