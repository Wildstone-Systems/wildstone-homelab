package integration

import (
	"regexp"
	"strings"
	"testing"
)

// Every name is unique, lives in its own domain's namespace, and is well
// formed; every action proposes a real class.
func TestTheVocabularyIsWellFormed(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	seen := map[string]bool{}
	for _, d := range Domains {
		names := []string{}
		for _, q := range d.Queries {
			names = append(names, q.Name)
		}
		for _, a := range d.Actions {
			names = append(names, a.Name)
			if a.ProposedClass != LowImpact && a.ProposedClass != Consequential {
				t.Errorf("%s proposes class %q", a.Name, a.ProposedClass)
			}
		}
		names = append(names, d.Events...)
		for _, n := range names {
			if !shape.MatchString(n) || !strings.HasPrefix(n, d.Name+".") {
				t.Errorf("%q is not a %s.* name", n, d.Name)
			}
			if seen[n] {
				t.Errorf("%q is declared twice", n)
			}
			seen[n] = true
		}
	}
}

func TestLookups(t *testing.T) {
	if !IsQuery("pantry.expiring") || IsQuery("pantry.expirng") || IsQuery("pantry.add") {
		t.Fatal("IsQuery")
	}
	if !IsAction("pantry.cook.finish") || IsAction("pantry.list") || IsAction("calendar.add") {
		t.Fatal("IsAction")
	}
	if !IsEvent("pantry.timer_finished") || IsEvent("pantry.timer_done") {
		t.Fatal("IsEvent")
	}
	if _, ok := DomainOf("pantry.list"); !ok {
		t.Fatal("DomainOf pantry")
	}
	if _, ok := DomainOf("person.left_home"); ok {
		t.Fatal("person has no declared domain")
	}
}

// The mail domain is read-only by construction (epic #326): no action at
// all, and no query whose name reads like a write. A mutating mail
// operation is a new decision, not a vocabulary addition.
func TestMailIsReadOnly(t *testing.T) {
	if len(Mail.Actions) != 0 {
		t.Fatalf("mail declares actions %+v; the mailbox surface is read-only", Mail.Actions)
	}
	mutating := regexp.MustCompile(`(send|reply|draft|move|archive|delete|expunge|flag|mark|seen|store|copy|append|rule|create|rename|subscribe)`)
	for _, q := range Mail.Queries {
		if mutating.MatchString(strings.TrimPrefix(q.Name, "mail.")) {
			t.Errorf("mail query %q reads like a write", q.Name)
		}
	}
	for _, name := range []string{"mail.send", "mail.move", "mail.delete", "mail.mark_read", "mail.draft"} {
		if IsQuery(name) || IsAction(name) {
			t.Errorf("%s is declared", name)
		}
	}
	if q, ok := LookupQuery("mail.message"); !ok || len(q.Params) != 1 || !q.Params[0].Required {
		t.Fatalf("LookupQuery(mail.message) = %+v, %v", q, ok)
	}
	if _, ok := Lookup("mail"); !ok {
		t.Fatal("Lookup(mail)")
	}
}
