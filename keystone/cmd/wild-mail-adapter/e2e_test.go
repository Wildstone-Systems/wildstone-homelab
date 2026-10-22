package main

// Epic #326 S04, end to end: the wild-mail-adapter program — config from
// its rendered environment, readerFor, mail.Upstream on adapter.Runner —
// against a fake control plane and a fake IMAP provider over verified
// TLS. Two Personal connections share the provider; each answers all
// four reads for its own mailbox only, and nothing on the wire mutates.

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail/imapread/imapfake"
)

func letter(id, from, to, subject string) []byte {
	return []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: Mon, 05 Oct 2026 09:00:00 +0000\r\n"+
		"Message-ID: <%s@example.internal>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s body\r\n",
		from, to, subject, id, id))
}

// resultData decodes one reported result's data.
func resultData(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	raw, _ := res["result"].(string)
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("result %v: %v", res, err)
	}
	return m
}

type e2eMessage struct {
	Ref, Fingerprint, Text string
}

func messages(t *testing.T, data map[string]any, key string) []e2eMessage {
	t.Helper()
	list, _ := data[key].([]any)
	var out []e2eMessage
	for _, item := range list {
		m := item.(map[string]any)
		msg := e2eMessage{Ref: m["ref"].(string), Fingerprint: m["fingerprint"].(string)}
		if content, ok := m["content"].(map[string]any); ok {
			msg.Text, _ = content["text"].(string)
		}
		out = append(out, msg)
	}
	return out
}

