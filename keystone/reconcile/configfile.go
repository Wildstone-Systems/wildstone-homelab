package reconcile

// ConfigFile applier: write a package-rendered config file into that
// package's capability subtree of the managed prefix.
//
// The path is DERIVED from the owning capability, never taken from the
// resource (see resource.ConfigFileSpec). That is what makes this safe to
// hand to packages: a package cannot address another capability's
// subtree, so the worst it can do with this kind is write a bad file of
// its own — and the derivation matches the per-capability read-only mount
// each container gets, so a file lands exactly where its app can read it
// and nowhere else.

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// configFilePath derives the managed path for a ConfigFile.
//
// It takes the whole resource rather than a name because BOTH halves come
// from the resource and neither may be supplied loose: the directory is
// the owning capability, the leaf is the validated filename. A signature
// that accepted a path would be a signature a future caller could pass an
// attacker-influenced one to.
func configFilePath(managedPrefix string, r resource.Resource) (string, error) {
	capability, ok := strings.CutPrefix(r.Metadata.Owner, "capability/")
	if !ok || capability == "" {
		return "", fmt.Errorf("ConfigFile/%s: owner %q is not capability/<name>", r.Metadata.Name, r.Metadata.Owner)
	}
	if r.ConfigFile == nil || r.ConfigFile.Filename == "" {
		return "", fmt.Errorf("ConfigFile/%s: no filename", r.Metadata.Name)
	}
	return path.Join(managedPrefix, capability, r.ConfigFile.Filename), nil
}

// configFileNeedsMaterialization reports whether the content carries a
// secret reference the node must resolve.
//
// Derived rather than declared: a package author who forgot a flag would
// ship a config with the literal text "secret/authelia-oidc" where a
// secret belongs — an app that starts and rejects every login, with
// nothing in any log to say why. Deriving it means that cannot happen.
//
// The cost is a file that legitimately contains that text (documentation,
// say) is treated as referencing a secret and fails loudly with NOT_FOUND
// rather than shipping wrong. Loud and wrong-way-round beats quiet.
// secretRefInContent mirrors the agent's own reference syntax
// (wardstone materializeFileContent). Kept as a small duplicated constant
// — keystone cannot import wardstone — and deliberately the same shape,
// because a mismatch here means Keystone asks for materialization the
// agent will not perform, or fails to ask for one it would.
var secretRefInContent = regexp.MustCompile(`(pbkdf2:)?secret/[A-Za-z0-9][A-Za-z0-9._-]{0,63}`)

func configFileNeedsMaterialization(content string) bool {
	return secretRefInContent.MatchString(content)
}

// applyConfigFileStep writes (or, on Delete, clears) one ConfigFile.
//
// Health gate: read back and compare. For a materialized file the agent
// returns the pre-materialization hash rather than the content, and
// managedFileMatches knows the difference — Keystone holds no secrets and
// so cannot compare the bytes that actually landed.
func (r *Reconciler) applyConfigFileStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	// A Delete has no desired resource to derive the path from, so the
	// managed cache is what remembers where the file went.
	var filePath string
	if want, ok := desiredByKey[key]; ok {
		p, err := configFilePath(r.ManagedPrefix, want)
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		filePath = p
	} else if p, ok := r.configFilePathFor(nodeID, key); ok {
		filePath = p
	} else {
		sr.Error = fmt.Sprintf("internal: no path known for ConfigFile/%s", step.Name)
		return sr
	}

	var priorContent []byte
	if priorRes, priorErr := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFile{Path: filePath},
	}}); priorErr == nil && priorRes.GetError() == nil {
		priorContent = priorRes.GetReadManagedFile().GetContent()
	}

	var content []byte
	var materialize bool
	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.ConfigFile == nil {
			sr.Error = "internal: no desired ConfigFile spec for " + step.Name
			return sr
		}
		content = []byte(want.ConfigFile.Content)
		materialize = configFileNeedsMaterialization(want.ConfigFile.Content)
	case plan.ActionDelete:
		// No delete-file op exists (only Write/Read), same as Route.
		content = []byte{}
	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}

	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: filePath, Content: content, MaterializeSecrets: materialize},
	}})
	if err != nil {
		sr.Error = err.Error()
		return sr
	}
	if oe := res.GetError(); oe != nil {
		sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
		return sr
	}
	sr.Applied = true

	if step.Action == plan.ActionDelete {
		checkRes, checkErr := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFile{Path: filePath},
		}})
		sr.HealthOK = checkErr == nil && checkRes.GetError() == nil &&
			len(checkRes.GetReadManagedFile().GetContent()) == 0
		if sr.HealthOK {
			r.markManaged(nodeID, key, false)
			r.forgetConfigFilePath(nodeID, key)
		} else {
			r.restoreManagedFile(ctx, nodeID, filePath, priorContent)
			sr.Reversed = true
		}
		return sr
	}

	if r.probeConfigFile(ctx, nodeID, filePath, content) {
		sr.HealthOK = true
		r.markManaged(nodeID, key, true)
		r.rememberConfigFilePath(nodeID, key, filePath)
		return sr
	}
	r.restoreManagedFile(ctx, nodeID, filePath, priorContent)
	sr.Reversed = true
	return sr
}

func (r *Reconciler) probeConfigFile(ctx context.Context, nodeID, filePath string, want []byte) bool {
	attempts, backoff := r.healthGateParams()
	for i := 0; i < attempts; i++ {
		res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFile{Path: filePath},
		}})
		if err == nil && res.GetError() == nil && managedFileMatches(res.GetReadManagedFile(), want) {
			return true
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
		}
	}
	return false
}

func (r *Reconciler) restoreManagedFile(ctx context.Context, nodeID, filePath string, content []byte) {
	r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{ //nolint:errcheck
		WriteManagedFile: &wsv1.WriteManagedFile{Path: filePath, Content: content},
	}})
}

// observeConfigFile: present iff the file reads back equal to desired.
//
// Unlike Route, a ConfigFile that is no longer desired cannot have its
// path derived — the spec that carried the filename is gone. The managed
// cache remembers it, which is also what makes a delete possible at all.
func (r *Reconciler) observeConfigFile(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool) (resource.Resource, bool, error) {
	var filePath string
	if isDesired {
		p, err := configFilePath(r.ManagedPrefix, want)
		if err != nil {
			return resource.Resource{}, false, fmt.Errorf("observe config file %s: %w", k.Name, err)
		}
		filePath = p
	} else {
		p, ok := r.configFilePathFor(nodeID, k)
		if !ok {
			// Never applied by us, or the path record is gone: absent.
			return resource.Resource{}, false, nil
		}
		filePath = p
	}

	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ReadManagedFile{
		ReadManagedFile: &wsv1.ReadManagedFile{Path: filePath},
	}})
	if err != nil {
		return resource.Resource{}, false, fmt.Errorf("observe config file %s: %w", k.Name, err)
	}
	if oe := res.GetError(); oe != nil {
		if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			return resource.Resource{}, false, nil
		}
		return resource.Resource{}, false, fmt.Errorf("observe config file %s: %s: %s", k.Name, oe.GetCode(), oe.GetMessage())
	}
	if !isDesired {
		return placeholderResource(resource.KindConfigFile, k.Name), true, nil
	}
	if managedFileMatches(res.GetReadManagedFile(), []byte(want.ConfigFile.Content)) {
		return want, true, nil
	}
	return placeholderResource(resource.KindConfigFile, k.Name), true, nil
}
