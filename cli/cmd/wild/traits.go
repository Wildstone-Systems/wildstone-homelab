// `wild pkg traits` prints the trait vocabulary (ADR-W025 A1): what a
// package may `provide`, what it may `require`, and which `.Resolved`
// keys each of those carries.
//
// It exists because the vocabulary is closed, and a closed vocabulary
// nobody can list is indistinguishable from an arbitrary one. Before
// this, an author's only route to "what can I require?" was reading the
// shipped packages and inferring the pattern — which finds `identity:oidc`
// and misses that `.Resolved.IssuerURL` comes with it, that `storage`
// takes a class rather than a mode, or that `ingress` is satisfied by the
// system whether or not any package advertises it.
//
// Local and offline: the vocabulary is compiled into the binary, so this
// is not an API client like most of `wild` (ADR-W015). It answers what
// THIS build understands, which is the right answer for someone writing a
// package against it.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/trait"
)

func runTraits(args []string) int {
	fs := flag.NewFlagSet("wild pkg traits", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "include each trait's and key's description")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	out := os.Stdout
	for _, t := range trait.All() {
		fmt.Fprintf(out, "%s", t.Name)
		if t.Structural {
			// Worth saying first: a requirement for a structural trait is
			// satisfied whether or not anything provides it, so an author
			// hunting for "which package do I need installed" would
			// otherwise look for one that does not have to exist.
			fmt.Fprint(out, "  (structural — satisfied by the system, no provider needed)")
		}
		fmt.Fprintln(out)
		if *verbose {
			fmt.Fprintln(out, indent(t.Description, "    "))
		}

		for _, m := range t.Modes {
			fmt.Fprintf(out, "  mode: %s\n", m.Name)
			if *verbose {
				fmt.Fprintln(out, indent(m.Description, "      "))
			}
			for _, e := range m.Exports {
				fmt.Fprintf(out, "    .Resolved.%s\n", e.Key)
				if *verbose {
					fmt.Fprintln(out, indent(e.Description, "        "))
				}
			}
		}
		for _, c := range t.Classes {
			fmt.Fprintf(out, "  class: %s\n", c.Name)
			if *verbose {
				fmt.Fprintln(out, indent(c.Description, "      "))
			}
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, "always available, from the package itself or the node:")
	for _, e := range trait.Intrinsic {
		fmt.Fprintf(out, "  .Resolved.%s\n", e.Key)
		if *verbose {
			fmt.Fprintln(out, indent(e.Description, "      "))
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "functions available inside an `exports` value: %s (plus Go's own built-ins)\n",
		strings.Join(trait.ExportFuncs, ", "))
	fmt.Fprintf(out, "fields available inside an `exports` value: .%s\n",
		strings.Join(trait.ExportContext, ", ."))
	return 0
}

// indent wraps text to a readable width under a prefix. Descriptions are
// written as prose in keystone/trait, and prose printed as one long line
// is prose nobody reads.
func indent(text, prefix string) string {
	lines := wrap(text, 68)
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
