package runtimedb

// B1's evidence (ADR-W031): a Butler is a named specialist with one
// assigned model and its own access list, the ceiling it was created
// against cannot move, and a Butler whose model is gone waits rather
// than answering from somewhere else.

import (
	"errors"
	"strings"
	"testing"
)

// butlerFixture gives a database two routes — one local, one cloud —
// and two admitted people.
func butlerFixture(t *testing.T) *DB {
	t.Helper()
	db := butlerTestDB(t)
	for _, r := range []Model{
		{Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama",
			Readiness: ModelReady, Enabled: true},
		{Name: "cloud", Kind: ModelCloud, Provider: "anthropic", ProviderModel: "sonnet",
			Readiness: ModelReady, Enabled: true},
	} {
		if err := db.UpsertModel(r, "root-admin"); err != nil {
			t.Fatalf("UpsertModel(%s): %v", r.Name, err)
		}
	}
	for _, m := range []struct{ name, profile string }{
		{"mika", ProfileOperator}, {"sam", ProfileHousehold},
	} {
		if err := db.RecordButlerMember(m.name, m.profile, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func newButler(t *testing.T, db *DB, name, route string) {
	t.Helper()
	if _, err := db.CreateButler(Butler{
		Name: name, Directive: "look after " + name, Surface: ButlerSurfaceFamily,
		Profile: ProfileHousehold, Model: route,
	}, "mika"); err != nil {
		t.Fatalf("CreateButler(%s): %v", name, err)
	}
}

// The shape of the thing: a household has Butlers, plural, each with
// its own name and its own model, and the model is answerable by name
// rather than by "whichever was free" (A1, A2, A11).
func TestAButlerCarriesItsOwnNameAndModel(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "alfred", "kitchen-box")
	newButler(t, db, "robert", "cloud")

	alfred, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if alfred.Model != "kitchen-box" || alfred.ModelKind != ModelLocal {
		t.Fatalf("alfred runs on %q (%s), want kitchen-box (local)", alfred.Model, alfred.ModelKind)
	}
	robert, err := db.GetButler("robert")
	if err != nil {
		t.Fatal(err)
	}
	if robert.ModelKind != ModelCloud {
		t.Fatalf("robert runs on a %s route, want cloud", robert.ModelKind)
	}
	// Two Butlers, two models, no shared "the Butler".
	all, err := db.ListButlers()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d butlers, want 2", len(all))
	}

	// A Butler needs a name and a model. Neither is a field to fill in
	// later: an unnamed Butler cannot be assigned a routine, and one
	// with no model has nowhere to run.
	if _, err := db.CreateButler(Butler{
		Surface: ButlerSurfaceFamily, Profile: ProfileHousehold, Model: "kitchen-box",
	}, "mika"); err == nil {
		t.Fatal("a butler was created with no name")
	}
	if _, err := db.CreateButler(Butler{
		Name: "nameless-model", Surface: ButlerSurfaceFamily, Profile: ProfileHousehold,
	}, "mika"); err == nil {
		t.Fatal("a butler was created with no model route")
	}
	if _, err := db.CreateButler(Butler{
		Name: "ghost", Surface: ButlerSurfaceFamily, Profile: ProfileHousehold,
		Model: "no-such-route",
	}, "mika"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a butler was created against a route that does not exist: %v", err)
	}
}

// A3: the surface is the structural ceiling a Butler was created
// against, and it cannot move afterwards. Nor can a profile sit on a
// surface that does not enforce it.
func TestAButlersCeilingCannotMove(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "alfred", "kitchen-box")

	if _, err := db.ExecForTest(
		`UPDATE butlers SET surface = 'admin' WHERE name = 'alfred'`); err == nil {
		t.Fatal("a family butler was moved onto the admin surface")
	}
	if _, err := db.ExecForTest(
		`UPDATE butlers SET profile = 'operator' WHERE name = 'alfred'`); err == nil {
		t.Fatal("a household butler was given the operator ceiling")
	}
	// And the two have to agree at creation, or the ceiling written on
	// the record is not the one the gateway enforces.
	if _, err := db.CreateButler(Butler{
		Name: "mismatched", Surface: ButlerSurfaceFamily, Profile: ProfileOperator,
		Model: "kitchen-box",
	}, "mika"); err == nil {
		t.Fatal("an operator-profile butler was created on the family surface")
	}
}

// A10: the Console holder decides who may talk to which Butler, and a
// person sees only what they were granted — they do not learn that the
// others exist.
func TestAccessIsABoundaryNotAFilter(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newButler(t, db, "nutrition", "cloud")

	if err := db.GrantButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	mine, err := db.ButlersFor("sam")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Name != "health" {
		t.Fatalf("sam sees %+v, want only health", mine)
	}
	// Reaching one they were not granted is refused, and the refusal is
	// access rather than anything that describes the Butler.
	if _, err := db.ResolveButlerFor("nutrition", "sam"); !errors.Is(err, ErrNoButlerAccess) {
		t.Fatalf("sam reached a butler they were never granted: %v", err)
	}
	if _, err := db.ResolveButlerFor("health", "sam"); err != nil {
		t.Fatalf("sam could not reach the butler they were granted: %v", err)
	}

	// A repeated grant is the state the caller asked for, not an error.
	if err := db.GrantButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatalf("re-granting existing access failed: %v", err)
	}

	// Revocation is immediate.
	if err := db.RevokeButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveButlerFor("health", "sam"); !errors.Is(err, ErrNoButlerAccess) {
		t.Fatalf("a revoked grant still reached the butler: %v", err)
	}

	// Access is downstream of admission, never a way around it.
	if err := db.GrantButlerAccess("health", "stranger", "mika"); err == nil {
		t.Fatal("someone who was never admitted was granted access to a butler")
	}
	// And admission is rechecked at USE, not trusted from the grant.
	if err := db.GrantButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.OffboardButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveButlerFor("health", "sam"); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("an offboarded member still reached a butler they had been granted: %v", err)
	}
}

