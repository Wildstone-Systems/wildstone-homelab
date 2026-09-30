package trait

import (
	"strings"
	"testing"
	"text/template"
)

// The vocabulary is data a human maintains by hand, and the failures a
// bad entry causes are all remote from it: a package lints clean and
// hands nobody anything, or a consumer's template fails on a key three
// packages away. These tests are the guard rails on the table itself.

// TestEverythingIsDescribed. A description is not decoration here: it is
// what a package author reads when deciding whether a trait is the one
// they want, and what `wild pkg traits` prints. A key nobody can describe
// is a key nobody should be asked to depend on.
func TestEverythingIsDescribed(t *testing.T) {
	for _, tr := range All() {
		if strings.TrimSpace(tr.Description) == "" {
			t.Errorf("trait %q has no description", tr.Name)
		}
		for _, m := range tr.Modes {
			if strings.TrimSpace(m.Description) == "" {
				t.Errorf("trait %q mode %q has no description", tr.Name, m.Name)
			}
			for _, e := range m.Exports {
				if strings.TrimSpace(e.Description) == "" {
					t.Errorf("trait %q mode %q export %q has no description", tr.Name, m.Name, e.Key)
				}
			}
		}
		for _, c := range tr.Classes {
			if strings.TrimSpace(c.Description) == "" {
				t.Errorf("trait %q class %q has no description", tr.Name, c.Name)
			}
		}
	}
	for _, e := range Intrinsic {
		if strings.TrimSpace(e.Description) == "" {
			t.Errorf("intrinsic key %q has no description", e.Key)
		}
	}
}

// TestNothingIsNamedTwice. A duplicate mode makes LookupMode return the
// first and the "its modes are ..." diagnostic list the name twice, so the
// author is told their correct spelling is wrong.
func TestNothingIsNamedTwice(t *testing.T) {
	seenTrait := map[string]bool{}
	for _, tr := range All() {
		if seenTrait[tr.Name] {
			t.Errorf("trait %q declared twice", tr.Name)
		}
		seenTrait[tr.Name] = true

		seenMode := map[string]bool{}
		for _, m := range tr.Modes {
			if seenMode[m.Name] {
				t.Errorf("trait %q declares mode %q twice", tr.Name, m.Name)
			}
			seenMode[m.Name] = true

			seenKey := map[string]bool{}
			for _, e := range m.Exports {
				if seenKey[e.Key] {
					t.Errorf("trait %q mode %q declares export %q twice", tr.Name, m.Name, e.Key)
				}
				seenKey[e.Key] = true
			}
		}

		seenClass := map[string]bool{}
		for _, c := range tr.Classes {
			if seenClass[c.Name] {
				t.Errorf("trait %q declares class %q twice", tr.Name, c.Name)
			}
			seenClass[c.Name] = true
		}
	}
}

// TestAnExportKeyBelongsToExactlyOneMode. `.Resolved` is one flat map, so
// two modes carrying the same key would silently race — whichever
// provider the catalog happened to list first would win, and the losing
// consumer would receive a value derived from a service it never
// required. If two traits ever genuinely need to offer the same fact,
// they need two key names.
func TestAnExportKeyBelongsToExactlyOneMode(t *testing.T) {
	owner := map[string]string{}
	for _, tr := range All() {
		for _, m := range tr.Modes {
			for _, e := range m.Exports {
				where := tr.Name + ":" + m.Name
				if prev, ok := owner[e.Key]; ok {
					t.Errorf("export key %q is carried by both %s and %s; .Resolved is one flat map, so one would silently win",
						e.Key, prev, where)
				}
				owner[e.Key] = where
			}
		}
	}
}

// TestNoExportShadowsAnIntrinsicKey. The engine writes intrinsic keys
// first and refuses to let an export replace one, so a trait declaring a
// key that collides is declaring something that can never be delivered —
// and the author has no way to see that from their own package.
func TestNoExportShadowsAnIntrinsicKey(t *testing.T) {
	for _, tr := range All() {
		for _, m := range tr.Modes {
			for _, e := range m.Exports {
				if IsIntrinsic(e.Key) {
					t.Errorf("trait %s:%s carries %q, which is intrinsic; the engine writes intrinsic keys first and an export can never replace one",
						tr.Name, m.Name, e.Key)
				}
			}
		}
	}
}

// TestStructuralTraitsAreTheOnesTheSystemSatisfies pins the two that are,
// because the consequence of a wrong answer is asymmetric and quiet:
// marking a package-satisfied trait structural means every requirement
// for it passes with no provider anywhere, and nothing fails until the
// service is running and cannot log anyone in.
func TestStructuralTraitsAreTheOnesTheSystemSatisfies(t *testing.T) {
	want := map[string]bool{"ingress": true, "storage": true}
	for _, tr := range All() {
		if tr.Structural != want[tr.Name] {
			t.Errorf("trait %q: Structural = %v, want %v — adding one means naming the system mechanism that satisfies it",
				tr.Name, tr.Structural, want[tr.Name])
		}
	}
	if IsStructural("nonesuch") {
		t.Error("an unknown trait must not be structural: that would make every typo a requirement nothing can fail")
	}
}

