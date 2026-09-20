package butler

// The effective-authority seam every dispatch goes through (ADR-W030
// A7/A8, work package P7; design §10).
//
// A tool being registered on a gateway is the FIRST of the questions,
// not the only one. The others — who is asking, over which Connection,
// has an administrator turned it off here, does this piece of work hold
// a grant for it — are identity and administrative state, and this
// package still holds no database. So the answer arrives through a
// seam, implemented in keystone/butlerstore over the runtime DB's
// resolver.
//
// The ordering matters and is enforced on the store side: structural
// first (a grant cannot conjure a tool the gateway does not register),
// then disabled (which wins over a cached manifest, a saved task, and a
// grant issued a minute ago), then grants, then the tool's default.
//
// # The two paths, and why they are stated rather than inferred
//
// This seam serves two genuinely different questions, and design §10's
// fail-closed rule applies to exactly one of them:
//
//	PERSON-FACING — a human is asking, directly or through a Task
//	derived from their request. §10.1's formula applies in full, and
//	"missing identity fails closed" is its own sentence in the design.
//	A missing authorizer here is not "no controls configured", it is a
//	control plane that has not said yes.
//
//	ADMINISTRATIVE — a check about administrative state rather than
//	about a person's reach: which tools a class registers, what an
//	Owner may configure, what an inventory should list. Here "no
//	administrative controls are configured" genuinely means
//	structural-only, and turning it into a hard denial would brick a
//	deployment that has simply never configured a control.
//
// The caller STATES which it is. It is never inferred from whether a
// field happens to be filled in, because an empty Person means opposite
// things on the two paths and a function that guessed would give both
// situations one answer. Ledger.AdmissionCheck in keystone/butlerstore
// is the house precedent, and its own comment is the rule: wiring is
// the act, never the inference.
//
// # Layers
//
// Design §10 decides authority in three layers, in order, and a later
// layer never widens an earlier one. Layer 1 is general execution
// authority — the Person, their Connection, the Agent's reach. Layer 2
// is resource-specific — may it reach THIS memory record, THIS secret,
// THIS file. Layer 3 is disclosure.
//
// The general rule below REQUIRES layer 2 without enumerating it. A
// query that names a resource is refused unless a rule for that
// resource's kind admits it, and this package knows no kinds. That is
// the property the design spent twelve rounds establishing: a resource
// added later cannot be silently permitted by the general rule, because
// the general rule has no way to say yes to one it has never heard of.

import "fmt"

// CallerKind states which of the two authority paths a check is on.
//
// The zero value is not one of them, and is refused. A query that never
// said which path it is on is a query nobody wired, and guessing on its
// behalf is the inference this type exists to prevent.
type CallerKind string

const (
	// CallerPersonFacing is any path where a human is asking, directly
	// or through a Task derived from their request. Missing controls,
	// missing identity and missing Connection all fail closed here.
	CallerPersonFacing CallerKind = "person"

	// CallerAdministrative is a check about administrative state. A nil
	// authorizer leaves structural presence deciding, which is what
	// this seam did before design §10.
	CallerAdministrative CallerKind = "administrative"
)

// ResourceRef names the resource a tool call reaches for, when it
// reaches for one.
type ResourceRef struct {
	// Kind is the resource class — a memory record, a secret, a file.
	//
	// It is a free string rather than a closed set ON PURPOSE. §10
	// requires the general rule to demand resource-specific authority
	// WITHOUT enumerating the resources that have it; a closed set here
	// would be that enumeration, and the day somebody added a case to
	// it would be the day the general rule started answering for a
	// resource whose own rule had not been written yet.
	Kind string
	// ID names the one resource within its kind.
	ID string
}

// Named reports whether a resource is being reached at all.
func (r ResourceRef) Named() bool { return r.Kind != "" }

// ResourceRule is one resource kind's own authority check — layer 2,
// defined by the resource (memory in §11, secrets in §12) and never by
// this package.
type ResourceRule func(q ToolAuthorityQuery) (ToolDecision, error)

