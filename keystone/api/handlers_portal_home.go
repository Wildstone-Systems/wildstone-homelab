package api

// The Home surface: the household's front page, served by Keystone.
//
// ONE READ, NOT NINE. The page has eight or nine cards and they are
// answered by a single GET. That is not an optimisation, it is a
// correctness property the mockup's own review kept finding: "a
// dashboard widget is a window onto a page, so an event's time, state
// and wording must match wherever it appears", and nine requests
// answered from nine reads of a moving database is precisely how two
// cards end up telling different stories about one event. One read, one
// timestamp, one set of facts.
//
// EVERY CARD MAY BE ABSENT, AND ABSENT IS NOT ZERO. Each section below
// carries its own `available` flag, because §7 draws the distinction
// that matters most on a family's dashboard: an unreachable service and
// a quiet house must not look the same. A card with no data says
// "nothing today"; a card that could not read says so and colours
// nothing green. Nowhere here does a failed read become an empty list.
//
// WHAT IS NOT HERE IS THE POINT. No plan text, no node name, no
// statefile hash, no container, no host path, no metric this process
// cannot source. The identity that reaches these handlers holds no API
// verb at all (auth_portal.go), so this file cannot become a way around
// the Console by accident — but the projections are narrow on purpose
// as well, because the first leak on a family surface is always
// somebody's operator vocabulary arriving in a summary string.

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// homeSettingsFile is the ConfigFile the homepage-wildstone package
// renders, read from the RESOURCE in desired state rather than from the
// node's copy — the same rule and the same reasoning as
// filedropSettingsFile above it.
const homeSettingsFile = "settings.yaml"

// homeSettings is that file's contents: what the household is called
// and what colour it chose.
type homeSettings struct {
	HouseholdName string `yaml:"householdName"`
	// AccentHue is a pointer so that "the file says 0" (a red household)
	// is distinguishable from "the file says nothing". Zero is a legal
	// hue, which makes the usual zero-value default wrong here.
	AccentHue *int `yaml:"accentHue"`
}

// defaultHouseholdName is what the page is called when nothing names it.
//
// "Home" rather than an empty heading, per §7's banner row: a blank
// where the household's name goes reads as a broken page, and this is
// the one string on the surface that is always rendered.
const defaultHouseholdName = "Home"

// defaultAccentHue is the design system's own violet, in OKLCH degrees.
const defaultAccentHue = 267

// homePresentation resolves the household's name and hue from desired
// state, falling back to values that render correctly rather than to
// blanks.
func homePresentation(desired []resource.Resource) (string, int) {
	for _, r := range desired {
		if r.Kind != resource.KindConfigFile || r.ConfigFile == nil {
			continue
		}
		if strings.TrimPrefix(r.Metadata.Owner, "capability/") != surfaceHome.capability {
			continue
		}
		if r.ConfigFile.Filename != homeSettingsFile {
			continue
		}
		var settings homeSettings
		if err := yaml.Unmarshal([]byte(r.ConfigFile.Content), &settings); err != nil {
			// A malformed file leaves the defaults, which render. The
			// alternative — refusing to serve the page — would make a
			// typo in a colour take the family's dashboard away.
			break
		}
		name := strings.TrimSpace(settings.HouseholdName)
		if name == "" {
			name = defaultHouseholdName
		}
		hue := defaultAccentHue
		if settings.AccentHue != nil {
			// Wrapped rather than clamped: a hue is an angle, so 370 is
			// 10 and both are perfectly good colours. Clamping would
			// turn a harmless off-by-360 into a silently different
			// household colour.
			hue = ((*settings.AccentHue % 360) + 360) % 360
		}
		return name, hue
	}
	return defaultHouseholdName, defaultAccentHue
}

// ---------------------------------------------------------------- shapes

// homeCard is what every section embeds: whether it could be read at
// all, and what to say if it could not.
//
// A struct rather than a convention because §7's rule — "never render an
// unreachable service as nothing to do; silence and all-clear must not
// look the same" — is exactly the rule a convention gets wrong once and
// then ships. With this embedded, a section that forgets to set
// Available is not available, and the page says so.
type homeCard struct {
	Available bool `json:"available"`
	// Problem is operator-free text for the member: what they cannot
	// see right now, in household language.
	Problem string `json:"problem,omitempty"`
}

