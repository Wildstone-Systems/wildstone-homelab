// Command wild is the Wildstone CLI, a thin client of the Keystone public
// API — the same API the Console and the Butler use. E5 lands the first
// real subcommands: the package toolchain (`wild pkg ...`), which runs
// locally against package directories and the registry directory
// (ADR-W010); the API-client subcommands land with E11.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgtool"
)

var version = "dev" // set via -ldflags at release

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "-version", "--version":
		fmt.Println("wild", version)
		os.Exit(0)
	case "pkg":
		os.Exit(runPkg(os.Args[2:]))
	case "secret":
		os.Exit(runSecret(os.Args[2:]))
	case "node":
		os.Exit(runNode(os.Args[2:]))
	case "status":
		os.Exit(runStatus(os.Args[2:]))
	case "plan":
		os.Exit(runPlan(os.Args[2:]))
	case "capability":
		os.Exit(runCapability(os.Args[2:]))
	case "logs":
		os.Exit(runLogs(os.Args[2:]))
	case "audit":
		os.Exit(runAudit(os.Args[2:]))
	case "backup":
		os.Exit(runBackup(os.Args[2:]))
	case "recovery":
		os.Exit(runRecovery(os.Args[2:]))
	case "restore":
		os.Exit(runRestore(os.Args[2:]))
	case "household":
		os.Exit(runHousehold(os.Args[2:]))
	case "automation":
		os.Exit(runAutomation(os.Args[2:]))
	case "meter":
		os.Exit(runMeter(os.Args[2:]))
	case "butler":
		os.Exit(runButler(os.Args[2:]))
	case "report":
		os.Exit(runReport(os.Args[2:]))
	case "chat":
		os.Exit(runChat(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "wild: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild pkg new <name> -shape <shape>         (scaffold a package that lints)
  wild pkg list [-api URL]                   (what Keystone can install)
  wild pkg lint <pkg-dir> [-set k=v ...] [-catalog dir ...]
  wild pkg keygen <key-path>
  wild pkg publish <pkg-dir> -registry <dir> -key <key-path>
  wild pkg resolve <name> [-constraint v] -registry <dir>
  wild pkg fetch <name> [-constraint v] -url <registry-url> -pub <key.pub> -dest <dir>
  wild pkg refresh-digests <pkg-dir>
  wild pkg traits [-v]                       (what a package may provide/require)
  wild secret list|generate|set|rotate ...   (see wild secret)
  wild node list | mint-join ...             (see wild node)
  wild status | plan | capability | logs | audit | backup ...  (API client, ADR-W015)
  wild recovery keygen|import-node ...       (whole-site recovery key and node import)
  wild restore status|reconcile ...          (restore quarantine)
  wild household events|emit|publisher ...   (see wild household)
  wild automation list|replay ...            (see wild automation)
  wild meter read|list|history ...           (see wild meter)
  wild report morning [-json]                (see wild report)
  wild version`)
}

func runPkg(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	switch args[0] {
	case "lint":
		return runLint(args[1:])
	case "list":
		return runPkgList(args[1:])
	case "keygen":
		if len(args) != 2 {
			usage()
			return 2
		}
		if _, _, err := pkgtool.GenerateSigningKey(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "wild pkg keygen:", err)
			return 1
		}
		fmt.Printf("wrote %s (private, keep safe) and %s.pub (public)\n", args[1], args[1])
		return 0
	case "publish":
		return runPublish(args[1:])
	case "resolve":
		return runResolve(args[1:])
	case "fetch":
		return runFetch(args[1:])
	case "refresh-digests":
		return runRefreshDigests(args[1:])
	case "traits":
		return runTraits(args[1:])
	case "new":
		return runNew(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wild pkg: unknown subcommand %q\n", args[0])
		usage()
		return 2
	}
}

// splitLeadingArg lets subcommands take their positional argument first
// ("wild pkg lint <dir> -set k=v"): stdlib flag stops at the first
// non-flag token, so peel the positional off before Parse.
func splitLeadingArg(args []string) (pos string, rest []string) {
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		return args[0], args[1:]
	}
	return "", args
}

// repeatedFlag collects a repeatable string flag.
type repeatedFlag []string

func (r *repeatedFlag) String() string     { return fmt.Sprint([]string(*r)) }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

func runLint(args []string) int {
	pos, args := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild pkg lint", flag.ContinueOnError)
	var sets, catalogs repeatedFlag
	fs.Var(&sets, "set", "required-param filler as k=v (repeatable)")
	fs.Var(&catalogs, "catalog", "package dir whose Provides satisfy requires (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if pos == "" && fs.NArg() == 1 {
		pos = fs.Arg(0)
	}
	if pos == "" {
		usage()
		return 2
	}
	fillers := map[string]string{}
	for _, kv := range sets {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				fillers[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	issues, err := pkgtool.Lint(pos, pkgtool.LintOptions{Fillers: fillers, CatalogDirs: catalogs})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg lint:", err)
		return 1
	}
	for _, issue := range issues {
		if issue.Warning {
			fmt.Fprintln(os.Stderr, issue.String())
		}
	}
	issues = pkgtool.Failures(issues)
	if len(issues) == 0 {
		fmt.Println("ok: package lints clean")
		return 0
	}
	for _, issue := range issues {
		fmt.Fprintln(os.Stderr, issue.String())
	}
	return 1
}

func runPublish(args []string) int {
	pos, args := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild pkg publish", flag.ContinueOnError)
	registry := fs.String("registry", "", "registry root directory")
	keyPath := fs.String("key", "", "publisher private key (from wild pkg keygen)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if pos == "" && fs.NArg() == 1 {
		pos = fs.Arg(0)
	}
	if pos == "" || *registry == "" || *keyPath == "" {
		usage()
		return 2
	}
	priv, err := pkgtool.LoadPrivateKey(*keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg publish:", err)
		return 1
	}
	entry, err := pkgtool.Publish(*registry, pos, priv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg publish:", err)
		return 1
	}
	fmt.Printf("published %s@%s (%s, sha256 %.12s…)\n", entry.Name, entry.Version, entry.Path, entry.Sha256)
	return 0
}

func runResolve(args []string) int {
	pos, args := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild pkg resolve", flag.ContinueOnError)
	registry := fs.String("registry", "", "registry root directory")
	constraint := fs.String("constraint", "", `version constraint: "" (highest), exact "X.Y.Z", or "^N"`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if pos == "" && fs.NArg() == 1 {
		pos = fs.Arg(0)
	}
	if pos == "" || *registry == "" {
		usage()
		return 2
	}
	idx, err := pkgtool.ReadIndex(*registry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg resolve:", err)
		return 1
	}
	entry, err := idx.Resolve(pos, *constraint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg resolve:", err)
		return 1
	}
	fmt.Printf("%s@%s %s sha256:%s\n", entry.Name, entry.Version, entry.Path, entry.Sha256)
	return 0
}

func runFetch(args []string) int {
	pos, args := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild pkg fetch", flag.ContinueOnError)
	rawURL := fs.String("url", "", "registry base URL")
	pubPath := fs.String("pub", "", "publisher public key (.pub from wild pkg keygen)")
	dest := fs.String("dest", ".", "directory to extract into")
	constraint := fs.String("constraint", "", "version constraint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if pos == "" && fs.NArg() == 1 {
		pos = fs.Arg(0)
	}
	if pos == "" || *rawURL == "" || *pubPath == "" {
		usage()
		return 2
	}
	pub, err := pkgtool.LoadPublicKey(*pubPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg fetch:", err)
		return 1
	}
	dir, err := pkgtool.FetchPackage(*rawURL, pos, *constraint, pub, *dest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg fetch:", err)
		return 1
	}
	fmt.Println("fetched, verified, and extracted to", dir)
	return 0
}

func runRefreshDigests(args []string) int {
	pos, args := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild pkg refresh-digests", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if pos == "" && fs.NArg() == 1 {
		pos = fs.Arg(0)
	}
	if pos == "" {
		usage()
		return 2
	}
	results, err := pkgtool.RefreshDigests(pos, pkgtool.RegistryDigestResolver(nil))
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg refresh-digests:", err)
		return 1
	}
	failed := false
	// A held-back set is reported once, not once per member: the reason
	// is a property of the set, and repeating it reads as several
	// problems rather than one.
	reported := map[string]bool{}
	for _, r := range results {
		switch {
		case r.Err != nil:
			failed = true
			fmt.Fprintf(os.Stderr, "%s: %v\n", r.Image, r.Err)
		case r.HeldBack:
			// Not written, and not a success to pass over quietly: the
			// operator asked for a refresh and did not get one.
			failed = true
			fmt.Fprintf(os.Stderr, "%s: HELD BACK (a newer digest exists but was not written)\n", r.Image)
			if !reported[r.Reason] {
				reported[r.Reason] = true
				fmt.Fprintf(os.Stderr, "  %s\n", r.Reason)
			}
		case r.NewDigest != r.OldDigest:
			fmt.Printf("%s: %.19s… -> %.19s…\n", r.Image, r.OldDigest, r.NewDigest)
		default:
			fmt.Printf("%s: current\n", r.Image)
		}
	}
	if failed {
		return 1
	}
	return 0
}
