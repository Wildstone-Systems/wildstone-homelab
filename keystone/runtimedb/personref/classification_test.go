package personref_test

// The classification is only worth having if it cannot silently fall
// behind the schema. These tests reconcile it against a REAL migrated
// database — sqlite_master and PRAGMA table_info — rather than against
// the migration text, so a column added by any means is caught.

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb/personref"

	_ "modernc.org/sqlite"
)

// dismissed names every TEXT column name in the schema that does NOT
// name a party, with the reason.
//
// It exists because the first version of this test did the opposite:
// it carried a list of person-ish names and required a verdict for
// anything matching. That list was built by the same survey it was
// meant to check, so a column the survey had never thought of —
// notices.recipient, documented "recipient is the member" — passed both
// completeness tests while being invisible to Columns and to Survey. A
// person referenced only there would have been reported migration-ready
// and then had their notices orphaned by a rename. Found by review on
// #239, and it was a fair hit on the method, not just on one column.
//
// So the default is inverted. Every TEXT column must be either
// classified in personref.Columns or dismissed BY NAME here. A column
// whose name appears in neither fails the build, which is the only
// arrangement where a name nobody anticipated cannot slip through.
//
// Dismissal is by name rather than by table.column on purpose: `kind`
// and `created_at` mean the same thing wherever they appear, and a new
// table reusing them needs no ceremony. A new name does.
var dismissed = map[string]string{
	"audience_id":         "names an audience — a disclosure rule, not a party; who it resolves to lives in audience_members.ref (design §10 layer 3)",
	"group_id":            "names a group — a set of people, which is not itself a party; who is in it lives in person_group_members.person_id (design §4)",
	"seed_key":            "names the ROLE a seeded group plays ('adults'), not a person and not the group's display name",
	"service":             "names a capability ('photos') — a thing the household runs, not a party (design §4, slice 8)",
	"ldap_group":          "names a DIRECTORY group — a set of people, which is not itself a party; who is in it lives in projected_memberships.person_id (design §4)",
	"origin_connection":   "names the CONNECTION a task arrived over ('console', 'matrix:!room:example.org') — a route into the house, not a party; who came through it is butler_tasks.author_person_id (design §8.1, §10.1)",
	"placed_at":           "a timestamp",
	"paired_at":           "a timestamp",
	"assigned_at":         "a timestamp",
	"since":               "a timestamp — when an agent became default-deny",
	"effect":              "an enum: whether an entitlement allows or restricts",
	"dormant_at":          "a timestamp",
	"butler":              "names a Butler — an agent, which is a different axis from a Person (#201 Decision 5)",
	"app_health":          "a health verdict for an app, not a party",
	"applied_at":          "a timestamp",
	"approximate":         "meter names whose position was guessed, not people",
	"account":             "names a machine, service or artifact, not a person",
	"acknowledged_at":     "a timestamp",
	"acquired_at":         "a timestamp",
	"action":              "an enum or classification, not a party",
	"adapter":             "names a machine, service or artifact, not a person",
	"added_at":            "a timestamp",
	"admitted_at":         "a timestamp",
	"agent_version":       "an enum or classification, not a party",
	"answer":              "descriptive text, not a party",
	"answered_at":         "a timestamp",
	"asked_at":            "a timestamp",
	"at":                  "a timestamp",
	"attached_at":         "a timestamp",
	"attrs":               "a serialized blob",
	"audience":            "an enum or classification, not a party",
	"body":                "descriptive text, not a party",
	"bound_at":            "a timestamp",
	"budget_json":         "a serialized blob",
	"butlers":             "names a machine, service or artifact, not a person",
	"capability":          "names a machine, service or artifact, not a person",
	"change":              "an enum or classification, not a party",
	"claimed_at":          "a timestamp",
	"cleanup_plan":        "descriptive text, not a party",
	"completion_criteria": "names a machine, service or artifact, not a person",
	"confidence":          "an enum or classification, not a party",
	"connection":          "names a machine, service or artifact, not a person",
	"content":             "descriptive text, not a party",
	"content_key":         "an identifier for a non-person entity",
	"conversation":        "a transport's own conversation id, not a Wildstone handle",
	"correlation_id":      "an identifier for a non-person entity",
	"created_at":          "a timestamp",
	"deadline_at":         "a timestamp",
	"decided_at":          "a timestamp",
	"decision_note":       "descriptive text, not a party",
	"dedupe_key":          "an identifier for a non-person entity",
	"degraded_reason":     "descriptive text, not a party",
	"delivered_at":        "a timestamp",
	"description":         "descriptive text, not a party",
	"detail":              "descriptive text, not a party",
	"directive":           "names a machine, service or artifact, not a person",
	"disabled_at":         "a timestamp",
	"disclosure":          "an enum or classification, not a party",
	"display_name":        "names a machine, service or artifact, not a person",
	"document":            "a serialized blob",
	"ended_at":            "a timestamp",
	"endpoint":            "names a machine, service or artifact, not a person",
	"entered_at":          "a timestamp",
	"entity":              "names a machine, service or artifact, not a person",
	"error":               "descriptive text, not a party",
	"error_text":          "descriptive text, not a party",
	"evaluator_version":   "an enum or classification, not a party",
	"event_id":            "an identifier for a non-person entity",
	"expected":            "descriptive text, not a party",
	"expires_at":          "a timestamp",
	"external_id":         "an identifier for a non-person entity",
	"final_text":          "descriptive text, not a party",
	"finished_at":         "a timestamp",
	"first_joined_at":     "a timestamp",
	"fixture":             "a serialized blob",
	"fixture_json":        "a serialized blob",
	"forgotten_at":        "a timestamp",
	"from_state":          "an enum or classification, not a party",
	"from_surface":        "an enum or classification, not a party",
	"granted_at":          "a timestamp",
	"grants_json":         "a serialized blob",
	"handle":              "names a machine, service or artifact, not a person",
	"hash":                "an identifier for a non-person entity",
	"head":                "an identifier for a non-person entity",
	"healthy_since":       "a timestamp",
	"heartbeat_at":        "a timestamp",
	"hold_until":          "a timestamp",
	"holder":              "names a machine, service or artifact, not a person",
	"indexed_at":          "a timestamp",
	"job":                 "names a machine, service or artifact, not a person",
	"kind":                "an enum or classification, not a party",
	"label":               "an enum or classification, not a party",
	"last_health":         "descriptive text, not a party",
	"last_seen_at":        "a timestamp",
	"last_used":           "a timestamp",
	"last_used_at":        "a timestamp",
	"link_type":           "names a machine, service or artifact, not a person",
	"media_type":          "an enum or classification, not a party",
	"manifest_version":    "a serialized blob",
	"measured_at":         "a timestamp",
	"message":             "descriptive text, not a party",
	"method":              "an enum or classification, not a party",
	"mode":                "an enum or classification, not a party",
	"model":               "names a machine, service or artifact, not a person",
	"model_endpoint":      "names a machine, service or artifact, not a person",
	"model_verdict":       "descriptive text, not a party",
	"name":                "names a machine, service or artifact, not a person",
	"new_role":            "an enum or classification, not a party",
	"next_wakeup_at":      "a timestamp",
	"node":                "names a machine, service or artifact, not a person",
	"node_id":             "an identifier for a non-person entity",
	"note":                "descriptive text, not a party",
	"objective_kind":      "closed informational/effectful classification (0072), not a person",
	"outcome":             "closed succeeded/failed/uncertain receipt result (0072), not a person",
	"objective":           "names a machine, service or artifact, not a person",
	"observed_at":         "a timestamp",
	"occurred_at":         "a timestamp",
	"offboarded_at":       "a timestamp",
	"old_role":            "an enum or classification, not a party",
	"ownership":           "an enum or classification, not a party",
	"params_json":         "a serialized blob",
	"path":                "names a machine, service or artifact, not a person",
	"payload":             "a serialized blob",
	"payload_json":        "a serialized blob",
	"permission_label":    "an enum or classification, not a party",
	"plan_id":             "an identifier for a non-person entity",
	"plan_ids":            "an identifier for a non-person entity",
	"plan_json":           "a serialized blob",
	"plan_name":           "names a machine, service or artifact, not a person",
	"policy_version":      "an enum or classification, not a party",
	"probe_detail":        "descriptive text, not a party",
	"probe_mode":          "an enum or classification, not a party",
	"probed_at":           "a timestamp",
	"profile":             "an enum or classification, not a party",
	// 0058 split the Agent's ceiling out of the Person's vocabulary
	// (design §5.2). An agent class says which gateway an AGENT reaches
	// and names nobody: one Agent serves several people at once (§5.3),
	// so reading a person out of this column is the exact mistake the
	// rename exists to prevent.
	"agent_class":    "an agent's ceiling, not a party — one agent serves many people",
	"former_profile": "what an agent's class used to be spelled as, kept as migration evidence",
	"promoted_at":    "a timestamp",
	"promoted_from":  "the coordinator process that was fenced — a machine party, never a person",
	"prompt_version": "an enum or classification, not a party",
	"provider":       "names a machine, service or artifact, not a person",
	// It is the sentence a notice asked, e.g. "Delete 'Dentist for Sam'
	// from Fri 18 Sep?". It can MENTION a person, the way any free text
	// can, and that is not the same as being a reference to one: nothing
	// resolves it, joins on it, or would orphan a record if somebody
	// were renamed. Same shape as `answer` above, which is the reply to
	// it.
	"question":         "descriptive text, not a party",
	"readiness":        "an enum or classification, not a party",
	"readiness_detail": "descriptive text, not a party",
	"reason":           "descriptive text, not a party",
	"recorded_at":      "a timestamp",
	"redeemed_at":      "a timestamp",
	"released_at":      "a timestamp",
	"remote":           "names a machine, service or artifact, not a person",
	"removing_at":      "a timestamp",
	"replayed_at":      "a timestamp",
	"request_json":     "a serialized blob",
	"resolved_at":      "a timestamp",
	"result_at":        "a timestamp",
	"result_json":      "a serialized blob",
	"result_summary":   "descriptive text, not a party",
	"result_type":      "an enum or classification, not a party",
	"resume_at":        "a timestamp",
	"retention":        "an enum or classification, not a party",
	"retired_at":       "a timestamp",
	"revoked_at":       "a timestamp",
	"risk":             "an enum or classification, not a party",
	"role":             "an enum or classification, not a party",
	"rule":             "names a machine, service or artifact, not a person",
	"run_kind":         "an enum or classification, not a party",
	"scope":            "an enum or classification, not a party",
	"scope_id":         "an identifier for a non-person entity",
	"secret_ref":       "an identifier for a non-person entity",
	"seen_at":          "a timestamp",
	"session_id":       "an identifier for a non-person entity",
	"snapshot_at":      "a timestamp",
	"source":           "names a machine, service or artifact, not a person",
	"expected_body":    "queued chat content used to distinguish actual delivery from a privacy replacement; descriptive text, not a Person reference",
	"source_key":       "exact tool-step or scheduler/executor receipt identity for required-effect coverage (0072), never a Person",
	"source_id":        "an identifier for a non-person entity",
	"source_ids":       "an identifier for a non-person entity",
	"source_kind":      "an enum or classification, not a party",
	"speech":           "a chat reply to be read aloud: descriptive text, not a party",
	"started_at":       "a timestamp",
	"state":            "an enum or classification, not a party",
	"status":           "an enum or classification, not a party",
	"store":            "names a machine, service or artifact, not a person",
	"strategy":         "an enum or classification, not a party",
	"subject_id":       "an identifier for a non-person entity",
	"subject_kind":     "an enum or classification, not a party",
	"summary":          "descriptive text, not a party",
	"suppressed_at":    "a timestamp",
	"surface":          "an enum or classification, not a party",
	"taken_at":         "a timestamp",
	"target":           "names a machine, service or artifact, not a person",
	"text":             "descriptive text, not a party",
	"time":             "a timestamp",
	"timezone":         "names a machine, service or artifact, not a person",
	"to_state":         "an enum or classification, not a party",
	"to_surface":       "an enum or classification, not a party",
	"tool":             "names a machine, service or artifact, not a person",
	"tool_json":        "a serialized blob",
	"transcript_json":  "a serialized blob",
	"transport":        "names a machine, service or artifact, not a person",
	"type":             "an enum or classification, not a party",
	"uncertainty":      "an enum or classification, not a party",
	"undelivered_at":   "a timestamp",
	"updated_at":       "a timestamp",
	"used_at":          "a timestamp",
	"verified_at":      "a timestamp",
	"version":          "an enum or classification, not a party",
}

