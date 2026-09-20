package butler

import (
	"context"
	"strings"
	"testing"
	"time"
)

// homeSeams adds the four Home-page readings to the ordinary family
// surface, over fakes that answer the way the real seams do: an upkeep
// list with one unresolved meter in it, a timeline of sentences, a
// per-member notification digest whose counts are NOT derived from its
// lines, and two machines of which one is unreadable.
func homeSeams(t *testing.T) HouseholdSeams {
	t.Helper()
	seams, _ := householdSeams(t)
	at := time.Date(2026, 9, 19, 8, 30, 0, 0, time.UTC)

	seams.Upkeep = func(context.Context) ([]CommitmentLine, error) {
		return []CommitmentLine{
			{Name: "truck-oil", State: "overdue", Reason: "5,400 miles since the last change", WaitsOn: "household/mika"},
			{Name: "boiler-service", State: "can't tell", Reason: "no reading since June"},
		}, nil
	}
	seams.Activity = func(_ context.Context, q ActivityLookup) ([]ActivityLine, error) {
		lines := []ActivityLine{
			{At: at, Sentence: "Sandy got home", Tone: "ok"},
			{At: at.Add(-time.Hour), Sentence: "the morning report could not be written", Tone: "failed"},
			{At: at.Add(-2 * time.Hour), Sentence: "a request was refused", Tone: "refused"},
		}
		if q.Limit > 0 && len(lines) > q.Limit {
			lines = lines[:q.Limit]
		}
		return lines, nil
	}
	seams.Notifications = func(_ context.Context, q NotificationLookup) (NotificationDigest, error) {
		if q.Member == "" {
			t.Error("a notification lookup reached the seam with no member")
		}
		return NotificationDigest{
			// Deliberately larger than the lines: the counts come from
			// their own query, and a digest that derived them from the
			// window would go quiet in exactly the household with a
			// backlog.
			Unread: 9, Waiting: 2,
			Lines: []NotificationLine{
				{At: at, Title: "A question about the calendar", Unread: true, Asks: true,
					Question: "Delete 'Dentist for Sam' from Fri 18 Sep?"},
				{At: at.Add(-3 * time.Hour), Title: "Backup finished"},
			},
		}, nil
	}
	seams.Machines = func(context.Context) ([]MachineReading, error) {
		return []MachineReading{
			{Name: "Machine 1", Connected: true, CPU: 12, CPUKnown: true, Memory: 64, MemKnown: true},
			{Name: "Machine 2", Problem: "it did not answer just now"},
		}, nil
	}
	return seams
}

