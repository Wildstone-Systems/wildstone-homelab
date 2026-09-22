package e2e

// The gap register's shape, pinned (docs/audit/).
//
// Six audit slices run from independent sessions, in parallel, and
// write their findings into docs/audit/findings/ — one file each. The
// split is what stops them conflicting; this test is what stops the
// split quietly eroding. Both halves are needed, because the failure
// mode is not a merge conflict a human resolves. It is two findings
// that end up sharing an id, or a renumbering that makes a remediation
// issue's citation point at something else, and neither announces
// itself.
//
// It lives in testing/e2e rather than in keystone because it is about
// the repository rather than about the control plane, and because this
// module's unit tests always run in CI. It reaches up to the repo root
// and reads no code.
//
// It deliberately checks nothing about prose, table contents, or
// whether a finding is any good. Those are a reader's job. This checks
// the properties that break silently when six sessions write at once.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	auditDir    = "../../docs/audit"
	findingsDir = auditDir + "/findings"
	registry    = auditDir + "/gap-report.md"
)

// fileStatus is how far a slice has got. Closed, because "mostly done"
// is not a state anybody can plan against.
var fileStatuses = map[string]bool{
	"not started": true,
	"in progress": true,
	"complete":    true,
}

// findingStatuses are the four states from docs/audit/README.md. The
// distinction that matters is the last two: "missing" is a finding,
// "unverified" is a gap in the audit, and using the second to be polite
// about the first is how a report stops being worth reading.
var findingStatuses = map[string]bool{
	"implemented": true,
	"partial":     true,
	"missing":     true,
	"unverified":  true,
}

var (
	fileNameRe   = regexp.MustCompile(`^(\d+)-[a-z0-9-]+\.md$`)
	issueLineRe  = regexp.MustCompile(`(?m)^- \*\*Issue:\*\* #(\d+)\s*$`)
	statusLineRe = regexp.MustCompile(`(?m)^- \*\*Status:\*\* (.+?)\s*$`)
	findingRe    = regexp.MustCompile(`(?m)^### ~?~?(F-(\d+)-(\d+))~?~? — (.+?)\s*$`)
	// Within a finding, its own status and evidence lines.
	inlineStatusRe = regexp.MustCompile(`(?m)^- \*\*Audit status:\*\* (.+?)\s*$`)
	// Evidence values wrap. Capturing only the first line truncated one
	// mid-citation — "…; `GET" — and reported a well-evidenced finding
	// as unsupported, so continuation lines are part of the value.
	inlineEvidenceRe = regexp.MustCompile(`(?m)^- \*\*Audit evidence:\*\* (.*(?:\n[ \t]+\S.*)*)`)
)

func TestAuditRegister(t *testing.T) {
	entries, err := os.ReadDir(findingsDir)
	if err != nil {
		t.Fatalf("reading %s: %v", findingsDir, err)
	}
	index, err := os.ReadFile(registry)
	if err != nil {
		t.Fatalf("reading %s: %v", registry, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		files = append(files, e.Name())
	}
	if len(files) == 0 {
		t.Fatalf("no findings files in %s — the register cannot be empty", findingsDir)
	}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(findingsDir, name))
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			for _, p := range problemsIn(name, string(raw)) {
				t.Error(p)
			}
			// Every slice reachable from the index. A findings file
			// nobody links is one nobody reads, which is the same as
			// not having audited it.
			if !strings.Contains(string(index), "findings/"+name) {
				t.Errorf("%s is not linked from %s — an unlinked slice is an invisible one",
					name, registry)
			}
		})
	}

	// And the reverse: the index must not promise a file that is gone.
	for _, ref := range regexp.MustCompile(`findings/([0-9a-z-]+\.md)`).FindAllStringSubmatch(string(index), -1) {
		if _, err := os.Stat(filepath.Join(findingsDir, ref[1])); err != nil {
			t.Errorf("%s links findings/%s, which does not exist", registry, ref[1])
		}
	}
}

