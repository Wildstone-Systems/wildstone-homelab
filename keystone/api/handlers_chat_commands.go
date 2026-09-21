package api

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Slash commands over chat.
//
// These are deterministic: no model runs, nothing is proposed, and every
// answer comes straight from the runtime DB or the Statefile. That is
// the point rather than an optimisation — an exact question deserves an
// exact answer, instantly, and these keep working when the Butler is
// off or the startup probe put it in qa-only. It is the same argument
// ADR-W024 makes for household rules having a deterministic backbone.
//
// Every command declares which SURFACES offer it, and /help renders only
// that surface's set. The household bot must not answer a home-lab
// question (ADR-W026 A1), and with two bots in one Telegram app a shared
// help text would actively mislead.

// chatCommandCtx is what a command gets to work with. It is the message's
// own context, never the caller's ambient state, so a command cannot see
// past the binding that produced it.
type chatCommandCtx struct {
	ReplySessions *[]string
	// At is the Connection the command arrived on — one Agent, one bot.
	At           runtimedb.Connection
	Conversation string
	Identity     string
	Binding      runtimedb.ChatBinding
	// Group is true when the message came from a room several people
	// can read. It is the difference between answering a person and
	// announcing to an audience (ADR-W026 A7).
	Group bool
	Arg   string
}

type chatCommand struct {
	Name     string
	Arg      string
	Help     string
	Surfaces []runtimedb.ButlerSurface
	Run      func(s *Server, c chatCommandCtx) (string, error)
}

func (c chatCommand) offeredOn(surface runtimedb.ButlerSurface) bool {
	for _, s := range c.Surfaces {
		if s == surface {
			return true
		}
	}
	return false
}

var (
	bothSurfaces = []runtimedb.ButlerSurface{runtimedb.ButlerSurfaceAdmin, runtimedb.ButlerSurfaceFamily}
	adminOnly    = []runtimedb.ButlerSurface{runtimedb.ButlerSurfaceAdmin}
	familyOnly   = []runtimedb.ButlerSurface{runtimedb.ButlerSurfaceFamily}
)

// chatCommands is the whole vocabulary, in the order /help lists it.
// Populated in init because /help renders this table, and a table that
// names /help's handler cannot also be its initialiser.
var chatCommands []chatCommand

func init() {
	chatCommands = []chatCommand{
		{Name: "help", Help: "what this bot answers", Surfaces: bothSurfaces, Run: (*Server).chatHelp},
		{Name: "whoami", Help: "which bot this is, and who you speak as", Surfaces: bothSurfaces, Run: (*Server).chatWhoami},
		{Name: "new", Help: "start a new conversation", Surfaces: bothSurfaces, Run: (*Server).chatNewConversation},
		{Name: "reset", Help: "start a new conversation", Surfaces: bothSurfaces, Run: (*Server).chatNewConversation},
		{Name: "clear", Help: "start a new conversation", Surfaces: bothSurfaces, Run: (*Server).chatNewConversation},
		{Name: "sessions", Arg: "[page]", Help: "recent conversations you can continue here", Surfaces: bothSurfaces, Run: (*Server).chatSessions},
		{Name: "resume", Arg: "<choice>", Help: "continue a conversation from /sessions", Surfaces: bothSurfaces, Run: (*Server).chatResumeConversation},

		{Name: "status", Help: "is anything wrong, and is anything waiting for you", Surfaces: adminOnly, Run: (*Server).chatStatus},
		{Name: "plans", Help: "plans waiting for your approval", Surfaces: adminOnly, Run: (*Server).chatPlans},
		{Name: "plan", Arg: "<id>", Help: "one plan's steps and blast radius", Surfaces: adminOnly, Run: (*Server).chatPlan},
		{Name: "nodes", Help: "the fleet, and how long each has been up", Surfaces: adminOnly, Run: (*Server).chatNodes},
		{Name: "backups", Help: "the last run of each backup plan", Surfaces: adminOnly, Run: (*Server).chatBackups},
		{Name: "services", Help: "what is installed, and what is healthy", Surfaces: adminOnly, Run: (*Server).chatServices},

		// /approve exists in order to REFUSE, on both surfaces
		// (ADR-W030 A8.1). Without it a person typing "approve plan-12"
		// gets "I don't know /approve", learns nothing, and tries
		// phrasing it differently. With it they are told why chat cannot
		// approve and where it can be done instead, which is the whole
		// content of the decision.
		{Name: "approve", Arg: "<id>", Help: "why approving happens elsewhere", Surfaces: bothSurfaces, Run: (*Server).chatApprove},

		{Name: "today", Help: "what has happened in the house today", Surfaces: familyOnly, Run: (*Server).chatToday},
		{Name: "actions", Help: "what I can do here", Surfaces: familyOnly, Run: (*Server).chatActions},
	}
}

