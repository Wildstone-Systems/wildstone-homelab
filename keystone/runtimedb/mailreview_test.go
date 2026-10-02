package runtimedb

// Epic #326 S06 at the store: a review stores no token, a closed review
// never reopens, a restore or a new key leaves no usable token, a
// revocation closes pending approvals, and the conflict calendars are a
// material, backward-compatible part of the authorization.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (f *candidateFixture) issue(t *testing.T, id string) (MailReview, map[string]string) {
	t.Helper()
	g, err := f.db.AuthorizeBackgroundUse(context.Background(), "sam-mail", f.sub(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.db.MailCandidateInternal(id)
	if err != nil {
		t.Fatal(err)
	}
	r, tokens, err := f.db.IssueMailReview(f.vault, MailReviewIssue{Candidate: id, Revision: c.Revision, Kind: MailReviewCreate,
		TargetCalendar: g.TargetCalendar, Grant: g, Route: MailRouteNotice, Availability: MailAvailabilityFree, At: mailNow})
	if err != nil {
		t.Fatal(err)
	}
	return r, tokens
}

func (f *candidateFixture) sub(t *testing.T) int64 {
	t.Helper()
	a, err := f.db.LiveBackgroundAuthorization("sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	return a.Subscription
}

func TestReviewTokensAreNeverStored(t *testing.T) {
	f := newCandidateFixture(t)
	c := onlyNew(t, f.record(t, "mfp1_aaa", draft(schoolNight())))
	_, tokens := f.issue(t, c.ID)
	if len(tokens) != 3 {
		t.Fatalf("tokens %v", tokens)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if _, err := f.db.sql.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if bytes.Contains(raw, []byte(tok)) || bytes.Contains(raw, []byte(strings.TrimPrefix(tok, "mrt_"))) {
			t.Fatal("the database holds a review token")
		}
	}
	// A different key (a restore onto a fresh data directory) derives
	// tokens that match nothing.
	other, err := f.db.OpenMailExcerptVault(filepath.Join(t.TempDir(), "other.key"))
	if err != nil {
		t.Fatal(err)
	}
	_, derived, err := f.db.MailReviewTokens(other, "sam", c.ID, mailNow)
	if err != nil || len(derived) != 0 {
		t.Fatalf("another key derived %v (%v)", derived, err)
	}
}

func TestAClosedReviewNeverReopensAndRestoreRevokesOpenOnes(t *testing.T) {
	f := newCandidateFixture(t)
	c := onlyNew(t, f.record(t, "mfp1_aaa", draft(schoolNight())))
	r, _ := f.issue(t, c.ID)
	if err := purgeMailOnRestore(f.db); err != nil {
		t.Fatal(err)
	}
	got, err := f.db.MailReviewFor("sam", c.ID)
	if err != nil || got.State != MailReviewRevoked || got.ID != r.ID {
		t.Fatalf("after restore: %+v %v", got, err)
	}
	if _, err := f.db.sql.Exec(`UPDATE mail_reviews SET state = 'open' WHERE id = ?`, r.ID); err == nil {
		t.Fatal("a revoked review was reopened")
	}
}

func TestRevocationClosesPendingApprovals(t *testing.T) {
	f := newCandidateFixture(t)
	c := onlyNew(t, f.record(t, "mfp1_aaa", draft(schoolNight())))
	f.issue(t, c.ID)
	if _, err := f.db.PurgeMailConnection("sam-mail", "sam", "the mailbox owner revoked the monitor", mailNow); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.MailReviewFor("sam", c.ID); got.State != MailReviewRevoked {
		t.Fatalf("review after revocation: %s", got.State)
	}
	_, err := f.db.ClaimMailReview(MailReviewClaim{Actor: "sam", Candidate: c.ID, Revision: 1, Action: MailActionDismiss, Via: "console", At: mailNow})
	if !errors.Is(err, ErrMailReviewRefused) {
		t.Fatalf("claim after revocation: %v", err)
	}
}

func TestConflictCalendarsAreMaterialAndOptional(t *testing.T) {
	f := newMailFixture(t)
	without, err := f.db.PrepareBackgroundAuthorization("sam", f.request(0))
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := json.Marshal(without.Scope)
	if strings.Contains(string(scope), "conflictCalendars") {
		t.Fatalf("an authorization without conflict calendars changed shape: %s", scope)
	}
	req := f.request(0)
	req.Scope.ConflictCalendars = []string{"household", " household ", "school"}
	with, err := f.db.PrepareBackgroundAuthorization("sam", req)
	if err != nil {
		t.Fatal(err)
	}
	if with.Digest == without.Digest || strings.Join(with.Scope.ConflictCalendars, ",") != "household,school" {
		t.Fatalf("conflict calendars: %v, digest changed %v", with.Scope.ConflictCalendars, with.Digest != without.Digest)
	}
	req.Scope.ConflictCalendars = make([]string, maxConflictCalendars+1)
	for i := range req.Scope.ConflictCalendars {
		req.Scope.ConflictCalendars[i] = "c" + string(rune('a'+i))
	}
	if _, err := f.db.PrepareBackgroundAuthorization("sam", req); err == nil {
		t.Fatal("an unbounded conflict calendar list was accepted")
	}
}

func TestMailEventWindow(t *testing.T) {
	f := schoolNight()
	if s, e, ok := MailEventWindow(f); !ok || e.Sub(s) != 90*time.Minute {
		t.Fatalf("window %v %v %v", s, e, ok)
	}
	f.End = time.Time{}
	f.DurationMinutes = 30
	if s, e, ok := MailEventWindow(f); !ok || e.Sub(s) != 30*time.Minute {
		t.Fatal("duration")
	}
	f.DurationMinutes = 0
	if _, _, ok := MailEventWindow(f); ok {
		t.Fatal("an event with no end has a window")
	}
}