// ResourceRules is a set of them keyed by resource kind, and is the
// ready-made ResourceAuthorizer a resolver can carry.
//
// A kind with no rule is REFUSED rather than admitted. That single line
// is the class closure: adding a resource adds a rule, and forgetting
// to add one produces a refusal rather than a quiet yes.
type ResourceRules map[string]ResourceRule

// AdmitResource implements ResourceAuthorizer.
func (rr ResourceRules) AdmitResource(q ToolAuthorityQuery) (ToolDecision, error) {
	rule, ok := rr[q.Resource.Kind]
	if !ok || rule == nil {
		return ToolDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"%s reaches a %s, and nothing here decides who may reach one, so it was not run",
				q.Tool, q.Resource.Kind),
		}, nil
	}
	return rule(q)
}

// ResourceAuthorizer is the layer-2 half of the seam. A resolver that
// does not implement it admits no resource at all, which is the right
// answer for a resolver that has no resource rules.
type ResourceAuthorizer interface {
	AdmitResource(q ToolAuthorityQuery) (ToolDecision, error)
}

// ToolDecision is the resolver's answer for one dispatch.
type ToolDecision struct {
	Allowed bool
	// Reason is shown to the model and to the person. "You do not have
	// that tool here" and "an administrator turned that off" are
	// different situations, and reporting them identically produces a
	// household that cannot tell a missing capability from a withdrawn
	// one.
	Reason string
	// Requestable is true when the answer is no but a person could
	// change it — the case A7 calls asking for a missing capability.
	Requestable bool
	// GrantSeq names the grant that allowed it, for the work trace.
	GrantSeq int64
}

// ToolAuthorityQuery is what the resolver needs. The gateway supplies
// the structural facts and the identity because only it knows them; the
// store supplies the administrative ones.
type ToolAuthorityQuery struct {
	// Caller is which of the two paths this check is on. It is required
	// (see CallerKind).
	Caller CallerKind
	// Person is who is asking, on a person-facing check. It is the
	// member handle the rest of the Butler carries, so the store can
	// join on it without this package learning a person id.
	//
	// Empty on a person-facing check fails closed: §10.1's "missing
	// identity fails closed" is about exactly this field.
	Person string
	// Connection names the Connection the request arrived through.
	//
	// THIS SLICE CARRIES THE IDENTIFIER AND FAILS CLOSED WHEN IT IS
	// ABSENT. It does not decide what a Connection admits — that is
	// slice 11 (#212), which is what makes a Connection an admission
	// boundary in the first place. The split is written down here
	// because a Connection leg that looks finished and is not is
	// exactly what #212 must not find.
	Connection string
	// Resource is what the call reaches for, empty when it reaches for
	// nothing. A named resource requires a rule for its kind; see
	// ResourceRules.
	Resource ResourceRef

	AgentClass string
	Tool       string
	// Structural is whether this gateway registers the tool at all.
	Structural bool
	// GrantRequired is the tool's own metadata.
	GrantRequired bool
	// TaskID and RoutineName scope a work-bound grant. Empty means no
	// grant can apply, which is the right default for a one-shot
	// question that is not durable work.
	TaskID      string
	RoutineName string
}

// ToolAuthorizer resolves effective authority for one dispatch.
type ToolAuthorizer interface {
	AuthorizeTool(q ToolAuthorityQuery) (ToolDecision, error)
}

// authorizeForPerson runs the seam for a human's request.
//
// It is a separate entry point from authorizeAdministrative rather than
// a flag, so a reviewer can see which path they are reading from the
// call site alone.
func authorizeForPerson(a ToolAuthorizer, q ToolAuthorityQuery) ToolDecision {
	q.Caller = CallerPersonFacing
	return runAuthority(a, q)
}

