package api

import (
	"fmt"
	"sort"
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
	Surface      runtimedb.ButlerSurface
	Transport    string
	Conversation string
	Identity     string
	Binding      runtimedb.ChatBinding
	Arg          string
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

		{Name: "status", Help: "is anything wrong, and is anything waiting for you", Surfaces: adminOnly, Run: (*Server).chatStatus},
		{Name: "plans", Help: "plans waiting for your approval", Surfaces: adminOnly, Run: (*Server).chatPlans},
		{Name: "plan", Arg: "<id>", Help: "one plan's steps and blast radius", Surfaces: adminOnly, Run: (*Server).chatPlan},
		{Name: "nodes", Help: "the fleet, and how long each has been up", Surfaces: adminOnly, Run: (*Server).chatNodes},
		{Name: "backups", Help: "the last run of each backup plan", Surfaces: adminOnly, Run: (*Server).chatBackups},
		{Name: "services", Help: "what is installed, and what is healthy", Surfaces: adminOnly, Run: (*Server).chatServices},

		{Name: "today", Help: "what has happened in the house today", Surfaces: familyOnly, Run: (*Server).chatToday},
		{Name: "actions", Help: "what I can do here", Surfaces: familyOnly, Run: (*Server).chatActions},
	}
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
		if cmd.Name != name || !cmd.offeredOn(c.Surface) {
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
	if c.Surface == runtimedb.ButlerSurfaceAdmin {
		which = "the operator"
	}
	fmt.Fprintf(&b, "I'm %s butler. Ask me anything in plain language, or use:\n", which)
	seen := map[string]bool{}
	for _, cmd := range chatCommands {
		if !cmd.offeredOn(c.Surface) || seen[cmd.Help] {
			continue
		}
		seen[cmd.Help] = true
		name := "/" + cmd.Name
		if cmd.Arg != "" {
			name += " " + cmd.Arg
		}
		fmt.Fprintf(&b, "\n%s — %s", name, cmd.Help)
	}
	return b.String(), nil
}

func (s *Server) chatWhoami(c chatCommandCtx) (string, error) {
	var b strings.Builder
	if c.Surface == runtimedb.ButlerSurfaceAdmin {
		b.WriteString("This is the OPERATOR butler.\n")
		b.WriteString("It reads system state and can propose changes — a proposal waits for you to approve it in the Console. It cannot approve anything itself, and neither can you from here.\n")
	} else {
		b.WriteString("This is the HOUSEHOLD butler.\n")
		b.WriteString("It reads the household timeline and can do household things. It has no access to the home lab itself — no containers, storage, accounts or access rules.\n")
	}
	fmt.Fprintf(&b, "\nYou speak as: %s\nOver: %s", c.Identity, c.Transport)
	return b.String(), nil
}

func (s *Server) chatNewConversation(c chatCommandCtx) (string, error) {
	if err := s.DB.ClearChatThread(c.Transport, c.Conversation, c.Surface, c.Identity); err != nil {
		return "", err
	}
	return "Starting fresh. The previous conversation is still recorded — this one just won't build on it.", nil
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
	plans, err := s.DB.ListPlans(200)
	if err != nil {
		return st, err
	}
	for _, p := range plans {
		if p.State == "proposed" {
			st.PendingPlans = append(st.PendingPlans, p)
		}
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
	events, err := s.DB.EventsAfter(0, 300)
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

func (s *Server) chatStatus(_ chatCommandCtx) (string, error) {
	st, err := s.statusSummary()
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
	fmt.Fprintf(&b, "\n%d of %d nodes connected", st.Connected, len(st.Nodes))
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

func (s *Server) chatPlans(_ chatCommandCtx) (string, error) {
	st, err := s.statusSummary()
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
		fmt.Fprintf(&b, "\n\n· %s", strings.TrimSpace(st.Description))
		fmt.Fprintf(&b, "\n  %s/%s — %s", st.Kind, st.Name, st.Action)
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
	type owner struct{ total, bad int }
	byOwner := map[string]*owner{}
	for _, r := range current {
		name := strings.TrimPrefix(r.Metadata.Owner, "capability/")
		if name == "" || r.Metadata.Owner == "system" {
			continue
		}
		o := byOwner[name]
		if o == nil {
			o = &owner{}
			byOwner[name] = o
		}
		o.total++
		if h, ok := health[string(r.Kind)+"/"+r.Metadata.Name]; ok && !h.Healthy && !h.AwaitingApproval {
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
		return names[i] < names[j]
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%s installed:", plural(len(names), "capability", "capabilities"))
	for _, n := range names {
		o := byOwner[n]
		if o.bad > 0 {
			fmt.Fprintf(&b, "\n· %s — %d of %d unhealthy", n, o.bad, o.total)
		} else {
			fmt.Fprintf(&b, "\n· %s — healthy (%s)", n, plural(o.total, "resource", "resources"))
		}
	}
	return b.String(), nil
}

// ---------------------------------------------------------- household

func (s *Server) chatToday(_ chatCommandCtx) (string, error) {
	midnight := time.Now().Truncate(24 * time.Hour)
	rows, err := s.DB.HouseholdEventsSince(midnight, 40)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "Nothing has been recorded in the house today.", nil
	}
	var b strings.Builder
	b.WriteString("Today:")
	for _, e := range rows {
		fmt.Fprintf(&b, "\n· %s %s", e.OccurredAt.Local().Format("15:04"), e.Type)
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