// liveColumns opens a fully migrated database and returns every
// candidate column in it.
func liveColumns(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := runtimedb.Open(path)
	if err != nil {
		t.Fatalf("runtimedb.Open: %v", err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open for inspection: %v", err)
	}
	defer raw.Close()

	tables, err := raw.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	tables.Close()
	if len(names) == 0 {
		t.Fatal("a migrated database reported no tables")
	}

	out := map[string]string{}
	for _, table := range names {
		cols, err := raw.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		for cols.Next() {
			var c, typ string
			if err := cols.Scan(&c, &typ); err != nil {
				t.Fatal(err)
			}
			out[table+"."+c] = typ
		}
		cols.Close()
	}
	return out
}

func classified() map[string]personref.Column {
	out := map[string]personref.Column{}
	for _, c := range personref.Columns {
		out[c.Table+"."+c.Column] = c
	}
	return out
}

// The point of the whole package: a column that could name a person and
// has no verdict is a column slice 2b would have to guess about.
func TestEveryCandidateColumnHasAVerdict(t *testing.T) {
	live, have := liveColumns(t), classified()
	var missing []string
	for col, typ := range live {
		// Only TEXT can hold a handle. A verdict on a non-TEXT column is
		// still allowed — butler_memory.superseded_by is kept as a
		// recorded false positive — but none is required.
		if !strings.HasPrefix(strings.ToUpper(typ), "TEXT") {
			continue
		}
		if _, ok := have[col]; ok {
			continue
		}
		name := strings.ToLower(col[strings.Index(col, ".")+1:])
		if _, ok := dismissed[name]; ok {
			continue
		}
		missing = append(missing, col)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d TEXT column(s) are neither classified nor dismissed:\n  %s\n\n"+
			"Either add it to personref.Columns with the evidence — what the write path "+
			"passes, or what the schema comment says — or, if it names no party at all, "+
			"add its NAME to dismissed with the reason. Do not guess from the name alone.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// And the converse: a verdict for a column that no longer exists is
// stale, and a stale verdict is worse than none because it reads as
// current.
func TestNoVerdictOutlivesItsColumn(t *testing.T) {
	live, have := liveColumns(t), classified()
	var gone []string
	for col := range have {
		if _, ok := live[col]; !ok {
			gone = append(gone, col)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Fatalf("%d verdict(s) name a column that is not in the schema:\n  %s",
			len(gone), strings.Join(gone, "\n  "))
	}
}

// Every verdict carries its evidence. "Why" is what a reviewer reads
// instead of re-deriving the classification, and what tells a later
// reader whether a changed write path has invalidated it.
func TestEveryVerdictCarriesItsEvidence(t *testing.T) {
	for _, c := range append(append([]personref.Column{}, personref.Columns...), personref.LegacyColumns...) {
		if strings.TrimSpace(c.Why) == "" {
			t.Errorf("%s.%s is classified %s with no evidence", c.Table, c.Column, c.Kind)
		}
		switch c.Kind {
		case personref.Human, personref.Actor, personref.Scoped, personref.NotAPerson, personref.PersonID:
		default:
			t.Errorf("%s.%s has kind %q, which is not a recognised verdict", c.Table, c.Column, c.Kind)
		}
	}
}

// Every legacy verdict names a column migration 0043 actually removed.
//
// Without this the legacy table becomes a place stale entries accumulate
// unnoticed: Survey skips whatever the schema does not have, so a column
// listed here that still exists would be surveyed TWICE — once as free
// text and once under its person_id verdict — and one that was never
// removed would sit here forever claiming to describe the past.
func TestEveryLegacyVerdictNamesAColumnThatIsGone(t *testing.T) {
	live := liveColumns(t)
	for _, c := range personref.LegacyColumns {
		if _, ok := live[c.Table+"."+c.Column]; ok {
			t.Errorf("%s.%s is listed as pre-0043 but is still in the schema", c.Table, c.Column)
		}
	}
}

func TestNoColumnIsClassifiedTwice(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range personref.Columns {
		key := c.Table + "." + c.Column
		if seen[key] {
			t.Errorf("%s has two verdicts; the later one silently wins", key)
		}
		seen[key] = true
	}
}

// The Connection identity columns name a Person, and the writer does
// not get a vote.
//
// This is the correction slice 2b had to make, and it is worth pinning
// because the wrong answer was reached by following this package's own
// rule. chat_bindings.identity carries a comment from ADR-W026 A3 —
// "principal (admin) or household member (family)" — and
// principal/chatclaim.go enforces it, requiring a machine principal for
// an admin-surface claim. Classified by its writer, the column is
// polymorphic. Classified by ADR-W033 B9 and design §8.3, which say a
// Connection admits a Person or a Group, it is a person reference and
// the writer is the defect.
//
// An ADR that names the subject outranks both the schema comment and
// the code. The evidence check is half the test: a later reader must
// not be able to demote these again without arguing with the ADR,
// because the demotion would look perfectly reasonable from the writer.
func TestConnectionIdentityColumnsNameAPerson(t *testing.T) {
	want := map[string]bool{
		"chat_bindings.identity":    true,
		"chat_threads.identity":     true,
		"chat_claim_codes.identity": true,
	}
	// The legacy table, because 0044 repointed all three in the same
	// slice that corrected them. The verdict is what made the repoint
	// right, and it has to stay readable for a database that has not
	// run 0044 yet — which is precisely a deployment still admitting a
	// principal to an Owner Agent.
	for _, c := range personref.LegacyColumns {
		key := c.Table + "." + c.Column
		if !want[key] {
			continue
		}
		if c.Kind != personref.Human {
			t.Errorf("%s is classified %s; ADR-W033 B9 admits a Person to a Connection, "+
				"and the principal its writer stores is the model W033 superseded", key, c.Kind)
		}
		if !strings.Contains(c.Why, "W033") {
			t.Errorf("%s does not cite the ADR that decides it, so the next reader "+
				"has only the writer to go on: %q", key, c.Why)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("%s has no verdict at all", key)
	}
}

// household_banner.set_by is classified Human because the portal is its
// only writer, and the portal passes the person it resolved. That is a
// property of the call sites, not of the column, and nothing else holds
// it: TestEveryCandidateColumnHasAVerdict only notices new columns. This
// reads the source so a new writer — a Console endpoint, a routine —
// has to come and argue with the verdict instead of landing a machine
// principal such as root-admin in a Human column (#243, #240).
func TestTheBannerHasNoMachineWriter(t *testing.T) {
	const keystone = "../.."
	writers := map[string]bool{"SetHouseholdBanner": true, "SetHouseholdBannerPosition": true}
	allowedFile := filepath.ToSlash(filepath.Join(keystone, "api", "handlers_portal_banner.go"))
	allowedFunc := map[string]bool{"homeBannerSet": true, "homeBannerFrame": true}
	sqlFile := filepath.ToSlash(filepath.Join(keystone, "runtimedb", "householdbanner.go"))
	classFile := filepath.ToSlash(filepath.Join(keystone, "runtimedb", "personref", "classification.go"))

	found := 0
	err := filepath.WalkDir(keystone, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || n == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		slash := filepath.ToSlash(path)
		// SQL that touches the table is confined to its one file, so a
		// routine cannot write the column without going through the two
		// methods below.
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "household_banner") &&
				slash != sqlFile && slash != classFile {
				t.Errorf("%s mentions household_banner outside %s: a second SQL writer would "+
					"bypass the Human verdict on set_by (#243, #240)", slash, sqlFile)
			}
			return true
		})
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			// Every selector naming a writer is either the callee of a
			// call (checked below) or a method value, which could be
			// invoked with anything and is refused outright.
			callees := map[*ast.SelectorExpr]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						callees[sel] = true
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !writers[sel.Sel.Name] {
					return true
				}
				found++
				if !callees[sel] {
					t.Errorf("%s: %s in %s is used as a method value; call it directly so the "+
						"writer can be checked (#243, #240)", slash, sel.Sel.Name, fn.Name.Name)
					return true
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !writers[sel.Sel.Name] {
					return true
				}
				last := call.Args[len(call.Args)-1]
				id, isIdent := last.(*ast.Ident)
				if slash != allowedFile || !allowedFunc[fn.Name.Name] || !isIdent || id.Name != "member" {
					t.Errorf("%s: %s in %s is a new writer of household_banner.set_by, or passes something "+
						"other than the portal's `member`. The column is classified Human because the portal "+
						"is its only writer; a new writer must re-argue that verdict (#243, #240)",
						slash, sel.Sel.Name, fn.Name.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking keystone: %v", err)
	}
	if found != 2 {
		t.Errorf("found %d writers of household_banner.set_by, want exactly 2 "+
			"(homeBannerSet, homeBannerFrame); if a method was renamed, update this test", found)
	}
}
