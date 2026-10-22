package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/automation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/mailmonitor"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// The scheduled family this process starts is gated by the real
// Personal evaluator, refuses mail.changes until its consumer registers,
// and holds itself without the coordinator lease.
func TestScheduledSubscriptionsWiring(t *testing.T) {
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "keystone.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Not the coordinator: the whole family is held.
	held := newScheduledSubscriptions(db, nil, log, nil)
	if _, err := held.TickScheduled(context.Background()); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("a tick without the coordinator lease = %v, want fenced", err)
	}

	lease, err := db.AcquireCoordinator("subscriptions-wiring-test")
	if err != nil {
		t.Fatal(err)
	}
	e := newScheduledSubscriptions(db, butlerstore.New(db, lease), log, nil)
	if _, ok := e.Authority.(automation.PersonalBackgroundAuthority); !ok {
		t.Fatalf("authority is %T, want the Personal evaluator", e.Authority)
	}
	if err := e.Authority.AuthorizeBackgroundUse(context.Background(), automation.BackgroundUse{Subscription: 1}); !errors.Is(err, automation.ErrBackgroundUseDenied) {
		t.Fatalf("a sourceless use = %v, want denied", err)
	}
	d, ok := e.Dispatchers[MailChangesMatchKind]
	if !ok {
		t.Fatal("mail.changes has no registered dispatcher; it would fall back to an Agent Task nothing completes")
	}
	if _, err := d.DispatchScheduled(context.Background(), automation.ScheduledRun{}); err == nil {
		t.Fatal("the placeholder mail.changes dispatcher accepted a run")
	}
	if _, err := e.TickScheduled(context.Background()); err != nil {
		t.Fatalf("a coordinator tick with nothing due = %v", err)
	}

	// With the mailbox monitor built, mail.changes is its dispatcher.
	mail, err := newMailMonitor(db, t.TempDir(), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	withMail := newScheduledSubscriptions(db, butlerstore.New(db, lease), log, mail)
	if _, ok := withMail.Dispatchers[MailChangesMatchKind].(*mailmonitor.Dispatcher); !ok {
		t.Fatalf("mail.changes dispatcher is %T, want the mailbox monitor", withMail.Dispatchers[MailChangesMatchKind])
	}
}

// main starts the family inside the coordinator-held block, on the
// runner's own ledger — not before the lease is known.
func TestScheduledSubscriptionsStartUnderTheLease(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	call := strings.Index(body, "startScheduledSubscriptions(")
	if call < 0 {
		t.Fatal("main.go does not start the scheduled subscription family")
	}
	if strings.Count(body, "startScheduledSubscriptions(") != 1 {
		t.Fatal("the scheduled family is started more than once")
	}
	block := regexp.MustCompile(`if coordinating \{\n\t+ledger := butlerstore\.New\(db, lease\)`).FindStringIndex(body)
	if block == nil || block[0] > call {
		t.Fatal("the scheduled family must start inside the coordinator-held block that builds the ledger")
	}
}
