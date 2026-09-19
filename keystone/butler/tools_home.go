package butler

// The family dashboard's own readings, as tools.
//
// The Home page already answers four questions for a household member
// who opens it: what needs them, what happened today, what they missed,
// and whether the machines are all right. A member in chat — or on a
// phone, or asking out loud through a chat capability — could not get
// any of them, so the Butler's honest answer to "is everything okay?"
// was to describe the page they were not looking at.
//
// WHY THESE ARE NOT HOUSEHOLD ACTIONS. The mixed rule ADR-W024 leaves
// room for is drawn on ownership: a reading that comes from an
// INSTALLED CAPABILITY is declared by that capability's package and
// dispatched through the domestic gateway, because the gateway's
// declaration check is what stops a Butler offering a household
// something it has not got. These four come from KEYSTONE — the
// commitment pass, the household timeline, the notices table, the node
// agent — and they work on a deployment with no capabilities installed
// at all. There is no package to declare them, and inventing one would
// make a core reading look optional.
//
// So they are registered the way record_meter_reading is: a seam
// cmd/keystone wires, a tool registered only when that seam exists, and
// a gateway that holds no store of its own. A deployment that has not
// wired one withholds its tool rather than advertising a reading it
// cannot take.
//
// EVERY ONE OF THEM IS A READ. Nothing here closes a commitment, clears
// a notification, or touches a machine — those are answers a person
// gives and plans a person approves, and the ceiling ADR-W035 A13 puts
// on the agent's relationship with the tracker is the same ceiling here.
//
// THE MEMBER IS THE SESSION'S, NEVER A PARAMETER. Notifications are per
// person, so the lookup is stamped with the gateway's member exactly as
// a household action is (ADR-W001). There is no field in any schema
// below in which a model could name somebody else.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

const (
	// MaxActivityLines bounds one get_activity call. A family question
	// is answered by a handful of recent lines; anything more is a
	// report, and a report is the Activity page.
	MaxActivityLines = 20
	// defaultActivityLines is what a call that names no limit gets.
	defaultActivityLines = 8
	// MaxNotificationLines bounds one get_notifications call, for the
	// reason above. The COUNTS are never bounded by it: "3 unread" must
	// not mean "3 among the ones I happened to read".
	MaxNotificationLines = 20
	// defaultNotificationLines is what a call that names no limit gets.
	defaultNotificationLines = 6
	// maxUpkeepLines bounds what one answer names individually. Past it
	// the summary gives a count, because a model handed eighty lines
	// will read eighty lines back to somebody.
	maxUpkeepLines = 30
)

// ActivityLookup is one read of the household's own timeline, rendered.
//
// It is NOT get_household_events: that tool retrieves raw facts by type
// and subject, for a model that knows what it is looking for. This one
// asks for the same stream as SENTENCES — the rendering the Activity
// page uses — because "what happened today" is a question about the
// house, not a query.
type ActivityLookup struct {
	// Limit is how many lines, newest first. 1..MaxActivityLines.
	Limit int
	// Since narrows to what happened after this moment. Zero means the
	// seam's own default window.
	Since time.Time
	// GroupSafe asks the seam to withhold lines ABOUT ANOTHER PERSON,
	// exactly as HouseholdLookup does and for the same reason: a group
	// session may hear about the house, its devices and the asker, and
	// never about the member who is not in the room.
	GroupSafe bool
	// Member is who is asking. Only meaningful with GroupSafe.
	Member string
}

// ActivityLine is one rendered row of the household's day.
type ActivityLine struct {
	At time.Time
	// Sentence is the line as the Activity page shows it — already
	// mapped from a machine verb by the closed table in keystone/portal.
	// Untrusted, because a publisher wrote the subject it names.
	Sentence string
	// Tone is "ok", "waiting", "failed" or "refused". Carried rather
	// than inferred, so a refusal is not read aloud as a failure.
	Tone string
}

// NotificationLookup is one member's own notifications.
type NotificationLookup struct {
	// Member is whose notifications these are. Stamped by the gateway.
	Member string
	// Limit is how many, unread first then newest.
	Limit int
	// UnansweredOnly narrows to questions still waiting on this member.
	UnansweredOnly bool
}

// NotificationLine is one notice as a person hears it.
type NotificationLine struct {
	At    time.Time
	Title string
	Kind  string
	// Unread is a notice the member has not cleared.
	Unread bool
	// Asks marks a notice that poses a question. Question is what it
	// asked — untrusted text, since an executor phrased it from a
	// call's own parameters.
	Asks     bool
	Question string
}

// NotificationDigest is the answer to "what did I miss".
//
// The counts are separate from the lines and are NOT derived from them.
// A digest that said "3 unread" because three of the six lines it
// happened to read were unread goes quietly wrong in exactly the
// household that has a backlog, which is the one reading it.
type NotificationDigest struct {
	Unread  int
	Waiting int
	Lines   []NotificationLine
}

