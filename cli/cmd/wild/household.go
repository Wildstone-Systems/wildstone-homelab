package main

// `wild household` (ADR-W024 H1): the household event stream as an API
// client. `events` follows the cursor; `emit` publishes one fact (an
// admin token may emit — handy for testing a rule before a real shim
// exists); `publisher` gives a capability its publisher credential.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func runHousehold(args []string) int {
	if len(args) < 1 {
		householdUsage()
		return 2
	}
	switch args[0] {
	case "events":
		return runHouseholdEvents(args[1:])
	case "emit":
		return runHouseholdEmit(args[1:])
	case "publisher":
		return runHouseholdPublisher(args[1:])
	case "members":
		return runHouseholdMembers(args[1:])
	case "member":
		return runHouseholdMemberAdd(args[1:])
	case "groups":
		return runHouseholdGroups(args[1:])
	case "group":
		return runHouseholdGroup(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wild household: unknown subcommand %q\n", args[0])
		householdUsage()
		return 2
	}
}

func runHouseholdEvents(args []string) int {
	fs := flag.NewFlagSet("wild household events", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	after := fs.Int64("after", 0, "cursor: return events with seq greater than this")
	limit := fs.Int("limit", 0, "maximum events per page (0 = server default)")
	follow := fs.Bool("follow", false, "keep long-polling for new events (Ctrl-C to stop)")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("household events")
	if !ok {
		return 2
	}
	cursor := *after
	for {
		q := url.Values{}
		q.Set("after", strconv.FormatInt(cursor, 10))
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		if *follow {
			q.Set("waitMs", "30000")
		}
		out, err := apiCall(*apiBase, token, http.MethodGet, "/api/v1/household/events?"+q.Encode(), nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild household events:", err)
			return 1
		}
		if !*follow {
			fmt.Println(out)
			return 0
		}
		var page []struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(out), &page); err != nil {
			fmt.Fprintln(os.Stderr, "wild household events: decoding page:", err)
			return 1
		}
		if len(page) > 0 {
			fmt.Println(out)
			cursor = page[len(page)-1].Seq
		}
	}
}

