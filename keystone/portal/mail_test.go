package portal

// The Homepage's Watch this mailbox page (epic #326 S07, #460).
//
// A browser shows whether the page renders; it does not show which
// door the page knocks on, whether the password is still in the field
// when the request leaves, or whether an Owner-only step is attempted
// and fails instead of being handed to an Owner. Those are held here.

import (
	"regexp"
	"strings"
	"testing"
)

func mailPage(t *testing.T) string {
	t.Helper()
	body, ok := Page("mail")
	if !ok {
		t.Fatal("the mailbox page is not served")
	}
	return string(body)
}

// Every request goes through the Person's own Homepage session, and
// never to the Console's routes or to an Owner-only one.
func TestTheMailboxPageUsesOnlyTheHomepageDoor(t *testing.T) {
	page := stripComments(mailPage(t))
	if !strings.Contains(page, `fetch("/api/v1/home" + path`) {
		t.Fatal("the page does not send its requests through /api/v1/home")
	}
	if n := strings.Count(page, "fetch("); n != 1 {
		t.Errorf("the page fetches from %d places; every request goes through the one api() door", n)
	}
	for _, owner := range []string{"adapter/render", "/placement", `"/integrations"`, "credential/erase", "/api/v1/integrations", "/api/v1/mail"} {
		if strings.Contains(page, owner) {
			t.Errorf("the page reaches for %s, which is the Console's or an Owner's", owner)
		}
	}
	// What only an Owner can do is handed to an Owner, in these words.
	if !strings.Contains(page, `"Ask an Owner to prepare mailbox watching"`) {
		t.Error("the Owner-only steps do not say to ask an Owner")
	}
	for _, stage := range []string{`case "not-installed":`, `case "no-connection":`, `case "not-rendered":`} {
		i := strings.Index(page, stage)
		if i < 0 || !strings.Contains(page[i:min(len(page), i+400)], "askOwner(") {
			t.Errorf("%s does not ask an Owner", stage)
		}
	}
}

// The password is a password field, and is out of it before the request
// that carries it is sent.
func TestTheMailboxPasswordGoesOneWay(t *testing.T) {
	page := stripComments(mailPage(t))
	for _, want := range []string{`pw.type = "password"`, `pw.autocomplete = "new-password"`, "pw.spellcheck = false"} {
		if !strings.Contains(page, want) {
			t.Errorf("the password field is missing %s", want)
		}
	}
	cleared := strings.Index(page, `pw.value = "";`)
	sent := strings.Index(page, `connPath(mon.connection.name, "credential")`)
	if cleared < 0 || sent < 0 || cleared > sent {
		t.Error("the password field is not emptied before the credential is sent")
	}
	// It is sent as what was typed, from a local, and from nowhere else.
	if n := regexp.MustCompile(`password:`).FindAllStringIndex(page, -1); len(n) != 1 || !strings.Contains(page, "password: typed") {
		t.Error("the page sends a password from anything but what was just typed")
	}
}

// The two approvals are two states, and the review is answered with the
// review's own token for the one action pressed.
func TestTheMailboxPageKeepsTheTwoApprovalsApart(t *testing.T) {
	page := stripComments(mailPage(t))
	for _, want := range []string{`"Waiting for Owner approval"`, `"Authorized by you"`, `"Not yet authorized by you"`, `"Approved by an Owner"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never says %s", want)
		}
	}
	if !strings.Contains(page, "d.tokens[action]") {
		t.Error("an action is not answered with the review's token for it")
	}
	for _, action := range []string{`send("add")`, `send("apply")`, `send("edit", fields)`, `send("dismiss")`} {
		if !strings.Contains(page, action) {
			t.Errorf("the review offers no %s", action)
		}
	}
}
