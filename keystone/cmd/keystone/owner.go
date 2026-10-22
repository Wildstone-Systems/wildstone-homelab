package main

// `keystone owner` — host recovery for the identity foundation
// (design §2.6, work slice 5 of the agent-system epic, #206).
//
// This is the break-glass. Every other way to create or repair an Owner
// runs through the API, and the API is behind the Console check that
// slice 3 made Owner-only — so a household with nobody recorded, or
// whose Console holder was never recorded as a Person, has no way back
// in through any surface Wildstone serves. §2.6 names root-equivalent
// host access as the recovery authority, and this is it.
//
// Three properties, each load-bearing:
//
//  1. It is NOT reachable over the API. It lives on the keystone binary
//     beside claim-code and person-refs, operating on the data directory
//     rather than on a running server, so there is no route to reach and
//     no credential that grants it. Being on `wild` would have made it an
//     API client, which is the one thing it must not be.
//  2. It requires root. The data directory is root-owned, so an
//     unprivileged caller would fail on the open anyway — but failing on
//     a permission error is not the same as refusing, and a recovery
//     path should say what it requires rather than let the filesystem
//     say it by accident.
//  3. It is audited. A credential that can mint an Owner and leaves no
//     trace is indistinguishable from an attacker who found the host.
//
// It deliberately does NOT set a password. Wildstone does not hold one
// (ADR-W020 A4) — the directory does, and `reset-password` drives the
// directory rather than storing anything here.

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// geteuid is indirected so the refusal can be tested. The test suite
// does not get to run as two different users, and a root check nothing
// exercises is a root check that stops working silently.
var geteuid = os.Geteuid

// hostActor names who ran a recovery command, for the audit row.
//
// SUDO_USER before the effective user, because that is the question the
// audit is actually asking: `root` is true of every recovery and tells
// nobody anything, where "mika, via sudo" is the person to ask about
// it. Falling back to the effective user keeps a direct root shell
// honest rather than blank.
func hostActor() string {
	if via := strings.TrimSpace(os.Getenv("SUDO_USER")); via != "" {
		return via + " (via sudo)"
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid " + strconv.Itoa(geteuid())
}

func ownerCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage:\n" +
			"  keystone owner create <username>          record an owner and admit them\n" +
			"  keystone owner reset-password <username>  hand them a one-time code for the directory\n" +
			"  keystone owner list                       who the household has on record\n" +
			"\nThese require root and are not reachable over the API (design §2.6).")
	}
	switch args[0] {
	case "create":
		return ownerCreate(args[1:])
	case "reset-password":
		return ownerResetPassword(args[1:])
	case "list":
		return ownerList(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; try create, reset-password or list", args[0])
	}
}

// requireRoot refuses an unprivileged caller before anything is opened.
//
// Said plainly rather than left to the filesystem: "permission denied on
// keystone.db" is what an operator sees today, and it reads like a
// broken install rather than a deliberate boundary.
func requireRoot(what string) error {
	if geteuid() != 0 {
		return fmt.Errorf("keystone owner %s needs root: it is the host recovery path (design §2.6), "+
			"so it is deliberately not something a service account or an API token can reach — try sudo", what)
	}
	return nil
}

// ownerFlags is the data-dir pair every subcommand takes, matching
// person-refs so an operator who has used one knows this one.
func ownerFlags(name string, args []string) (string, *flag.FlagSet, error) {
	fs := flag.NewFlagSet("owner "+name, flag.ExitOnError)
	dataDir := fs.String("data-dir", "/var/lib/wildstone/keystone", "directory holding keystone.db")
	dbPath := fs.String("db", "", "path to keystone.db; overrides -data-dir")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	path := *dbPath
	if path == "" {
		path = *dataDir + "/keystone.db"
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil, fmt.Errorf("no database at %s: %w", path, err)
	}
	return path, fs, nil
}