// TestExportFuncNamesAreUsableAsTemplateFunctions. A name text/template
// cannot accept as a function would make every export using it a parse
// error, reported against the package rather than against this list.
func TestExportFuncNamesAreUsableAsTemplateFunctions(t *testing.T) {
	if len(ExportFuncs) == 0 {
		t.Fatal("no export functions declared; exports could then only concatenate params, which is the bug ADR-W019 A10 exists about")
	}
	seen := map[string]bool{}
	for _, name := range ExportFuncs {
		if seen[name] {
			t.Errorf("export function %q declared twice", name)
		}
		seen[name] = true
		fm := template.FuncMap{name: func(...any) string { return "" }}
		if _, err := template.New("t").Funcs(fm).Parse("{{ " + name + " \"x\" }}"); err != nil {
			t.Errorf("export function %q is not a usable template function name: %v", name, err)
		}
	}
}

// TestMqttBrokerCarriesTheConsumerContract pins the keys Zigbee2MQTT,
// Frigate and Home Assistant render against (#438). Renaming one is a
// change to three consumer packages, not to this table alone.
func TestMqttBrokerCarriesTheConsumerContract(t *testing.T) {
	tr, ok := Lookup("mqtt")
	if !ok {
		t.Fatal("trait mqtt is missing")
	}
	if tr.Structural {
		t.Error("mqtt is satisfied by a broker package, not by the system")
	}
	m, ok := tr.LookupMode("broker")
	if !ok {
		t.Fatal("mqtt has no broker mode")
	}
	want := []string{"MqttHost", "MqttPasswordSecretRef", "MqttPort", "MqttURL", "MqttUsername"}
	if got := strings.Join(m.ExportKeys(), ","); got != strings.Join(want, ",") {
		t.Errorf("mqtt:broker exports %s, want %s", got, strings.Join(want, ","))
	}
}

// TestZwaveServerCarriesTheConsumerContract pins the keys Home
// Assistant renders its Z-Wave JS connection from (#444). No key may
// carry a security key: those belong to the server, not its clients.
func TestZwaveServerCarriesTheConsumerContract(t *testing.T) {
	tr, ok := Lookup("zwave")
	if !ok {
		t.Fatal("trait zwave is missing")
	}
	if tr.Structural {
		t.Error("zwave is satisfied by a Z-Wave server package, not by the system")
	}
	m, ok := tr.LookupMode("server")
	if !ok {
		t.Fatal("zwave has no server mode")
	}
	want := []string{"ZwaveWsHost", "ZwaveWsPort", "ZwaveWsURL"}
	if got := strings.Join(m.ExportKeys(), ","); got != strings.Join(want, ",") {
		t.Errorf("zwave:server exports %s, want %s", got, strings.Join(want, ","))
	}
	for _, k := range m.ExportKeys() {
		if strings.Contains(strings.ToLower(k), "key") || strings.Contains(strings.ToLower(k), "secret") {
			t.Errorf("zwave:server export %s looks like key material; the network keys never leave the server", k)
		}
	}
}

// TestExportContextNamesTheConsumer: .Consumer is what lets a provider
// issue per-consumer credentials; losing it from the declared context
// would make pkgspec refuse the shipped broker.
func TestExportContextNamesTheConsumer(t *testing.T) {
	have := map[string]bool{}
	for _, f := range ExportContext {
		have[f] = true
	}
	for _, f := range []string{"Params", "Consumer"} {
		if !have[f] {
			t.Errorf("ExportContext lacks %s", f)
		}
	}
}

// TestLookupsAgreeWithTheTable. Names/ModeNames/ClassNames feed "must be
// one of ..." messages, and a list that disagrees with what Lookup
// accepts tells an author their correct spelling is wrong.
func TestLookupsAgreeWithTheTable(t *testing.T) {
	for _, name := range Names() {
		if _, ok := Lookup(name); !ok {
			t.Errorf("Names lists %q but Lookup does not find it", name)
		}
	}
	if _, ok := Lookup("nonesuch"); ok {
		t.Error("Lookup found a trait that is not in the vocabulary")
	}
	for _, tr := range All() {
		for _, mode := range tr.ModeNames() {
			m, ok := tr.LookupMode(mode)
			if !ok {
				t.Errorf("trait %q lists mode %q but LookupMode does not find it", tr.Name, mode)
				continue
			}
			for _, key := range m.ExportKeys() {
				if !m.HasExport(key) {
					t.Errorf("%s:%s lists export %q but HasExport denies it", tr.Name, mode, key)
				}
				owner, ownerMode, found := ExportKeyOwner(key)
				if !found || owner != tr.Name || ownerMode != mode {
					t.Errorf("ExportKeyOwner(%q) = %q/%q, want %q/%q", key, owner, ownerMode, tr.Name, mode)
				}
			}
		}
		for _, class := range tr.ClassNames() {
			if !tr.HasClass(class) {
				t.Errorf("trait %q lists class %q but HasClass denies it", tr.Name, class)
			}
		}
	}
	if _, _, found := ExportKeyOwner("TailnetHost"); found {
		t.Error("an intrinsic key must not report a trait owner; it comes from the node, not from a provider")
	}
}
