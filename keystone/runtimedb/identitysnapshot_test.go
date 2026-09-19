package runtimedb

// The identity migration, proven on a realistic snapshot.
//
// Gate 1 of the agent-system epic (#201) asks for two things: the person
// core and the repoint merged, AND the migration proven clean on a
// realistic snapshot. The tests beside this one prove behaviours one at
// a time — this binding refuses, that one carries, this queue is one
// Agent's. Each builds the two or three rows its own case needs.
//
// That is not the same question. A household arriving at 0043 has every
// affected table populated AT ONCE, with rows that reference each other
// across the tables being rebuilt in the same migration, and the failure
// mode this file exists for is the one no single-behaviour test can see:
// a rebuild that is correct about its own table and wrong about how it
// meets the next one.
//
// The two real defects found in this work were both of that shape, and
// both were columns a rebuild silently dropped — `hold_until` on
// chat_outbound, added by 0019, and `question` on notices, added by
// 0040 between this migration being written and it being merged. Each
// was caught by a test that happened to read the column. So the central
// assertion here is generic and not about any column in particular:
// EVERY column that existed before the migration still exists after it.
// That question does not need updating when somebody adds a column, and
// it is the question both defects failed.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// seedButlerAtOldSchema writes a Butler row the way the schema at this
// snapshot's point in history spells one: a `profile` column holding
// 'operator' or 'household'.
//
// Raw SQL rather than CreateButler, and that is the point of the test
// rather than an inconvenience. A snapshot is only realistic if it is
// built the way a real deployment of that vintage was, and CreateButler
// speaks TODAY's schema — after 0058 it writes agent_class, a column
// that does not exist yet at this point, so seeding through it would
// either fail or quietly test the migration against a database no
// household ever had.
func seedButlerAtOldSchema(t *testing.T, db *DB, name string, surface ButlerSurface, profile string) int64 {
	t.Helper()
	var model int64
	if err := db.sql.QueryRow(`SELECT seq FROM models WHERE name = ?`, "kitchen-box").Scan(&model); err != nil {
		t.Fatalf("finding the seeded model: %v", err)
	}
	stamp := now()
	if _, err := db.sql.Exec(
		`INSERT INTO butlers (name, directive, surface, profile, model, enabled,
		     created_at, created_by, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		name, "look after the house", string(surface), profile, model,
		stamp, "root-admin", stamp, "root-admin"); err != nil {
		t.Fatalf("seeding butler %s: %v", name, err)
	}
	var seq int64
	if err := db.sql.QueryRow(`SELECT seq FROM butlers WHERE name = ?`, name).Scan(&seq); err != nil {
		t.Fatalf("reading back butler %s: %v", name, err)
	}
	return seq
}

// seedButlerAddressAtOldSchema is the same story one table along:
// AddButlerAddress reads the Butler back through GetButler, which
// selects agent_class, so it cannot run against a pre-0058 database
// either.
func seedButlerAddressAtOldSchema(t *testing.T, db *DB, butler int64, transport, externalID string) {
	t.Helper()
	if _, err := db.sql.Exec(
		`INSERT INTO butler_addresses (butler, transport, external_id, label, created_at, created_by)
		 VALUES (?, ?, ?, '', ?, ?)`,
		butler, transport, externalID, now(), "root-admin"); err != nil {
		t.Fatalf("seeding address %s/%s: %v", transport, externalID, err)
	}
}

// theMigrationBefore is the last migration that ships ahead of this
// slice's three. Named once: a snapshot is only realistic if it is built
// at exactly the point a real deployment would be.
const theMigrationBefore = "migrations/0040_notice_question.sql"

// TestTheIdentityMigrationIsCleanOnARealisticSnapshot populates every
// table the three migrations rebuild, migrates, and asks the questions a
// household would ask afterwards: did it apply, is the schema still
// self-consistent, did anything silently disappear.
func TestTheIdentityMigrationIsCleanOnARealisticSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrateThrough(theMigrationBefore); err != nil {
		t.Fatalf("migrating through %s: %v", theMigrationBefore, err)
	}
	seedRealisticHousehold(t, old)

	// What the schema looked like before, so the comparison after is
	// against the real thing rather than against a list somebody
	// maintains by hand.
	before := schemaShape(t, old)
	beforeRows := rowCounts(t, old, rebuiltTables)
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("the identity migration refused a realistic household: %v", err)
	}
	defer db.Close()

	// 1. The schema is still self-consistent. Every rebuild repoints a
	//    foreign key, and three of them reference tables the SAME
	//    migration is rebuilding — so a row landing on a key that no
	//    longer resolves is the failure to look for.
	if bad := foreignKeyCheck(t, db); len(bad) > 0 {
		t.Fatalf("foreign_key_check after the migration: %v", bad)
	}

	// 2. Nothing silently disappeared. Columns the migrations
	//    DELIBERATELY drop are named here, and naming them is the point:
	//    a drop somebody meant is one line of test, and a drop nobody
	//    meant fails.
	after := schemaShape(t, db)
	for table, cols := range before {
		gone := []string{}
		for _, c := range cols {
			if !survived(table, c, after[table]) {
				gone = append(gone, c)
			}
		}
		if len(gone) > 0 {
			t.Errorf("%s lost %v — a rebuild written before those columns existed", table, gone)
		}
	}

	// 3. The rows are still there. A rebuild whose INSERT ... SELECT
	//    filters too hard loses people's history without failing, which
	//    is the quietest way this could go wrong.
	afterRows := rowCounts(t, db, rebuiltTables)
	for table, want := range beforeRows {
		if got := afterRows[table]; got != want {
			t.Errorf("%s has %d rows, had %d", table, got, want)
		}
	}

	// 4. And they landed on the right people, which is the whole
	//    purpose. Counting rows proves nothing arrived nowhere; this
	//    proves it arrived at the right person.
	assertLandedOnTheRightPeople(t, db)
}

// renamedTo is every column a migration RENAMES rather than removes,
// mapped to what replaced it.
//
// Separate from deliberatelyDropped, and stricter, because the two mean
// different things: a drop says "this fact is gone", a rename says "this
// fact is over there now". Listing a rename here is not a way of
// silencing the guard — survived() below still fails if the replacement
// is missing, so getting the new name wrong, or forgetting the column in
// one of the five tables, fails exactly as a plain drop would.
var renamedTo = map[string]string{
	// 0058: an Agent's class is not a Person's profile (design §5.2).
	"butlers.profile":                    "agent_class",
	"butler_tasks.profile":               "agent_class",
	"butler_tool_controls.profile":       "agent_class",
	"butler_tool_grants.profile":         "agent_class",
	"butler_capability_requests.profile": "agent_class",
}

// survived reports whether a column is still accounted for after a
// migration: present under its own name, renamed to a column that is
// present, or deliberately dropped.
func survived(table, col string, after []string) bool {
	if contains(after, col) {
		return true
	}
	if to, ok := renamedTo[table+"."+col]; ok {
		return contains(after, to)
	}
	return deliberatelyDropped[table+"."+col]
}

// deliberatelyDropped is every column the three migrations remove ON
// PURPOSE, with the reason. Anything not listed here that disappears is
// a defect.
var deliberatelyDropped = map[string]bool{
	// Replaced by person_id / <role>_person_id (0043). The handle is a
	// label; the identity is the key.
	"butler_access.member":           true,
	"butler_identity_links.member":   true,
	"butler_conversations.member":    true,
	"butler_handoffs.member":         true,
	"butler_memory.author":           true,
	"butler_memory.owner":            true,
	"butler_index.owner":             true,
	"butler_tasks.author":            true,
	"butler_tasks.owner":             true,
	"household_review_tokens.member": true,
	"offboarding_records.member":     true,
	"notices.recipient":              true,
	"butler_members.guardian":        true,

	// Replaced by person_id (0044): a Connection admits a Person.
	"chat_bindings.identity":    true,
	"chat_claim_codes.identity": true,
	"chat_threads.identity":     true,

	// Replaced by the Connection (0045). Both name a CLASS of Agent, so
	// both had to go: keeping either would leave a second answer to
	// "which Agent" that could disagree with the first.
	"chat_bindings.surface":      true,
	"chat_bindings.transport":    true,
	"chat_claim_codes.surface":   true,
	"chat_claim_codes.transport": true,
	"chat_threads.surface":       true,
	"chat_threads.transport":     true,
	"chat_outbound.surface":      true,
	"chat_outbound.transport":    true,
	"chat_questions.surface":     true,
	"chat_questions.transport":   true,
}

// rebuiltTables is every table the three migrations create-copy-drop-
// rename. Row counts are compared across all of them.
var rebuiltTables = []string{
	"butler_access", "butler_members", "butler_identity_links",
	"butler_conversations", "butler_handoffs", "butler_memory", "butler_index",
	"butler_tasks", "household_review_tokens", "offboarding_records", "notices",
	"chat_bindings", "chat_claim_codes", "chat_threads", "chat_outbound",
	"chat_questions",
}

// seedRealisticHousehold writes the household a deployment actually
// has: two adults and a child, an Owner Agent and a member Agent with a
// bot each, and a row in every table the migration touches — including
// the awkward ones, because those are where a rebuild goes wrong.
func seedRealisticHousehold(t *testing.T, db *DB) {
	t.Helper()
	exec := func(what, q string, args ...any) {
		t.Helper()
		if _, err := db.sql.Exec(q, args...); err != nil {
			t.Fatalf("seeding %s: %v", what, err)
		}
	}

	// Three people. mika is the Owner, sandy a Member, ari a child
	// whose guardian is mika — the guardian column is nullable after
	// the rebuild and was '' before, which is a conversion worth having
	// a row for.
	//
	// profile is the closed preset the person core introduced, not the
	// old household/operator vocabulary: an adult has none, and `child`
	// is the only member of it (decision 4 of #201).
	//
	// A child is recorded and NOT admitted — the schema says so, and it
	// is the right shape for this fixture anyway: a person on record who
	// has never been let in is a row the rebuild still has to carry.
	for _, p := range []struct {
		id, member, role, guardian string
		profile                    any
		admitted                   int
	}{
		{"per_mika", "mika", "owner", "", nil, 1},
		{"per_sandy", "sandy", "member", "", nil, 1},
		{"per_ari", "ari", "member", "mika", "child", 0},
	} {
		exec("member "+p.member,
			`INSERT INTO butler_members (person_id, member, role, profile, guardian, admitted, generation, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, 1, '2026-01-01T00:00:00Z')`,
			p.id, p.member, p.role, p.profile, p.guardian, p.admitted)
	}

	// Models, Agents and their addresses go through the Go API rather
	// than raw SQL: none of those three tables is being rebuilt, so
	// they already have the shape the package expects, and writing
	// their columns out by hand would be a second copy of a schema this
	// test has no business knowing.
	if err := db.UpsertModel(Model{
		Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatalf("seeding the model: %v", err)
	}
	for _, b := range []struct {
		name    string
		surface ButlerSurface
		profile string
	}{
		{"alfred", ButlerSurfaceAdmin, "operator"},
		{"hobson", ButlerSurfaceFamily, "household"},
	} {
		seq := seedButlerAtOldSchema(t, db, b.name, b.surface, b.profile)
		// The bot each Agent answers at, named the way a chat
		// capability is, because that name is how 0045 finds it.
		seedButlerAddressAtOldSchema(t, db, seq, "telegram", "chat-telegram-"+string(b.surface))
	}

	exec("access", `INSERT INTO butler_access (butler, member, granted_at, granted_by)
	     SELECT seq, 'sandy', '2026-01-02T00:00:00Z', 'mika' FROM butlers WHERE name = 'hobson'`)
	exec("identity link", `INSERT INTO butler_identity_links (link_type, external_id, member, generation, created_at, created_by)
	     VALUES ('chat-binding', '12345', 'sandy', 1, '2026-01-02T00:00:00Z', 'mika')`)
	exec("conversation", `INSERT INTO butler_conversations (member, created_at, subject)
	     VALUES ('sandy', '2026-01-03T00:00:00Z', 'the bins')`)
	exec("handoff", `INSERT INTO butler_handoffs (conversation, member, from_surface, to_surface, at, carried)
	     VALUES (1, 'sandy', 'family', 'admin', '2026-01-03T01:00:00Z', 'asked about the server')`)

	// Memory: one personal record and one the HOUSEHOLD owns. The
	// household one is the interesting row — 0043 stores that owner as
	// NULL rather than as a person who is not one, so a rebuild that
	// treated it like any other handle would either drop it or invent a
	// person called "household".
	exec("personal memory", `INSERT INTO butler_memory (kind, subject, content, audience, permission_label,
	         owner, source_kind, source_id, created_at, author, envelope_version)
	     VALUES ('fact', 'sandy', 'sandy does not eat fish', 'private', 'household',
	             'sandy', 'conversation', 'c1', '2026-01-04T00:00:00Z', 'sandy', 1)`)
	exec("household memory", `INSERT INTO butler_memory (kind, subject, content, audience, permission_label,
	         owner, source_kind, source_id, created_at, author, envelope_version)
	     VALUES ('fact', 'house', 'the bins go out on tuesday', 'shared', 'household',
	             'household', 'conversation', 'c2', '2026-01-04T01:00:00Z', 'mika', 1)`)
	exec("index personal", `INSERT INTO butler_index (source_kind, source_id, memory, owner, audience,
	         permission_label, content, indexed_at)
	     VALUES ('memory', '1', 1, 'sandy', 'private', 'household', 'sandy does not eat fish', '2026-01-04T00:00:00Z')`)
	exec("index household", `INSERT INTO butler_index (source_kind, source_id, memory, owner, audience,
	         permission_label, content, indexed_at)
	     VALUES ('memory', '2', 2, 'household', 'shared', 'household', 'the bins go out on tuesday', '2026-01-04T01:00:00Z')`)

	exec("task", `INSERT INTO butler_tasks (created_at, updated_at, kind, state, author, owner, surface, profile,
	         audience, objective, completion_criteria, retention, envelope_version)
	     VALUES ('2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z', 'conversation', 'pending', 'sandy', 'sandy',
	             'family', 'household', 'private', 'order more coffee', 'coffee ordered', 'conversation', 1)`)
	// routine is a foreign key to household_routines, so the token gets
	// a real routine to pin rather than a name.
	exec("routine", `INSERT INTO household_routines (name, ownership, author, revision, state, document,
	         created_at, updated_at, updated_by)
	     VALUES ('bins', 'wildstone', 'mika', 1, 'enabled', 'when: tuesday',
	             '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z', 'mika')`)
	exec("review token", `INSERT INTO household_review_tokens (hash, plan_id, routine, revision, member, created_at, created_by, expires_at)
	     SELECT 'abc123', 'plan-1', seq, 1, 'mika', '2026-01-06T00:00:00Z', 'root-admin', '2026-02-01T00:00:00Z'
	     FROM household_routines WHERE name = 'bins'`)
	exec("offboarding", `INSERT INTO offboarding_records (member, at, actor, store, affected, detail)
	     VALUES ('sandy', '2026-01-07T00:00:00Z', 'mika', 'memory', 1, 'paused')`)
	exec("notice", `INSERT INTO notices (kind, recipient, dedupe_key, created_at, expires_at, asks, question)
	     VALUES ('lock-up', 'sandy', 'lockup-1', '2026-01-08T00:00:00Z', '2099-01-01T00:00:00Z', 1,
	             'did you lock the back door?')`)

	// Chat, on both surfaces, including the rows that are NOT simply
	// live: a revoked binding (history about a model that is going
	// away), a redeemed code, a delivered message and an answered
	// question. A rebuild that only handles the live case loses these.
	exec("live family binding", `INSERT INTO chat_bindings (transport, account, surface, identity, conversation, created_at, created_by)
	     VALUES ('telegram', '12345', 'family', 'sandy', '12345', '2026-01-09T00:00:00Z', 'mika')`)
	exec("live admin binding", `INSERT INTO chat_bindings (transport, account, surface, identity, conversation, created_at, created_by)
	     VALUES ('telegram', '99999', 'admin', 'mika', '99999', '2026-01-09T01:00:00Z', 'mika')`)
	exec("revoked binding", `INSERT INTO chat_bindings (transport, account, surface, identity, created_at, created_by, revoked_at)
	     VALUES ('telegram', '55555', 'family', 'ari', '2026-01-09T02:00:00Z', 'mika', '2026-01-10T00:00:00Z')`)
	exec("open code", `INSERT INTO chat_claim_codes (hash, surface, identity, transport, created_at, created_by, expires_at)
	     VALUES ('code-open', 'family', 'ari', 'telegram', '2026-01-11T00:00:00Z', 'mika', '2099-01-01T00:00:00Z')`)
	exec("redeemed code", `INSERT INTO chat_claim_codes (hash, surface, identity, transport, created_at, created_by, expires_at, redeemed_at)
	     VALUES ('code-used', 'family', 'sandy', 'telegram', '2026-01-09T00:00:00Z', 'mika', '2099-01-01T00:00:00Z', '2026-01-09T00:05:00Z')`)
	exec("thread", `INSERT INTO chat_threads (transport, conversation, surface, identity, session_id, updated_at)
	     VALUES ('telegram', '12345', 'family', 'sandy', 'sess-1', '2026-01-12T00:00:00Z')`)
	exec("pending outbound", `INSERT INTO chat_outbound (transport, surface, target, body, kind, dedupe_key, created_at, expires_at)
	     VALUES ('telegram', 'family', '12345', 'the bins go out tonight', 'push', 'bins-1',
	             '2026-01-13T00:00:00Z', '2099-01-01T00:00:00Z')`)
	exec("delivered outbound", `INSERT INTO chat_outbound (transport, surface, target, body, kind, dedupe_key, created_at, expires_at, delivered_at)
	     VALUES ('telegram', 'family', '12345', 'morning', 'reply', 'morning-1',
	             '2026-01-13T01:00:00Z', '2099-01-01T00:00:00Z', '2026-01-13T01:00:05Z')`)
	exec("open question", `INSERT INTO chat_questions (correlation_id, surface, transport, target, rule, subject, asked_at)
	     VALUES ('corr-1', 'family', 'telegram', '12345', 'bins', 'sandy', '2026-01-13T00:00:00Z')`)
	exec("answered question", `INSERT INTO chat_questions (correlation_id, surface, transport, target, rule, subject, asked_at, answered_at)
	     VALUES ('corr-0', 'family', 'telegram', '12345', 'mail', 'sandy', '2026-01-01T00:00:00Z', '2026-01-01T00:10:00Z')`)
}

// assertLandedOnTheRightPeople spot-checks the conversions that carry an
// actual decision, rather than re-reading every row. Counting proves
// nothing was lost; these prove it was not merely kept.
func assertLandedOnTheRightPeople(t *testing.T, db *DB) {
	t.Helper()

	// A handle resolves to its person, everywhere it was a handle.
	for _, c := range []struct{ table, column, want string }{
		{"butler_access", "person_id", "per_sandy"},
		{"butler_identity_links", "person_id", "per_sandy"},
		{"butler_conversations", "person_id", "per_sandy"},
		{"butler_handoffs", "person_id", "per_sandy"},
		{"household_review_tokens", "person_id", "per_mika"},
		{"offboarding_records", "person_id", "per_sandy"},
		{"notices", "recipient_person_id", "per_sandy"},
	} {
		var got string
		if err := db.sql.QueryRow(
			fmt.Sprintf(`SELECT %s FROM %s LIMIT 1`, c.column, c.table)).Scan(&got); err != nil {
			t.Errorf("reading %s.%s: %v", c.table, c.column, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s.%s = %q, want %q", c.table, c.column, got, c.want)
		}
	}

	// The household's own record is NULL, not a person called
	// "household". This is the conversion a rebuild is most likely to
	// get subtly wrong, because "household" looks exactly like a handle.
	var householdOwner any
	if err := db.sql.QueryRow(
		`SELECT owner_person_id FROM butler_memory WHERE content LIKE 'the bins%'`).Scan(&householdOwner); err != nil {
		t.Fatalf("reading the household record: %v", err)
	}
	if householdOwner != nil {
		t.Errorf("the household's own memory is owned by %v, want NULL", householdOwner)
	}
	var personal string
	if err := db.sql.QueryRow(
		`SELECT owner_person_id FROM butler_memory WHERE content LIKE 'sandy does%'`).Scan(&personal); err != nil {
		t.Fatalf("reading sandy's record: %v", err)
	}
	if personal != "per_sandy" {
		t.Errorf("sandy's memory is owned by %q, want per_sandy", personal)
	}

	// The child's guardian is the person, and an adult's is NULL rather
	// than the '' it was.
	var guardian any
	if err := db.sql.QueryRow(
		`SELECT guardian_person_id FROM butler_members WHERE member = 'ari'`).Scan(&guardian); err != nil {
		t.Fatalf("reading ari's guardian: %v", err)
	}
	if guardian != "per_mika" {
		t.Errorf("ari's guardian is %v, want per_mika", guardian)
	}
	if err := db.sql.QueryRow(
		`SELECT guardian_person_id FROM butler_members WHERE member = 'mika'`).Scan(&guardian); err != nil {
		t.Fatalf("reading mika's guardian: %v", err)
	}
	if guardian != nil {
		t.Errorf("mika's guardian is %v, want NULL — '' was never an identity", guardian)
	}

	// Each chat row landed on the Connection of the Agent that serves
	// its old surface, which is the whole of 0045: the family rows on
	// hobson's bot, the admin one on alfred's, and no row on both.
	for _, c := range []struct{ query, want string }{
		{`SELECT b.name FROM chat_bindings x
		    JOIN butler_addresses a ON a.seq = x.connection
		    JOIN butlers b ON b.seq = a.butler WHERE x.account = '12345'`, "hobson"},
		{`SELECT b.name FROM chat_bindings x
		    JOIN butler_addresses a ON a.seq = x.connection
		    JOIN butlers b ON b.seq = a.butler WHERE x.account = '99999'`, "alfred"},
		{`SELECT b.name FROM chat_questions q
		    JOIN butler_addresses a ON a.seq = q.connection
		    JOIN butlers b ON b.seq = a.butler WHERE q.correlation_id = 'corr-1'`, "hobson"},
		{`SELECT b.name FROM chat_outbound o
		    JOIN butler_addresses a ON a.seq = o.connection
		    JOIN butlers b ON b.seq = a.butler WHERE o.dedupe_key = 'bins-1'`, "hobson"},
	} {
		var got string
		if err := db.sql.QueryRow(c.query).Scan(&got); err != nil {
			t.Errorf("resolving a connection: %v", err)
			continue
		}
		if got != c.want {
			t.Errorf("a chat row answers as %q, want %q", got, c.want)
		}
	}
}

// schemaShape is every table's column list, read from the database
// rather than declared here. A test that declared it would need editing
// whenever anybody added a column, which is exactly when it stops being
// checked.
func schemaShape(t *testing.T, db *DB) map[string][]string {
	t.Helper()
	shape := map[string][]string{}
	rows, err := db.sql.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		cols, err := db.sql.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for cols.Next() {
			var name string
			if err := cols.Scan(&name); err != nil {
				cols.Close()
				t.Fatal(err)
			}
			shape[table] = append(shape[table], name)
		}
		cols.Close()
	}
	for _, cols := range shape {
		sort.Strings(cols)
	}
	return shape
}

func rowCounts(t *testing.T, db *DB, tables []string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range tables {
		var n int
		if err := db.sql.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

// foreignKeyCheck asks SQLite whether every key still resolves. It is
// the question the migration runner asks after a rebuild, asked again
// here against a database with rows in it — the runner's own gate runs
// on whatever the migration left behind, and a migration that filtered
// its rows away passes it trivially.
func foreignKeyCheck(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.sql.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var table, parent any
		var rowid, fkid any
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Fatal(err)
		}
		bad = append(bad, fmt.Sprintf("%v row %v -> %v", table, rowid, parent))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return bad
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}
