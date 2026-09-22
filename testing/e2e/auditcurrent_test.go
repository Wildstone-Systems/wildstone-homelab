package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	currentStatusRe   = regexp.MustCompile(`(?m)^- \*\*Current status:\*\* (implemented|open|superseded)\s*$`)
	currentEvidenceRe = regexp.MustCompile(`(?m)^- \*\*Current evidence:\*\* (.+)$`)
	currentOwnerRe    = regexp.MustCompile(`(?m)^- \*\*Current owner:\*\* ([^\r\n]+)$`)
	currentTrackingRe = regexp.MustCompile(`(?m)^- \*\*Tracking:\*\* (#[0-9]+)\s*$`)
	currentDesignRe   = regexp.MustCompile(`(?m)^- \*\*Superseded by:\*\* (.+)$`)
)

// Every historical finding must have a fresh, checkable disposition.
// A missing line fails the test even if the old audit status was accurate
// when written: that is the stale-register failure slice 24 exists to stop.
func TestEveryFindingHasACurrentStatus(t *testing.T) {
	entries, err := os.ReadDir(findingsDir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	states := map[string]int{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(findingsDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		headings := findingRe.FindAllStringSubmatchIndex(body, -1)
		for i, heading := range headings {
			id := body[heading[2]:heading[3]]
			end := len(body)
			if i+1 < len(headings) {
				end = headings[i+1][0]
			}
			section := body[heading[1]:end]
			count++
			t.Run(id, func(t *testing.T) {
				if !inlineStatusRe.MatchString(section) || !inlineEvidenceRe.MatchString(section) {
					t.Fatal("historical audit status and evidence must remain beside the current disposition")
				}
				matches := currentStatusRe.FindAllStringSubmatch(section, -1)
				if len(matches) != 1 {
					t.Fatalf("expected exactly one current disposition, got %d", len(matches))
				}
				states[matches[0][1]]++
				evidence := currentEvidenceRe.FindAllStringSubmatch(section, -1)
				if len(evidence) != 1 || len(strings.TrimSpace(evidence[0][1])) < 15 {
					t.Fatal("current disposition needs concrete evidence")
				}
				switch matches[0][1] {
				case "implemented":
					if !regexp.MustCompile(`[0-9a-f]{7,40}`).MatchString(evidence[0][1]) || !strings.Contains(evidence[0][1], "Test") {
						t.Fatal("implemented needs a commit and named test")
					}
				case "open":
					if !currentOwnerRe.MatchString(section) || !currentTrackingRe.MatchString(section) {
						t.Fatal("open finding needs named owner and tracking issue")
					}
				case "superseded":
					if !currentDesignRe.MatchString(section) {
						t.Fatal("superseded finding needs the design section")
					}
				}
			})
		}
	}
	if count != 121 {
		t.Fatal(fmt.Sprintf("expected 121 stable finding ids; got %d", count))
	}
	register, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	for state, actual := range states {
		row := fmt.Sprintf("| %s | %d |", state, actual)
		if !strings.Contains(string(register), row) {
			t.Errorf("gap report current count is stale: want %s", row)
		}
	}
}

func TestCurrentStatusGateRejectsStaleFinding(t *testing.T) {
	stale := "### F-153-01 — old claim\n- **Audit status:** missing\n- **Audit evidence:** `old.go`\n"
	if currentStatusRe.MatchString(stale) {
		t.Fatal("historical status was mistaken for a current disposition")
	}
}
