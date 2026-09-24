package api

// The Agent's runtime record and its runtime principal (ADR-W039 §3.3,
// §3.7, §4.1; Phase 1 of the plan in §8).
//
// Every Agent has a runtime — today always the native, in-process
// runner — and every runtime authenticates as a real Keystone
// principal bound to that Agent for the principal's life. The native
// runtime's credential is minted rather than synthesised (§3.3), so an
// Agent that was made yesterday and an Agent that will run on Hermes
// next year are credentialed the same way: `runtime-<agent>-<n>`, role
// butler, acting for nobody, bound once.
//
// This slice writes the record and mints the principal. It issues no
// bearer token for it: nothing calls in as the native runtime yet, and
// a token nobody presents is only a secret waiting to leak. Rotation
// and revocation are the next slice; the gateway that serves calls
// made under the principal is Phase 2.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// runtimePrincipalPrefix names the principal minted for an Agent's
// runtime, as chatAdapterPrincipalPrefix does for a chat adapter.
const runtimePrincipalPrefix = "runtime-"

// runtimePrincipalName is `runtime-<agent>-<serial>` in the principal
// name discipline (lowercase [a-z0-9._-], 2–63 chars). A Butler's name
// is a person's word for it — "Kitchen Helper" — so it is slugged, not
// used raw; and because two names can slug alike ("Health", "health"),
// the caller passes disambiguate=true to fold the Butler's seq in when
// the plain form is already somebody else's.
func runtimePrincipalName(b runtimedb.Butler, serial int, disambiguate bool) string {
	var slug strings.Builder
	dash := false
	for _, r := range strings.ToLower(b.Name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			slug.WriteRune(r)
			dash = false
		case !dash && slug.Len() > 0:
			slug.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(slug.String(), "-")
	// Leave room for the prefix, a seq, and a serial inside 63 chars.
	if len(s) > 36 {
		s = strings.TrimRight(s[:36], "-")
	}
	if s == "" {
		s = fmt.Sprintf("agent%d", b.Seq)
	} else if disambiguate {
		s = fmt.Sprintf("%s.%d", s, b.Seq)
	}
	return fmt.Sprintf("%s%s-%d", runtimePrincipalPrefix, s, serial)
}

// ensureNativeRuntime gives an Agent the native runtime record and a
// live, bound runtime principal, and marks the runtime connected.
//
// It converges rather than creates: an Agent with a record keeps it,
// an Agent with a credential keeps that credential (re-enabled, since
// the one path that disables it is retiring), and only an Agent with
// none is minted one. That makes it the single routine for a new
// Agent, a duplicated one, a restored one, and the startup backfill of
// Agents that predate 0081 — four callers, one set of invariants.
//
// `connected` is truthful for native and only for native: the runner
// is loaded with the process and has nothing to register. An Agent
// whose runtime is something else is left alone; bringing an external
// runtime back after a retirement is a re-registration, and the
// credential it needs is minted by that flow, not this one.
func (s *Server) ensureNativeRuntime(b runtimedb.Butler, id principal.Identity) error {
	by := actingMember(id)
	rt, err := s.DB.GetButlerRuntime(b.Seq)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		if err := s.DB.SetButlerRuntime(b.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, by); err != nil {
			return err
		}
	case err != nil:
		return err
	case rt.Type != agentapi.RuntimeNative:
		return nil
	}

	bound, err := s.DB.ListRuntimePrincipals(b.Seq)
	if err != nil {
		return err
	}
	var name string
	if len(bound) > 0 {
		name = bound[len(bound)-1]
		if err := s.DB.SetPrincipalDisabled(name, false); err != nil {
			return fmt.Errorf("re-enabling %s: %w", name, err)
		}
	} else {
		name, err = s.mintRuntimePrincipal(b, 1, id)
		if err != nil {
			return err
		}
	}
	if err := s.DB.BindRuntimePrincipal(name, b.Seq); err != nil {
		return err
	}
	return s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthConnected)
}