// MachineReading is one machine, as the Home page's card sees it.
//
// Each percentage has its own Known flag rather than a sentinel,
// because the three fail independently — a kernel can publish CPU and
// not available memory. Zero is "not measured", never "idle", and a
// tool that read it the other way would tell a household their server
// was resting while it was actually unreadable.
type MachineReading struct {
	Name      string
	Connected bool
	CPU       int
	CPUKnown  bool
	Memory    int
	MemKnown  bool
	Disk      int
	DiskKnown bool
	// Problem is why this machine could not be read, when it could not.
	Problem string
}

// --- upkeep ---

var upkeepSpec = provider.Tool{
	Name: "get_upkeep",
	Description: "Where the household's upkeep stands right now: what is due, what is overdue, " +
		"what is booked, and which numbers nobody can currently read. This is the \"needs you\" " +
		"card on the Home page. Use it for \"what's coming up\", \"is anything overdue\", " +
		"\"when is the truck due\". It records nothing and changes nothing: you cannot mark " +
		"anything done here, and you must not tell anyone you have.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

func handleGetUpkeep(ctx context.Context, gw *HouseholdGateway, _ map[string]any) (ToolOutcome, error) {
	lines, err := gw.seams.Upkeep(ctx)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("reading the household's upkeep: %w", err)
	}
	if len(lines) == 0 {
		return ToolOutcome{Content: "Nothing is being tracked yet."}, nil
	}
	var b strings.Builder
	b.WriteString("Where the household's upkeep stands:\n\n")
	shown := lines
	if len(shown) > maxUpkeepLines {
		shown = shown[:maxUpkeepLines]
	}
	for _, l := range shown {
		fmt.Fprintf(&b, "  %-28s %-12s %s", l.Name, l.State, l.Reason)
		if l.WaitsOn != "" {
			fmt.Fprintf(&b, " (%s answers this one)", l.WaitsOn)
		}
		b.WriteString("\n")
	}
	if len(lines) > len(shown) {
		fmt.Fprintf(&b, "\n...and %d more. The Upkeep page has the whole list.\n", len(lines)-len(shown))
	}
	b.WriteString(`
"can't tell" means a meter has not been read recently enough to say. It is
not the same as "not due": the honest answer is to ask somebody for the
number, not to reassure them.
`)
	return ToolOutcome{Content: b.String()}, nil
}

// --- activity ---

var activitySpec = provider.Tool{
	Name: "get_activity",
	Description: "What has happened around the house lately, as plain sentences — the same " +
		"\"today at home\" list the Home page shows. Use it for \"what's been going on\", " +
		"\"did anything happen while I was out\", \"is everything all right\". This is what was " +
		"RECORDED, not what is planned: appointments are the calendar. For a specific fact — one " +
		"event type, one person or device — get_household_events is the narrower tool and the " +
		"better one.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many lines, newest first. 1 to %d, default %d.",
					MaxActivityLines, defaultActivityLines),
			},
			"hours": map[string]any{
				"type": "integer",
				"description": "Only what happened in the last this-many hours. Leave it out for " +
					"the household's usual window.",
			},
		},
	},
}

func handleGetActivity(ctx context.Context, gw *HouseholdGateway, input map[string]any) (ToolOutcome, error) {
	limit := intInput(input, "limit")
	if limit <= 0 {
		limit = defaultActivityLines
	}
	if limit > MaxActivityLines {
		limit = MaxActivityLines
	}
	q := ActivityLookup{Limit: limit, GroupSafe: gw.group, Member: gw.member}
	if hours := intInput(input, "hours"); hours > 0 {
		q.Since = time.Now().Add(-time.Duration(hours) * time.Hour)
	}
	lines, err := gw.seams.Activity(ctx, q)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("reading what happened: %w", err)
	}
	if len(lines) == 0 {
		return ToolOutcome{Content: "Nothing has been recorded in that window. A quiet house is the ordinary case."}, nil
	}
	var b strings.Builder
	b.WriteString("What has happened around the house, newest first:\n\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "  %s  %s", l.At.Format("Mon 15:04"), l.Sentence)
		// The tone is carried rather than inferred so that a refusal is
		// not read aloud as a failure: nothing broke, nobody is
		// blocked, and the house did what it was configured to do.
		if l.Tone != "" && l.Tone != "ok" {
			fmt.Fprintf(&b, " [%s]", l.Tone)
		}
		b.WriteString("\n")
	}
	return ToolOutcome{Content: b.String()}, nil
}

// --- notifications ---

