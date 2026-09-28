package pkgtool

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
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
	t.Run("personal calendars over HTTP", func(t *testing.T) { personalCalendarsAgainstRadicale(t, python) })
}

// personalCalendarsAgainstRadicale runs the Calendar page's CalDAV
// client against a real Radicale 3.8.0 over HTTP, with the rights and
// storage module the package renders: listing a person's own calendars as
// the service account, and making one. A subtest of the discovery test so
// that CI's one Radicale run covers it.
func personalCalendarsAgainstRadicale(t *testing.T, python string) {
	dir := t.TempDir()
	for _, r := range calendarPackage(t) {
		if r.ConfigFile != nil && r.ConfigFile.Filename == "wildstone_calendars.py" {
			if err := os.WriteFile(filepath.Join(dir, r.ConfigFile.Filename), []byte(r.ConfigFile.Content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	reg := sharedcalendar.Legacy()
	rights, err := reg.Rights("radicale")
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	config := fmt.Sprintf(`[server]
hosts = %s
[auth]
type = htpasswd
htpasswd_filename = %s
htpasswd_encryption = plain
delay = 0
[rights]
type = from_file
file = %s
[storage]
type = wildstone_calendars
filesystem_folder = %s
predefined_collections = {"personal": {"C:supported-calendar-component-set": "VEVENT", "D:displayname": "My calendar", "tag": "VCALENDAR"}}
`, addr, filepath.Join(dir, "users"), filepath.Join(dir, "rights"), filepath.Join(dir, "data"))
	for name, body := range map[string]string{"calendars.json": reg.JSON(), "rights": rights, "config": config,
		"users": "sam:test-sam\nalex:test-alex\nradicale:test-service\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(python, "-m", "radicale", "--config", filepath.Join(dir, "config"))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+dir)
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	origin := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Radicale did not start:\n%s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	ctx := context.Background()
	sam := &caldav.Upstream{Username: "sam", Password: "test-sam"}
	// Sam signs in once, which is what makes their principal and first
	// calendar; alex never has.
	if _, err := sam.Calendars(ctx, origin+"/sam/"); err != nil {
		t.Fatal(err)
	}
	service := &caldav.Upstream{Username: "radicale", Password: "test-service", Writable: true}
	cals, err := service.Calendars(ctx, origin+"/sam/")
	if err != nil || len(cals) != 1 || cals[0].Segment != "personal" || cals[0].Name != "My calendar" || cals[0].URL != origin+"/sam/personal/" {
		t.Fatalf("sam's calendars = %+v, %v\n%s", cals, err, logs.String())
	}
	if err := service.MakeCalendar(ctx, origin+"/alex/work/", "Work & <shifts>", "#aa3300"); err != caldav.ErrNoPrincipal {
		t.Fatalf("a calendar for somebody with no principal: %v", err)
	}
	if err := service.MakePrincipal(ctx, origin+"/alex/"); err != nil {
		t.Fatal(err)
	}
	if err := service.MakeCalendar(ctx, origin+"/alex/work/", "Work & <shifts>", "#aa3300"); err != nil {
		t.Fatal(err)
	}
	cals, err = service.Calendars(ctx, origin+"/alex/")
	if err != nil || len(cals) != 1 || cals[0].Name != "Work & <shifts>" || cals[0].Color != "#aa3300" {
		t.Fatalf("alex's calendars = %+v, %v", cals, err)
	}
	// Sam, signed in as themselves, does not reach alex's.
	if cals, err := sam.Calendars(ctx, origin+"/alex/"); err == nil && len(cals) != 0 {
		t.Fatalf("sam listed alex's calendars: %+v", cals)
	}
	// Events written to it read back through the ordinary path.
	book := &caldav.Upstream{Username: "radicale", Password: "test-service", Writable: true, CollectionURL: origin + "/alex/work/"}
	title := "Late shift"
	if _, err := book.CreateEvent(ctx, caldav.EventWrite{Summary: &title, Start: "2026-09-21T18:00:00Z", End: "2026-09-21T22:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	events, err := book.Events(ctx, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
	if err != nil || len(events) != 1 || events[0].Summary != title {
		t.Fatalf("events = %+v, %v", events, err)
	}
}
