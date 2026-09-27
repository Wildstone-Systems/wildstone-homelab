package api

// Chat Connections, made in one step (ADR-W033 B7, amended B7.1).
//
// A Connection joins a Person to an Agent over a chat platform. Making
// one used to be six screens: pick a package, credential it, paste the
// bot token, fill in an image and an address, install, then invite. The
// Owner now says three things — which Agent, which Person, and the
// platform's credential — and this does the rest, in the order the
// pieces depend on each other:
//
//  1. mint the adapter's Keystone credential, which also gives the Agent
//     the bot's address (mintChatAdapter);
//  2. store the platform credential as the instance's provided secrets
//     (setSecret);
//  3. install the platform package as a new instance and approve it —
//     the Owner submitting this form is the approval (enableCapability,
//     approvePlan);
//  4. invite the Person, and have the Agent tell them so where they are
//     already signed in.
//
// Each step is the existing handler, called in-process, so every check,
// event, plan and audit row those make is made here too. A failure part
// way leaves what was done and says which step failed; submitting again
// resumes, because an instance whose address already belongs to this
// Agent and was never installed is reused rather than duplicated.
//
// The key the Person sends to the bot is not made here. It is minted
// when they ask for it on their own signed-in page (connectionInviteKey),
// so it is never stored and the Owner never sees it.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// chatPlatform is what each platform needs from the Owner.
type chatPlatform struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Fields are the credentials the form asks for, in order. Each is
	// stored as the provided secret "<capability>-<Secret>", except a
	// field with Param set, which is an install param instead.
	Fields []chatField `json:"fields"`
}

type chatField struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Hint   string `json:"hint"`
	Secret string `json:"-"`
	Param  string `json:"-"`
	// Prefix, when set, is what a valid value starts with — a Slack bot
	// token pasted into the app-token box is caught here rather than by
	// a socket that never opens.
	Prefix string `json:"prefix,omitempty"`
}

// chatPlatforms is every platform a Connection can use, in the order the
// Console offers them. Each has a package named chat-<name>.
var chatPlatforms = []chatPlatform{
	{Name: "telegram", Label: "Telegram", Fields: []chatField{
		{Key: "botToken", Label: "Bot token", Secret: "bot",
			Hint: "Message @BotFather in Telegram, send /newbot, and paste the token it gives you."},
	}},
	{Name: "discord", Label: "Discord", Fields: []chatField{
		{Key: "botToken", Label: "Bot token", Secret: "bot",
			Hint: "discord.com/developers → New Application → Bot → Reset Token. Turn on Message Content Intent on the same page."},
	}},
	{Name: "slack", Label: "Slack", Fields: []chatField{
		{Key: "botToken", Label: "Bot token", Secret: "bot", Prefix: "xoxb-",
			Hint: "api.slack.com/apps → your app → OAuth & Permissions. Needs chat:write, im:history and files:read (files:read lets it hear voice clips), and the message.im event."},
		{Key: "appToken", Label: "App-level token", Secret: "app", Prefix: "xapp-",
			Hint: "Basic Information → App-Level Tokens, with connections:write. Turn on Socket Mode too."},
	}},
	{Name: "matrix", Label: "Matrix", Fields: []chatField{
		{Key: "homeserver", Label: "Homeserver address", Param: "homeserver_url",
			Hint: "Where the bot's account lives, e.g. https://matrix.example.com."},
		{Key: "accessToken", Label: "Access token", Secret: "token",
			Hint: "Sign in as the bot account in Element → Settings → Help & About → Access token. Keep that session active; Agent rooms must have encryption disabled."},
	}},
}

func chatPlatformNamed(name string) (chatPlatform, bool) {
	for _, p := range chatPlatforms {
		if p.Name == name {
			return p, true
		}
	}
	return chatPlatform{}, false
}

// listChatPlatforms is what the Connect form is built from.
func (s *Server) listChatPlatforms(_ *http.Request, _ principal.Identity) (any, int, error) {
	return map[string]any{"platforms": chatPlatforms}, http.StatusOK, nil
}