// ownerCreate records a person as an Owner and admits them, creating the
// row if it is not there and promoting it if it is.
//
// It answers the two lockouts that are actually reachable. "Every Owner
// disabled" is NOT one of them: the Last Owner invariant is a database
// trigger, so it holds against a direct UPDATE as well as the API and a
// household cannot arrive at zero active Owners by any road. What it can
// arrive at is nobody recorded at all — the state of every deployment
// until the installer creates the first Owner — and somebody recorded as
// the wrong thing, which is the Console holder slice 3 now refuses.
//
// Idempotent on purpose. An operator who runs it twice because the first
// run scrolled past should get the same household, not an error that
// makes them wonder what half-happened.
func ownerCreate(args []string) error {
	if err := requireRoot("create"); err != nil {
		return err
	}
	path, fs, err := ownerFlags("create", args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: keystone owner create <username>")
	}
	member := strings.TrimSpace(fs.Arg(0))
	if member == "" {
		return fmt.Errorf("a username is required")
	}

	db, err := runtimedb.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer db.Close()

	actor := hostActor()
	// wasFound, rather than reading the outer `err` further down. Both
	// blocks below shadow `err`, so a later `err == nil` would be
	// testing this lookup's leftover value while reading as "the call
	// above succeeded" — right today only because RecordButlerMember
	// hardcodes RoleOwner, and silently wrong the day it does not.
	existing, err := db.GetButlerMember(member)
	wasFound := err == nil
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		if err := db.RecordButlerMember(member, runtimedb.RoleOwner, "", ""); err != nil {
			return fmt.Errorf("recording %s: %w", member, err)
		}
	case err != nil:
		return fmt.Errorf("reading %s: %w", member, err)
	}

	// Admitting IS the reinstatement: it clears disabled_at and
	// offboarded_at and bumps the generation. One verb rather than two
	// that could disagree about what "active" means.
	if err := db.AdmitButlerMember(member, actor); err != nil {
		return fmt.Errorf("admitting %s: %w", member, err)
	}
	// Role after admission, so the change lands on an active row. A
	// Member being repaired into an Owner is the ordinary case here, not
	// an exception: it is what a Console holder who was recorded as the
	// wrong thing needs.
	if wasFound && existing.Role != runtimedb.RoleOwner {
		if err := db.SetButlerMemberRole(member, runtimedb.RoleOwner, actor); err != nil {
			return fmt.Errorf("making %s an owner: %w", member, err)
		}
	}

	auditRecovery(db, actor, "create", member)
	fmt.Printf("%s is an owner and admitted.\n", member)
	fmt.Printf("They still need a directory account to sign in with; "+
		"`keystone owner reset-password %s` hands them a one-time code for it.\n", member)
	return nil
}

// ownerResetPassword points the operator at the directory's own reset
// code, and stores nothing.
//
// Wildstone does not hold the password and this does not change that
// (ADR-W020 A4): LLDAP writes a one-time code when a reset is requested,
// and the person exchanges it for a password they choose. So this reads
// the code out and says what to do with it — the same exchange the
// Console's Identity screen drives, available when the Console is not.
func ownerResetPassword(args []string) error {
	if err := requireRoot("reset-password"); err != nil {
		return err
	}
	path, fs, err := ownerFlags("reset-password", args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: keystone owner reset-password <username>")
	}
	member := strings.TrimSpace(fs.Arg(0))
	if member == "" {
		return fmt.Errorf("a username is required")
	}

	db, err := runtimedb.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer db.Close()

	actor := hostActor()
	auditRecovery(db, actor, "reset-password", member)

	// Instructions rather than an action, and they point at Authelia
	// rather than at LLDAP.
	//
	// The first draft of this printed an `lldap_set_password` command.
	// That command cannot be run: it needs an admin token, the token
	// needs the directory's service password, and since the installer
	// started generating that password into a write-only Secret there
	// is no way for a person to read it back. A break-glass instruction
	// nobody can execute is worse than none, because it looks like a
	// way out.
	//
	// Authelia's password reset needs none of that. It binds to the
	// directory itself, writes a one-time code to the notification file,
	// and the person exchanges it for a password they choose — which is
	// also the path every household member after the first one takes.
	fmt.Printf("Wildstone does not hold %s's password and will not set one.\n\n", member)
	fmt.Println("Have them choose \"forgotten password\" at the sign-in page, then")
	fmt.Println("read the one-time code back and hand it over. Either of:")
	fmt.Println()
	fmt.Println("  - the Console's Identity screen, which reads it for you; or")
	fmt.Println("  - on the node running the identity capability, the file")
	fmt.Println("    Authelia writes it to:")
	fmt.Println("      cat /srv/wildstone/identity/notification.txt")
	fmt.Println()
	fmt.Println("Asking for another code REPLACES the pending one, so read it")
	fmt.Println("once and hand it over before asking again.")
	fmt.Println()
	fmt.Println("They exchange it for a password they choose. Nothing about it is")
	fmt.Println("recorded here — this command wrote an audit row saying it was run,")
	fmt.Println("and nothing else.")
	return nil
}

