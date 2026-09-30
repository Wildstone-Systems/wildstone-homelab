package imapread

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The read-only guarantee is pinned three ways: the command surface is
// a closed interface (this file), nothing but that interface's
// implementation can reach the IMAP client (this file), and on the
// wire the fake server refuses and records every mutating command
// (every harness test's cleanup, and TestOnlyReadCommandsOnTheWire).

func TestReadSessionSurfaceIsClosed(t *testing.T) {
	typ := reflect.TypeOf((*readSession)(nil)).Elem()
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{"examine", "fetchBody", "fetchHeaders", "logout", "searchUIDs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readSession grew: %v, want exactly %v. A new method here is a new IMAP "+
			"command the reader can issue; it must be a read, and this list is where "+
			"that is reviewed.", got, want)
	}
}

// forbidden are imapclient methods that change a mailbox, or that
// could (IDLE/ENABLE are not writes but are outside the reviewed
// surface; DialInsecure would drop TLS).
var forbidden = map[string]bool{
	"Store": true, "Copy": true, "Move": true, "Expunge": true, "UIDExpunge": true,
	"Append": true, "Create": true, "Delete": true, "Rename": true, "Subscribe": true,
	"Unsubscribe": true, "UnselectAndExpunge": true, "SetMetadata": true, "SetACL": true,
	"SetQuota": true, "Idle": true, "Enable": true, "DialInsecure": true, "Authenticate": true,
}

func sourceFiles(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = f
	}
	return out
}

func TestOnlySessionFileTalksIMAP(t *testing.T) {
	for _, dir := range []string{".", "mimenorm"} {
		for name, f := range sourceFiles(t, dir) {
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if !strings.HasPrefix(path, "github.com/emersion/go-imap") {
					continue
				}
				if dir != "." || name != "session.go" {
					t.Errorf("%s/%s imports %s; only session.go may talk IMAP", dir, name, path)
				}
			}
		}
	}

	f := sourceFiles(t, ".")["session.go"]
	selects := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if forbidden[n.Sel.Name] {
				t.Errorf("session.go references %s", n.Sel.Name)
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Select" {
				return true
			}
			selects++
			// The one Select must be EXAMINE: its options literal sets
			// ReadOnly: true.
			readOnly := false
			ast.Inspect(n, func(m ast.Node) bool {
				if kv, ok := m.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ReadOnly" {
						if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "true" {
							readOnly = true
						}
					}
				}
				return true
			})
			if !readOnly {
				t.Error("a Select call without ReadOnly: true (SELECT is read-write; use EXAMINE)")
			}
		case *ast.KeyValueExpr:
			// Every body-section request must PEEK: a plain BODY[] fetch
			// sets \Seen on the provider.
			if k, ok := n.Key.(*ast.Ident); ok && k.Name == "Peek" {
				if v, ok := n.Value.(*ast.Ident); !ok || v.Name != "true" {
					t.Error("a body section without Peek: true")
				}
			}
		}
		return true
	})
	if selects != 1 {
		t.Errorf("expected exactly one (read-only) Select call, found %d", selects)
	}

	peeks := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Peek" {
				peeks++
			}
		}
		return true
	})
	// Both fetches (headers and body) request sections; if none is
	// seen, the check above is looking at the wrong file.
	if peeks < 2 {
		t.Errorf("found %d PEEK body sections, want the header and body fetches", peeks)
	}
}

func TestNoCertificateBypassAnywhere(t *testing.T) {
	for _, dir := range []string{".", "mimenorm", "imapfake"} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if strings.Contains(string(b), "InsecureSkipVerify") {
				t.Errorf("%s/%s disables certificate validation", dir, e.Name())
			}
		}
	}
}

func TestOfferedSurfaceIsReadOnly(t *testing.T) {
	if !reflect.DeepEqual(Operations, []string{"mail.capabilities", "mail.changes", "mail.message", "mail.search"}) {
		t.Fatalf("offered surface changed: %v", Operations)
	}
	// Reader exposes no method that could act.
	typ := reflect.TypeOf(&Reader{})
	var methods []string
	for i := 0; i < typ.NumMethod(); i++ {
		methods = append(methods, typ.Method(i).Name)
	}
	sort.Strings(methods)
	if !reflect.DeepEqual(methods, []string{"Capabilities", "Changes", "Message", "Query", "Search"}) {
		t.Fatalf("Reader's exported methods changed: %v", methods)
	}
	h := newHarness(t, SecurityTLS)
	r := h.reader("ada@example.internal", Filters{})
	for _, op := range []string{"mail.send", "mail.delete", "mail.move", "mail.mark_read", "mail.draft", "mail.act", ""} {
		if _, err := r.Query(ctx(t), op, nil); !errors.Is(err, ErrUnknownOperation) {
			t.Errorf("%q: %v", op, err)
		}
	}
	if _, err := r.Query(ctx(t), OpChanges, []byte(`{"cursor":{},"store":"\\Seen"}`)); !errors.Is(err, ErrBadRequest) {
		t.Errorf("unknown request fields must be refused: %v", err)
	}
	if caps := r.Capabilities(ctx(t)); !caps.ReadOnly || caps.Outcome != OutcomeOK {
		t.Errorf("capabilities: %+v", caps)
	}
}

// TestOnlyReadCommandsOnTheWire drives every operation in both TLS
// modes and checks the exact set of commands the server saw.
func TestOnlyReadCommandsOnTheWire(t *testing.T) {
	allowed := map[string]bool{"LOGIN": true, "EXAMINE": true, "UID SEARCH": true, "UID FETCH": true}
	for _, mode := range []Security{SecurityTLS, SecurityStartTLS} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t, mode)
			u := "ada@example.internal"
			h.deliver(u, 3, 0)
			r := h.reader(u, Filters{})
			res := r.Changes(ctx(t), startAt(h, u), 0)
			if len(res.Messages) != 3 {
				t.Fatalf("setup: %+v", res)
			}
			for _, op := range []struct {
				name string
				req  string
			}{
				{OpCapabilities, `{}`},
				{OpChanges, `{"cursor":{"folders":[]},"limit":2}`},
				{OpMessage, `{"ref":"` + res.Messages[0].Ref + `"}`},
				{OpSearch, `{"since":"` + time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339) + `","subject":"Note"}`},
			} {
				out, err := r.Query(ctx(t), op.name, []byte(op.req))
				if err != nil {
					t.Fatalf("%s: %v", op.name, err)
				}
				if o := out.ResultOutcome(); o != OutcomeOK && o != OutcomeEmpty {
					t.Errorf("%s: outcome %s", op.name, o)
				}
			}
			seen := map[string]bool{}
			for _, c := range h.srv.Commands() {
				seen[c] = true
				if !allowed[c] {
					t.Errorf("non-read command on the wire: %s", c)
				}
			}
			for c := range allowed {
				if !seen[c] {
					t.Errorf("expected %s on the wire (is the recorder seeing traffic?)", c)
				}
			}
		})
	}
}