type connectRequest struct {
	Platform    string            `json:"platform"`
	Butler      string            `json:"butler"`
	Person      string            `json:"person"`
	Credentials map[string]string `json:"credentials"`
	// Node is which machine runs the bot. Optional: a household with one
	// machine is not asked.
	Node string `json:"node,omitempty"`
	// Bot names one of the Agent's existing bots to invite the Person to,
	// when it has several on this platform. With no credentials given,
	// the Person is invited to the Agent's existing bot instead of a new
	// one being made.
	Bot string `json:"bot,omitempty"`
}

var nonInstanceRune = regexp.MustCompile(`[^a-z0-9]+`)

// createConnection is POST /api/v1/connections.
func (s *Server) createConnection(r *http.Request, id principal.Identity) (any, int, error) {
	var req connectRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<10)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	platform, ok := chatPlatformNamed(strings.TrimSpace(req.Platform))
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("platform must be one of telegram, discord, slack, matrix")
	}
	if s.Catalog == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no catalog is configured (keystone -catalog-dir)")
	}
	pkg, err := s.Catalog.Get("chat-"+platform.Name, "")
	if err != nil || !pkg.Metadata.Instanced {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this Keystone's catalog has no chat-%s package to install", platform.Name)
	}

	// Everything that can be refused is refused before anything is made.
	agent, err := s.DB.GetButler(strings.TrimSpace(req.Butler))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusBadRequest, fmt.Errorf("no Agent called %q", req.Butler)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	person := strings.TrimSpace(req.Person)
	m, err := s.DB.GetButlerMember(person)
	if err != nil || !m.Active() {
		return nil, http.StatusBadRequest, fmt.Errorf("%q is not an admitted member of this household", person)
	}
	// B9: an Owner-class Agent's Connections admit Owners only. Checked
	// here as well as when the key is issued, so the Owner hears it now
	// rather than the Person hearing it later.
	if agent.Surface == runtimedb.ButlerSurfaceAdmin && m.Role != runtimedb.RoleOwner {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"%s is an Owner Agent, and its Connections admit Owners only; %s is a Member (ADR-W033 B9)", agent.Name, person)
	}
	// No credential: invite them to the bot the Agent already has. One
	// bot serves many people; a second bot on the same token would be
	// the same bot polled twice, each stealing the other's messages.
	if !anyCredential(req.Credentials) {
		return s.inviteToExistingBot(r, id, agent, platform, person, strings.TrimSpace(req.Bot))
	}
	// A new bot mints a credential, which is consequential (A4.1);
	// inviting somebody to an existing one is not.
	if body, code, err := s.stepUpRefusal(id); err != nil {
		return body, code, err
	}
	values, params, err := platformValues(platform, req.Credentials)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	fingerprint := credentialFingerprint(platform.Name, values)
	switch at, used, err := s.DB.ConnectionUsingCredential(fingerprint); {
	case err != nil:
		return nil, http.StatusInternalServerError, err
	case used:
		return nil, http.StatusConflict, fmt.Errorf(
			"that token is already %s's %s bot (%s). To connect another person to it, leave the token empty",
			at.Butler, platform.Label, at.ExternalID)
	}
	node, keystoneURL, err := s.connectionPlacement(req.Node)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	instance, err := s.instanceFor(pkg, platform.Name, agent.Name)
	if err != nil {
		return nil, http.StatusConflict, err
	}
	capName := pkg.Metadata.Capability + "-" + instance

	step := func(what string, h handlerFunc, body any, path map[string]string) (map[string]any, error) {
		out, code, err := s.subcall(r, id, h, body, path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (status %d)", what, err, code)
		}
		return out, nil
	}
	// 1. The adapter's own credential, and the Agent's address.
	if _, err := step("minting the adapter's credential", s.mintChatAdapter,
		map[string]string{"node": node, "butler": agent.Name}, map[string]string{"capability": capName}); err != nil {
		return nil, http.StatusBadGateway, err
	}
	// 2. The platform credential.
	for secret, value := range values {
		if _, err := step("storing the "+platform.Label+" credential", s.setSecret,
			map[string]string{"value": value}, map[string]string{"node": node, "name": capName + "-" + secret}); err != nil {
			return nil, http.StatusBadGateway, err
		}
	}
	// 3. Install the instance and approve it.
	params[pkgspec.InstanceParam] = instance
	params["node"] = node
	params["keystone_url"] = keystoneURL
	proposed, err := step("proposing the install", s.enableCapability,
		map[string]any{"package": pkg.Metadata.Name, "params": params}, nil)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	planID, _ := proposed["id"].(string)
	if planID == "" {
		return nil, http.StatusInternalServerError, fmt.Errorf("the install proposal came back without a plan id")
	}
	if _, err := step("approving the install", s.approvePlan, nil, map[string]string{"id": planID}); err != nil {
		return nil, http.StatusBadGateway, err
	}
	// 4. The invite, and the Agent's word to the Person.
	at, err := s.DB.ChatConnectionAt(platform.Name, capName)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("finding the new Connection: %w", err)
	}
	if err := s.DB.RecordConnectionCredential(at.Seq, fingerprint); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	invite, err := s.DB.InviteToConnection(at.Seq, person, actingMember(id))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	where := "your household dashboard"
	if agent.Surface == runtimedb.ButlerSurfaceAdmin {
		where = "the Console"
	}
	s.tellInvitee(invite, agent.Name, platform.Label, where)
	s.event("chat.connection_created", person, id,
		fmt.Sprintf("%s on %s (%s) for %s, plan %s", agent.Name, platform.Label, capName, person, planID))

	note := fmt.Sprintf("%s's %s bot is being installed. %s will find their invite on %s, and get their key there.",
		agent.Name, platform.Label, person, where)
	if platform.Name == "matrix" {
		note += " Invite the bot to a new room with encryption disabled; it will accept the room invitation automatically."
	}
	return map[string]any{
		"capability": capName, "instance": instance, "platform": platform.Name,
		"butler": agent.Name, "person": person, "node": node, "plan": planID, "invite": invite.Seq,
		"note": note,
	}, http.StatusCreated, nil
}

