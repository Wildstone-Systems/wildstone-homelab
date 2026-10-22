package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/recovery"
	"github.com/Wildstone-Systems/wildstone-homelab/proto/recoveryseal"
)

func runRecovery(args []string) int {
	if len(args) == 0 {
		recoveryUsage()
		return 2
	}
	switch args[0] {
	case "keygen":
		path, rest := splitLeadingArg(args[1:])
		if path == "" || len(rest) != 0 {
			recoveryUsage()
			return 2
		}
		publicKey, privateKey, err := recoveryseal.GenerateKeyPair()
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild recovery keygen:", err)
			return 1
		}
		if err := writeExclusive(path, []byte(privateKey+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "wild recovery keygen:", err)
			return 1
		}
		if err := writeExclusive(path+".pub", []byte(publicKey+"\n"), 0o644); err != nil {
			_ = os.Remove(path)
			fmt.Fprintln(os.Stderr, "wild recovery keygen:", err)
			return 1
		}
		fmt.Printf("wrote %s (offline private key) and %s.pub (Statefile public key)\n", path, path)
		return 0
	case "import-node":
		node, rest := splitLeadingArg(args[1:])
		fs := flag.NewFlagSet("wild recovery import-node", flag.ExitOnError)
		apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
		bundlePath := fs.String("bundle", "", "node .sealed bundle from the recovery set")
		privateKeyPath := fs.String("private-key", "", "offline recovery private-key file")
		kitPath := fs.String("kit", "", "recovery kit containing the offline private key")
		fs.Parse(rest) //nolint:errcheck // ExitOnError
		if node == "" || *bundlePath == "" || (*privateKeyPath == "") == (*kitPath == "") {
			recoveryUsage()
			return 2
		}
		token, ok := requireToken("recovery")
		if !ok {
			return 2
		}
		bundle, err := os.ReadFile(*bundlePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild recovery import-node:", err)
			return 1
		}
		defer clear(bundle)
		var privateKey string
		if *kitPath != "" {
			kit, err := recovery.LoadKit(*kitPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "wild recovery import-node:", err)
				return 1
			}
			privateKey = kit.RecoveryPrivateKey
		} else {
			body, err := os.ReadFile(*privateKeyPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "wild recovery import-node:", err)
				return 1
			}
			defer clear(body)
			privateKey = strings.TrimSpace(string(body))
		}
		return simpleCall("recovery", *apiBase, token, http.MethodPost,
			"/api/v1/nodes/"+url.PathEscape(node)+"/recovery/secrets", map[string]any{
				"privateKey": privateKey, "sealedBundle": bundle,
			})
	case "kit":
		fs := flag.NewFlagSet("wild recovery kit", flag.ExitOnError)
		out := fs.String("out", "", "output JSON file (created mode 0600)")
		endpoint := fs.String("endpoint", "", "S3-compatible endpoint")
		bucket := fs.String("bucket", "", "recovery bucket")
		prefix := fs.String("prefix", "", "optional target prefix")
		region := fs.String("region", "", "optional S3 region")
		pathStyle := fs.Bool("path-style", false, "use path-style S3 bucket lookup")
		accessKeyID := fs.String("access-key-id", "", "read-only recovery access-key id")
		privateKeyPath := fs.String("private-key", "", "offline recovery private-key file")
		fs.Parse(args[1:]) //nolint:errcheck // ExitOnError
		secretAccessKey := os.Getenv("WILD_RECOVERY_READ_KEY")
		passwordJSON := os.Getenv("WILD_RECOVERY_REPOSITORY_PASSWORDS")
		if *out == "" || *endpoint == "" || *bucket == "" || *accessKeyID == "" || *privateKeyPath == "" || secretAccessKey == "" || passwordJSON == "" {
			fmt.Fprintln(os.Stderr, "wild recovery kit: endpoint, bucket, access-key-id, private-key, WILD_RECOVERY_READ_KEY, and WILD_RECOVERY_REPOSITORY_PASSWORDS are required")
			return 2
		}
		var passwords map[string]string
		if err := json.Unmarshal([]byte(passwordJSON), &passwords); err != nil || len(passwords) == 0 {
			fmt.Fprintln(os.Stderr, "wild recovery kit: WILD_RECOVERY_REPOSITORY_PASSWORDS must be a JSON object of plan names to passwords")
			return 2
		}
		privateKey, err := os.ReadFile(*privateKeyPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild recovery kit:", err)
			return 1
		}
		defer clear(privateKey)
		kit := recovery.Kit{
			Format: "wildstone-recovery-kit-v1", Endpoint: *endpoint, Bucket: *bucket, Prefix: *prefix,
			Region: *region, PathStyle: *pathStyle,
			ReadOnlyAccessKeyID: *accessKeyID, ReadOnlySecretAccessKey: secretAccessKey,
			RepositoryPasswords: passwords, RecoveryPrivateKey: strings.TrimSpace(string(privateKey)),
		}
		body, err := json.MarshalIndent(kit, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild recovery kit:", err)
			return 1
		}
		defer clear(body)
		if err := writeExclusive(*out, append(body, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "wild recovery kit:", err)
			return 1
		}
		fmt.Printf("wrote recovery kit %s (mode 0600); import it into your password manager and keep an offline paper copy\n", *out)
		return 0
	default:
		recoveryUsage()
		return 2
	}
}

func writeExclusive(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

func recoveryUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild recovery keygen <private-key-path>
  wild recovery kit -out <kit.json> -endpoint <url> -bucket <name> -access-key-id <id> -private-key <path>
  wild recovery import-node <node> -bundle <node.sealed> (-kit <kit.json> | -private-key <path>) [-api URL]`)
}
