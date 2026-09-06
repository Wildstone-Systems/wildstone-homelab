package main

// `wild butler` (ADR-W017 A2, ADR-W026 A1): the Butler as an API client.
//
// One command, two surfaces, and the surface is chosen by a flag that
// picks a DIFFERENT ENDPOINT rather than a mode of one endpoint —
// `-surface family` posts to /api/v1/household/butler/ask, which is
// backed by a gateway that holds no Statefile access at all. The
// distinction is worth keeping visible in the CLI because it is the
// whole point: asking the family butler to restart a container is not a
// permission error, it is a question the family butler has no words for.

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func runButler(args []string) int {
	if len(args) < 1 {
		butlerUsage()
		return 2
	}
	switch args[0] {
	case "ask":
		return runButlerAsk(args[1:])
	case "sessions":
		return runButlerSessions(args[1:])
	case "status":
		return runButlerStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wild butler: unknown subcommand %q\n", args[0])
		butlerUsage()
		return 2
	}
}

func butlerUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild butler ask "<question>" [-surface admin|family] [-api URL]
  wild butler status [-surface admin|family] [-api URL]
  wild butler sessions [-surface admin|family] [-limit n] [-id bs-N] [-api URL]

surfaces (ADR-W026):
  admin   the operator's butler: reads system state, proposes plans a human
          approves. Cannot approve or apply anything itself.
  family  the household's butler: reads the household timeline and performs
          domestic-tier actions. Has no access to the home lab itself.`)
}

// surfacePath maps the surface flag to its endpoint. An unknown surface
// is refused here rather than sent to the server, so a typo reads as a
// typo instead of a 404.
func surfacePath(surface, suffix string) (string, error) {
	switch surface {
	case "admin", "":
		return "/api/v1/butler" + suffix, nil
	case "family":
		return "/api/v1/household/butler" + suffix, nil
	default:
		return "", fmt.Errorf("unknown surface %q (admin|family)", surface)
	}
}

func runButlerAsk(args []string) int {
	// The question is a positional and comes off FIRST. Go's flag package
	// stops parsing at the first non-flag argument, so parsing before
	// this split silently swallowed every trailing flag into the
	// question — and `-surface family` becoming part of the text meant
	// the question went to the OPERATOR butler with no sign anything was
	// wrong. A wrong surface must never be something a typo can cause
	// quietly (ADR-W026 A1).
	first, rest := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild butler ask", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	surface := fs.String("surface", "admin", "which butler to ask: admin | family")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	// An unquoted question leaves its remaining words as positionals.
	// Joining them back is fine; a FLAG among them is not — it means a
	// flag came after unquoted words and was never parsed, so refuse
	// rather than ask the wrong butler.
	words := []string{first}
	for _, extra := range fs.Args() {
		if strings.HasPrefix(extra, "-") {
			fmt.Fprintf(os.Stderr, "wild butler ask: %q came after the question, where it is text rather than a flag.\n", extra)
			fmt.Fprintln(os.Stderr, `  Quote the question and put flags after it:  wild butler ask "why is jellyfin unhealthy?" -surface family`)
			return 2
		}
		words = append(words, extra)
	}
	message := strings.TrimSpace(strings.Join(words, " "))
	if message == "" {
		fmt.Fprintln(os.Stderr, `wild butler ask: give a question, e.g. wild butler ask "why is jellyfin unhealthy?"`)
		return 2
	}
	path, err := surfacePath(*surface, "/ask")
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild butler ask: %v\n", err)
		return 2
	}
	token, ok := requireToken("butler ask")
	if !ok {
		return 2
	}
	return simpleCall("butler ask", *apiBase, token, http.MethodPost, path, map[string]string{"message": message})
}

func runButlerStatus(args []string) int {
	fs := flag.NewFlagSet("wild butler status", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	surface := fs.String("surface", "admin", "which butler to report on: admin | family")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	path, err := surfacePath(*surface, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild butler status: %v\n", err)
		return 2
	}
	token, ok := requireToken("butler status")
	if !ok {
		return 2
	}
	return simpleCall("butler status", *apiBase, token, http.MethodGet, path, nil)
}

// runButlerSessions reads the audit trail. Sessions from both surfaces
// live in one record set — one Butler, two surfaces — so -surface here
// filters the list rather than choosing an endpoint.
func runButlerSessions(args []string) int {
	fs := flag.NewFlagSet("wild butler sessions", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	surface := fs.String("surface", "", "only sessions from this surface: admin | family (default both)")
	limit := fs.Int("limit", 0, "maximum sessions to list (0 = server default)")
	id := fs.String("id", "", "read one session by id, with its transcript")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("butler sessions")
	if !ok {
		return 2
	}
	if *id != "" {
		return simpleCall("butler sessions", *apiBase, token, http.MethodGet,
			"/api/v1/butler/sessions/"+url.PathEscape(*id), nil)
	}
	if *surface != "" && *surface != "admin" && *surface != "family" {
		fmt.Fprintf(os.Stderr, "wild butler sessions: unknown surface %q (admin|family)\n", *surface)
		return 2
	}
	q := url.Values{}
	if *surface != "" {
		q.Set("surface", *surface)
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	path := "/api/v1/butler/sessions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return simpleCall("butler sessions", *apiBase, token, http.MethodGet, path, nil)
}