// platformValues checks the credentials a platform needs and splits them
// into provided secrets (by suffix) and install params.
func platformValues(p chatPlatform, given map[string]string) (secrets, params map[string]string, err error) {
	secrets, params = map[string]string{}, map[string]string{}
	for _, f := range p.Fields {
		v := strings.TrimSpace(given[f.Key])
		if v == "" {
			return nil, nil, fmt.Errorf("%s needs its %s", p.Label, strings.ToLower(f.Label))
		}
		if f.Param != "" {
			u, err := url.Parse(v)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return nil, nil, fmt.Errorf("the %s must be an https:// address", strings.ToLower(f.Label))
			}
			params[f.Param] = strings.TrimRight(v, "/")
			continue
		}
		// Shape only, and never echoed: a credential in an error message
		// is a credential in a log.
		if len(v) < 8 || len(v) > 4096 || strings.ContainsAny(v, " \t\r\n") {
			return nil, nil, fmt.Errorf("that does not look like a %s %s (it should be one unbroken string)", p.Label, strings.ToLower(f.Label))
		}
		if f.Prefix != "" && !strings.HasPrefix(v, f.Prefix) {
			return nil, nil, fmt.Errorf("a %s %s starts with %s", p.Label, strings.ToLower(f.Label), f.Prefix)
		}
		secrets[f.Secret] = v
	}
	return secrets, params, nil
}

// connectionPlacement is the machine a new bot runs on and the address
// it reaches Keystone at. One machine is chosen without asking;
// with several, the request must say which.
func (s *Server) connectionPlacement(asked string) (node, keystoneURL string, err error) {
	facts := s.gatherFacts()
	if facts.keystoneURL == "" {
		return "", "", fmt.Errorf("Keystone does not know an address a container can reach it at; start it with -api-listen on the tailnet or LAN address")
	}
	nodes := append([]string(nil), facts.nodes...)
	sort.Strings(nodes)
	switch {
	case asked != "":
		for _, n := range nodes {
			if n == asked {
				return n, facts.keystoneURL, nil
			}
		}
		return "", "", fmt.Errorf("no machine called %q is connected", asked)
	case len(nodes) == 1:
		return nodes[0], facts.keystoneURL, nil
	case len(nodes) == 0:
		return "", "", fmt.Errorf("no machine is connected to run the bot on")
	default:
		return "", "", fmt.Errorf("choose which machine runs the bot: %s", strings.Join(nodes, ", "))
	}
}