// ownerList is the diagnosis that comes before either repair: who the
// household has on record, and which of them can actually get in.
//
// It exists because the failure this command set answers — a Console
// that refuses everybody — looks identical whether nobody is recorded or
// the recorded Owner is disabled, and the two need different fixes.
func ownerList(args []string) error {
	if err := requireRoot("list"); err != nil {
		return err
	}
	path, _, err := ownerFlags("list", args)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("opening %s read-only: %w", path, err)
	}
	defer db.Close()
	return writeOwnerList(os.Stdout, db)
}

func writeOwnerList(w io.Writer, db *sql.DB) error {
	rows, err := db.Query(
		`SELECT member, role, admitted, disabled_at, offboarded_at
		 FROM butler_members ORDER BY role, member`)
	if err != nil {
		return fmt.Errorf("reading the household: %w", err)
	}
	defer rows.Close()
	owners := 0
	any := false
	for rows.Next() {
		var member, role, disabled, offboarded string
		var admitted int
		if err := rows.Scan(&member, &role, &admitted, &disabled, &offboarded); err != nil {
			return err
		}
		any = true
		state := "active"
		switch {
		case offboarded != "":
			state = "offboarded"
		case disabled != "":
			state = "disabled"
		case admitted == 0:
			state = "not admitted"
		default:
			if role == runtimedb.RoleOwner {
				owners++
			}
		}
		fmt.Fprintf(w, "%-24s %-6s %s\n", member, role, state)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !any {
		fmt.Fprintln(w, "Nobody is on record.")
		fmt.Fprintln(w, "The Console still admits whoever the directory says, because a household")
		fmt.Fprintln(w, "that has never had an owner has made no statement about who administers")
		fmt.Fprintln(w, "it. `keystone owner create <username>` makes the first one.")
		return nil
	}
	if owners == 0 {
		fmt.Fprintln(w, "\nNo ACTIVE owner. The Console refuses everybody in this state.")
		fmt.Fprintln(w, "`keystone owner create <username>` repairs it.")
	}
	return nil
}

// auditRecovery records that a recovery command ran.
//
// Best-effort on the write, and that is the right trade: a recovery
// command that refused to repair a locked-out household because it could
// not write its own audit row would be a boundary doing the opposite of
// its job. The failure is said out loud instead, so it is not silent.
//
// Method and Path carry the command rather than HTTP, because that is
// what happened. An audit row that dressed a root shell up as a request
// would be a more convenient shape and a less true one.
func auditRecovery(db *runtimedb.DB, actor, verb, member string) {
	if err := db.AppendAudit(runtimedb.AuditRecord{
		Principal: actor,
		ActingFor: member,
		Method:    "CLI",
		Path:      "keystone owner " + verb,
		Status:    200,
		Remote:    "host",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write the audit row: %v\n", err)
	}
}