// chatCommandInfo is one entry in a vendor's own command menu. Name
// carries no slash and no argument: Telegram's setMyCommands takes the
// bare word and renders the slash itself.
type chatCommandInfo struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// commandsFor is the menu a bot on this surface should publish.
//
// Aliases collapse to their first name — /new appears, /reset and
// /clear still work but do not clutter a menu whose whole job is
// discovery. It is filtered by surface for the same reason /help is:
// a household bot offering /status in its menu would teach exactly the
// thing ADR-W026 A1 makes structural.
func commandsFor(surface runtimedb.ButlerSurface) []chatCommandInfo {
	out := []chatCommandInfo{}
	seen := map[string]bool{}
	for _, cmd := range chatCommands {
		if !cmd.offeredOn(surface) || seen[cmd.Help] {
			continue
		}
		seen[cmd.Help] = true
		out = append(out, chatCommandInfo{Command: cmd.Name, Description: cmd.Help})
	}
	return out
}

// parseChatCommand splits "/plan plan-0f3a91" into name and argument. A
// message that does not begin with "/" is not a command — it is a
// question, and it goes to the Butler untouched.
func parseChatCommand(text string) (name, arg string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", "", false
	}
	t = strings.TrimPrefix(t, "/")
	if t == "" {
		return "", "", false
	}
	name, arg, _ = strings.Cut(t, " ")
	// Telegram addresses a command in a group as /status@thebot.
	name, _, _ = strings.Cut(name, "@")
	return strings.ToLower(name), strings.TrimSpace(arg), true
}

// runChatCommand handles the message if it is a command for this
// surface, and reports whether it did.
//
// An unrecognised /command is answered, not passed to the Butler. A
// typo'd /aprove fed to a model produces a confident sentence about
// approving something, which is the worst of both — and a person who
// typed a slash meant a command.
func (s *Server) runChatCommand(c chatCommandCtx, text string) (body, reason string, handled bool) {
	name, arg, ok := parseChatCommand(text)
	if !ok {
		return "", "", false
	}
	c.Arg = arg
	for _, cmd := range chatCommands {
		if cmd.Name != name || !cmd.offeredOn(c.At.Surface) {
			continue
		}
		out, err := cmd.Run(s, c)
		if err != nil {
			s.Log.Error("chat command failed", "command", name, "err", err)
			return "I couldn't answer that just now. The control plane logged why.",
				"/" + name + " failed", true
		}
		return out, "answered /" + name, true
	}
	return fmt.Sprintf("I don't know /%s. Try /help.", name), "unknown command", true
}

func (s *Server) chatHelp(c chatCommandCtx) (string, error) {
	var b strings.Builder
	which := "the household"
	if c.At.AdmitsOwnersOnly() {
		which = "the operator"
	}
	fmt.Fprintf(&b, "I'm %s butler. Ask me anything in plain language, or use:\n", which)
	// Commands that share a help line are aliases, and listing only the
	// first would hide /reset and /clear from the person most likely to
	// try them. They go on one line together.
	var order []string
	names := map[string][]string{}
	for _, cmd := range chatCommands {
		if !cmd.offeredOn(c.At.Surface) {
			continue
		}
		name := "/" + cmd.Name
		if cmd.Arg != "" {
			name += " " + cmd.Arg
		}
		if _, seen := names[cmd.Help]; !seen {
			order = append(order, cmd.Help)
		}
		names[cmd.Help] = append(names[cmd.Help], name)
	}
	for _, help := range order {
		fmt.Fprintf(&b, "\n%s — %s", strings.Join(names[help], ", "), help)
	}
	return b.String(), nil
}

func (s *Server) chatWhoami(c chatCommandCtx) (string, error) {
	var b strings.Builder
	if c.At.AdmitsOwnersOnly() {
		b.WriteString("This is the OPERATOR butler.\n")
		b.WriteString("It reads system state and can propose changes — a proposal waits for you to approve it in the Console. It cannot approve anything itself, and neither can you from here.\n")
	} else {
		b.WriteString("This is the HOUSEHOLD butler.\n")
		b.WriteString("It reads the household timeline and can do household things. It has no access to the home lab itself — no containers, storage, accounts or access rules.\n")
	}
	fmt.Fprintf(&b, "\nYou speak as: %s\nOver: %s", c.Identity, c.At.Transport)
	return b.String(), nil
}

