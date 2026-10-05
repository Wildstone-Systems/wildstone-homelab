package pkgspec

import (
	"strings"
	"testing"
)

// The integrates: block (epic #326 S01). It is a dispatch surface, so it
// fails closed: a declaration this Keystone cannot fully understand is a
// package it refuses, never one it half-runs.

const distroless = "gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab"

func integratesPkg(block string) string {
	return `
apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: mail-test, version: 1.0.0, capability: mail, profile: test }
params:
  - { name: node, required: true }
  - { name: imap_host, required: true }
  - { name: imap_port, default: "993" }
  - { name: imap_password }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: v, node: "{{ .Params.node }}" }
    spec: { name: v }
integrates:
` + block
}

const goodMail = `
  - domain: mail
    shape: connection-only
    adapter:
      contract: 1
      image: "` + distroless + `"
      program: wild-mail-adapter
      inherits: [imap_host, imap_port]
      needs:
        secrets:
          - { name: mailbox-app-password, scope: member, purpose: "reads mail" }
    answers:
      - { query: mail.capabilities, deadline: 5s }
      - { query: mail.changes, deadline: 30s, supports: [folder, cursor, limit] }
`

func TestIntegratesAcceptsAReadOnlyMailAdapter(t *testing.T) {
	pkg, err := Parse([]byte(integratesPkg(goodMail)))
	if err != nil {
		t.Fatal(err)
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	in, ok := pkg.AdapterFor("mail")
	if !ok || in.Adapter.Program != "wild-mail-adapter" || len(in.Answers) != 2 {
		t.Fatalf("AdapterFor(mail) = %+v, %v", in, ok)
	}
	if _, ok := pkg.AdapterFor("calendar"); ok {
		t.Fatal("AdapterFor(calendar) found a declaration that is not there")
	}
}

func TestIntegratesFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, want string
	}{
		// Structurally read-only: a mutating operation cannot be offered.
		{"a mutating mail op", "mail.capabilities, deadline: 5s", "mail.send", `"mail.send" is not a mail query`},
		{"a move op", "mail.capabilities, deadline: 5s", "mail.move", `"mail.move" is not a mail query`},
		{"another domain's action", "mail.capabilities, deadline: 5s", "pantry.add", "is an action"},
		{"another domain's query", "mail.capabilities, deadline: 5s", "pantry.list", "is not a mail query"},
		// Compatibility gating: what this Keystone does not know, it refuses.
		{"a newer contract", "contract: 1", "contract: 2", "newer than this Keystone understands"},
		{"no contract", "contract: 1", "contract: 0", "must name the adapter contract"},
		{"an unshipped program", "program: wild-mail-adapter", "program: wild-mail-adapter-v2", "not an adapter program this release ships"},
		{"an unknown domain", "domain: mail", "domain: fax", "not an integration domain this Keystone knows"},
		// Shape and needs.
		{"an unpinned image", distroless, "gcr.io/distroless/static-debian12:latest", "pinned by digest"},
		{"an inherited credential", "inherits: [imap_host, imap_port]", "inherits: [imap_host, imap_password]", "looks like a credential"},
		{"an undeclared inherit", "inherits: [imap_host, imap_port]", "inherits: [smtp_host]", "not a param of this package"},
		{"a bad scope", "scope: member", "scope: everyone", "household or member"},
		{"no purpose", `purpose: "reads mail"`, `purpose: ""`, "must say what"},
		{"a long deadline", "deadline: 30s", "deadline: 5m", "at most 1m0s"},
		{"a bad shape", "shape: connection-only", "shape: plugin", "must be one of"},
		{"an adapter on a native shape", "shape: connection-only", "shape: native", "runs no Wildstone adapter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(goodMail, tc.from) {
				t.Fatalf("fixture has no %q", tc.from)
			}
			pkg, err := Parse([]byte(integratesPkg(strings.Replace(goodMail, tc.from, tc.to, 1))))
			if err != nil {
				t.Fatal(err)
			}
			err = pkg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestIntegratesNeedsAnAdapterForConnectionOnly(t *testing.T) {
	pkg, err := Parse([]byte(integratesPkg("  - { domain: mail, shape: connection-only }\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := pkg.Validate(); err == nil || !strings.Contains(err.Error(), "needs an adapter block") {
		t.Fatalf("Validate = %v", err)
	}
}

func TestIntegratesRefusesAWritableMount(t *testing.T) {
	block := strings.Replace(goodMail, "        secrets:",
		"        mounts:\n          - { source: v, target: /v }\n        secrets:", 1)
	pkg, err := Parse([]byte(integratesPkg(block)))
	if err != nil {
		t.Fatal(err)
	}
	if err := pkg.Validate(); err == nil || !strings.Contains(err.Error(), "must be read-only") {
		t.Fatalf("Validate = %v", err)
	}
}

// Strict parsing reaches inside the block. A field a NEWER declaration
// carries — placement, say, which a package must never name anyway — is
// refused here rather than silently dropped. This is also exactly how a
// Keystone older than this block answers the whole of `integrates:`.
func TestIntegratesIsParsedStrictly(t *testing.T) {
	for _, extra := range []string{"      placement: node-a\n", "      performs: [mail.send]\n"} {
		block := strings.Replace(goodMail, "      program: wild-mail-adapter\n",
			"      program: wild-mail-adapter\n"+extra, 1)
		if _, err := Parse([]byte(integratesPkg(block))); err == nil || !strings.Contains(err.Error(), "field") {
			t.Fatalf("Parse with %q = %v, want an unknown-field error", extra, err)
		}
	}
}

// The shipped mail package declares the dormant adapter, inheriting the
// provider endpoint and nothing that could carry a credential.
func TestMailRoundcubeDeclaresADormantReadOnlyAdapter(t *testing.T) {
	pkg, err := Load("../../packages/mail-roundcube/package.yaml")
	if err != nil {
		t.Fatal(err)
	}
	in, ok := pkg.AdapterFor("mail")
	if !ok {
		t.Fatal("mail-roundcube declares no mail adapter")
	}
	if in.Shape != "connection-only" || in.Adapter.Program != "wild-mail-adapter" {
		t.Fatalf("mail integration = %+v", in)
	}
	want := []string{"imap_host", "imap_port", "imap_encryption", "username_domain"}
	if strings.Join(in.Adapter.Inherits, ",") != strings.Join(want, ",") {
		t.Fatalf("inherits = %v, want %v", in.Adapter.Inherits, want)
	}
	var answered []string
	for _, a := range in.Answers {
		answered = append(answered, a.Query)
	}
	if got := strings.Join(answered, ","); got != "mail.capabilities,mail.changes,mail.message,mail.search" {
		t.Fatalf("answers = %s", got)
	}
	for _, s := range in.Adapter.Needs.Secrets {
		if s.Scope != "member" {
			t.Fatalf("secret %s is %s-scoped; a mailbox credential is the Person's own", s.Name, s.Scope)
		}
	}
	// Nothing about the adapter is a template: installing renders none
	// of it (capability's dormant-install test proves the rendering).
	for _, tmpl := range pkg.Templates {
		if strings.Contains(tmpl, in.Adapter.Program) {
			t.Fatal("a template names the adapter program; the adapter must not render at install")
		}
	}
}
