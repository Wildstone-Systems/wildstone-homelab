package api

// One member's conversation history, in a household where somebody else
// does most of the talking.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// A quiet member still sees their own history behind a chatty one.
//
// The list used to read a fixed number of the newest family sessions
// and filter by member afterwards, which is not a filter in the only
// household it mattered in: a partner who owns the newest 200 fills
// every row that is read, all 200 are discarded, and the quiet member
// is shown nothing beneath a heading that says it is their history.
func TestAChattyMemberDoesNotHideAnothersHistory(t *testing.T) {
	h := homeHarness(t)

	// Sandy's conversations are the OLDEST, so anything that reads a
	// window off the top and filters afterwards loses them.
	for i := range 3 {
		insertFamilySession(t, h, "sandy", fmt.Sprintf("Sandy asked about the bins, %d", i))
	}
	for i := range 250 {
		insertFamilySession(t, h, "maya", fmt.Sprintf("Maya asked about the car, %d", i))
	}

	h.unthrottled()
	res := h.member(http.MethodGet, "/api/v1/assistant/conversations", "sandy", "assistant_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("conversations: status %d", res.StatusCode)
	}
	var got struct {
		Conversations []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"conversations"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Conversations) != 3 {
		t.Fatalf("sandy sees %d of her own conversations, want 3", len(got.Conversations))
	}
	// And not one of maya's came with them.
	for _, c := range got.Conversations {
		if c.Title == "" || c.Title[:5] != "Sandy" {
			t.Errorf("sandy's list contains %q", c.Title)
		}
	}
}

func insertFamilySession(t *testing.T, h *harness, member, text string) {
	t.Helper()
	if _, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "portal", ActingFor: member, Provider: "test",
		Surface: runtimedb.ButlerSurfaceFamily, Butler: "sandy", FinalText: text,
	}); err != nil {
		t.Fatalf("InsertButlerSession(%s): %v", member, err)
	}
}