// instanceFor names a new bot after its Agent, suffixed when taken. A
// name whose address already belongs to this Agent but which was never
// installed is a previous attempt, and is reused so submitting again
// resumes it.
func (s *Server) instanceFor(pkg *pkgspec.Package, transport, agent string) (string, error) {
	base := strings.Trim(nonInstanceRune.ReplaceAllString(strings.ToLower(agent), "-"), "-")
	if base == "" {
		base = "bot"
	}
	if len(base) > 20 {
		base = strings.Trim(base[:20], "-")
	}
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return "", err
	}
	for i := 1; i <= 20; i++ {
		inst := base
		if i > 1 {
			inst = fmt.Sprintf("%s-%d", base, i)
		}
		capName, err := pkg.CapabilityFor(map[string]string{pkgspec.InstanceParam: inst})
		if err != nil {
			return "", err
		}
		// A removed bot's name stays taken: its Keystone credential was
		// disabled on removal, and bringing it back would revive that
		// credential rather than mint a fresh one.
		if p, perr := s.DB.GetPrincipal(chatAdapterPrincipalPrefix + capName); perr == nil && p.Disabled {
			continue
		}
		at, err := s.DB.ChatConnectionAt(transport, capName)
		switch {
		case errors.Is(err, runtimedb.ErrNoConnection) && !enabled[capName]:
			return inst, nil
		case err == nil && at.Butler == agent && !enabled[capName]:
			return inst, nil // a previous attempt, resumed
		case err != nil && !errors.Is(err, runtimedb.ErrNoConnection):
			return "", err
		}
	}
	return "", fmt.Errorf("%s already has 20 %s bots", agent, transport)
}

// subcall runs another handler in-process as the same caller, with its
// own body and path values, and decodes what it answered.
func (s *Server) subcall(r *http.Request, id principal.Identity, h handlerFunc, body any, path map[string]string) (map[string]any, int, error) {
	raw := []byte("{}")
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		raw = b
	}
	sub := r.Clone(r.Context())
	sub.Body = io.NopCloser(bytes.NewReader(raw))
	sub.ContentLength = int64(len(raw))
	for k, v := range path {
		sub.SetPathValue(k, v)
	}
	out, code, err := h(sub, id)
	if err != nil {
		return nil, code, err
	}
	decoded := map[string]any{}
	if out != nil {
		b, merr := json.Marshal(out)
		if merr == nil {
			_ = json.Unmarshal(b, &decoded)
		}
	}
	return decoded, code, nil
}

// tellInvitee is the Agent's word to the Person: that a Connection is
// waiting, and where to get their key. It says nothing secret; the key
// is fetched on that page.
func (s *Server) tellInvitee(inv runtimedb.ConnectionInvite, agent, platform, where string) {
	settings, _, err := s.DB.GetButlerSettings()
	if err != nil {
		s.Log.Warn("chat invite notice: reading settings", "err", err)
		return
	}
	if _, err := s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind:      "chat.invite",
		Recipient: inv.Member,
		DedupeKey: fmt.Sprintf("chat-invite:%d", inv.Seq),
		Body: fmt.Sprintf("%s can talk with you on %s now. Open Connections on %s to get your one-time key, then send it to the bot.",
			agent, platform, where),
		TTL: 14 * 24 * time.Hour,
	}, settings, time.Now()); err != nil {
		s.Log.Warn("chat invite notice", "member", inv.Member, "err", err)
	}
}