// mintRuntimePrincipal creates `runtime-<agent>-<serial>` with the
// butler role and nobody as its acting identity. The Person on any
// call the runtime makes comes from the task handle Wildstone issued
// (§4.1), never from the credential, so the credential carries none.
//
// No token is issued. That is deliberate for this slice (see the file
// comment), and it is what keeps the mint out of the consequential
// freshness rule: nothing a bearer could present has been created.
func (s *Server) mintRuntimePrincipal(b runtimedb.Butler, serial int, id principal.Identity) (string, error) {
	name := runtimePrincipalName(b, serial, false)
	if existing, err := s.DB.GetPrincipal(name); err == nil {
		// The plain name is taken. It is ours only if it is bound to
		// this Agent — which ensureNativeRuntime would have found — so
		// this is a slug collision with another Agent, or a name an
		// operator made by hand. Either way, fold the seq in.
		if owner, ok, err := s.DB.RuntimePrincipalButler(existing.Name); err != nil {
			return "", err
		} else if !ok || owner != b.Seq {
			name = runtimePrincipalName(b, serial, true)
		}
	} else if !errors.Is(err, runtimedb.ErrNotFound) {
		return "", err
	}
	if _, err := s.DB.GetPrincipal(name); errors.Is(err, runtimedb.ErrNotFound) {
		if err := s.Principals.Create(name, runtimedb.RoleButler, ""); err != nil {
			return "", fmt.Errorf("minting the runtime principal for %s: %w", b.Name, err)
		}
		s.event("principal.created", "", id, fmt.Sprintf("%s (%s, runtime of %s)", name, runtimedb.RoleButler, b.Name))
	} else if err != nil {
		return "", err
	}
	return name, nil
}

// revokeAgentRuntime is §3.7's immediate revocation as far as this
// slice reaches: every credential the Agent ever held is disabled, so
// each fails closed on its next request, and the runtime is marked
// revoked. Subscriptions and in-flight attempts do not exist for a
// runtime principal yet; the later slices fence those here too.
func (s *Server) revokeAgentRuntime(b runtimedb.Butler) error {
	bound, err := s.DB.ListRuntimePrincipals(b.Seq)
	if err != nil {
		return err
	}
	for _, name := range bound {
		if err := s.DB.SetPrincipalDisabled(name, true); err != nil {
			return fmt.Errorf("disabling %s: %w", name, err)
		}
	}
	err = s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthRevoked)
	if errors.Is(err, runtimedb.ErrNotFound) {
		// An Agent with no runtime record has nothing to revoke. It
		// cannot happen after 0081, but a retire must not fail on it.
		return nil
	}
	return err
}

// BackfillRuntimePrincipals mints a runtime principal for every live
// Agent that has none. 0081 gives each existing Agent its native
// runtime record but cannot mint the credential — a principal name
// goes through keystone/principal's discipline, which SQL does not
// apply — so the process does it once at startup, as it enrols
// pre-0059 Agents in assigned tools. Idempotent: an Agent whose record
// already names a credential is skipped, and a retired one is left
// revoked.
func (s *Server) BackfillRuntimePrincipals() error {
	if s.DB == nil || s.Principals == nil {
		return nil
	}
	butlers, err := s.DB.ListButlers()
	if err != nil {
		return err
	}
	system := principal.Identity{Name: "system"}
	for _, b := range butlers {
		if !b.RetiredAt.IsZero() {
			continue
		}
		rt, err := s.DB.GetButlerRuntime(b.Seq)
		if err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
			return err
		}
		if err == nil && (rt.Principal != "" || rt.Type != agentapi.RuntimeNative) {
			continue
		}
		if err := s.ensureNativeRuntime(b, system); err != nil {
			return fmt.Errorf("agent %s: %w", b.Name, err)
		}
	}
	return nil
}

// butlerRuntime is GET /api/v1/butlers/{name}/runtime: the Agent's
// runtime record in the Agent API's shape (§3.3). A read, like the rest
// of what the Console shows about an Agent; the credential is a
// reference here and never a secret.
func (s *Server) butlerRuntime(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	rt, err := s.DB.GetButlerRuntimeByName(name)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no butler named %q", name)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return rt.Record(), http.StatusOK, nil
}
