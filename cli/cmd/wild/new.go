// `wild pkg new` scaffolds a package (ADR-W025 Part B item 2).
//
// The command's real job is the thing after the write: it lints what it
// just produced, against the catalog it found alongside, and prints the
// result. A scaffold that merely claims to be correct is a claim the
// author has no way to check on their first minute; one that has just
// been checked in front of them is a starting point they can trust, and
// the command they watched it run is the one they will run again.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgtool"
)

func runNew(args []string) int {
	pos, args := splitLeadingArg(args)
	fs := flag.NewFlagSet("wild pkg new", flag.ContinueOnError)
	shape := fs.String("shape", "", "one of "+strings.Join(pkgtool.ShapeNames(), ", "))
	capability := fs.String("capability", "", "the capability this implements (default: the package name)")
	profile := fs.String("profile", "", "which implementation of that capability this is (default: the package name)")
	dir := fs.String("dir", "", "catalog directory to create the package in (default: packages/ if it exists, else .)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if pos == "" && fs.NArg() == 1 {
		pos = fs.Arg(0)
	}
	// No name, or no shape: this is someone deciding rather than someone
	// mistyping, so answer the question they actually have.
	if pos == "" || *shape == "" {
		printShapes()
		return 2
	}

	root := *dir
	if root == "" {
		root = defaultCatalogDir()
	}

	path, err := pkgtool.Scaffold(root, pkgtool.ScaffoldOptions{
		Name:       pos,
		Capability: *capability,
		Profile:    *profile,
		Shape:      *shape,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg new:", err)
		return 1
	}
	pkgDir := filepath.Dir(path)
	fmt.Printf("wrote %s (%s)\n\n", path, *shape)

	// The catalog this package landed in, narrowed to the packages that
	// actually provide something it requires — so the printed command is
	// short enough to be read before it is pasted.
	var catalogs []string
	pkg, err := pkgspec.Load(path)
	if err != nil {
		// Cannot happen for a scaffold that lints in CI, and if it ever
		// does the author needs to know rather than to get a lint command
		// that silently omits its catalog.
		fmt.Fprintln(os.Stderr, "wild pkg new: the scaffold did not load:", err)
		return 1
	}
	catalogs = pkgtool.CatalogDirsFor(pkg, pkgtool.CatalogSiblings(pkgDir))

	lintCmd := "wild pkg lint " + pkgDir
	for _, c := range catalogs {
		lintCmd += " -catalog " + c
	}
	todos := countTODOs(path)

	fmt.Printf("  %-18s grep -n TODO %s\n", fmt.Sprintf("%d TODOs left:", todos), path)
	fmt.Printf("  %-18s %s\n", "lint it again:", lintCmd)
	fmt.Printf("  %-18s wild pkg traits -v\n", "what you can use:")
	if s, ok := pkgtool.LookupShape(*shape); ok && s.PinsImages {
		fmt.Printf("  %-18s wild pkg refresh-digests %s\n", "real image pins:", pkgDir)
	}
	fmt.Println()

	issues, err := pkgtool.Lint(pkgDir, pkgtool.LintOptions{CatalogDirs: catalogs})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg new: lint could not run:", err)
		return 1
	}
	if len(issues) == 0 {
		fmt.Println("lint: clean")
		return 0
	}
	fmt.Fprintf(os.Stderr, "lint: %d issue(s)\n", len(issues))
	for _, issue := range issues {
		fmt.Fprintln(os.Stderr, " ", issue.String())
	}
	// The likeliest issue by far, and the one that is not the scaffold's
	// fault: a requirement whose provider is not in this catalog. Saying
	// so here saves the author from debugging a package that is correct.
	fmt.Fprintln(os.Stderr, "\nAn unsatisfied requirement usually means no package in this catalog provides it yet;")
	fmt.Fprintln(os.Stderr, "drop that `requires` entry if this deployment will not have one.")
	return 1
}

// defaultCatalogDir prefers packages/ when it exists, because that is
// where a package in this repository belongs and typing -dir every time
// is how it ends up somewhere else.
func defaultCatalogDir() string {
	if info, err := os.Stat("packages"); err == nil && info.IsDir() {
		return "packages"
	}
	return "."
}

func countTODOs(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "TODO")
}

// printShapes is the menu, and it is prose rather than a list of words
// because choosing wrong is expensive: a service that could have been
// integrated with one config file gets an adapter container nobody needed,
// and nothing later says so.
func printShapes() {
	fmt.Fprintln(os.Stderr, "usage: wild pkg new <name> -shape <shape> [-capability c] [-profile p] [-dir catalog]")
	fmt.Fprintln(os.Stderr, "\nStart from the shape that matches what you already run:")
	for _, s := range pkgtool.Shapes {
		fmt.Fprintf(os.Stderr, "\n  %s — %s\n", s.Name, s.Summary)
		for _, line := range wrap(s.Description, 68) {
			fmt.Fprintf(os.Stderr, "      %s\n", line)
		}
	}
	fmt.Fprintln(os.Stderr, "\nExample:\n  wild pkg new photos-immich -shape service -capability photos -profile immich")
}

// wrap breaks prose to width, so a description reads as a paragraph in a
// terminal rather than as one line that scrolls.
func wrap(text string, width int) []string {
	var lines []string
	var line string
	for _, word := range strings.Fields(text) {
		if len(line)+1+len(word) > width && line != "" {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
