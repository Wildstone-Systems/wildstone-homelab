package main

// `wild secret` (ADR-W012 W2): the first API-client subcommand family —
// a thin mapping over the admin-only secrets endpoints. The token comes
// from WILD_TOKEN (never a flag: argv is visible in process listings and
// shell history). A value for `set` is read from stdin by default for
// the same reason; -value exists for scripting where the caller accepts
// that trade-off.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func runSecret(args []string) int {
	if len(args) < 1 {
		secretUsage()
		return 2
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("wild secret "+sub, flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	node := fs.String("node", "", "target node id (required)")
	value := fs.String("value", "", "secret value for `set` (default: read from stdin)")

	var name string
	switch sub {
	case "list":
		fs.Parse(rest) //nolint:errcheck // ExitOnError
	case "generate", "set", "rotate":
		name, rest = splitLeadingArg(rest)
		if name == "" {
			fmt.Fprintf(os.Stderr, "wild secret %s: secret name required\n", sub)
			return 2
		}
		fs.Parse(rest) //nolint:errcheck // ExitOnError
	default:
		fmt.Fprintf(os.Stderr, "wild secret: unknown subcommand %q\n", sub)
		secretUsage()
		return 2
	}
	if *node == "" {
		fmt.Fprintln(os.Stderr, "wild secret: -node is required")
		return 2
	}
	token := os.Getenv("WILD_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "wild secret: set WILD_TOKEN to an API token")
		return 2
	}

	var method, path string
	var body any
	switch sub {
	case "list":
		method, path = http.MethodGet, fmt.Sprintf("/api/v1/nodes/%s/secrets", *node)
	case "generate":
		method, path = http.MethodPost, fmt.Sprintf("/api/v1/nodes/%s/secrets/%s", *node, name)
	case "set":
		v := *value
		if v == "" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(os.Stderr, "wild secret set: reading stdin:", err)
				return 1
			}
			v = strings.TrimRight(string(data), "\r\n")
		}
		if v == "" {
			fmt.Fprintln(os.Stderr, "wild secret set: empty value (pipe it on stdin or pass -value)")
			return 2
		}
		method, path = http.MethodPost, fmt.Sprintf("/api/v1/nodes/%s/secrets/%s", *node, name)
		body = map[string]string{"value": v}
	case "rotate":
		method, path = http.MethodPost, fmt.Sprintf("/api/v1/nodes/%s/secrets/%s/rotate", *node, name)
	}

	out, err := apiCall(*apiBase, token, method, path, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild secret:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}

// apiCall performs one authenticated JSON request and returns the
// pretty-printed response body.
func apiCall(base, token, method, path string, body any) (string, error) {
	var buf io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		buf = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(data)))
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		return pretty.String(), nil
	}
	return string(data), nil
}

func secretUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild secret list -node <id> [-api URL]
  wild secret generate <name> -node <id> [-api URL]
  wild secret set <name> -node <id> [-value v] [-api URL]   (default: value from stdin)
  wild secret rotate <name> -node <id> [-api URL]
Authentication: set WILD_TOKEN to an admin API token.`)
}