func runHouseholdEmit(args []string) int {
	fs := flag.NewFlagSet("wild household emit", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	typ := fs.String("type", "", "event type, e.g. person.left_home (required)")
	id := fs.String("id", "", "event id; default: a time-based id (retrying with the same id is a no-op)")
	subject := fs.String("subject", "", "member/device/household the event is about")
	entity := fs.String("entity", "", "the thing affected (calendar entry, device, list item)")
	payload := fs.String("payload", "", "small JSON object with the details")
	correlation := fs.String("correlation", "", "correlation id grouping this with related events")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	if *typ == "" {
		fmt.Fprintln(os.Stderr, "wild household emit: -type is required")
		return 2
	}
	token, ok := requireToken("household emit")
	if !ok {
		return 2
	}
	eventID := *id
	if eventID == "" {
		eventID = "wild-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	body := map[string]any{
		"eventId": eventID, "occurredAt": time.Now().UTC(), "type": *typ,
	}
	if *subject != "" {
		body["subject"] = *subject
	}
	if *entity != "" {
		body["entity"] = *entity
	}
	if *correlation != "" {
		body["correlationId"] = *correlation
	}
	if *payload != "" {
		body["payload"] = json.RawMessage(*payload)
	}
	return simpleCall("household emit", *apiBase, token, http.MethodPost, "/api/v1/household/events", body)
}

func runHouseholdPublisher(args []string) int {
	capability, rest := splitLeadingArg(args)
	if capability == "" {
		fmt.Fprintln(os.Stderr, "usage: wild household publisher <capability> -node <id> [-api URL]")
		return 2
	}
	fs := flag.NewFlagSet("wild household publisher", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	node := fs.String("node", "", "node the publisher secret is placed on (required)")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if *node == "" {
		fmt.Fprintln(os.Stderr, "wild household publisher: -node is required")
		return 2
	}
	token, ok := requireToken("household publisher")
	if !ok {
		return 2
	}
	path := "/api/v1/household/publishers/" + url.PathEscape(capability)
	return simpleCall("household publisher", *apiBase, token, http.MethodPost, path, map[string]string{"node": *node})
}

// runHouseholdMembers lists the household's declared accounts — the
// LdapUsers in the Statefile (ADR-W020 A3), which is the same roster the
// Console shows and the same one a chat invite may name.
func runHouseholdMembers(args []string) int {
	fs := flag.NewFlagSet("wild household members", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("household members")
	if !ok {
		return 2
	}
	out, err := apiCall(*apiBase, token, http.MethodGet, "/api/v1/household/members", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild household members: %v\n", err)
		return 1
	}
	var members []struct {
		Username    string   `json:"username"`
		DisplayName string   `json:"displayName"`
		Email       string   `json:"email"`
		Groups      []string `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &members); err != nil {
		fmt.Fprintf(os.Stderr, "wild household members: %v\n", err)
		return 1
	}
	if len(members) == 0 {
		fmt.Println("no household members are declared")
		return 0
	}
	for _, m := range members {
		fmt.Printf("%s\t%s\t%s\t%s\n", m.Username, m.DisplayName, m.Email, strings.Join(m.Groups, ","))
	}
	return 0
}

// runHouseholdMemberAdd proposes the plan that declares a person. It
// creates nothing: a human approves the plan, and the identity
// capability's provisioning job creates the account — with no password,
// because a credential never lives in declared state (ADR-W020 A4).
func runHouseholdMemberAdd(args []string) int {
	username, rest := splitLeadingArg(args)
	if username == "" {
		fmt.Fprintln(os.Stderr, "usage: wild household member <username> -email <address> [-name \"Full Name\"] [-groups a,b] [-node id] [-api URL]")
		return 2
	}
	fs := flag.NewFlagSet("wild household member", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	email := fs.String("email", "", "the person's email address (required: LLDAP cannot create an account without one)")
	name := fs.String("name", "", "display name")
	groups := fs.String("groups", "", "comma-separated groups to put them in; each must be one a capability declares")
	node := fs.String("node", "", "directory node (only needed when more than one runs a directory)")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if *email == "" {
		fmt.Fprintln(os.Stderr, "wild household member: -email is required")
		return 2
	}
	token, ok := requireToken("household member")
	if !ok {
		return 2
	}
	body := map[string]any{"username": username, "displayName": *name, "email": *email}
	if *groups != "" {
		body["groups"] = strings.Split(*groups, ",")
	}
	if *node != "" {
		body["node"] = *node
	}
	return simpleCall("household member", *apiBase, token, http.MethodPost, "/api/v1/household/members", body)
}

// runHouseholdGroups lists the household's own groups (not the
// directory groups packages declare).
func runHouseholdGroups(args []string) int {
	fs := flag.NewFlagSet("wild household groups", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("household groups")
	if !ok {
		return 2
	}
	return simpleCall("household groups", *apiBase, token, http.MethodGet, "/api/v1/household/person-groups", nil)
}

// runHouseholdGroup creates or deletes a household group.
func runHouseholdGroup(args []string) int {
	const usage = "usage: wild household group create <name> [-api URL] | wild household group delete <id> [-api URL]"
	if len(args) < 1 || (args[0] != "create" && args[0] != "delete") {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	verb := args[0]
	arg, rest := splitLeadingArg(args[1:])
	if arg == "" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("wild household group "+verb, flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	token, ok := requireToken("household group " + verb)
	if !ok {
		return 2
	}
	if verb == "create" {
		return simpleCall("household group create", *apiBase, token, http.MethodPost,
			"/api/v1/household/person-groups", map[string]any{"name": arg})
	}
	return simpleCall("household group delete", *apiBase, token, http.MethodDelete,
		"/api/v1/household/person-groups/"+url.PathEscape(arg), nil)
}

func householdUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild household events [-after N] [-limit N] [-follow] [-api URL]
  wild household emit -type <ns.name> [-subject S] [-entity E] [-id ID] [-payload JSON] [-correlation C] [-api URL]
  wild household publisher <capability> -node <id> [-api URL]
  wild household members [-api URL]
  wild household member <username> -email <address> [-name "Full Name"] [-groups a,b] [-node id] [-api URL]
  wild household groups [-api URL]
  wild household group create <name> [-api URL]
  wild household group delete <id> [-api URL]
Authentication: set WILD_TOKEN. events needs read; emit needs emit (a
publisher or admin token); publisher needs an admin token — it mints
the capability's publisher principal and places its token on the node
as secret/<capability>-publisher (minting again rotates it). members
needs read; member needs propose — it PROPOSES the account, which
exists once the plan is approved and carries no password. groups needs
read; group create/delete need manage and are refused for the seeded
groups and for any group an entitlement, audience or secret grant still
names.`)
}