func (s *Server) chatNewConversation(c chatCommandCtx) (string, error) {
	if err := s.DB.BreakChatThread(c.At, c.Conversation, c.Identity); err != nil {
		return "", err
	}
	return "Starting fresh. The previous conversation is still recorded — this one just won't build on it.", nil
}

func (s *Server) chatSessions(c chatCommandCtx) (string, error) {
	page := 1
	if arg := strings.TrimSpace(c.Arg); arg != "" {
		var err error
		page, err = strconv.Atoi(arg)
		if err != nil || page < 1 {
			return "Use `/sessions` or `/sessions <page>` with a positive page number.", nil
		}
	}
	candidates, protected, err := s.chatSessionCandidates(c)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 && protected {
		return trustedSurfaceRedirect, nil
	}
	return s.offerChatSessions(c, candidates, page)
}

func (s *Server) chatResumeConversation(c chatCommandCtx) (string, error) {
	token := strings.TrimSpace(c.Arg)
	if token == "" {
		return "Choose one of the private choices from /sessions: `/resume <choice>`.", nil
	}
	session, err := s.DB.ConsumeSessionResumeOffer(resumeTokenHash(token), c.Identity, c.At.Butler, c.At.Seq)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return "That choice is invalid or has expired. Use /sessions for a new list.", nil
		}
		return "", err
	}
	if err := s.DB.SetChatThreadSession(c.At, c.Conversation, c.Identity, session); err != nil {
		return "", err
	}
	return "Continuing that conversation. Send your message again when you're ready — the earlier message was not processed.", nil
}

// ---------------------------------------------------------- operator

// statusSummary is what /status says, as data.
//
// It is a struct rather than a string because the same answer is what an
// unprompted alert would send: "a plan is waiting", "a node dropped".
// Computing it once here means the command and any future notification
// cannot disagree about what "everything is fine" means.
type statusSummary struct {
	Resources     int
	Unhealthy     []runtimedb.Observed
	Awaiting      int
	PendingPlans  []runtimedb.PlanRecord
	Nodes         []runtimedb.Node
	Connected     int
	LastReconcile time.Time
}

// Quiet reports whether there is nothing a person needs to know about.
func (st statusSummary) Quiet() bool {
	return len(st.Unhealthy) == 0 && len(st.PendingPlans) == 0 && st.Connected == len(st.Nodes)
}

func (s *Server) statusSummary() (statusSummary, error) {
	var st statusSummary
	observed, err := s.DB.ListObserved()
	if err != nil {
		return st, err
	}
	st.Resources = len(observed)
	for _, o := range observed {
		switch {
		case o.Healthy:
		case o.AwaitingApproval:
			// A plan waiting for a human is not a fault (F75).
			st.Awaiting++
		default:
			st.Unhealthy = append(st.Unhealthy, o)
		}
	}
	// The state query, not a page of history: a plan left unresolved
	// while two hundred newer ones came and went is exactly the plan
	// somebody needs reminding about, and a newest-first page is where
	// it would disappear.
	if st.PendingPlans, err = s.DB.PlansInState(runtimedb.PlanProposed); err != nil {
		return st, err
	}
	if st.Nodes, err = s.DB.ListNodes(); err != nil {
		return st, err
	}
	live := map[string]bool{}
	if s.LiveNodes != nil {
		for _, n := range s.LiveNodes() {
			live[n.NodeID] = true
		}
	}
	for _, n := range st.Nodes {
		if live[n.NodeID] {
			st.Connected++
		}
	}
	// A window, not a page. EventsAfter(0, n) returns the OLDEST n, so
	// on any deployment with a real event history it would report the
	// first reconcile ever and never move again. A week is long enough
	// that "no reconcile in the window" is itself the honest answer.
	events, err := s.DB.EventsSince(time.Now().Add(-7*24*time.Hour), 5000)
	if err != nil {
		return st, err
	}
	for _, e := range events {
		if strings.HasPrefix(e.Type, "reconcile.") && e.Time.After(st.LastReconcile) {
			st.LastReconcile = e.Time
		}
	}
	return st, nil
}

