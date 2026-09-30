package imapfake

import (
	"crypto/tls"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// TestWitnessCatchesEveryMutation is the negative control for the
// reader's read-only tests: a client that DOES try to change the
// mailbox is refused and recorded, command by command. Without it, an
// empty violation log could just mean the fake was not looking.
func TestWitnessCatchesEveryMutation(t *testing.T) {
	serverTLS, pool, err := Certificate("imap.example.internal")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(Options{TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddUser("ada@example.internal", "REPLACED-APP-PASSWORD")
	s.CreateMailbox("ada@example.internal", "Archive")
	s.Deliver("ada@example.internal", "INBOX", []byte("Subject: x\r\n\r\nbody\r\n"))

	c, err := imapclient.DialTLS(s.Addr(), &imapclient.Options{TLSConfig: &tls.Config{RootCAs: pool, ServerName: "imap.example.internal"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login("ada@example.internal", "REPLACED-APP-PASSWORD").Wait(); err != nil {
		t.Fatal(err)
	}

	// A read-write SELECT is refused; EXAMINE works.
	if _, err := c.Select("INBOX", nil).Wait(); err == nil {
		t.Error("read-write SELECT accepted")
	}
	if _, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	all := imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}
	_ = c.Fetch(all, &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Close()
	_ = c.Store(all, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close()
	_, _ = c.Copy(all, "Archive").Wait()
	_, _ = c.Move(all, "Archive").Wait()
	_ = c.Expunge().Close()
	_ = c.Create("New", nil).Wait()
	_ = c.Delete("Archive").Wait()
	_ = c.Rename("Archive", "Old", nil).Wait()
	_ = c.Subscribe("Archive").Wait()
	_ = c.Unsubscribe("Archive").Wait()
	appendCmd := c.Append("INBOX", 4, nil)
	_, _ = appendCmd.Write([]byte("x\r\n\r"))
	_ = appendCmd.Close()
	_, _ = appendCmd.Wait()

	got := s.Violations()
	sort.Strings(got)
	want := []string{"APPEND", "COPY", "CREATE", "DELETE", "EXPUNGE", "FETCH BODY[] without PEEK",
		"MOVE", "RENAME", "SELECT", "STORE", "SUBSCRIBE", "UNSUBSCRIBE"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("violations:\n got %v\nwant %v", got, want)
	}
	for _, cmd := range s.Commands() {
		if strings.Contains(cmd, "REPLACED") {
			t.Error("the command log must not carry arguments (credentials)")
		}
	}
}