// inviteView is an open invite as its Person sees it.
type inviteView struct {
	Seq       int64  `json:"seq"`
	Butler    string `json:"butler"`
	Platform  string `json:"platform"`
	Label     string `json:"label"`
	InvitedBy string `json:"invitedBy"`
}

func viewInvites(invites []runtimedb.ConnectionInvite, surface runtimedb.ButlerSurface) []inviteView {
	out := []inviteView{}
	for _, i := range invites {
		if i.Surface != surface {
			continue
		}
		label := i.Transport
		if p, ok := chatPlatformNamed(i.Transport); ok {
			label = p.Label
		}
		out = append(out, inviteView{Seq: i.Seq, Butler: i.Butler, Platform: i.Transport, Label: label, InvitedBy: i.InvitedBy})
	}
	return out
}

// issueInviteKey mints the single-use key for one of the asking Person's
// own open invites (ADR-W033 B7.1). It is minted now, on their signed-in
// page, and stored only as a hash — so nobody else, the Owner who made
// the Connection included, ever sees it.
func (s *Server) issueInviteKey(member string, seq int64, surface runtimedb.ButlerSurface) (any, int, error) {
	inv, err := s.DB.OpenInvite(seq, member)
	if errors.Is(err, runtimedb.ErrNoInvite) || (err == nil && inv.Surface != surface) {
		return nil, http.StatusNotFound, fmt.Errorf("no open invite")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	at, err := s.DB.ChatConnectionAt(inv.Transport, inv.Capability)
	if err != nil {
		return nil, http.StatusConflict, fmt.Errorf("that bot is no longer connected to %s", inv.Butler)
	}
	code, rec, err := s.Principals.IssueChatClaim(at, member, member, 0)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	link, handle, carriesCode := s.linkForClaim(at, code)
	note := fmt.Sprintf("Send this key to the bot in a direct message. It works once and expires %s.",
		rec.ExpiresAt.Local().Format("Mon 2 Jan 15:04"))
	if carriesCode {
		note = "Open the link and press Start: it carries the key. It works once."
	}
	return map[string]any{
		"code": code, "link": link, "handle": handle, "carriesCode": carriesCode,
		"butler": inv.Butler, "platform": inv.Transport, "expiresAt": rec.ExpiresAt.UTC().Format(time.RFC3339),
		"note": note,
	}, http.StatusCreated, nil
}

func inviteSeq(r *http.Request) (int64, error) {
	var seq int64
	if _, err := fmt.Sscan(r.PathValue("seq"), &seq); err != nil || seq <= 0 {
		return 0, fmt.Errorf("invite number required")
	}
	return seq, nil
}

// homeInvites and homeInviteKey are the dashboard's half: invites to
// Member-class Agents, for the signed-in household member.
func (s *Server) homeInvites(_ http.ResponseWriter, _ *http.Request, member string, _ []resource.Resource) (any, int, error) {
	invites, err := s.DB.OpenInvitesFor(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"invites": viewInvites(invites, runtimedb.ButlerSurfaceFamily)}, http.StatusOK, nil
}

func (s *Server) homeInviteKey(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	seq, err := inviteSeq(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return s.issueInviteKey(member, seq, runtimedb.ButlerSurfaceFamily)
}

// consoleInvites and consoleInviteKey are the Console's half: invites to
// Owner-class Agents, for the signed-in Owner. A bearer token acting for
// nobody has no invites.
func (s *Server) consoleInvites(_ *http.Request, id principal.Identity) (any, int, error) {
	if id.ActingFor == "" {
		return map[string]any{"invites": []inviteView{}}, http.StatusOK, nil
	}
	invites, err := s.DB.OpenInvitesFor(id.ActingFor)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"invites": viewInvites(invites, runtimedb.ButlerSurfaceAdmin)}, http.StatusOK, nil
}

func (s *Server) consoleInviteKey(r *http.Request, id principal.Identity) (any, int, error) {
	if id.ActingFor == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("an invite key is for a person; sign in to the Console as yourself")
	}
	seq, err := inviteSeq(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return s.issueInviteKey(id.ActingFor, seq, runtimedb.ButlerSurfaceAdmin)
}

func anyCredential(given map[string]string) bool {
	for _, v := range given {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// credentialFingerprint identifies a bot's credential without holding it
// (migration 0057): SHA-256 over the platform and its secret values in
// a fixed order.
func credentialFingerprint(platform string, secrets map[string]string) string {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(platform))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k + "=" + secrets[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// agentBots is an Agent's installed bots on one platform.
func (s *Server) agentBots(agent, platform string) ([]runtimedb.Connection, error) {
	all, err := s.DB.ChatConnectionsForButler(agent, platform)
	if err != nil && !errors.Is(err, runtimedb.ErrNoConnection) {
		return nil, err
	}
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return nil, err
	}
	var out []runtimedb.Connection
	for _, at := range all {
		if enabled[at.ExternalID] {
			out = append(out, at)
		}
	}
	return out, nil
}

// inviteToExistingBot connects a Person to a bot the Agent already has.
func (s *Server) inviteToExistingBot(_ *http.Request, id principal.Identity, agent runtimedb.Butler, platform chatPlatform,
	person, bot string) (any, int, error) {
	bots, err := s.agentBots(agent.Name, platform.Name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var at runtimedb.Connection
	switch {
	case bot != "":
		found := false
		for _, b := range bots {
			if b.ExternalID == bot {
				at, found = b, true
			}
		}
		if !found {
			return nil, http.StatusBadRequest, fmt.Errorf("%s is not one of %s's %s bots", bot, agent.Name, platform.Label)
		}
	case len(bots) == 1:
		at = bots[0]
	case len(bots) == 0:
		return nil, http.StatusBadRequest, fmt.Errorf(
			"%s has no %s bot yet: paste the bot's token to make one", agent.Name, platform.Label)
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("%s has %d %s bots: choose which one", agent.Name, len(bots), platform.Label)
	}
	invite, err := s.DB.InviteToConnection(at.Seq, person, actingMember(id))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	where := "your household dashboard"
	if agent.Surface == runtimedb.ButlerSurfaceAdmin {
		where = "the Console"
	}
	s.tellInvitee(invite, agent.Name, platform.Label, where)
	s.event("chat.connection_invited", person, id, fmt.Sprintf("%s on %s (%s) for %s", agent.Name, platform.Label, at.ExternalID, person))
	return map[string]any{
		"capability": at.ExternalID, "platform": platform.Name, "butler": agent.Name, "person": person,
		"invite": invite.Seq, "reused": true,
		"note": fmt.Sprintf("%s is invited to %s's %s bot. They'll find their invite on %s, and get their key there.",
			person, agent.Name, platform.Label, where),
	}, http.StatusCreated, nil
}

// botView is one installed chat bot, for the Connect screen.
type botView struct {
	Capability string `json:"capability"`
	Platform   string `json:"platform"`
	Label      string `json:"label"`
	Butler     string `json:"butler"`
	// Admits is what this Connection admits (design §8.1). It is on
	// the list because the list is where somebody decides whether to
	// narrow one, and a setting invisible until you open a detail page
	// is a setting nobody audits.
	//
	// An empty string means no policy on record, which admits nobody.
	// Shown as it is rather than defaulted to a value in the
	// vocabulary: a screen that renders a missing policy as "household"
	// would tell an operator their bot is open when in fact it is shut.
	Admits string `json:"admits"`
}

// listConnections is GET /api/v1/connections: every installed chat bot
// and whose it is.
func (s *Server) listConnections(_ *http.Request, _ principal.Identity) (any, int, error) {
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := []botView{}
	for c := range enabled {
		transport, ok := capabilityTransport(c)
		if !ok {
			continue
		}
		p, known := chatPlatformNamed(transport)
		if !known {
			continue
		}
		at, err := s.DB.ChatConnectionAt(transport, c)
		if err != nil {
			continue // installed but never given to an Agent
		}
		out = append(out, botView{Capability: c, Platform: transport, Label: p.Label,
			Butler: at.Butler, Admits: string(at.Admission)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Capability < out[j].Capability })
	return map[string]any{"bots": out}, http.StatusOK, nil
}

// removeConnection is POST /api/v1/connections/{capability}/remove: the
// bot is uninstalled, its address revoked (so its bindings stop
// resolving), its Keystone credential disabled and its open invites
// withdrawn. Consequential (A4.1): the people on it lose that way to
// reach the Agent.
func (s *Server) removeConnection(r *http.Request, id principal.Identity) (any, int, error) {
	capName := r.PathValue("capability")
	transport, ok := capabilityTransport(capName)
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("%q is not a chat bot", capName)
	}
	instance := strings.TrimPrefix(capName, chatCapabilityPrefix+transport+"-")
	at, atErr := s.DB.ChatConnectionAt(transport, capName)

	proposed, code, err := s.subcall(r, id, s.disableCapability,
		map[string]any{"package": chatCapabilityPrefix + transport, "instance": instance}, nil)
	if err != nil {
		return nil, code, fmt.Errorf("removing %s: %w", capName, err)
	}
	planID, _ := proposed["id"].(string)
	if planID != "" {
		if _, code, err := s.subcall(r, id, s.approvePlan, nil, map[string]string{"id": planID}); err != nil {
			return nil, code, fmt.Errorf("approving the removal: %w", err)
		}
	}
	if atErr == nil {
		if err := s.DB.WithdrawInvitesOn(at.Seq, "bot removed"); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if err := s.DB.RevokeButlerAddress(transport, capName, actingMember(id)); err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusInternalServerError, err
		}
	}
	if err := s.DB.SetPrincipalDisabled(chatAdapterPrincipalPrefix+capName, true); err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusInternalServerError, err
	}
	s.event("chat.connection_removed", "", id, fmt.Sprintf("%s removed, plan %s", capName, planID))
	return map[string]any{"capability": capName, "plan": planID}, http.StatusOK, nil
}

// setConnectionAdmission is POST /api/v1/connections/{capability}/admission:
// who may address the Agent over this bot (design §8.1, §8.3).
//
// CONSEQUENTIAL, and #201 asks every slice from here on to say why
// rather than leave it to the default. Both directions are: narrowing
// to owners withdraws a way the household reaches an Agent, exactly as
// removeConnection does, and widening to the household hands people
// access they did not have. A4.1's question is whether an operation
// changes who may reach what, and this operation is nothing else.
//
// It never widens past the Agent class. That ceiling is enforced in
// the database (0063's triggers) and again in Connection.Admits, and
// this handler simply reports the refusal — there is no case here for
// it to get right or wrong.
func (s *Server) setConnectionAdmission(r *http.Request, id principal.Identity) (any, int, error) {
	capName := r.PathValue("capability")
	transport, ok := capabilityTransport(capName)
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("%q is not a chat bot", capName)
	}
	var req struct {
		Admits string `json:"admits"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	at, err := s.DB.SetConnectionAdmission(
		transport, capName, runtimedb.Admission(strings.TrimSpace(req.Admits)), actingMember(id))
	switch {
	case errors.Is(err, runtimedb.ErrNoConnection):
		return nil, http.StatusNotFound, fmt.Errorf("no agent answers on %s", capName)
	case errors.Is(err, runtimedb.ErrUnknownAdmission):
		// 400: the vocabulary is closed and this is not in it, which is
		// a malformed request and nothing to do with this deployment.
		// It was 409 alongside the case below until a review of #289
		// separated them — a client told "conflict" about a typo goes
		// looking for state that is perfectly fine.
		return nil, http.StatusBadRequest, err
	case err != nil:
		// 409 rather than 400: the request is well formed and the
		// deployment's state is what refuses it — an Owner Agent whose
		// Connections admit Owners only. An operator who reads "bad
		// request" for that goes looking for a typo.
		return nil, http.StatusConflict, err
	}
	s.event("chat.connection_admission_set", capName, id,
		fmt.Sprintf("%s now admits %s", at.Name(), string(at.Admission)))
	return map[string]any{
		"capability": capName, "butler": at.Butler, "admits": string(at.Admission),
	}, http.StatusOK, nil
}
