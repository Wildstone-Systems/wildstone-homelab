package backup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// DependencyUnavailableError is a closed, value-free reason a backup pass
// could not be dispatched. Secret names and node identities are metadata;
// transport errors and secret material never enter the run ledger.
type DependencyUnavailableError struct {
	Code    string
	Node    string
	Secrets []string
}

func (e *DependencyUnavailableError) Error() string {
	switch e.Code {
	case "secret_inventory_unavailable":
		return fmt.Sprintf("backup dependency unavailable: cannot confirm password secrets on node %s", e.Node)
	default:
		return fmt.Sprintf("backup dependency unavailable: password secret(s) %s are not present on node %s",
			strings.Join(e.Secrets, ", "), e.Node)
	}
}

func asDependencyUnavailable(err error) (*DependencyUnavailableError, bool) {
	var unavailable *DependencyUnavailableError
	ok := errors.As(err, &unavailable)
	return unavailable, ok
}

func secretName(ref string) string {
	return strings.TrimPrefix(ref, "secret/")
}

// checkSecretRefs asks the target node itself immediately before dispatch.
// An absent inventory is not evidence that a secret exists.
func checkSecretRefs(ctx context.Context, agent Applier, node string, refs []string) error {
	refs = uniqueSecretRefs(refs)
	if len(refs) == 0 {
		return nil
	}
	if agent == nil {
		return &DependencyUnavailableError{Code: "secret_inventory_unavailable", Node: node}
	}
	res, err := agent.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_ListSecrets{ListSecrets: &wsv1.ListSecrets{}}})
	if err != nil || res == nil || res.GetError() != nil || res.GetListSecrets() == nil {
		return &DependencyUnavailableError{Code: "secret_inventory_unavailable", Node: node}
	}
	have := make(map[string]bool, len(res.GetListSecrets().GetSecrets()))
	for _, info := range res.GetListSecrets().GetSecrets() {
		have[info.GetName()] = true
	}
	var missing []string
	for _, ref := range refs {
		if !have[secretName(ref)] {
			missing = append(missing, ref)
		}
	}
	if len(missing) != 0 {
		return &DependencyUnavailableError{Code: "password_secret_missing", Node: node, Secrets: missing}
	}
	return nil
}

func uniqueSecretRefs(refs []string) []string {
	set := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref, "secret/") && len(ref) > len("secret/") {
			set[ref] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ref := range set {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func passwordRefsForRun(r resource.Resource, kind runtimedb.BackupRunKind) []string {
	if r.BackupPlan == nil {
		return nil
	}
	spec := r.BackupPlan
	switch kind {
	case runtimedb.BackupRunOffsite, runtimedb.BackupRunOffsiteCheck,
		runtimedb.BackupRunOffsiteDrill, runtimedb.BackupRunOffsiteRestore:
		if spec.Offsite == nil {
			return nil
		}
		refs := []string{spec.Offsite.PasswordSecret}
		if kind == runtimedb.BackupRunOffsite && spec.Offsite.Route == resource.BackupRouteLocalAndOffsite {
			refs = append(refs, spec.PasswordSecret)
		}
		return refs
	default:
		return []string{spec.PasswordSecret}
	}
}

func (e *Engine) checkPasswordDependencies(ctx context.Context, r resource.Resource, kind runtimedb.BackupRunKind) error {
	return checkSecretRefs(ctx, e.Agent, r.Metadata.Node, passwordRefsForRun(r, kind))
}

// recordDependencyUnavailable stores one durable row per unchanged condition.
// Repeated scheduler ticks and restarts recheck the node but do not grow an
// hourly failure history or pretend a Restic run started.
func (e *Engine) recordDependencyUnavailable(r resource.Resource, kind runtimedb.BackupRunKind, err error) {
	unavailable, ok := asDependencyUnavailable(err)
	if !ok {
		return
	}
	_, inserted, recordErr := e.DB.RecordBackupDependencyUnavailable(r.Metadata.Name, kind, unavailable.Error())
	if recordErr != nil {
		e.log().Error("backup: recording unavailable dependency", "plan", r.Metadata.Name, "kind", string(kind), "err", recordErr)
		return
	}
	if inserted {
		e.event("backup.dependency_unavailable", r.Metadata.Node, unavailable.Error(), map[string]string{
			"backupPlan": r.Metadata.Name, "kind": string(kind), "code": unavailable.Code,
		})
	}
}

// passwordSecretRefs extracts only password-bearing references. Other
// credentials remain under their existing contracts; this gate cannot turn
// into a generic secret-value inspection path.
func passwordSecretRefs(spec resource.ContainerSpec) []string {
	var refs []string
	for key, value := range spec.Env {
		if strings.Contains(key, "PASSWORD") && strings.HasPrefix(value, "secret/") {
			refs = append(refs, value)
		}
	}
	return refs
}
