package main

// `wild node` (ADR-W014 A3): pairing. mint-join asks Keystone for a
// single-use join token and prints the ready-to-run install command the
// endpoint composes (token + CA fingerprint baked in).
//
// `wild node list` (#30) answers "what nodes do I have, and are they
// connected?" — the question nothing in the product could answer, because
// every other node route needs a name you already know.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

func runNode(args []string) int {
	if len(args) < 1 {
		nodeUsage()
		return 2
	}
	if args[0] == "list" {
		return runNodeList(args[1:])
	}
	if args[0] != "mint-join" {
		nodeUsage()
		return 2
	}
	fs := flag.NewFlagSet("wild node mint-join", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	ttl := fs.Int64("ttl", 0, "token TTL in seconds (default: server default, 15m)")
	asJSON := fs.Bool("json", false, "print the raw API response instead of the pairing instructions")
	fs.Parse(args[1:]) //nolint:errcheck // ExitOnError

	token := os.Getenv("WILD_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "wild node: set WILD_TOKEN to an admin API token")
		return 2
	}
	var body any
	if *ttl > 0 {
		body = map[string]int64{"ttlSeconds": *ttl}
	}
	out, err := apiCall(*apiBase, token, http.MethodPost, "/api/v1/join-tokens", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild node:", err)
		return 1
	}
	var resp joinTokenResponse
	if *asJSON || json.Unmarshal([]byte(out), &resp) != nil {
		fmt.Println(out)
		return 0
	}
	printPairing(resp)
	return 0
}

// joinTokenResponse mirrors POST /api/v1/join-tokens.
type joinTokenResponse struct {
	JoinToken     string   `json:"joinToken"`
	CAFingerprint string   `json:"caFingerprint"`
	ExpiresInSec  int64    `json:"expiresInSec"`
	Install       string   `json:"install"`
	InstallNotes  []string `json:"installNotes"`
}

// printPairing renders the instructions rather than the JSON envelope.
// The whole value of minting from Keystone is that it hands back a
// command you can paste; printing it inside a JSON object made the
// operator extract it by hand first.
func printPairing(r joinTokenResponse) {
	fmt.Printf("Join token minted; it is single-use and expires in %s.\n", time.Duration(r.ExpiresInSec)*time.Second)
	fmt.Println()
	fmt.Println("Run this on the new node:")
	fmt.Println()
	fmt.Println("  " + r.Install)
	for _, n := range r.InstallNotes {
		fmt.Println()
		fmt.Println("Note: " + n)
	}
}

// nodeListItem mirrors one row of GET /api/v1/nodes.
type nodeListItem struct {
	ID             string     `json:"id"`
	Connected      bool       `json:"connected"`
	AgentVersion   string     `json:"agentVersion"`
	ProtocolMinor  uint32     `json:"protocolMinor"`
	FirstJoinedAt  time.Time  `json:"firstJoinedAt"`
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	ConnectedSince *time.Time `json:"connectedSince"`
}

// runNodeList prints the fleet as a table rather than the raw JSON the
// other subcommands print. The whole point of this command is answering a
// question at a glance; -json keeps the response available unchanged for
// scripts.
func runNodeList(args []string) int {
	fs := flag.NewFlagSet("wild node list", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	asJSON := fs.Bool("json", false, "print the raw API response instead of a table")
	fs.Parse(args) //nolint:errcheck // ExitOnError

	token, ok := requireToken("node list")
	if !ok {
		return 2
	}
	out, err := apiCall(*apiBase, token, http.MethodGet, "/api/v1/nodes", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild node list:", err)
		return 1
	}
	if *asJSON {
		fmt.Println(out)
		return 0
	}
	var body struct {
		Nodes []nodeListItem `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		fmt.Fprintln(os.Stderr, "wild node list: decoding response:", err)
		return 1
	}
	if len(body.Nodes) == 0 {
		fmt.Println("no nodes have joined yet (pair one: wild node mint-join)")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tSTATE\tAGENT\tPROTOCOL\tSINCE")
	for _, n := range body.Nodes {
		state, since := "disconnected", relative(n.LastSeenAt)
		if n.Connected {
			state = "connected"
			if n.ConnectedSince != nil {
				since = relative(*n.ConnectedSince)
			}
		}
		version := n.AgentVersion
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\tminor %d\t%s\n", n.ID, state, version, n.ProtocolMinor, since)
	}
	w.Flush() //nolint:errcheck
	return 0
}

// relative renders a timestamp as a coarse age — "how stale is this?" is
// the only question a reader asks of these two columns.
func relative(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func nodeUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild node list [-json] [-api URL]
  wild node mint-join [-ttl seconds] [-json] [-api URL]
Authentication: set WILD_TOKEN to an API token (list needs read; mint-join needs admin).`)
}
