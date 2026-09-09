package runtimedb

// The quarantine gates (ADR-W030 A10, P16).
//
// A10 says a restored database holds four things: retrieval, task
// dispatch, ingestion and proactive delivery. "Holds" is a claim about
// every caller, not about the HTTP routes, so this test calls the store
// directly — the way a background loop or a chat adapter would.
//
// The second half is the part that is easy to get wrong. Quarantine is
// released by REPLAYING deletions over the restored state, and the
// replay is itself a series of store calls. Gating those too would make
// quarantine unreleasable: the hold would block the only thing that can
// lift it. So the rule is narrower than "nothing runs" — quarantine
// blocks doing and permits undoing.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestQuarantineHoldsEveryButlerFacingPath(t *testing.T) {
	db := butlerTestDB(t)
	const admin = "mika"

	for _, m := range []struct{ name, profile string }{
		{admin, ProfileOperator}, {"sam", ProfileHousehold},
	} {
		if err := db.RecordButlerMember(m.name, m.profile, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.LinkSurfaceIdentity(LinkAPIPrincipal, m.name, m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}

	// State laid down BEFORE the restore, so the gated calls below have
	// something real to act on and are refused for being gated rather
	// than for being empty.
	memory, err := db.RecordMemory(MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "the bins go out on Thursday",
		Audience: ButlerAudienceShared, PermissionLabel: "household",
		Owner: "household", Author: "sam", SourceKind: "conversation", SourceID: "q-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := db.RecordMemory(MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "the spare key is under the mat",
		Audience: ButlerAudienceShared, PermissionLabel: "household",
		Owner: "household", Author: "sam", SourceKind: "conversation", SourceID: "q-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.IndexDocumentInto(IndexDocument{
		SourceKind: "memory", SourceID: "q-1", MemorySeq: memory, Owner: "household",
		Audience: ButlerAudienceShared, PermissionLabel: "household",
		Content: "the bins go out on Thursday", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	task := newAcceptanceTask(t, db, admin, ButlerAudienceShared, "put the bins out")
	step, err := db.AppendButlerTaskStep(task, ButlerTaskStep{
		Kind: ButlerStepTool, Name: "remind",
	})
	if err != nil {
		t.Fatal(err)
	}

	// ---- the restore.
	if err := db.EnterRestoreQuarantine("restored from the nightly backup",
		time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Every category A10 names is refused, and each says which category
	// refused it — an operator reading "task dispatch is held" learns
	// more than one reading "quarantined".
	for _, c := range []struct {
		what string
		gate string
		call func() error
	}{
		{"RetrieveMemory", GateRetrieval, func() error {
			_, err := db.RetrieveMemory(MemoryQuery{Reader: admin})
			return err
		}},
		{"ExportMemoryFor", GateRetrieval, func() error {
			_, err := db.ExportMemoryFor(admin)
			return err
		}},
		{"SearchIndex", GateRetrieval, func() error {
			_, err := db.SearchIndex(IndexQuery{Reader: admin, Text: "bins"})
			return err
		}},
		{"StartButlerAttempt", GateDispatch, func() error {
			_, err := db.StartButlerAttempt(step)
			return err
		}},
		{"ClaimIntegrationWork", GateDispatch, func() error {
			_, _, err := db.ClaimIntegrationWork("adapter-1")
			return err
		}},
		{"RecordMemory", GateIngestion, func() error {
			_, err := db.RecordMemory(MemoryRecord{
				Kind: MemoryFact, Subject: "sam", Content: "learned during quarantine",
				Audience: ButlerAudienceShared, PermissionLabel: "household",
				Owner: "household", Author: "sam", SourceKind: "conversation", SourceID: "q-3",
			})
			return err
		}},
		{"DeriveMemory", GateIngestion, func() error {
			_, err := db.DeriveMemory(MemoryRecord{
				Kind: MemoryInference, Subject: "sam", Content: "bins and keys",
				Audience: ButlerAudienceShared, PermissionLabel: "household",
				Owner: "household", Author: "butler", SourceKind: "derivation", SourceID: "q-4",
			}, []int64{memory})
			return err
		}},
		{"IndexDocumentInto", GateIngestion, func() error {
			_, err := db.IndexDocumentInto(IndexDocument{
				SourceKind: "memory", SourceID: "q-5", MemorySeq: doomed, Owner: "household",
				Audience: ButlerAudienceShared, PermissionLabel: "household",
				Content: "the spare key is under the mat", ObservedAt: time.Now(),
			})
			return err
		}},
		{"AppendHouseholdEvent", GateIngestion, func() error {
			_, _, err := db.AppendHouseholdEvent(HouseholdEvent{
				EventID: "q-ev-1", Source: "publisher-home", Type: "person.left_home",
				OccurredAt: time.Now(), Subject: "sam", Payload: json.RawMessage(`{}`),
			})
			return err
		}},
		{"RaiseNotice", GateDelivery, func() error {
			_, err := db.RaiseNotice(NoticeRequest{
				Kind: "task-stalled", Recipient: admin, DedupeKey: "q-n-1",
			}, household(), midday())
			return err
		}},
		{"EnqueueChatOutbound(push)", GateDelivery, func() error {
			_, err := db.EnqueueChatOutbound(ChatOutbound{
				Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "sam",
				Body: "the bins are due", Kind: "push", DedupeKey: "q-o-1",
			})
			return err
		}},
	} {
		err := c.call()
		if !errors.Is(err, ErrQuarantined) {
			t.Fatalf("%s ran under quarantine: %v", c.what, err)
		}
		if got := err.Error(); !strings.Contains(got, c.gate) {
			t.Fatalf("%s was refused without naming %q: %v", c.what, c.gate, err)
		}
	}

	// A reply is not proactive delivery. Somebody asked a question and
	// is waiting for the answer; cutting them off mid-conversation is a
	// worse failure than the one quarantine is preventing, and it tells
	// them nothing about why.
	if _, err := db.EnqueueChatOutbound(ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "sam",
		Body: "the database was restored and is being reconciled", Kind: "reply",
		DedupeKey: "q-o-2",
	}); err != nil {
		t.Fatalf("a reply to a waiting person was refused under quarantine: %v", err)
	}

	// ---- undoing still works, or quarantine could never be released.
	// These are the calls ReconcileRestore itself makes.
	if err := db.ForgetMemory(doomed, "root-admin"); err != nil {
		t.Fatalf("ForgetMemory under quarantine: %v", err)
	}
	if _, err := db.RemoveIndexForMemory(doomed); err != nil {
		t.Fatalf("RemoveIndexForMemory under quarantine: %v", err)
	}
	if err := db.RevokeSurfaceLink(LinkAPIPrincipal, "sam", "root-admin"); err != nil {
		t.Fatalf("RevokeSurfaceLink under quarantine: %v", err)
	}
	if err := db.OffboardButlerMember("sam", "root-admin"); err != nil {
		t.Fatalf("OffboardButlerMember under quarantine: %v", err)
	}

	// ---- release, and the whole surface comes back.
	if err := db.AssertJournalCurrent("root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReconcileRestore(); err != nil {
		t.Fatalf("ReconcileRestore under quarantine: %v", err)
	}
	if err := db.ReleaseRestoreQuarantine("root-admin"); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		what string
		call func() error
	}{
		{"RetrieveMemory", func() error {
			_, err := db.RetrieveMemory(MemoryQuery{Reader: admin})
			return err
		}},
		{"SearchIndex", func() error {
			_, err := db.SearchIndex(IndexQuery{Reader: admin, Text: "bins"})
			return err
		}},
		{"StartButlerAttempt", func() error {
			_, err := db.StartButlerAttempt(step)
			return err
		}},
		{"ClaimIntegrationWork", func() error {
			_, _, err := db.ClaimIntegrationWork("adapter-1")
			return err
		}},
		{"AppendHouseholdEvent", func() error {
			_, _, err := db.AppendHouseholdEvent(HouseholdEvent{
				EventID: "q-ev-2", Source: "publisher-home", Type: "person.left_home",
				OccurredAt: time.Now(), Subject: admin, Payload: json.RawMessage(`{}`),
			})
			return err
		}},
		{"RaiseNotice", func() error {
			_, err := db.RaiseNotice(NoticeRequest{
				Kind: "task-stalled", Recipient: admin, DedupeKey: "q-n-2",
			}, household(), midday())
			return err
		}},
		{"EnqueueChatOutbound(push)", func() error {
			_, err := db.EnqueueChatOutbound(ChatOutbound{
				Surface: ButlerSurfaceFamily, Transport: "telegram", Target: admin,
				Body: "the bins are due", Kind: "push", DedupeKey: "q-o-3",
			})
			return err
		}},
	} {
		if err := c.call(); errors.Is(err, ErrQuarantined) {
			t.Fatalf("%s is still held after the quarantine was released: %v", c.what, err)
		} else if err != nil {
			t.Fatalf("%s after release: %v", c.what, err)
		}
	}
}
