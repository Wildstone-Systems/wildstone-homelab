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

  -identity is a Keystone principal on the admin surface, and a household
  member on the family surface. The code works once, expires, and can only
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
	fs := flag.NewFlagSet("wild chat revoke", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: wild chat revoke <binding-id>")
		return 2
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild chat revoke: %q is not a binding id\n", fs.Arg(0))
		return 2
	}
	token, ok := requireToken("chat revoke")
	if !ok {
		return 2
	}
	return simpleCall("chat revoke", *apiBase, token, http.MethodPost,
		fmt.Sprintf("/api/v1/chat/bindings/%d/revoke", id), nil)
}