var notificationsSpec = provider.Tool{
	Name: "get_notifications",
	Description: "What the house has told THIS person lately, and what it is still waiting for " +
		"them to answer. Use it for \"did I miss anything\", \"what's waiting on me\", \"anything " +
		"I need to answer\". It is always about the person you are talking to — you cannot read " +
		"anybody else's — and it clears nothing: reading a notification here does not mark it " +
		"read, and answering one is something they do, not you.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many, unread first then newest. 1 to %d, default %d.",
					MaxNotificationLines, defaultNotificationLines),
			},
			"waitingOnly": map[string]any{
				"type":        "boolean",
				"description": "Only the questions still waiting on an answer.",
			},
		},
	},
}

func handleGetNotifications(ctx context.Context, gw *HouseholdGateway, input map[string]any) (ToolOutcome, error) {
	if gw.member == "" {
		// The same backstop the action and reading tools carry: a
		// per-person read that names nobody is not a per-person read,
		// and defaulting to "everybody's" would be the disclosure this
		// surface exists to prevent.
		return ToolOutcome{
			Content:  "This session is not bound to a household member, so there are no notifications to show.",
			Rejected: true,
		}, nil
	}
	limit := intInput(input, "limit")
	if limit <= 0 {
		limit = defaultNotificationLines
	}
	if limit > MaxNotificationLines {
		limit = MaxNotificationLines
	}
	digest, err := gw.seams.Notifications(ctx, NotificationLookup{
		Member:         gw.member,
		Limit:          limit,
		UnansweredOnly: boolInput(input, "waitingOnly"),
	})
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("reading notifications: %w", err)
	}
	var b strings.Builder
	switch {
	case digest.Unread == 0 && digest.Waiting == 0 && len(digest.Lines) == 0:
		return ToolOutcome{Content: "Nothing is waiting for " + gw.member + " — no unread notifications and no questions to answer."}, nil
	default:
		fmt.Fprintf(&b, "For %s: %d unread, %d waiting on an answer.\n\n", gw.member, digest.Unread, digest.Waiting)
	}
	for _, l := range digest.Lines {
		mark := " "
		if l.Unread {
			mark = "*"
		}
		fmt.Fprintf(&b, " %s %s  %s", mark, l.At.Format("Mon 15:04"), l.Title)
		if l.Asks && l.Question != "" {
			fmt.Fprintf(&b, " — asks: %s", l.Question)
		} else if l.Asks {
			b.WriteString(" — waiting on an answer")
		}
		b.WriteString("\n")
	}
	if digest.Waiting > 0 {
		b.WriteString("\nAnswering is theirs to do, on the Home page or wherever the question reached " +
			"them. You cannot answer one for them.\n")
	}
	return ToolOutcome{Content: b.String()}, nil
}

// --- machines ---

var machineStatsSpec = provider.Tool{
	Name: "get_machine_stats",
	Description: "How the household's machines are doing right now: processor, memory and disk, " +
		"as percentages. This is the \"machines\" card on the Home page. Use it for \"is the " +
		"server okay\", \"why is everything slow\", \"is the disk filling up\". A reading that " +
		"is missing is missing, not zero — say \"can't read that one\" rather than \"idle\". You " +
		"cannot restart, free space, or change anything here: that is a plan, and plans are " +
		"approved by the person who runs this house.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

func handleGetMachineStats(ctx context.Context, gw *HouseholdGateway, _ map[string]any) (ToolOutcome, error) {
	machines, err := gw.seams.Machines(ctx)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("reading the machines: %w", err)
	}
	if len(machines) == 0 {
		return ToolOutcome{Content: "No machines are connected to this control plane right now."}, nil
	}
	var b strings.Builder
	b.WriteString("The household's machines:\n\n")
	for _, m := range machines {
		fmt.Fprintf(&b, "  %s: ", m.Name)
		switch {
		case !m.Connected:
			b.WriteString("not connected")
			if m.Problem != "" {
				fmt.Fprintf(&b, " (%s)", m.Problem)
			}
		default:
			parts := []string{
				percentOrUnknown("processor", m.CPU, m.CPUKnown),
				percentOrUnknown("memory", m.Memory, m.MemKnown),
				percentOrUnknown("disk", m.Disk, m.DiskKnown),
			}
			b.WriteString(strings.Join(parts, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n\"can't read\" means the machine did not report that number, which is not the " +
		"same as it being at zero.\n")
	return ToolOutcome{Content: b.String()}, nil
}

// percentOrUnknown is the line that keeps "not measured" and "idle"
// apart. §7 of the Home page design is explicit about it for the card,
// and a spoken answer has the same obligation.
func percentOrUnknown(what string, v int, known bool) string {
	if !known {
		return what + " can't read"
	}
	return fmt.Sprintf("%s %d%%", what, v)
}

// boolInput reads a JSON boolean, which a provider may hand over as a
// bool or as a string depending on the model. Anything else is false —
// a narrowing flag that cannot be read is safest left off, since the
// unnarrowed answer is the larger, honest one.
func boolInput(input map[string]any, key string) bool {
	switch v := input[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}
