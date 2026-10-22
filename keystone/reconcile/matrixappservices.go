package reconcile

// The merged Matrix application-service registrations.
//
// A chat bot on the household's own homeserver is registered with it as an
// application service rather than signed in as a person (ADR-W033 B7.2):
// the bot's capability declares a MatrixAppService, and this renders every
// one desired on the node into ONE file under the homeserver's capability
// subtree — the same shape as the merged Authelia clients file, and for the
// same reason. The bot's package cannot write into the homeserver's subtree
// (configfile.go), and the homeserver's package cannot know which bots
// exist, so the file is derived from desired state and belongs to neither.
//
// Synapse reads one registration per file, and it runs as uid 991 while a
// materialized file is 0600 root. So Synapse does not read this file: the
// matrix-synapse package's root-run Job splits it into one 991-owned file
// per registration in Synapse's data volume, and Synapse's startup module
// lists those. This file's contents are in the config digest, so a bot
// added or removed re-runs that Job and replaces Synapse, which is how a
// registration reaches the running homeserver.
//
// The format serves that split: each registration is introduced by a
// "# registration: <id>" line, which is the one thing the Job parses.
// Nothing in this file's comments may contain a secret reference, because
// the node would try to resolve it.

import (
	"context"
	"fmt"
	"log"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// matrixAppServicesPath is the merged file, under the homeserver's own
// capability subtree so the Synapse package's mounts reach it.
func matrixAppServicesPath(managedPrefix string) string {
	return path.Join(managedPrefix, "matrix-homeserver", "appservices.reg")
}

// renderMatrixAppServices renders every MatrixAppService in desired.
//
// Deterministic, sorted by registration id, because the content is both
// byte-compared for drift and folded into the homeserver's config digest:
// a reordering here would restart Synapse for nothing.
//
// With no registrations it is a comment-only file, which the Job turns
// into an empty set, and which carries no reference and so is written
// without materialization.
func renderMatrixAppServices(desired []resource.Resource) []byte {
	var regs []resource.Resource
	for _, r := range desired {
		if r.Kind == resource.KindMatrixAppService && r.MatrixAppService != nil {
			regs = append(regs, r)
		}
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].Metadata.Name < regs[j].Metadata.Name })

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("# Matrix application-service registrations, one per chat bot.\n")
	for _, r := range regs {
		s := r.MatrixAppService
		fmt.Fprintf(&b, "# registration: %s\n", r.Metadata.Name)
		fmt.Fprintf(&b, "id: %s\n", r.Metadata.Name)
		// Explicitly null: the homeserver pushes nothing, so the bot
		// needs no listener and the node gains no inbound port.
		b.WriteString("url: null\n")
		fmt.Fprintf(&b, "as_token: %q\n", s.AsTokenRef)
		fmt.Fprintf(&b, "hs_token: %q\n", s.HsTokenRef)
		// The sender is not the bot: Synapse refuses /sync to a sender.
		fmt.Fprintf(&b, "sender_localpart: %s\n", resource.MatrixAppServiceSender(s.BotLocalpart))
		// A bot answers people; the homeserver's per-user rate limit is
		// for people, and would throttle one bot serving a household.
		b.WriteString("rate_limited: false\n")
		b.WriteString("namespaces:\n")
		b.WriteString("  users:\n")
		b.WriteString("    - exclusive: true\n")
		fmt.Fprintf(&b, "      regex: '%s'\n", matrixBotRegex(s.BotLocalpart))
		b.WriteString("  aliases: []\n")
		b.WriteString("  rooms: []\n")
	}
	return []byte(b.String())
}

// matrixBotRegex is the registration's user namespace: the bot and its
// sender and nobody else, on any server name. Exclusive, so no person can
// register or be mapped to either.
func matrixBotRegex(bot string) string {
	return "@" + regexp.QuoteMeta(bot) + "(-as)?:.*"
}

// applyMatrixAppServiceStep is declared-only, like LdapGroup: the merged
// file above is a node prereq, and the homeserver's Job and container
// replacement are what reach Synapse.
func (r *Reconciler) applyMatrixAppServiceStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	_ = ctx // no external call is made; ctx is accepted for signature symmetry with the other apply* methods
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}
	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.MatrixAppService == nil {
			sr.Error = "internal: no desired MatrixAppService spec for " + step.Name
			return sr
		}
		log.Printf("reconcile: MatrixAppService %q declared for @%s; the rendered registrations and the "+
			"homeserver's own Job are what install it", step.Name, want.MatrixAppService.BotLocalpart)
	case plan.ActionDelete:
		log.Printf("reconcile: MatrixAppService %q removed from desired state; it drops out of the "+
			"rendered registrations, and the bot's user stays on the homeserver so its messages keep a sender", step.Name)
	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
	sr.Applied = true
	sr.HealthOK = true // declared-only: nothing to probe
	r.markManaged(nodeID, key, step.Action != plan.ActionDelete)
	return sr
}