// authorizeAdministrative runs the seam for a check about
// administrative state, where a deployment with no controls configured
// is a real state and not a denial.
func authorizeAdministrative(a ToolAuthorizer, q ToolAuthorityQuery) ToolDecision {
	q.Caller = CallerAdministrative
	return runAuthority(a, q)
}

// runAuthority is the one evaluation both entry points share.
//
// Both go through it so that DESCRIBING and DISPATCHING cannot grow
// separate answers: the manifest decides what to offer a model and the
// gateway decides what to run, and ADR-W030 P7 is that they must agree.
// It fails closed if the resolver itself errors — a control plane that
// cannot say whether something is permitted has not said yes.
func runAuthority(a ToolAuthorizer, q ToolAuthorityQuery) ToolDecision {
	switch q.Caller {
	case CallerPersonFacing:
		if d, ok := generalAuthority(a, q); !ok {
			return d
		}
	case CallerAdministrative:
		if a == nil {
			// No administrative controls configured. Structural is
			// still the boundary; a tool the gateway does not register
			// was already refused before reaching here.
			return ToolDecision{Allowed: q.Structural}
		}
	default:
		// Nobody said which path this is. Answering either way would
		// be the inference this seam refuses to make.
		return ToolDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"%s was checked without saying whether a person or an administrator was asking, so it was not run",
				q.Tool),
		}
	}
	d, err := a.AuthorizeTool(q)
	if err != nil {
		return ToolDecision{
			Allowed: false,
			Reason:  fmt.Sprintf("could not check whether %s is permitted right now, so it was not run", q.Tool),
		}
	}
	return d
}

// generalAuthority is layer 1 for a person's request, plus the demand
// that layer 2 exist for whatever resource is being reached.
//
// It returns (decision, false) when the answer is already no. Every
// branch here is a refusal; nothing in this function can turn a no into
// a yes, which is what keeps it a narrowing of §10.1's intersection
// rather than another way to be allowed.
func generalAuthority(a ToolAuthorizer, q ToolAuthorityQuery) (ToolDecision, bool) {
	if q.Person == "" {
		return ToolDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"this session does not say who is asking, so %s was not run", q.Tool),
		}, false
	}
	if q.Connection == "" {
		return ToolDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"this session does not say which connection it arrived on, so %s was not run", q.Tool),
		}, false
	}
	if a == nil {
		// The permissive default dies here. On this path a missing
		// resolver is not "no controls exist yet" — it is nobody having
		// said this person may do this, and §10.1 answers that with no.
		return ToolDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"nothing on this deployment can say whether %s may use %s, so it was not run",
				q.Person, q.Tool),
		}, false
	}
	if q.Resource.Named() {
		// The general rule REQUIRES the resource's own authority without
		// knowing what it is. A resolver with no resource rules admits
		// no resource; a resolver with rules admits only the kinds it
		// has one for.
		ra, ok := a.(ResourceAuthorizer)
		if !ok {
			return ToolDecision{
				Allowed: false,
				Reason: fmt.Sprintf(
					"%s reaches a %s, and nothing here decides who may reach one, so it was not run",
					q.Tool, q.Resource.Kind),
			}, false
		}
		d, err := ra.AdmitResource(q)
		if err != nil {
			return ToolDecision{
				Allowed: false,
				Reason: fmt.Sprintf(
					"could not check whether %s may be reached right now, so %s was not run",
					q.Resource.Kind, q.Tool),
			}, false
		}
		if !d.Allowed {
			return d, false
		}
	}
	return ToolDecision{}, true
}

// refusal turns a denied decision into the outcome a model sees. It is
// Rejected rather than an error because a refusal is a real answer to
// the call, and one the model should reason about rather than retry.
func refusal(tool string, d ToolDecision) ToolOutcome {
	reason := d.Reason
	if reason == "" {
		reason = fmt.Sprintf("%s is not available in this session", tool)
	}
	if d.Requestable {
		reason += ". You can ask for it to be enabled; you cannot use it until someone does."
	}
	return ToolOutcome{Content: reason, Rejected: true}
}