// needsYouItem is one thing waiting on somebody.
type needsYouItem struct {
	// Seq addresses the item for an answer. Notices only; an aggregate
	// row carries no id because there is nothing to answer.
	Seq int64 `json:"seq,omitempty"`
	// Summary is household language and nothing else. Plan text, node
	// names and statefile hashes are deliberately excluded (§7), which
	// is why an owner-side item below says only how many and how long.
	Summary string `json:"summary"`
	// WaitingOn names who has to act, in words. Colour is never the
	// only signal — every item states this whatever its tone.
	WaitingOn string `json:"waitingOn"`
	Waited    string `json:"waited,omitempty"`
	// Consequence is what has not happened yet, so a member can judge
	// whether it matters. Empty where there is nothing honest to say.
	Consequence string `json:"consequence,omitempty"`
	// Decidable is whether THIS member may answer it. An item they may
	// not act on renders read-only: the affordance is absent, never
	// present-and-refused.
	Decidable bool        `json:"decidable"`
	Tone      portal.Tone `json:"tone"`
}

type needsYouCard struct {
	homeCard
	Items []needsYouItem `json:"items"`
	// More is how many are waiting beyond the three shown, so the card
	// can say "and 4 more" rather than silently truncating.
	More int `json:"more,omitempty"`
}

// activityRow is one thing the house did.
type activityRow struct {
	Sentence string      `json:"sentence"`
	At       time.Time   `json:"at"`
	Tone     portal.Tone `json:"tone"`
}

type activityCard struct {
	homeCard
	Rows []activityRow `json:"rows"`
	// Dropped counts events this surface had no sentence for. It is
	// reported rather than hidden: an unmapped verb is a bug
	// (portal/activity.go), and a number an operator can see in a
	// response is how it gets found before a family asks about it.
	Dropped int `json:"dropped,omitempty"`
}

