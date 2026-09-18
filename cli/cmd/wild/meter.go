package main

// `wild meter` (ADR-W035 K1): the numbers a household keeps, from a
// terminal.
//
// `read` is the one that matters and is deliberately the shortest thing
// to type. Recording a mileage is not a plan, not a proposal and not a
// change to anything the household has declared — it is writing down
// what the odometer said — and the command should cost about as much
// effort as saying it out loud.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

func runMeter(args []string) int {
	if len(args) < 1 {
		meterUsage()
		return 2
	}
	switch args[0] {
	case "read":
		return runMeterRead(args[1:])
	case "list":
		return runMeterList(args[1:])
	case "history":
		return runMeterHistory(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wild meter: unknown subcommand %q\n", args[0])
		meterUsage()
		return 2
	}
}

func meterUsage() {
	fmt.Fprint(os.Stderr, `usage: wild meter <command>

  read <name> <value>   record what a meter reads now
  list                  every meter, and where it stands
  history <name>        one meter's readings, oldest first

wild meter read f150-odometer 84210
wild meter read f150-odometer 0 -rebase -prior 84210 -reason "cluster replaced"
`)
}

// meterView mirrors the API's listing. Kept narrow on purpose: the CLI
// prints what a person asked about, not everything the server knows.
type meterView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Unit        string `json:"unit"`
	StaleAfter  string `json:"staleAfter"`
	BuiltIn     bool   `json:"builtIn"`
	Stale       bool   `json:"stale"`
	Latest      *struct {
		Value      float64   `json:"value"`
		At         time.Time `json:"at"`
		RecordedBy string    `json:"recordedBy"`
		Source     string    `json:"source"`
		Rebase     bool      `json:"rebase"`
		Reason     string    `json:"reason"`
	} `json:"latest"`
}

func runMeterRead(args []string) int {
	fs := flag.NewFlagSet("wild meter read", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	at := fs.String("at", "", "when it was read, RFC3339; default now")
	rebase := fs.Bool("rebase", false, "the meter was replaced: this value starts a new coordinate space")
	prior := fs.Float64("prior", 0, "with -rebase: what the old meter read just before it went")
	reason := fs.String("reason", "", "why, for a rebase or a correction")
	fs.Parse(args) //nolint:errcheck // ExitOnError

	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: wild meter read <name> <value>")
		return 2
	}
	name := fs.Arg(0)
	value, err := strconv.ParseFloat(fs.Arg(1), 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild meter read: %q is not a number\n", fs.Arg(1))
		return 2
	}
	// Catching this here rather than letting the server say it: somebody
	// typing a rebase has a number in their hand, and being told to go
	// and find it after the fact is worse than being told before.
	if *rebase && *prior == 0 {
		fmt.Fprintln(os.Stderr, "wild meter read: -rebase needs -prior, what the old meter read before it was replaced")
		fmt.Fprintln(os.Stderr, "  without it the thresholds measured against this meter cannot be carried across")
		return 2
	}
	body := map[string]any{"value": value}
	if *at != "" {
		when, perr := time.Parse(time.RFC3339, *at)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "wild meter read: -at %q is not RFC3339 (e.g. 2026-09-18T07:30:00Z)\n", *at)
			return 2
		}
		body["at"] = when
	}
	if *rebase {
		body["rebase"] = true
		body["prior"] = *prior
	}
	if *reason != "" {
		body["reason"] = *reason
	}

	token, ok := requireToken("meter read")
	if !ok {
		return 2
	}
	out, err := apiCall(*apiBase, token, http.MethodPost,
		"/api/v1/household/meters/"+name+"/readings", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild meter read:", err)
		return 1
	}
	var receipt struct {
		Seq       int64 `json:"seq"`
		Duplicate bool  `json:"duplicate"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		fmt.Fprintln(os.Stderr, "wild meter read: decoding:", err)
		return 1
	}
	if receipt.Duplicate {
		fmt.Printf("%s already had that reading (#%d)\n", name, receipt.Seq)
		return 0
	}
	fmt.Printf("%s: %g recorded (#%d)\n", name, value, receipt.Seq)
	return 0
}

func runMeterList(args []string) int {
	fs := flag.NewFlagSet("wild meter list", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("meter list")
	if !ok {
		return 2
	}
	out, err := apiCall(*apiBase, token, http.MethodGet, "/api/v1/household/meters", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild meter list:", err)
		return 1
	}
	var meters []meterView
	if err := json.Unmarshal([]byte(out), &meters); err != nil {
		fmt.Fprintln(os.Stderr, "wild meter list: decoding:", err)
		return 1
	}
	for _, m := range meters {
		fmt.Printf("%-24s %s\n", m.Name, meterStanding(m))
	}
	return 0
}

// meterStanding is the one line a person reads. Three cases, and the
// difference between the last two is the whole point of ADR-W035 A11: a
// meter nobody has read and a meter read too long ago must not look like
// a meter that is fine.
func meterStanding(m meterView) string {
	switch {
	case m.BuiltIn:
		return "built in"
	case m.Latest == nil:
		return "never read"
	case m.Stale:
		return fmt.Sprintf("%g %s, last read %s — too old to rely on",
			m.Latest.Value, m.Unit, m.Latest.At.Format("2 Jan 2006"))
	default:
		return fmt.Sprintf("%g %s, read %s", m.Latest.Value, m.Unit, m.Latest.At.Format("2 Jan 2006"))
	}
}

func runMeterHistory(args []string) int {
	fs := flag.NewFlagSet("wild meter history", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	limit := fs.Int("limit", 0, "at most this many readings (0 = all)")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: wild meter history <name>")
		return 2
	}
	token, ok := requireToken("meter history")
	if !ok {
		return 2
	}
	path := "/api/v1/household/meters/" + fs.Arg(0) + "/readings"
	if *limit > 0 {
		path += "?limit=" + strconv.Itoa(*limit)
	}
	out, err := apiCall(*apiBase, token, http.MethodGet, path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild meter history:", err)
		return 1
	}
	var readings []struct {
		Value      float64   `json:"value"`
		At         time.Time `json:"at"`
		RecordedBy string    `json:"recordedBy"`
		Source     string    `json:"source"`
		Rebase     bool      `json:"rebase"`
		Prior      float64   `json:"prior"`
		Reason     string    `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &readings); err != nil {
		fmt.Fprintln(os.Stderr, "wild meter history: decoding:", err)
		return 1
	}
	if len(readings) == 0 {
		fmt.Printf("%s has never been read\n", fs.Arg(0))
		return 0
	}
	for _, r := range readings {
		line := fmt.Sprintf("%s  %12g  %s", r.At.Format("2006-01-02 15:04"), r.Value, r.RecordedBy)
		if r.Source != "person" {
			line += " (" + r.Source + ")"
		}
		if r.Rebase {
			line += fmt.Sprintf("  [replaced, was %g]", r.Prior)
		}
		if r.Reason != "" {
			line += "  " + r.Reason
		}
		fmt.Println(line)
	}
	return 0
}
