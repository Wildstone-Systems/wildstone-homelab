package main

// `wild chat` (ADR-W026 A4): who may talk to a Butler, and as whom.
//
// The three verbs are the whole lifecycle. `code` mints a single-use
// claim code for one identity on one surface — the operator reads it to
// the person, who sends it to the bot once. `bindings` lists what exists.
// `revoke` withdraws one, effective on that account's next message.
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
  wild chat code -surface admin|family -identity <who> [-transport telegram] [-ttl 15m]
  wild chat bindings [-revoked] [-api URL]
  wild chat revoke <binding-id> [-api URL]
  wild chat outbound [-surface admin|family] [-transport telegram] [-after N]
  wild chat adapter <capability> -node <node> -surface admin|family
  wild chat transports [-api URL]

  -identity is a Keystone principal on the admin surface, and a household
  member on the family surface — one this deployment has DECLARED (see
  wild household members), not any name. -transport must be one this
  deployment has an adapter installed for (see wild chat transports); a
  code for a bot that does not exist is valid, unredeemable, and looks
  exactly like a working one. The code works once, expires, and can only
  ever produce the binding it names.`)
}

func runChatCode(args []string) int {
	fs := flag.NewFlagSet("wild chat code", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	surface := fs.String("surface", "", "which butler this code binds to: admin | family")
	identity := fs.String("identity", "", "the principal (admin) or household member (family) to bind")
	transport := fs.String("transport", "", "restrict the code to one transport, e.g. telegram (default: any)")
	ttl := fs.Duration("ttl", 0, "how long the code stays redeemable (default: server default)")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	if *surface != "admin" && *surface != "family" {
		fmt.Fprintln(os.Stderr, "wild chat code: -surface must be admin or family")
		return 2
	}
	if *identity == "" {
		fmt.Fprintln(os.Stderr, "wild chat code: -identity is required")
		return 2
	}
	token, ok := requireToken("chat code")
	if !ok {
		return 2
	}
	body := map[string]any{"surface": *surface, "identity": *identity}
	if *transport != "" {
		body["transport"] = *transport
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
	surface := fs.String("surface", "family", "which queue to read: admin | family")
	transport := fs.String("transport", "", "only messages for this transport")
	after := fs.Int64("after", 0, "cursor: messages after this seq")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	if *surface != "admin" && *surface != "family" {
		fmt.Fprintln(os.Stderr, "wild chat outbound: -surface must be admin or family")
		return 2
	}
	token, ok := requireToken("chat outbound")
	if !ok {
		return 2
	}
	q := url.Values{"surface": {*surface}, "after": {strconv.FormatInt(*after, 10)}}
	if *transport != "" {
		q.Set("transport", *transport)
	}
	return simpleCall("chat outbound", *apiBase, token, http.MethodGet,
		"/api/v1/chat/outbound?"+q.Encode(), nil)
}

// runChatAdapter gives a chat capability its Keystone credential
// (ADR-W026 A3): the principal is minted with the role the SURFACE
// implies — concierge for family, butler for admin — and its token is
// placed on the node as the provided secret the adapter's container
// reads. Running it again rotates the token.
func runChatAdapter(args []string) int {
	// Positional first, then flags (see runChatRevoke): parsing first
	// would stop at the capability name and leave every flag unset,
	// which is exactly what the documented command looked like doing.
	capability, rest := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild chat adapter", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	node := fs.String("node", "", "which node the adapter runs on")
	surface := fs.String("surface", "", "which butler this adapter serves: admin | family")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if capability == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: wild chat adapter <capability> -node <node> -surface admin|family")
		return 2
	}
	if *node == "" {
		fmt.Fprintln(os.Stderr, "wild chat adapter: -node is required")
		return 2
	}
	if *surface != "admin" && *surface != "family" {
		fmt.Fprintln(os.Stderr, "wild chat adapter: -surface must be admin or family")
		return 2
	}
	token, ok := requireToken("chat adapter")
	if !ok {
		return 2
	}
	return simpleCall("chat adapter", *apiBase, token, http.MethodPost,
		"/api/v1/chat/adapters/"+url.PathEscape(capability),
		map[string]string{"node": *node, "surface": *surface})
}
