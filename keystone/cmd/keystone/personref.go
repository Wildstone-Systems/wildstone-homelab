package main

// `keystone person-refs` — the dry-run mapping report for the identity
// migration (#237).
//
// It exists so an Owner can see what the migration WOULD do before any
// schema changes, on their own database, with the service running or
// stopped. Slice 2b's migration aborts on anything this report calls
// blocking, so this is the list to clear first.

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb/personref"
)

func personRefs(args []string) error {
	fs := flag.NewFlagSet("person-refs", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/var/lib/wildstone/keystone", "directory holding keystone.db")
	dbPath := fs.String("db", "", "path to keystone.db; overrides -data-dir")
	all := fs.Bool("all", false, "list every handle, not only the ones that would block the migration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *dbPath
	if path == "" {
		path = *dataDir + "/keystone.db"
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no database at %s: %w", path, err)
	}

	// mode=ro, and the survey only ever holds a Querier. A report that
	// could write is not a dry run, and this makes that impossible
	// rather than careful.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("opening %s read-only: %w", path, err)
	}
	defer db.Close()

	// ONE read transaction, not the pool.
	//
	// This command is meant to be run against a live deployment, and a
	// pooled *sql.DB gives every query its own autocommit snapshot. A
	// rename landing between loading the people and scanning the
	// columns would have the report resolve post-rename references
	// against a pre-rename person list and conclude that nothing blocks
	// — on a database that, by the time it finished, contained an
	// orphan. A report whose parts disagree is worse than no report,
	// because its whole purpose is to be trusted before a migration.
	//
	// Deferred rollback rather than commit: a read transaction has
	// nothing to commit, and rollback is the honest close.
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("starting a read transaction on %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()

	rep, err := personref.Survey(tx)
	if err != nil {
		return err
	}
	writeReport(os.Stdout, rep, *all)
	if len(rep.Blocking()) > 0 {
		// A non-zero status so a script cannot mistake "there is work to
		// do first" for "ready".
		return fmt.Errorf("%d handle(s) must be resolved before the identity migration can run", len(rep.Blocking()))
	}
	return nil
}

func writeReport(w io.Writer, rep personref.Report, all bool) {
	fmt.Fprintf(w, "%d people on record.\n\n", rep.People)

	blocking := rep.Blocking()
	if len(blocking) == 0 {
		fmt.Fprintln(w, "Nothing blocks the identity migration.")
	} else {
		fmt.Fprintf(w, "%d handle(s) block the identity migration:\n\n", len(blocking))
		writeHandles(w, blocking)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  case-variant     a Person answers to it once case is folded, but not exactly.")
		fmt.Fprintln(w, "                   Decide which Person it is; nothing is merged automatically.")
		fmt.Fprintln(w, "  ambiguous        more than one Person answers to it. Never merged, never picked.")
		fmt.Fprintln(w, "  orphan           no Person and no principal. A Person is never invented.")
		fmt.Fprintln(w, "  scope            a non-person owner such as the household. Valid and permanent;")
		fmt.Fprintln(w, "                   never resolved, never blocking.")
		fmt.Fprintln(w, "  also-a-principal a Person AND a machine principal answer to this name, so no")
		fmt.Fprintln(w, "                   row bearing it can be attributed to either. Rename one.")
	}

	if !all {
		return
	}
	fmt.Fprintln(w, "\nEvery handle:")
	fmt.Fprintln(w)
	writeHandles(w, rep.Handles)
}

func writeHandles(w io.Writer, hs []personref.Handle) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  HANDLE\tVERDICT\tROWS\tWHERE\tCANDIDATES")
	for _, h := range hs {
		rows := 0
		where := map[string]bool{}
		for _, u := range h.Uses {
			rows += u.Rows
			where[u.Table+"."+u.Column] = true
		}
		cols := make([]string, 0, len(where))
		for c := range where {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		shown := cols[0]
		if len(cols) > 1 {
			shown = fmt.Sprintf("%s (+%d more)", cols[0], len(cols)-1)
		}
		cands := ""
		if len(h.Candidates) > 0 {
			cands = fmt.Sprint(h.Candidates)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%s\n", h.Value, h.Verdict, rows, shown, cands)
	}
	tw.Flush()
}
