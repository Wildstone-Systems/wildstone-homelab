package adapter

import (
	"encoding/json"
	"testing"
)

// Keystone sends requestJson as a string holding JSON; a fake may send
// the object itself. Both must reach an Upstream as the request.
func TestWorkDecodesBothRequestShapes(t *testing.T) {
	for _, body := range []string{
		`{"seq":1,"requestJson":"{\"folder\":\"INBOX\"}","epoch":3}`,
		`{"seq":1,"requestJson":{"folder":"INBOX"},"epoch":3}`,
	} {
		var w Work
		if err := json.Unmarshal([]byte(body), &w); err != nil {
			t.Fatal(err)
		}
		if string(w.RequestJSON) != `{"folder":"INBOX"}` || w.Epoch != 3 {
			t.Fatalf("%s decoded to %s epoch %d", body, w.RequestJSON, w.Epoch)
		}
	}
	var w Work
	if err := json.Unmarshal([]byte(`{"requestJson":"not json"}`), &w); err == nil {
		t.Fatal("a string that is not JSON was accepted as a request")
	}
}