func homeCall(t *testing.T, gw *HouseholdGateway, name string, input map[string]any) ToolOutcome {
	t.Helper()
	out, err := gw.Dispatch(context.Background(), name, input)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// The four readings answer, and each says the thing the card says
// rather than a shape a model has to interpret.
func TestTheHomePageReadingsAnswerInChat(t *testing.T) {
	gw := newTestHouseholdGateway("mika", homeSeams(t))

	upkeep := homeCall(t, gw, "get_upkeep", nil)
	for _, want := range []string{"truck-oil", "overdue", "5,400 miles", "household/mika", "can't tell"} {
		if !strings.Contains(upkeep.Content, want) {
			t.Errorf("get_upkeep does not mention %q: %s", want, upkeep.Content)
		}
	}

	activity := homeCall(t, gw, "get_activity", nil)
	if !strings.Contains(activity.Content, "Sandy got home") {
		t.Errorf("get_activity lost a line: %s", activity.Content)
	}

	machines := homeCall(t, gw, "get_machine_stats", nil)
	for _, want := range []string{"processor 12%", "memory 64%", "not connected"} {
		if !strings.Contains(machines.Content, want) {
			t.Errorf("get_machine_stats does not mention %q: %s", want, machines.Content)
		}
	}
}

// A refusal is not a failure. The tone is carried through rather than
// inferred, because a household sent looking for a problem that does
// not exist has been told the wrong thing by a card that was right.
func TestActivityKeepsRefusalsApartFromFailures(t *testing.T) {
	gw := newTestHouseholdGateway("mika", homeSeams(t))
	out := homeCall(t, gw, "get_activity", nil)
	if !strings.Contains(out.Content, "[failed]") || !strings.Contains(out.Content, "[refused]") {
		t.Fatalf("get_activity flattens tone: %s", out.Content)
	}
	if strings.Contains(out.Content, "[ok]") {
		t.Errorf("get_activity marks ordinary lines, which is noise: %s", out.Content)
	}
}

// "Not measured" and "idle" are different, and a spoken answer has the
// same obligation the card has: never a zero where there is no reading.
func TestAnUnreadableNumberIsNotZero(t *testing.T) {
	seams := homeSeams(t)
	seams.Machines = func(context.Context) ([]MachineReading, error) {
		return []MachineReading{{Name: "The machine", Connected: true, CPU: 40, CPUKnown: true}}, nil
	}
	gw := newTestHouseholdGateway("mika", seams)
	out := homeCall(t, gw, "get_machine_stats", nil)
	if !strings.Contains(out.Content, "memory can't read") || !strings.Contains(out.Content, "disk can't read") {
		t.Fatalf("a missing reading was not reported as missing: %s", out.Content)
	}
	if strings.Contains(out.Content, "memory 0%") || strings.Contains(out.Content, "disk 0%") {
		t.Fatalf("a missing reading was reported as zero: %s", out.Content)
	}
}

// Notifications are per person, and the person is the SESSION's. There
// is no field in the schema through which a model could name anybody
// else, and a session bound to nobody gets a refusal rather than
// everybody's.
func TestNotificationsAreTheSessionsMemberAndNobodyElses(t *testing.T) {
	seams := homeSeams(t)
	var asked []string
	seams.Notifications = func(_ context.Context, q NotificationLookup) (NotificationDigest, error) {
		asked = append(asked, q.Member)
		return NotificationDigest{Unread: 1, Lines: []NotificationLine{{Title: "Backup finished"}}}, nil
	}

	gw := newTestHouseholdGateway("mika", seams)
	out := homeCall(t, gw, "get_notifications", map[string]any{"member": "sam", "recipient": "sam"})
	if len(asked) != 1 || asked[0] != "mika" {
		t.Fatalf("the lookup was made for %v, not for the session's member", asked)
	}
	if !strings.Contains(out.Content, "mika") {
		t.Errorf("the answer does not say whose it is: %s", out.Content)
	}

	unbound := newTestHouseholdGateway("", seams)
	got := homeCall(t, unbound, "get_notifications", nil)
	if !got.Rejected {
		t.Fatalf("an unbound session read notifications: %s", got.Content)
	}
}

// The counts come from their own query and are reported as given. A
// digest that said "2 unread" because two of the lines it happened to
// read were unread is wrong in exactly the household with a backlog.
func TestNotificationCountsAreNotDerivedFromTheLines(t *testing.T) {
	gw := newTestHouseholdGateway("mika", homeSeams(t))
	out := homeCall(t, gw, "get_notifications", nil)
	if !strings.Contains(out.Content, "9 unread") || !strings.Contains(out.Content, "2 waiting") {
		t.Fatalf("the counts were not carried through: %s", out.Content)
	}
}

// A seam this deployment did not wire withholds its tool, rather than
// advertising a reading the Butler cannot take.
func TestUnwiredHomeReadingsAreWithheld(t *testing.T) {
	bare, _ := householdSeams(t)
	gw := newTestHouseholdGateway("mika", bare)
	offered := map[string]bool{}
	for _, spec := range gw.Specs() {
		offered[spec.Name] = true
	}
	for _, name := range []string{"get_upkeep", "get_activity", "get_notifications", "get_machine_stats"} {
		if offered[name] {
			t.Errorf("%s is offered on a deployment that has not wired it", name)
		}
	}

	wired := newTestHouseholdGateway("mika", homeSeams(t))
	offered = map[string]bool{}
	for _, spec := range wired.Specs() {
		offered[spec.Name] = true
	}
	for _, name := range []string{"get_upkeep", "get_activity", "get_notifications", "get_machine_stats"} {
		if !offered[name] {
			t.Errorf("%s is wired and not offered", name)
		}
	}
}

// All four are READS. A QA-only session — a model the household has not
// been shown to work — keeps every one of them, and none of them can
// close a commitment, clear a notification or touch a machine.
func TestTheHomePageReadingsAreReadsAndSurviveAQAOnlySession(t *testing.T) {
	gw := newTestHouseholdGateway("mika", homeSeams(t))
	for _, name := range []string{"get_upkeep", "get_activity", "get_notifications", "get_machine_stats"} {
		if gw.tools[name].Mutating {
			t.Errorf("%s is registered as mutating: nothing here writes", name)
		}
	}
	offered := map[string]bool{}
	for _, spec := range gw.OfferedSpecs(true) {
		offered[spec.Name] = true
	}
	for _, name := range []string{"get_upkeep", "get_activity", "get_notifications", "get_machine_stats"} {
		if !offered[name] {
			t.Errorf("%s is withheld from a read-only session, though it only reads", name)
		}
	}
}

// Text somebody else wrote carries the envelope. An activity line names
// a subject a publisher supplied, and a notification's question was
// phrased from a call's own parameters; upkeep and machine readings are
// a human-approved description and a number, and do not.
func TestOnlyTheAttackerInfluenceableReadingsAreEnveloped(t *testing.T) {
	gw := newTestHouseholdGateway("mika", homeSeams(t))
	for _, name := range []string{"get_activity", "get_notifications"} {
		if out := homeCall(t, gw, name, nil); !strings.Contains(out.Content, "<untrusted_data") {
			t.Errorf("%s is not enveloped: %s", name, out.Content)
		}
	}
	for _, name := range []string{"get_upkeep", "get_machine_stats"} {
		if out := homeCall(t, gw, name, nil); strings.Contains(out.Content, "<untrusted_data") {
			t.Errorf("%s is enveloped, which trains a model to discount readings it can trust", name)
		}
	}
}

// A group session narrows what it hears about people who are not in
// the room, exactly as the timeline read does. The gateway states the
// INTENT and the seam applies it, because which events are about a
// person is the deployment's vocabulary rather than this package's.
func TestActivityAsksForTheNarrowedReadInAGroup(t *testing.T) {
	seams := homeSeams(t)
	var asked []ActivityLookup
	seams.Activity = func(_ context.Context, q ActivityLookup) ([]ActivityLine, error) {
		asked = append(asked, q)
		return nil, nil
	}
	group := newTestHouseholdGroupGateway("mika", seams)
	homeCall(t, group, "get_activity", nil)
	if len(asked) != 1 || !asked[0].GroupSafe || asked[0].Member != "mika" {
		t.Fatalf("a group session did not ask for the narrowed read: %+v", asked)
	}

	direct := newTestHouseholdGateway("mika", seams)
	homeCall(t, direct, "get_activity", nil)
	if asked[1].GroupSafe {
		t.Fatalf("a one-to-one session asked for the group narrowing: %+v", asked[1])
	}
}

// The bounds are the gateway's, not the model's: a limit past the
// ceiling is clamped rather than passed through, so one question cannot
// turn into a report.
func TestActivityIsBounded(t *testing.T) {
	seams := homeSeams(t)
	var asked []int
	seams.Activity = func(_ context.Context, q ActivityLookup) ([]ActivityLine, error) {
		asked = append(asked, q.Limit)
		return nil, nil
	}
	gw := newTestHouseholdGateway("mika", seams)
	homeCall(t, gw, "get_activity", map[string]any{"limit": 500})
	homeCall(t, gw, "get_activity", nil)
	if asked[0] != MaxActivityLines {
		t.Errorf("limit 500 reached the seam as %d, want %d", asked[0], MaxActivityLines)
	}
	if asked[1] != defaultActivityLines {
		t.Errorf("an unspecified limit reached the seam as %d, want %d", asked[1], defaultActivityLines)
	}
}

// Joan says what a reading is a share of, and keeps "can't read" apart
// from "idle" while still naming the total.
func TestMachineReadingsSayWhatTheyAreOf(t *testing.T) {
	if got := percentOf("memory", 42, true, SizeGB(16<<30)); got != "memory 42% of 16 GB" {
		t.Errorf("known = %q", got)
	}
	if got := percentOf("memory", 0, false, SizeGB(4<<30)); got != "memory can't read (4.0 GB)" {
		t.Errorf("unknown = %q", got)
	}
	if got := percentOf("processor", 3, true, "8 cores"); got != "processor 3% of 8 cores" {
		t.Errorf("cpu = %q", got)
	}
	if got := percentOf("disk", 36, true, ""); got != "disk 36%" {
		t.Errorf("no total = %q", got)
	}
}
