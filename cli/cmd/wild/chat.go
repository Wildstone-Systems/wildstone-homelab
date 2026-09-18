package main

// `wild chat` (ADR-W026 A4): who may talk to a Butler, and as whom.
//
// The three verbs are the whole lifecycle. `code` mints a single-use
// claim code admitting one PERSON to one AGENT's bot — the operator
// reads it to them, and they send it to that bot once. `bindings` lists
// what exists. `revoke` withdraws one, effective on that account's next
// message.
//
// A bot belongs to exactly one Agent (ADR-W033 B7), so every verb here
// names the Agent rather than a surface. A surface is a class of
// Agents: an invite to `admin` admitted somebody to every Owner Agent
// at once, including ones made afterwards.
//
// There is deliberately no "add binding" verb. A binding is always the
// result of somebody redeeming a code, because an operator typing a chat
// account id into a config file is exactly the credential-in-a-file
// pattern the claim code exists to replace.

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

func runChat(args []string) int {
	if len(args) < 1 {
		chatUsage()
		return 2
	}
	switch args[0] {
	case "code":
		return runChatCode(args[1:])
	case "bindings":
		return runChatBindings(args[1:])
	case "revoke":
		return runChatRevoke(args[1:])
	case "outbound":
		return runChatOutbound(args[1:])
	case "transports":
		return runChatTransports(args[1:])
	case "adapter":
		return runChatAdapter(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wild chat: unknown subcommand %q\n", args[0])
		chatUsage()
		return 2
	}
}

func chatUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild chat code -butler <agent> -identity <person> -transport telegram [-connection <capability>] [-ttl 15m]
  wild chat bindings [-revoked] [-api URL]
  wild chat revoke <binding-id> [-api URL]
  wild chat outbound -butler <agent> [-transport telegram] [-connection <capability>] [-after N]
  wild chat adapter <capability> -node <node> -butler <agent>
  wild chat transports [-api URL]

  -butler is the Agent the bot belongs to. One bot, one Agent: giving a
  capability its credential assigns it, and a bot that already answers
  for somebody else is refused rather than repointed.

  -connection is only needed when one Agent has SEVERAL bots on a
  transport, which it may (ADR-W033 B7). It is the capability name the
  bot's address carries, and the refusal you get without it lists the
  ones to choose from.

  -identity is a PERSON — an admitted household member this deployment
  has declared (see wild household members), not any name, and not a
  machine principal. An Owner Agent's bot admits Owners only.

  -transport must be one this deployment has an adapter installed for
  (see wild chat transports); a code for a bot that does not exist is
  valid, unredeemable, and looks exactly like a working one. The code
  works once, expires, and can only ever produce the admission it
  names.`)
}

func runChatCode(args []string) int {
	fs := flag.NewFlagSet("wild chat code", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	butler := fs.String("butler", "", "the agent whose bot this code admits somebody to")
	identity := fs.String("identity", "", "the household member to admit")
	transport := fs.String("transport", "", "which of that agent's bots, e.g. telegram")
	connection := fs.String("connection", "", "which bot, when that agent has several on the transport (the capability name)")
	ttl := fs.Duration("ttl", 0, "how long the code stays redeemable (default: server default)")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	if *butler == "" {
		fmt.Fprintln(os.Stderr, "wild chat code: -butler is required — a code admits somebody to one agent")
		return 2
	}
	if *identity == "" {
		fmt.Fprintln(os.Stderr, "wild chat code: -identity is required")
		return 2
	}
	if *transport == "" {
		fmt.Fprintln(os.Stderr, "wild chat code: -transport is required — it says which of that agent's bots")
		return 2
	}
	token, ok := requireToken("chat code")
	if !ok {
		return 2
	}
	body := map[string]any{"butler": *butler, "identity": *identity, "transport": *transport}
	if *connection != "" {
		body["connection"] = *connection
	}
	if *ttl > 0 {
		body["ttlSeconds"] = int(ttl.Seconds())
	}
	return simpleCall("chat code", *apiBase, token, http.MethodPost, "/api/v1/chat/claims", body)
}

// runChatTransports lists the chat adapters this deployment has, which
// is the closed set of answers to `wild chat code -transport`.
func runChatTransports(args []string) int {
	fs := flag.NewFlagSet("wild chat transports", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("chat transports")
	if !ok {
		return 2
	}
	return simpleCall("chat transports", *apiBase, token, http.MethodGet, "/api/v1/chat/transports", nil)
}

func runChatBindings(args []string) int {
	fs := flag.NewFlagSet("wild chat bindings", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	revoked := fs.Bool("revoked", false, "include revoked bindings (the history, not the access list)")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("chat bindings")
	if !ok {
		return 2
	}
	path := "/api/v1/chat/bindings"
	if *revoked {
		path += "?" + url.Values{"revoked": {"true"}}.Encode()
	}
	return simpleCall("chat bindings", *apiBase, token, http.MethodGet, path, nil)
}

func runChatRevoke(args []string) int {
	// The positional comes off FIRST, then the flags — Go's flag package
	// stops parsing at the first non-flag argument, so parsing before
	// this split would leave `-api` sitting in Args() as text nobody
	// reads. Same shape as `wild secret set <name> -node ...`.
	raw, rest := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild chat revoke", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if raw == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: wild chat revoke <binding-id> [-api URL]")
		return 2
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild chat revoke: %q is not a binding id\n", raw)
		return 2
	}
	token, ok := requireToken("chat revoke")
	if !ok {
		return 2
	}
	return simpleCall("chat revoke", *apiBase, token, http.MethodPost,
		fmt.Sprintf("/api/v1/chat/bindings/%d/revoke", id), nil)
}

// runChatOutbound reads the delivery queue an adapter polls (ADR-W026
// A8). It is a debugging view: seeing that a reply was queued but never
// acked is the difference between "the Butler said nothing" and "the
// adapter is not running".
func runChatOutbound(args []string) int {
	fs := flag.NewFlagSet("wild chat outbound", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	butler := fs.String("butler", "", "whose queue to read: the agent the bot belongs to")
	transport := fs.String("transport", "", "which of that agent's bots")
	connection := fs.String("connection", "", "which bot, when that agent has several on the transport (the capability name)")
	after := fs.Int64("after", 0, "cursor: messages after this seq")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	if *butler == "" {
		fmt.Fprintln(os.Stderr, "wild chat outbound: -butler is required — a queue belongs to one agent")
		return 2
	}
	token, ok := requireToken("chat outbound")
	if !ok {
		return 2
	}
	q := url.Values{"butler": {*butler}, "after": {strconv.FormatInt(*after, 10)}}
	if *transport != "" {
		q.Set("transport", *transport)
	}
	if *connection != "" {
		q.Set("connection", *connection)
	}
	return simpleCall("chat outbound", *apiBase, token, http.MethodGet,
		"/api/v1/chat/outbound?"+q.Encode(), nil)
}

// runChatAdapter gives a chat capability its Keystone credential
// (ADR-W026 A3) and assigns its bot to an Agent (ADR-W033 B7).
//
// One act, because two would leave a credentialed bot answering for
// nobody — which reads as working right up until somebody sends it a
// message. The principal is minted with the role the AGENT's class
// implies: concierge for a Member Agent, valet for an Owner one. Its
// token is placed on the node as the provided secret the adapter's
// container reads, and that token is that Agent's alone: a bot already
// answering for somebody else is refused rather than repointed.
// Running it again rotates the token.
func runChatAdapter(args []string) int {
	// Positional first, then flags (see runChatRevoke): parsing first
	// would stop at the capability name and leave every flag unset,
	// which is exactly what the documented command looked like doing.
	capability, rest := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild chat adapter", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	node := fs.String("node", "", "which node the adapter runs on")
	butler := fs.String("butler", "", "the agent this bot belongs to")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if capability == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: wild chat adapter <capability> -node <node> -butler <agent>")
		return 2
	}
	if *node == "" {
		fmt.Fprintln(os.Stderr, "wild chat adapter: -node is required")
		return 2
	}
	if *butler == "" {
		fmt.Fprintln(os.Stderr, "wild chat adapter: -butler is required — a bot belongs to one agent")
		return 2
	}
	token, ok := requireToken("chat adapter")
	if !ok {
		return 2
	}
	return simpleCall("chat adapter", *apiBase, token, http.MethodPost,
		"/api/v1/chat/adapters/"+url.PathEscape(capability),
		map[string]string{"node": *node, "butler": *butler})
}