// A6: a Butler whose model is unavailable WAITS. It does not answer
// from another Butler's model, because that substitution is the
// disclosure the whole design exists to prevent.
func TestAButlerWithNoModelWaitsRatherThanSubstituting(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	if err := db.GrantButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordModelReadiness("kitchen-box", ModelUnavailable, "the box is off"); err != nil {
		t.Fatal(err)
	}

	_, err := db.ResolveButlerFor("health", "sam")
	if !errors.Is(err, ErrButlerUnreachable) {
		t.Fatalf("a butler with no model resolved anyway: %v", err)
	}
	// The refusal names the model that is missing, so a person reading
	// it knows what to fix rather than that "something is wrong".
	if !strings.Contains(err.Error(), "kitchen-box") {
		t.Fatalf("the refusal does not name the unavailable model: %v", err)
	}
	// Nothing quietly re-pointed it at the cloud route that is sitting
	// right there, ready.
	b, err := db.GetButler("health")
	if err != nil {
		t.Fatal(err)
	}
	if b.Model != "kitchen-box" {
		t.Fatalf("an unavailable model was substituted: health now runs on %q", b.Model)
	}

	// A QA-only verdict is a different thing: the model is THERE and is
	// not trusted with actions. It answers questions.
	if err := db.RecordModelReadiness("kitchen-box", ModelQAOnly, "it invented a tool name"); err != nil {
		t.Fatal(err)
	}
	qa, err := db.ResolveButlerFor("health", "sam")
	if err != nil {
		t.Fatalf("a QA-only butler was treated as unreachable: %v", err)
	}
	if qa.CarriesActionAuthority() {
		t.Fatal("a QA-only butler carries action authority")
	}
}

// A8: the Console holder changes a Butler's model directly, and the
// change is answerable afterwards. A local-to-cloud move is the one the
// caller has to warn about.
func TestChangingAButlersModelIsDirectAndReportsTheCrossing(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")

	from, to, err := db.SetButlerRoute("health", "cloud", "mika")
	if err != nil {
		t.Fatalf("SetButlerRoute: %v", err)
	}
	if from.ModelKind != ModelLocal || to.ModelKind != ModelCloud {
		t.Fatalf("route change reported %s -> %s", from.ModelKind, to.ModelKind)
	}
	if !CrossesToCloud(from, to) {
		t.Fatal("a local-to-cloud move did not report itself as one, so nothing could warn")
	}
	// It applied at once — no proposal, no pending anything.
	live, err := db.GetButler("health")
	if err != nil {
		t.Fatal(err)
	}
	if live.Model != "cloud" || live.UpdatedBy != "mika" {
		t.Fatalf("the change did not apply directly: %+v", live)
	}

	// The other direction narrows where material can go, and does not
	// warn — warning on it would teach people to click through the one
	// that matters.
	back, backTo, err := db.SetButlerRoute("health", "kitchen-box", "mika")
	if err != nil {
		t.Fatal(err)
	}
	if CrossesToCloud(back, backTo) {
		t.Fatal("a cloud-to-local move warned, which trains people to ignore warnings")
	}
}

// Retiring stops a Butler without deleting what it did.
func TestRetiringAButlerKeepsItsRecord(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	if err := db.GrantButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.RetireButler("health", "mika"); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ResolveButlerFor("health", "sam"); !errors.Is(err, ErrButlerUnreachable) {
		t.Fatalf("a retired butler still answered: %v", err)
	}
	if mine, _ := db.ButlersFor("sam"); len(mine) != 0 {
		t.Fatalf("a retired butler is still offered to talk to: %+v", mine)
	}
	// But it is still there to read: what it did is evidence, and
	// retiring is not a request to forget it.
	b, err := db.GetButler("health")
	if err != nil {
		t.Fatalf("a retired butler's record was deleted: %v", err)
	}
	if b.RetiredAt.IsZero() || b.RetiredBy != "mika" {
		t.Fatalf("retirement was not recorded: %+v", b)
	}
	if err := db.RetireButler("health", "mika"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retiring twice = %v", err)
	}
}

// A Butler cannot be pointed at a disabled route. The reference alone
// is not enough: a Butler assigned to a route somebody has since turned
// off has nowhere to run, and A6's wait only works if the assignment
// still names a model that is meant to be there.
func TestAButlerCannotBeAssignedADisabledRoute(t *testing.T) {
	db := butlerFixture(t)
	if err := db.UpsertModel(Model{
		Name: "retired-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "old",
		Readiness: ModelReady, Enabled: false,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateButler(Butler{
		Name: "stranded", Surface: ButlerSurfaceFamily, Profile: ProfileHousehold,
		Model: "retired-box",
	}, "mika"); err == nil {
		t.Fatal("a butler was created against a disabled route")
	}
	// And an existing Butler cannot be moved onto one either.
	newButler(t, db, "health", "kitchen-box")
	if _, _, err := db.SetButlerRoute("health", "retired-box", "mika"); err == nil {
		t.Fatal("a butler was moved onto a disabled route")
	}
	if b, _ := db.GetButler("health"); b.Model != "kitchen-box" {
		t.Fatalf("the refused move still changed the assignment: %q", b.Model)
	}
}