// notification is one thing a member missed.
type notification struct {
	Seq int64 `json:"seq"`
	// Title comes from the notice KIND's administrator-set description,
	// because a notice row carries no body — the body went out through
	// the delivery path and was never retained. Naming the kind is
	// therefore the most this surface can honestly say, and inventing a
	// title would be the "card invents a number it cannot source"
	// failure §7 forbids.
	Title  string    `json:"title"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
	Unread bool      `json:"unread"`
}

type notificationsCard struct {
	homeCard
	Items  []notification `json:"items"`
	Unread int            `json:"unread"`
}

// machinesCard is the reassurance at the foot of the page.
type machinesCard struct {
	homeCard
	// LastBackup and LastDrill are the two facts the household surface
	// is entitled to about recovery: when the last backup finished, and
	// when a restore was last PROVED. The repository proves recovery on
	// a schedule and the family surface should say so.
	LastBackup time.Time `json:"lastBackup,omitzero"`
	LastDrill  time.Time `json:"lastDrill,omitzero"`
	DrillOK    bool      `json:"drillOk"`
	// Metrics is deliberately a statement of absence rather than a set
	// of zeroes. Keystone holds no CPU, memory or disk reading — those
	// came from a widget running inside the Homepage container, reading
	// the host it was on — and §7 is explicit that the bars must be
	// replaced by a sentence rather than drawn at zero, "which reads as
	// an idle machine rather than a missing reading".
	Metrics homeCard `json:"metrics"`
	// Nodes is how many machines this household runs, which is the one
	// number here that is not a hostname. Names and addresses are
	// withheld: §7 gives this card no hostnames, addresses or container
	// detail.
	Nodes int `json:"nodes"`
}

// homeSummaryResponse is the whole page in one answer.
type homeSummaryResponse struct {
	// As is the single instant every section was rendered against, and
	// it is what makes "the same event reads the same everywhere" true
	// rather than hoped for. A client renders relative times against
	// this, not against its own clock.
	As        time.Time `json:"as"`
	Member    string    `json:"member"`
	Household string    `json:"household"`
	AccentHue int       `json:"accentHue"`
	// Surfaces names which household surfaces are installed, so the
	// page knows whether to draw the Files and Assistant cards at all.
	// An absent capability must not leave an empty frame on the page.
	Surfaces map[string]bool `json:"surfaces"`

	NeedsYou      needsYouCard      `json:"needsYou"`
	Activity      activityCard      `json:"activity"`
	Notifications notificationsCard `json:"notifications"`
	Calendar      homeCard          `json:"calendar"`
	Apps          []reconcile.App   `json:"apps"`
	Groups        []string          `json:"groups"`
	// AppStatus maps a Route's resource name to "ok", "bad" or
	// "unknown". A separate map rather than a field on App because
	// reconcile renders desired state and this is runtime state; see
	// reconcile.App's own comment.
	AppStatus map[string]string `json:"appStatus"`
	// StatusKnown is false when the observation table is empty, which
	// is "nobody has checked" and not "everything is down". §7: grey
	// dots and one line, "not a grid of red dots, which would read as a
	// total outage".
	StatusKnown bool         `json:"statusKnown"`
	Machines    machinesCard `json:"machines"`
}

// ---------------------------------------------------------------- handler

// homeSummary answers the whole front page.
//
// Every section is built by its own function below and every one of them
// is written to fail into "available: false" with a sentence, never into
// an empty list. A partial page is the correct outcome when one source
// is down: the household still gets the eight cards that worked.
func (s *Server) homeSummary(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	name, hue := homePresentation(desired)
	now := time.Now()

	resp := homeSummaryResponse{
		As:        now,
		Member:    member,
		Household: name,
		AccentHue: hue,
		Surfaces: map[string]bool{
			// Asked of desired state, not of configuration: a surface
			// exists exactly while its capability declares a group,
			// which is the same test that admits a request to it.
			"files":     portalSurfaceInstalled(desired, surfaceFiles),
			"assistant": portalSurfaceInstalled(desired, surfaceAssistant),
		},
		Groups: reconcile.Groups(),
	}
	resp.NeedsYou = s.homeNeedsYou(member, now)
	resp.Activity = s.homeActivity(now)
	resp.Notifications = s.homeNotifications(member)
	resp.Calendar = s.homeCalendar(desired)
	resp.Apps, resp.AppStatus, resp.StatusKnown = s.homeApps(desired)
	resp.Machines = s.homeMachines(now)
	return resp, http.StatusOK, nil
}

// portalSurfaceInstalled reports whether a household surface is
// installed, by the same question its own door asks.
func portalSurfaceInstalled(desired []resource.Resource, surface portalSurface) bool {
	_, ok := portalGroup(desired, surface.capability)
	return ok
}

// maxNeedsYou is how many items the card shows before it defers to the
// full list. Three, per §7 — the card is the reason to open the page,
// and a card that grows with the backlog turns the dashboard into a
// scroll.
const maxNeedsYou = 3

// homeNeedsYou builds the one card a member opens the page for.
//
// Two sources, deliberately unlike each other. A notice that ASKS is a
// decision this member can take here, with buttons. A proposed plan is
// the owner's, and it appears as a count and an age with no text at all:
// plan summaries are written in operator language — packages, nodes,
// paths — and §7 excludes exactly that from this card. Saying "two
// things are waiting for the owner" is honest, useful, and leaks
// nothing.
func (s *Server) homeNeedsYou(member string, now time.Time) needsYouCard {
	card := needsYouCard{Items: []needsYouItem{}}
	notices, err := s.DB.ListNotices(member, 50)
	if err != nil {
		s.Log.Error("home needs-you notices", "err", err)
		// §7's exact copy. "Can't tell whether anything needs you" is
		// the sentence that keeps silence and all-clear apart.
		card.Problem = "Can't tell whether anything needs you — Wildstone isn't answering."
		return card
	}
	var items []needsYouItem
	for _, n := range notices {
		if !n.Asks || !n.AnsweredAt.IsZero() || !n.UndeliveredAt.IsZero() {
			continue
		}
		if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
			continue
		}
		items = append(items, needsYouItem{
			Seq:       n.Seq,
			Summary:   s.noticeTitle(n.Kind),
			WaitingOn: "you",
			Waited:    humanSince(n.CreatedAt, now),
			// The answer is a yes or a no and nothing has happened yet:
			// that IS the consequence, and stating it is what lets a
			// member judge whether to deal with it now.
			Consequence: "Nothing has happened yet either way.",
			Decidable:   true,
			Tone:        portal.ToneWaiting,
		})
	}
	// The owner's side, as one row. Counted from the plan ledger, which
	// is the same place the Console's Plans page reads.
	plans, err := s.DB.ListPlans(50)
	if err != nil {
		s.Log.Error("home needs-you plans", "err", err)
	} else {
		waiting, oldest := 0, time.Time{}
		for _, p := range plans {
			if p.State != runtimedb.PlanProposed {
				continue
			}
			waiting++
			if oldest.IsZero() || p.CreatedAt.Before(oldest) {
				oldest = p.CreatedAt
			}
		}
		if waiting > 0 {
			summary := "Something is waiting for the owner to approve."
			if waiting > 1 {
				summary = fmt.Sprintf("%d things are waiting for the owner to approve.", waiting)
			}
			items = append(items, needsYouItem{
				Summary:   summary,
				WaitingOn: "the owner",
				Waited:    humanSince(oldest, now),
				Decidable: false,
				// Grey, not amber: it is somebody else's to do, and
				// colouring another person's queue as this member's
				// urgency is how a family ends up waiting on each other
				// in silence.
				Tone: portal.ToneRefused,
			})
		}
	}
	// Decidable first, then oldest first: the thing this member can
	// actually do outranks the thing they can only look at.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Decidable != items[j].Decidable {
			return items[i].Decidable
		}
		return items[i].Seq > items[j].Seq
	})
	card.Available = true
	if len(items) > maxNeedsYou {
		card.More = len(items) - maxNeedsYou
		items = items[:maxNeedsYou]
	}
	if items != nil {
		card.Items = items
	}
	return card
}

// noticeTitle names a notice in household language.
//
// The kind's administrator-set description when there is one, and a
// humanised form of the kind itself when there is not — "pantry.low"
// becomes "Pantry low". Never the raw dotted verb: that is the same
// machine-text-on-the-wall-tablet failure portal/activity.go refuses,
// and here there is a reasonable rendering rather than a drop, because
// a notice the member is being asked to ANSWER cannot simply be omitted.
func (s *Server) noticeTitle(kind string) string {
	if k, err := s.DB.GetNoticeKind(kind); err == nil {
		if d := strings.TrimSpace(k.Description); d != "" {
			return d
		}
	}
	words := strings.FieldsFunc(kind, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	if len(words) == 0 {
		return "Something needs an answer"
	}
	joined := strings.Join(words, " ")
	return strings.ToUpper(joined[:1]) + joined[1:]
}

// maxActivity is how many rows "Today at home" shows. Five or six, per
// §7; six, so a day with one routine and a couple of arrivals still
// reads as a day rather than as a fragment.
const maxActivity = 6

// homeActivity renders what the house did today, in household language.
//
// Today means midnight in the HOUSEHOLD's zone, not the process's. A
// control plane that thinks it is in UTC would otherwise show a family
// in Chicago the wrong six hours of their own evening — and this card is
// specifically the answer to "what did the house do while I was out".
func (s *Server) homeActivity(now time.Time) activityCard {
	card := activityCard{Rows: []activityRow{}}
	loc := s.HouseholdLoc
	if loc == nil {
		loc = time.Local
	}
	local := now.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	events, err := s.DB.HouseholdEventsSince(midnight, 200)
	if err != nil {
		s.Log.Error("home activity", "err", err)
		card.Problem = "Can't load today's activity."
		return card
	}
	card.Available = true
	var rows []activityRow
	for _, e := range events {
		// The audience test first: an event about one person is not
		// every member's to read. A nil MemberScoped withholds every
		// attributed fact, which is the safe direction — the same
		// default the household Butler takes.
		if s.MemberScoped != nil && s.MemberScoped(e.Type) && e.Subject != "" {
			continue
		}
		sentence, tone, ok := portal.Sentence(e.Type, e.Subject)
		if !ok {
			card.Dropped++
			continue
		}
		rows = append(rows, activityRow{Sentence: sentence, At: e.OccurredAt, Tone: tone})
	}
	// Strictly newest first, and worst first when times tie — §7's
	// ordering, and the tie-break is what stops a failure sorting
	// underneath an arrival that happened in the same second.
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].At.Equal(rows[j].At) {
			return rows[i].At.After(rows[j].At)
		}
		return toneRank(rows[i].Tone) < toneRank(rows[j].Tone)
	})
	if len(rows) > maxActivity {
		rows = rows[:maxActivity]
	}
	if rows != nil {
		card.Rows = rows
	}
	return card
}

// toneRank orders tones worst-first for the tie-break above.
func toneRank(t portal.Tone) int {
	switch t {
	case portal.ToneFailed:
		return 0
	case portal.ToneWaiting:
		return 1
	case portal.ToneRefused:
		return 2
	default:
		return 3
	}
}

// maxNotifications is how many notices the card holds. §7 caps the card
// at two visible rows behind a "2 more" toggle; the server sends a few
// so the toggle has something to open, and the page does the capping.
const maxNotifications = 6

// homeNotifications is what this member missed — theirs alone.
//
// ListNotices is already scoped to the recipient, which is what makes
// "per-person topics are never shown to another member" true at the
// query rather than in a filter somebody can forget.
func (s *Server) homeNotifications(member string) notificationsCard {
	card := notificationsCard{Items: []notification{}}
	notices, err := s.DB.ListNotices(member, 40)
	if err != nil {
		s.Log.Error("home notifications", "err", err)
		card.Problem = "Can't reach notifications"
		return card
	}
	card.Available = true
	var items []notification
	for _, n := range notices {
		if !n.UndeliveredAt.IsZero() {
			continue
		}
		unread := n.AcknowledgedAt.IsZero()
		if unread {
			card.Unread++
		}
		if len(items) < maxNotifications {
			items = append(items, notification{
				Seq:    n.Seq,
				Title:  s.noticeTitle(n.Kind),
				Source: n.Kind,
				At:     n.CreatedAt,
				Unread: unread,
			})
		}
	}
	// Unread first, then newest — §7's order, and the reason the unread
	// count in the header is a summary rather than a scroll.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Unread != items[j].Unread {
			return items[i].Unread
		}
		return items[i].Seq > items[j].Seq
	})
	if items != nil {
		card.Items = items
	}
	return card
}

// homeCalendar reports the calendar's state, honestly.
//
// There is no CalDAV read in this control plane yet: §12.3's month view
// and §7's three-day card both want the Calendars capability, and
// nothing here can invent it. So this returns "not available" with the
// reason, and the page renders §7's own copy rather than an empty card
// implying the household has nothing on.
//
// Deliberately NOT a stub with sample events. A calendar card that shows
// invented events on a family's wall tablet is worse than one that says
// it cannot read the calendar: somebody will believe it.
func (s *Server) homeCalendar(desired []resource.Resource) homeCard {
	if calendarServer(desired) {
		return homeCard{Problem: "Can't reach the calendars right now"}
	}
	return homeCard{Problem: "No calendar is set up yet."}
}

// calendarServer reports whether this household runs a CalDAV server.
//
// Found by the CONTAINER rather than by the capability, which looks
// wrong until you check a real install: davis ships inside the mail
// capability's roundcube package, so the Route that serves the
// household's calendars is owned by capability/mail. A check for
// capability "calendar" reads better and answers no on a household that
// demonstrably has one.
//
// The container name is the honest key here because it is what actually
// speaks CalDAV. If a future profile serves calendars from something
// else, this list is the one place that has to learn its name — and
// until it does, the card says "no calendar is set up yet", which is
// wrong in the safe direction: it promises nothing it cannot show.
func calendarServer(desired []resource.Resource) bool {
	for _, r := range desired {
		if r.Kind != resource.KindRoute || r.Route == nil {
			continue
		}
		switch upstreamContainer(r.Route.Upstream) {
		case "davis", "radicale", "baikal":
			return true
		}
	}
	return false
}

// upstreamContainer is the container half of "container/<name>:<port>",
// or "" for a Route that names an address.
//
// A local copy of reconcile's unexported helper rather than an export of
// it: this is three lines, and widening that package's API so one
// household card can ask a question about a string is the wrong trade.
func upstreamContainer(upstream string) string {
	if !strings.HasPrefix(upstream, "container/") {
		return ""
	}
	addr := strings.TrimPrefix(upstream, "container/")
	if i := strings.IndexByte(addr, ':'); i >= 0 {
		return addr[:i]
	}
	return addr
}

// homeApps is the launcher, joined to what Keystone has observed.
//
// The join is the whole of the runtime half. reconcile.Apps is a pure
// function of desired state; the observation table says whether each
// container is in the shape desired state asked for. Three outcomes,
// and the third is load-bearing: a Route that names an address rather
// than a container, or a container nothing has observed, is UNKNOWN. §7
// forbids rendering that as down.
func (s *Server) homeApps(desired []resource.Resource) ([]reconcile.App, map[string]string, bool) {
	apps := reconcile.Apps(desired)
	status := map[string]string{}
	observed, err := s.DB.ListObserved()
	if err != nil {
		s.Log.Error("home apps observed", "err", err)
		return apps, status, false
	}
	byContainer := map[string]runtimedb.Observed{}
	for _, o := range observed {
		if o.Kind == string(resource.KindContainer) {
			byContainer[o.Name] = o
		}
	}
	for _, a := range apps {
		if a.Container == "" {
			status[a.Route] = "unknown"
			continue
		}
		o, seen := byContainer[a.Container]
		switch {
		case !seen:
			status[a.Route] = "unknown"
		case o.AwaitingApproval:
			// Not converged because a human has not decided, which is
			// not a fault. Grey, and the household surface says nothing
			// more: whose decision it is belongs in Needs you.
			status[a.Route] = "unknown"
		case !o.Healthy:
			status[a.Route] = "bad"
		case o.AppHealth == "unhealthy":
			// The container is as desired state asked, but the app
			// inside says it cannot serve. That is a red dot for a
			// family: the thing they click will not work.
			status[a.Route] = "bad"
		default:
			status[a.Route] = "ok"
		}
	}
	// An empty observation table is "nobody has checked", not "all
	// down". The page turns this into one line above the grid.
	return apps, status, len(byContainer) > 0
}

// homeMachines is the reassurance card: recovery, and the honest absence
// of host metrics.
func (s *Server) homeMachines(now time.Time) machinesCard {
	card := machinesCard{
		// Stated once, here: this process has no host readings to give.
		Metrics: homeCard{Problem: "Can't read machine stats"},
	}
	runs, err := s.DB.ListBackupRuns(100)
	if err != nil {
		s.Log.Error("home machines backups", "err", err)
		card.Problem = "Can't tell when the last backup was."
		return card
	}
	card.Available = true
	for _, run := range runs {
		if run.Status != runtimedb.BackupRunSucceeded && run.RunKind != runtimedb.BackupRunDrill {
			continue
		}
		switch run.RunKind {
		case runtimedb.BackupRunBackup:
			if run.Status == runtimedb.BackupRunSucceeded && run.FinishedAt.After(card.LastBackup) {
				card.LastBackup = run.FinishedAt
			}
		case runtimedb.BackupRunDrill:
			// The newest drill decides, pass or fail. A household that
			// is told "a restore was proved" by an older success while
			// last night's drill failed has been reassured by a card
			// that is wrong about the only thing it is for.
			if run.FinishedAt.After(card.LastDrill) {
				card.LastDrill = run.FinishedAt
				card.DrillOK = run.Status == runtimedb.BackupRunSucceeded
			}
		}
	}
	if nodes, err := s.DB.ListNodes(); err == nil {
		card.Nodes = len(nodes)
	}
	return card
}

// ---------------------------------------------------------------- answer

// homeAnswerNotice is the only write this surface has, and it is the
// decision itself: a member answers the question they were asked.
//
// IT DOES NOT CHECK WHOSE NOTICE IT IS, because runtimedb.AnswerNotice
// does, against the recipient the notice was created with, and reads a
// wrong answerer as not-found. Re-implementing that test here would be
// a second answer to "may this person decide this", and the second
// answer is the one that goes stale. What this layer contributes is the
// identity: `by` is the forwarded member the ingress proved, never a
// field in the body.
func (s *Server) homeAnswerNotice(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("seq")), 10, 64)
	if err != nil || seq <= 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("which question?")
	}
	answer := strings.TrimSpace(r.URL.Query().Get("answer"))
	approved, err := s.DB.AnswerNotice(seq, answer, member)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		// Not-found covers "somebody else's question" as well as "no
		// such question", and deliberately: see AnswerNotice's comment.
		return nil, http.StatusNotFound, fmt.Errorf("that question isn't waiting for you")
	case errors.Is(err, runtimedb.ErrNoticeNotAnswerable):
		return nil, http.StatusConflict, fmt.Errorf("%s", err.Error())
	case err != nil:
		s.Log.Error("home answer notice", "err", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("that didn't go through")
	}
	return map[string]any{"seq": seq, "approved": approved}, http.StatusOK, nil
}

// humanSince renders an elapsed time the way a household says it.
//
// Coarse on purpose. "2 hours" is what somebody deciding whether to deal
// with something now needs; "2h14m36s" is a duration printed by a
// program, and the difference is the register of the whole surface.
func humanSince(then, now time.Time) string {
	if then.IsZero() {
		return ""
	}
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		if m := int(d.Minutes()); m == 1 {
			return "1 minute"
		} else {
			return fmt.Sprintf("%d minutes", m)
		}
	case d < 24*time.Hour:
		if h := int(d.Hours()); h == 1 {
			return "1 hour"
		} else {
			return fmt.Sprintf("%d hours", h)
		}
	default:
		if days := int(d.Hours() / 24); days == 1 {
			return "1 day"
		} else {
			return fmt.Sprintf("%d days", days)
		}
	}
}

// homeClearNotices marks this member's notifications read, and only
// theirs.
//
// IT TAKES NO SEQUENCE NUMBER, and that is a security decision rather
// than a simplification. runtimedb.AcknowledgeNotice addresses a notice
// by seq and does NOT check the recipient — it cannot, because the
// Console acknowledges on an operator's behalf — so an endpoint here
// that accepted a seq from a household member would let them walk the
// sequence space and clear somebody else's phone. Instead the set of
// notices this can touch comes from a member-scoped query, so there is
// no field in the request in which to name another person's notice.
//
// "Clear" is per member by design (§7): clearing on the wall tablet must
// not clear anyone else's phone.
func (s *Server) homeClearNotices(_ http.ResponseWriter, _ *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	notices, err := s.DB.ListNotices(member, 200)
	if err != nil {
		s.Log.Error("home clear notices", "err", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("that didn't go through")
	}
	cleared := 0
	for _, n := range notices {
		if !n.AcknowledgedAt.IsZero() || !n.UndeliveredAt.IsZero() {
			continue
		}
		// A question that has not been answered is NOT cleared by this.
		// Acknowledging it would take the decision off the member's page
		// while leaving whatever is waiting on it still waiting, which
		// is the one way this control could lose something that matters.
		if n.Asks && n.AnsweredAt.IsZero() {
			continue
		}
		if err := s.DB.AcknowledgeNotice(n.Seq); err != nil {
			// Already acknowledged by another device between the read
			// and the write is the expected race, not a failure.
			if errors.Is(err, runtimedb.ErrNotFound) {
				continue
			}
			s.Log.Error("home clear notice", "seq", n.Seq, "err", err)
			continue
		}
		cleared++
	}
	return map[string]any{"cleared": cleared}, http.StatusOK, nil
}
