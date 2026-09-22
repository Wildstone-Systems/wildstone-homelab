package pkgtool

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/sharedcalendar"
)

func TestSharedCalendarRadicaleDiscovery(t *testing.T) {
	python := os.Getenv("RADICALE_TEST_PYTHON")
	if python == "" {
		t.Skip("set RADICALE_TEST_PYTHON to Python with radicale==3.8.0 installed")
	}
	dir := t.TempDir()
	for _, r := range calendarPackage(t) {
		if r.ConfigFile != nil {
			if err := os.WriteFile(filepath.Join(dir, r.ConfigFile.Filename), []byte(r.ConfigFile.Content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	reg := sharedcalendar.Legacy()
	reg.Calendars = append(reg.Calendars, sharedcalendar.Calendar{ID: "school", Name: "School", Color: "#f59e0b", ReadGroups: []string{"calendar_users"}, WriteGroups: []string{"adults"}}, sharedcalendar.Calendar{ID: "private", Name: "Private shared", Color: "#444444", ReadMembers: []string{"alex"}, WriteMembers: []string{"alex"}})
	rights, err := reg.Rights("radicale")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"calendars.json": reg.JSON(), "rights": rights} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(python, "testdata/calendar_discovery.py", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Radicale: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
