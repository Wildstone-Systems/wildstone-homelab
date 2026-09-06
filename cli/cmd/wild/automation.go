package main

// `wild automation` (ADR-W024 H3): household rules as an API client.
// `list` shows the approved rules; `replay` runs one — approved by name
// or a draft from a file — over history with no side effects, which is
// how a rule is tested before anyone approves it.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func runAutomation(args []string) int {
	if len(args) < 1 {
		automationUsage()
		return 2
	}
	switch args[0] {
	case "list":
		return runAutomationList(args[1:])
	case "replay":
		return runAutomationReplay(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wild automation: unknown subcommand %q\n", args[0])
		automationUsage()
		return 2
	}
}

func runAutomationList(args []string) int {
	fs := flag.NewFlagSet("wild automation list", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("automation list")
	if !ok {
		return 2
	}
	out, err := apiCall(*apiBase, token, http.MethodGet, "/api/v1/resources", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild automation list:", err)
		return 1
	}
	var views []struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		fmt.Fprintln(os.Stderr, "wild automation list: decoding:", err)
		return 1
	}
	n := 0
	for _, v := range views {
		if v.Kind != "Automation" {
			continue
		}
		n++
		fmt.Printf("%s\t%s\n", v.Name, v.Owner)
	}
	if n == 0 {
		fmt.Println("no automations")
	}
	return 0
}

func runAutomationReplay(args []string) int {
	name, rest := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild automation replay", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	file := fs.String("file", "", "replay a draft rule from this YAML file instead of an approved one by name")
	since := fs.Duration("since", 7*24*time.Hour, "how far back to replay")
	limit := fs.Int("limit", 0, "maximum events to read (0 = server default)")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	if (name == "") == (*file == "") {
		fmt.Fprintln(os.Stderr, "wild automation replay: give an approved rule's name, or -file <draft.yaml>, not both")
		return 2
	}
	token, ok := requireToken("automation replay")
	if !ok {
		return 2
	}
	body := map[string]any{"from": time.Now().Add(-*since), "to": time.Now()}
	if *limit > 0 {
		body["limit"] = *limit
	}
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild automation replay:", err)
			return 1
		}
		body["rule"] = string(data)
	} else {
		body["name"] = name
	}
	return simpleCall("automation replay", *apiBase, token, http.MethodPost, "/api/v1/household/automations/replay", body)
}

func automationUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild automation list [-api URL]
  wild automation replay <name> [-since 168h] [-limit N] [-api URL]
  wild automation replay -file <draft.yaml> [-since 168h] [-limit N] [-api URL]
Authentication: set WILD_TOKEN to a token with read access. Replay
evaluates a rule against the household stream and writes nothing; a
draft is validated and policy-checked, and its violations are reported.`)
}