func TestEndToEndTwoConnectionsOverTLS(t *testing.T) {
	serverTLS, roots, err := imapfake.Certificate("imap.example.internal")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := imapfake.Start(imapfake.Options{TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	host, port, _ := net.SplitHostPort(provider.Addr())

	people := []struct{ user, connection, password string }{
		{"ada", "ada-mailbox", "REPLACED-APP-PASSWORD-ADA"},
		{"grace", "grace-mailbox", "REPLACED-APP-PASSWORD-GRACE"},
	}
	for _, p := range people {
		addr := p.user + "@example.internal"
		provider.AddUser(addr, p.password)
		// The same message reaches both mailboxes; each also has its own.
		provider.Deliver(addr, "INBOX", letter("shared", "office@school.example.internal", addr, "Picture day"))
		provider.Deliver(addr, "INBOX", letter("only-"+p.user, "office@school.example.internal", addr, "Note for "+p.user))
	}

	type placed struct {
		fk      *fakeKeystone
		stop    func()
		changes []e2eMessage
		search  []e2eMessage
	}
	runs := map[string]*placed{}
	for _, p := range people {
		p := p
		addr := p.user + "@example.internal"
		cursor := fmt.Sprintf(`{"cursor":{"folders":[{"folder":"INBOX","uidValidity":%d,"lastUid":0}]}}`,
			provider.UIDValidity(addr, "INBOX"))
		fk := &fakeKeystone{queue: []adapter.Work{
			{Seq: 1, Connection: p.connection, Kind: "query", Capability: mail.CapCapabilities, Epoch: 1},
			{Seq: 2, Connection: p.connection, Kind: "query", Capability: mail.CapChanges, RequestJSON: json.RawMessage(cursor), Epoch: 1},
			{Seq: 3, Connection: p.connection, Kind: "query", Capability: mail.CapSearch,
				RequestJSON: json.RawMessage(`{"subject":"picture","since":"2026-09-01","before":"2026-11-01"}`), Epoch: 1},
			{Seq: 4, Connection: p.connection, Kind: "action", Capability: "mail.mark_read", RequestJSON: json.RawMessage(`{}`), Epoch: 1},
		}}
		stop := startRun(t, fk, func(e map[string]string) {
			e["WILD_CONNECTION"] = p.connection
			e["WILD_PARAM_IMAP_HOST"], e["WILD_PARAM_IMAP_PORT"] = host, port
			e["WILD_MAILBOX_USERNAME"] = p.user // bare: the package's username_domain completes it
			e["WILD_SECRET_MAILBOX_APP_PASSWORD"] = p.password
			// Deliberately the SAME secret for both connections: the key
			// is bound to the connection name, so they still cannot
			// compare fingerprints or trade references.
			e["WILD_SECRET_MAILBOX_FINGERPRINT_KEY"] = "REPLACED-fingerprint-secret-0123456789abcdef"
			e["WILD_MAILBOX_FILTERS"] = `{"folders":["INBOX"],"senders":["school.example.internal"]}`
		}, roots)
		defer stop()
		runs[p.connection] = &placed{fk: fk, stop: stop}
	}

	for _, p := range people {
		r := runs[p.connection]
		results := r.fk.waitResults(t, 4)
		if results[0]["resultType"] != "ok" || resultData(t, results[0])["readOnly"] != true {
			t.Fatalf("%s capabilities = %v", p.connection, results[0])
		}
		if results[1]["resultType"] != "ok" {
			t.Fatalf("%s changes = %v", p.connection, results[1])
		}
		r.changes = messages(t, resultData(t, results[1]), "messages")
		if len(r.changes) != 2 || !strings.Contains(r.changes[0].Text+r.changes[1].Text, "only-"+p.user) {
			t.Fatalf("%s changes = %+v", p.connection, r.changes)
		}
		for _, m := range r.changes {
			for _, other := range people {
				if other.user != p.user && strings.Contains(m.Text, other.user) {
					t.Fatalf("%s read %s's mail: %+v", p.connection, other.user, m)
				}
			}
		}
		if results[2]["resultType"] != "ok" {
			t.Fatalf("%s search = %v", p.connection, results[2])
		}
		r.search = messages(t, resultData(t, results[2]), "results")
		if len(r.search) != 1 {
			t.Fatalf("%s search = %+v", p.connection, r.search)
		}
		if results[3]["resultType"] != "unavailable" || !strings.Contains(results[3]["error"].(string), "does not implement") {
			t.Fatalf("%s write = %v", p.connection, results[3])
		}
	}

	ada, grace := runs["ada-mailbox"], runs["grace-mailbox"]
	// The shared message: stable within a connection (search and changes
	// agree), unrelated across connections.
	if ada.search[0].Fingerprint == grace.search[0].Fingerprint {
		t.Fatal("two connections produced the same fingerprint for the same message")
	}
	found := false
	for _, m := range ada.changes {
		found = found || m.Fingerprint == ada.search[0].Fingerprint
	}
	if !found {
		t.Fatal("search and changes disagree on a message's fingerprint")
	}

	// mail.message: each connection reads its own reference, and refuses
	// the other's even though the provider, folder and UID line up.
	ada.fk.push(
		adapter.Work{Seq: 5, Connection: "ada-mailbox", Kind: "query", Capability: mail.CapMessage,
			RequestJSON: json.RawMessage(`{"ref":"` + ada.changes[1].Ref + `"}`), Epoch: 1},
		adapter.Work{Seq: 6, Connection: "ada-mailbox", Kind: "query", Capability: mail.CapMessage,
			RequestJSON: json.RawMessage(`{"ref":"` + grace.changes[1].Ref + `"}`), Epoch: 1},
	)
	grace.fk.push(adapter.Work{Seq: 5, Connection: "grace-mailbox", Kind: "query", Capability: mail.CapMessage,
		RequestJSON: json.RawMessage(`{"ref":"` + ada.changes[1].Ref + `"}`), Epoch: 1})
	results := ada.fk.waitResults(t, 6)
	own := resultData(t, results[4])
	if results[4]["resultType"] != "ok" || own["outcome"] != "ok" ||
		!strings.Contains(fmt.Sprint(own["message"]), "only-ada body") {
		t.Fatalf("ada's own message = %v", results[4])
	}
	for _, res := range []map[string]any{results[5], grace.fk.waitResults(t, 5)[4]} {
		if res["resultType"] != "unavailable" || !strings.Contains(res["error"].(string), mail.OutcomeBadRequest) {
			t.Fatalf("a reference crossed connections: %v", res)
		}
		if strings.Contains(fmt.Sprint(res), "only-") {
			t.Fatalf("a refused reference leaked content: %v", res)
		}
	}

	ada.stop()
	grace.stop()
	if v := provider.Violations(); len(v) > 0 {
		t.Fatalf("mutating IMAP commands reached the provider: %v", v)
	}
	allowed := map[string]bool{"LOGIN": true, "EXAMINE": true, "UID SEARCH": true, "UID FETCH": true}
	for _, c := range provider.Commands() {
		if !allowed[c] {
			t.Errorf("non-read command on the wire: %s", c)
		}
	}
}