func (s *Server) chatStatus(c chatCommandCtx) (string, error) {
	st, err := s.chatStatusSummary(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if st.Quiet() {
		b.WriteString("Everything is converged and nothing is waiting on you.\n")
	}
	fmt.Fprintf(&b, "%d of %d resources unhealthy", len(st.Unhealthy), st.Resources)
	if st.Awaiting > 0 {
		fmt.Fprintf(&b, " (%d more held back by a plan awaiting you)", st.Awaiting)
	}
	fmt.Fprintf(&b, "\n%s waiting for approval", plural(len(st.PendingPlans), "plan", "plans"))
	if len(st.Nodes) == 0 {
		b.WriteString("\nno nodes have joined yet")
	} else {
		fmt.Fprintf(&b, "\n%d of %d nodes connected", st.Connected, len(st.Nodes))
	}
	if !st.LastReconcile.IsZero() {
		fmt.Fprintf(&b, "\nlast reconcile %s ago", roughly(time.Since(st.LastReconcile)))
	}
	for i, o := range st.Unhealthy {
		if i == 0 {
			b.WriteString("\n")
		}
		if i == 5 {
			fmt.Fprintf(&b, "\n…and %d more", len(st.Unhealthy)-5)
			break
		}
		fmt.Fprintf(&b, "\n· %s/%s", o.Kind, o.Name)
		if o.Detail != "" {
			fmt.Fprintf(&b, " — %s", o.Detail)
		}
	}
	return b.String(), nil
}

func (s *Server) chatPlans(c chatCommandCtx) (string, error) {
	st, err := s.chatStatusSummary(c)
	if err != nil {
		return "", err
	}
	if len(st.PendingPlans) == 0 {
		return "Nothing is waiting for your approval.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s waiting:", plural(len(st.PendingPlans), "plan", "plans"))
	for _, p := range st.PendingPlans {
		fmt.Fprintf(&b, "\n\n%s · %s risk\n%s", p.ID, p.Risk, p.Summary)
		fmt.Fprintf(&b, "\nproposed by %s", p.Principal)
		if p.ActingFor != "" {
			fmt.Fprintf(&b, " for %s", p.ActingFor)
		}
	}
	b.WriteString("\n\n/plan <id> for the steps. Approving happens in the Console.")
	return b.String(), nil
}

func (s *Server) chatPlan(c chatCommandCtx) (string, error) {
	if c.Arg == "" {
		return "Which plan? Try /plans for the list.", nil
	}
	rec, err := s.DB.GetPlan(c.Arg)
	if err != nil {
		return fmt.Sprintf("I don't have a plan called %s. Try /plans.", c.Arg), nil
	}
	if err := s.checkChatPlanDisclosure(c, rec.ID); err != nil {
		return "This Plan is available on your household screen because of its privacy settings.", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · %s risk\n%s", rec.ID, rec.State, rec.Risk, rec.Summary)
	fmt.Fprintf(&b, "\nproposed by %s", rec.Principal)
	if rec.ActingFor != "" {
		fmt.Fprintf(&b, " for %s", rec.ActingFor)
	}
	// PlanJSON is the stored blob; viewPlan is what decodes it into
	// steps, and reusing it keeps chat and the API reading a plan the
	// same way.
	view := viewPlan(rec)
	var steps []plan.PlanStep
	if view.Plan != nil {
		steps = view.Plan.Steps
	}
	if len(steps) == 0 {
		b.WriteString("\n\nThis plan carries no expanded steps.")
	}
	for _, st := range steps {
		// The description usually already reads "Create Route/grafana",
		// so printing kind/name and action underneath it says the same
		// thing twice — which on a phone is half the screen.
		if d := strings.TrimSpace(st.Description); d != "" {
			fmt.Fprintf(&b, "\n\n· %s", d)
		} else {
			fmt.Fprintf(&b, "\n\n· %s/%s — %s", st.Kind, st.Name, st.Action)
		}
		if st.BlastRadius != "" {
			fmt.Fprintf(&b, "\n  %s", st.BlastRadius)
		}
	}
	if rec.State == "proposed" {
		b.WriteString("\n\nApproving happens in the Console — a chat message cannot move the human gate.")
	}
	return b.String(), nil
}

func (s *Server) chatNodes(_ chatCommandCtx) (string, error) {
	st, err := s.statusSummary()
	if err != nil {
		return "", err
	}
	if len(st.Nodes) == 0 {
		return "No nodes have joined yet.", nil
	}
	live := map[string]time.Time{}
	if s.LiveNodes != nil {
		for _, n := range s.LiveNodes() {
			live[n.NodeID] = n.Since
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d connected", st.Connected, len(st.Nodes))
	for _, n := range st.Nodes {
		if since, ok := live[n.NodeID]; ok {
			fmt.Fprintf(&b, "\n· %s — connected %s", n.NodeID, roughly(time.Since(since)))
		} else {
			fmt.Fprintf(&b, "\n· %s — NOT connected, last seen %s ago", n.NodeID, roughly(time.Since(n.LastSeenAt)))
		}
		if n.AgentVersion != "" {
			fmt.Fprintf(&b, " · agent %s", n.AgentVersion)
		}
	}
	return b.String(), nil
}

func (s *Server) chatBackups(_ chatCommandCtx) (string, error) {
	runs, err := s.DB.ListBackupRuns(200)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "No backup runs recorded yet — the engine ticks on its own schedule.", nil
	}
	// Newest run per plan. ListBackupRuns is newest first, so the first
	// time a plan is seen is its latest run.
	latest := map[string]runtimedb.BackupRun{}
	var order []string
	for _, r := range runs {
		if _, seen := latest[r.PlanName]; seen {
			continue
		}
		latest[r.PlanName] = r
		order = append(order, r.PlanName)
	}
	sort.Slice(order, func(i, j int) bool {
		fi, fj := latest[order[i]].Status == "failed", latest[order[j]].Status == "failed"
		if fi != fj {
			return fi // failures first: they are the reason to ask
		}
		return order[i] < order[j]
	})
	var b strings.Builder
	b.WriteString("Last run of each backup plan:")
	for _, name := range order {
		r := latest[name]
		fmt.Fprintf(&b, "\n· %s — %s, started %s ago", name, r.Status, roughly(time.Since(r.StartedAt)))
		if r.Detail != "" {
			fmt.Fprintf(&b, "\n  %s", r.Detail)
		}
	}
	return b.String(), nil
}

func (s *Server) chatServices(_ chatCommandCtx) (string, error) {
	current, err := s.State.Load()
	if err != nil {
		return "", err
	}
	observed, err := s.DB.ListObserved()
	if err != nil {
		return "", err
	}
	health := map[string]runtimedb.Observed{}
	for _, o := range observed {
		health[o.Kind+"/"+o.Name] = o
	}
	type owner struct{ total, bad, unknown int }
	byOwner := map[string]*owner{}
	for _, r := range current {
		// An actual prefix check. TrimPrefix returns the string
		// unchanged when the prefix is absent, so "household" and
		// "system" would have been listed as installed capabilities.
		name, ok := strings.CutPrefix(r.Metadata.Owner, "capability/")
		if !ok || name == "" {
			continue
		}
		o := byOwner[name]
		if o == nil {
			o = &owner{}
			byOwner[name] = o
		}
		o.total++
		h, seen := health[string(r.Kind)+"/"+r.Metadata.Name]
		switch {
		case !seen:
			// Nothing has reported on it — before its first reconcile,
			// or while its node is away. Not healthy; not a fault
			// either. Counting it as healthy would report success from
			// the absence of evidence.
			o.unknown++
		case !h.Healthy && !h.AwaitingApproval:
			o.bad++
		}
	}
	if len(byOwner) == 0 {
		return "Nothing is installed yet.", nil
	}
	names := make([]string, 0, len(byOwner))
	for n := range byOwner {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		bi, bj := byOwner[names[i]].bad > 0, byOwner[names[j]].bad > 0
		if bi != bj {
			return bi
		}
		ui, uj := byOwner[names[i]].unknown > 0, byOwner[names[j]].unknown > 0
		if ui != uj {
			return ui
		}
		return names[i] < names[j]
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%s installed:", plural(len(names), "capability", "capabilities"))
	for _, n := range names {
		o := byOwner[n]
		switch {
		case o.bad > 0:
			fmt.Fprintf(&b, "\n· %s — %d of %d unhealthy", n, o.bad, o.total)
		case o.unknown > 0:
			fmt.Fprintf(&b, "\n· %s — %d of %d not yet observed", n, o.unknown, o.total)
		default:
			fmt.Fprintf(&b, "\n· %s — healthy (%s)", n, plural(o.total, "resource", "resources"))
		}
	}
	return b.String(), nil
}

// ---------------------------------------------------------- household

func (s *Server) chatToday(c chatCommandCtx) (string, error) {
	// Midnight where the HOUSEHOLD is, not where the process thinks it
	// is. Around either zone's midnight the two disagree about which
	// day it is, and the household's answer is the right one.
	loc := s.householdLoc()
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// A room hears less than a person does. Overfetch, because the
	// filter removes rows after the query.
	fetch := 40
	if c.Group {
		fetch *= groupOverfetch
	}
	rows, err := s.DB.HouseholdEventsSince(midnight, fetch)
	if err != nil {
		return "", err
	}
	if c.Group {
		// The same rule the spoken answer applies (ADR-W026 A7): a
		// group hears about the house, not about the other person.
		rows = groupSafeRows(rows, c.Identity, s.MemberScoped)
	}
	if len(rows) > 40 {
		rows = rows[:40]
	}
	if len(rows) == 0 {
		return "Nothing has been recorded in the house today.", nil
	}
	var b strings.Builder
	b.WriteString("Today:")
	for _, e := range rows {
		fmt.Fprintf(&b, "\n· %s %s", e.OccurredAt.In(loc).Format("15:04"), e.Type)
		if e.Subject != "" {
			fmt.Fprintf(&b, " — %s", e.Subject)
		}
	}
	return b.String(), nil
}

func (s *Server) chatActions(_ chatCommandCtx) (string, error) {
	host := s.ButlerHost
	if host == nil || host.Household.Vocabulary == nil {
		return "This deployment has no household actions wired, so I can answer questions but not do anything.", nil
	}
	vocab, err := host.Household.Vocabulary()
	if err != nil {
		return "", err
	}
	if len(vocab.Actions) == 0 {
		return "I have no actions available here — I can answer questions about the house, but not do anything.", nil
	}
	var b strings.Builder
	b.WriteString("Things I can do here:")
	for _, a := range vocab.Actions {
		fmt.Fprintf(&b, "\n· %s", a.Tool)
		if a.Tier != "" {
			fmt.Fprintf(&b, " (%s)", a.Tier)
		}
	}
	return b.String(), nil
}

// ---------------------------------------------------------- rendering

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// roughly renders a duration the way a person says it. Chat is read on a
// phone; "3d 4h" beats "76h21m3.4s".
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// chatApprove refuses, and says why.
//
// Chat cannot approve, and the reason is not that chat is insecure — it
// is that a message does not establish who typed it or what they read.
// A group thread makes agreement and silence look identical, and a
// forwarded message makes anyone's words look like the author's. So
// approval happens where an identity was established: the Console for
// infrastructure, and the Authelia-authenticated household page for a
// pure routine change.
//
// The reply is different per surface because the alternative is
// different: an operator has a Console and a CLI, and a household member
// has a link somebody sends them.
func (s *Server) chatApprove(c chatCommandCtx) (string, error) {
	if c.At.Surface == runtimedb.ButlerSurfaceFamily {
		return "I can't take an approval here — a message doesn't show who typed it or what they read, " +
			"and in a group chat agreement and silence look the same. If this is a change to one of " +
			"the house's routines, ask for a review link: it opens a page that signs you in, shows " +
			"you exactly what changes, and works once.", nil
	}
	return "I can't approve anything, and neither can you from here. Approve it in the Console, or " +
		"with `wild plan approve`. Both establish who you are first; a chat message doesn't.", nil
}

func (s *Server) checkChatPlanDisclosure(c chatCommandCtx, id string) error {
	if err := s.DB.CheckPlanPrivacy(id, runtimedb.ModelLocal, runtimedb.MemoryDestination{ConnectionRef: c.At.Seq}); err != nil {
		return err
	}
	sessions, _, err := s.DB.PlanPrivacySessions(id)
	if err != nil {
		return err
	}
	if c.ReplySessions != nil {
		*c.ReplySessions = append(*c.ReplySessions, sessions...)
	}
	return nil
}
func (s *Server) chatStatusSummary(c chatCommandCtx) (statusSummary, error) {
	st, err := s.statusSummary()
	if err != nil {
		return st, err
	}
	visible := st.PendingPlans[:0]
	for _, p := range st.PendingPlans {
		if err := s.checkChatPlanDisclosure(c, p.ID); err != nil {
			if errors.Is(err, runtimedb.ErrPrivacy) {
				continue
			}
			return st, err
		}
		visible = append(visible, p)
	}
	st.PendingPlans = visible
	return st, nil
}
