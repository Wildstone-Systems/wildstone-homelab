package runtimedb

import (
	"errors"
	"testing"
)

// An invite is recorded once, listed for its Person alone, and closed by
// the binding it was for (ADR-W033 B7.1).
func TestAnInviteClosesWhenThePersonBinds(t *testing.T) {
	db := householdWith(t, "mika", "sam")
	at := connect(t, db, "joan", "telegram", ButlerSurfaceFamily)

	inv, err := db.InviteToConnection(at.Seq, "mika", "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	again, err := db.InviteToConnection(at.Seq, "mika", "root-admin")
	if err != nil || again.Seq != inv.Seq {
		t.Fatalf("inviting twice made a second invite: %+v %v", again, err)
	}
	if inv.Butler != "joan" || inv.Transport != "telegram" || inv.Member != "mika" || inv.Surface != ButlerSurfaceFamily {
		t.Errorf("invite = %+v", inv)
	}
	if got, _ := db.OpenInvitesFor("sam"); len(got) != 0 {
		t.Errorf("sam sees mika's invite: %+v", got)
	}
	if _, err := db.OpenInvite(inv.Seq, "sam"); !errors.Is(err, ErrNoInvite) {
		t.Errorf("sam can open mika's invite: %v", err)
	}

	bind(t, db, at, "mika", "12345")
	if got, _ := db.OpenInvitesFor("mika"); len(got) != 0 {
		t.Errorf("the invite is still open after mika bound: %+v", got)
	}
}

func TestAWithdrawnInviteIsGone(t *testing.T) {
	db := householdWith(t, "mika")
	at := connect(t, db, "joan", "telegram", ButlerSurfaceFamily)
	inv, err := db.InviteToConnection(at.Seq, "mika", "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithdrawInvite(inv.Seq, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if err := db.WithdrawInvite(inv.Seq, "again"); !errors.Is(err, ErrNoInvite) {
		t.Errorf("withdrawing twice: %v", err)
	}
	if _, err := db.OpenInvite(inv.Seq, "mika"); !errors.Is(err, ErrNoInvite) {
		t.Errorf("a withdrawn invite opened: %v", err)
	}
	// Inviting again after a withdrawal is a fresh invite.
	if next, err := db.InviteToConnection(at.Seq, "mika", "root-admin"); err != nil || next.Seq == inv.Seq {
		t.Errorf("re-invite = %+v %v", next, err)
	}
}

func TestInvitingNobodyIsRefused(t *testing.T) {
	db := householdWith(t, "mika")
	at := connect(t, db, "joan", "telegram", ButlerSurfaceFamily)
	if _, err := db.InviteToConnection(at.Seq, "nobody", "root-admin"); err == nil {
		t.Error("an invite for somebody with no Person record was recorded")
	}
}