// problemsIn reports everything wrong with one findings file. It
// returns rather than fails so the lint's own teeth can be tested: a
// checker that has never been seen to reject anything is decoration.
func problemsIn(name, body string) []string {
	m := fileNameRe.FindStringSubmatch(name)
	if m == nil {
		return []string{fmt.Sprintf("filename must be <issue>-<slug>.md, got %q", name)}
	}
	fileIssue := m[1]

	var bad []string
	// The header's issue number is the one findings are checked
	// against, and it has to agree with the filename or the id scheme
	// means nothing.
	im := issueLineRe.FindStringSubmatch(body)
	if im == nil {
		return []string{`no "- **Issue:** #N" line in the header`}
	}
	if im[1] != fileIssue {
		bad = append(bad, fmt.Sprintf("header says issue #%s, filename says #%s", im[1], fileIssue))
	}
	if sm := statusLineRe.FindStringSubmatch(body); sm == nil {
		bad = append(bad, `no "- **Status:** ..." line in the header`)
	} else if !fileStatuses[sm[1]] {
		bad = append(bad, fmt.Sprintf("file status %q is not one of not started|in progress|complete", sm[1]))
	}
	return append(bad, findingProblems(body, fileIssue)...)
}

// evidential reports whether an evidence line points at something a
// reader could go and check. Deliberately loose: it is looking for the
// SHAPE of a citation, not judging whether the citation is any good.
// A path, a test name, an HTTP call, or a statement that something was
// run all pass; a sentence with none of those does not.
func evidential(s string) bool {
	// Evidence values wrap, and a citation can straddle the break —
	// "`GET" ending one line and "/api/v1/..." starting the next. Match
	// against the value as one line, or the wrap decides the verdict.
	s = strings.Join(strings.Fields(s), " ")
	for _, re := range evidenceShapes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

var evidenceShapes = []*regexp.Regexp{
	regexp.MustCompile(`\.(go|sql|ts|tsx|yaml|yml|md)\b`),     // a file
	regexp.MustCompile(`\bTest[A-Z]\w+`),                      // a test
	regexp.MustCompile(`(?i)\b(GET|POST|PUT|DELETE|PATCH) /`), // an endpoint
	regexp.MustCompile("(?i)\\b(ran|run|runtime|executed|observed|reproduced|returned|curl|go test|grep|no callers|zero callers)\\b"),
	regexp.MustCompile(`(?i)\bmigration \d`), // a migration
}

func findingProblems(body, fileIssue string) []string {
	var bad []string
	heads := findingRe.FindAllStringSubmatchIndex(body, -1)
	seen := map[string]bool{}
	highest := 0

	for i, h := range heads {
		id := body[h[2]:h[3]]
		issue := body[h[4]:h[5]]
		numText := body[h[6]:h[7]]
		title := strings.TrimSpace(body[h[8]:h[9]])

		if issue != fileIssue {
			bad = append(bad, fmt.Sprintf("%s: id names issue %s but lives in the file for %s", id, issue, fileIssue))
		}
		if seen[id] {
			bad = append(bad, fmt.Sprintf("%s: duplicate id — two findings sharing an id makes every citation of it ambiguous", id))
		}
		seen[id] = true
		if title == "" {
			bad = append(bad, fmt.Sprintf("%s: no title after the em dash", id))
		}

		// Monotonic, gaps allowed. Requiring them gapless would force a
		// renumber whenever one is withdrawn, and a renumber silently
		// repoints whatever already cited the old id.
		n, err := strconv.Atoi(numText)
		if err != nil || n < 1 {
			bad = append(bad, fmt.Sprintf("%s: %q is not a finding number", id, numText))
			continue
		}
		if n <= highest {
			bad = append(bad, fmt.Sprintf("%s: numbers must only increase (previous was %02d) — never renumber, leave gaps", id, highest))
		}
		highest = n

		// Body of this finding runs to the next finding heading, or to
		// the next section, or to the end.
		end := len(body)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		section := body[h[1]:end]
		if next := regexp.MustCompile(`(?m)^## `).FindStringIndex(section); next != nil {
			section = section[:next[0]]
		}

		if sm := inlineStatusRe.FindStringSubmatch(section); sm == nil {
			bad = append(bad, fmt.Sprintf("%s: no \"- **Audit status:** ...\" line", id))
		} else if !findingStatuses[sm[1]] {
			bad = append(bad, fmt.Sprintf("%s: status %q is not one of implemented|partial|missing|unverified", id, sm[1]))
		}
		if em := inlineEvidenceRe.FindStringSubmatch(section); em == nil {
			bad = append(bad, fmt.Sprintf("%s: no \"- **Audit evidence:** ...\" line — a claim without evidence is an opinion", id))
		} else if e := strings.TrimSpace(em[1]); e == "" {
			bad = append(bad, fmt.Sprintf("%s: empty evidence line", id))
		} else if !evidential(e) {
			// The first round produced one of these: an evidence line
			// quoting the baseline sentence the finding was about. The
			// body underneath carried three test names and the wiring,
			// so nothing was actually unsupported — but a reader
			// skimming evidence lines would have seen a requirement
			// restated as its own proof, which is the shape of a
			// finding with nothing behind it.
			bad = append(bad, fmt.Sprintf(
				"%s: evidence line cites nothing checkable (%.60q) — name a file, a test, a command or a run; "+
					"quoting the baseline restates the requirement as its own proof", id, e))
		}
	}

	// A file claiming to be finished with nothing in it is more likely
	// a session that forgot to write than a clean bill of health, so
	// say so rather than passing quietly.
	if sm := statusLineRe.FindStringSubmatch(body); sm != nil && sm[1] == "complete" && len(heads) == 0 {
		if !strings.Contains(body, "No findings.") {
			bad = append(bad, `marked complete with no findings — if that is genuinely the result, say "No findings." explicitly`)
		}
	}
	return bad
}

// The lint's own teeth.
//
// Every rule above exists because of a way six parallel sessions can
// corrupt the register without anyone noticing. A rule that has never
// been seen to reject anything is not protecting against its case, so
// each one gets an example it must catch.
func TestAuditRegisterLintCatchesMistakes(t *testing.T) {
	const header = "# X\n\n- **Issue:** #153\n- **Status:** in progress\n\n## Findings\n\n"
	good := "### F-153-01 — a real one\n- **Audit status:** missing\n- **Audit evidence:** `a/b.go:10`\n\n"

	for _, c := range []struct {
		name, file, body, want string
	}{
		{
			// The one that silently repoints a remediation issue's citation.
			name: "a duplicate id",
			file: "153-x.md", body: header + good + "### F-153-01 — same id again\n- **Audit status:** partial\n- **Audit evidence:** `c/d.go:1`\n",
			want: "duplicate id",
		},
		{
			// A renumber looks like tidying and changes what a citation means.
			name: "numbers going backwards",
			file: "153-x.md", body: header + "### F-153-04 — later\n- **Audit status:** missing\n- **Audit evidence:** `a:1`\n\n### F-153-02 — earlier\n- **Audit status:** missing\n- **Audit evidence:** `b:2`\n",
			want: "must only increase",
		},
		{
			// A finding filed under the wrong slice's namespace.
			name: "an id from another slice",
			file: "153-x.md", body: header + "### F-156-01 — wrong file\n- **Audit status:** missing\n- **Audit evidence:** `a:1`\n",
			want: "lives in the file for",
		},
		{
			name: "a claim with no evidence",
			file: "153-x.md", body: header + "### F-153-01 — unsupported\n- **Audit status:** missing\n",
			want: "a claim without evidence is an opinion",
		},
		{
			// "probably fine" is the status that makes a report useless.
			name: "a status outside the four states",
			file: "153-x.md", body: header + "### F-153-01 — vague\n- **Audit status:** probably fine\n- **Audit evidence:** `a:1`\n",
			want: "not one of implemented|partial|missing|unverified",
		},
		{
			name: "a header disagreeing with the filename",
			file: "158-x.md", body: header,
			want: "filename says #158",
		},
		{
			name: "a file status outside the closed set",
			file: "153-x.md", body: "# X\n\n- **Issue:** #153\n- **Status:** mostly done\n",
			want: "not one of not started|in progress|complete",
		},
		{
			// A session that forgot to write looks identical to a clean bill.
			name: "complete but empty",
			file: "153-x.md", body: "# X\n\n- **Issue:** #153\n- **Status:** complete\n\n## Findings\n",
			want: "marked complete with no findings",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := problemsIn(c.file, c.body)
			for _, p := range got {
				if strings.Contains(p, c.want) {
					return
				}
			}
			t.Errorf("lint let %s through; problems were %v", c.name, got)
		})
	}

	// And it must not cry wolf: the shape the templates use has to pass,
	// or six sessions will learn to ignore it.
	if bad := problemsIn("153-x.md", header+good); len(bad) != 0 {
		t.Errorf("a well-formed file was rejected: %v", bad)
	}
}
