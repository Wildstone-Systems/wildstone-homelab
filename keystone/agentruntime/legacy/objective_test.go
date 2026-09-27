package legacy

import "testing"

// #364: a model asked for exactly one JSON object often fences it. One
// fence is forgiven; nothing else is.
func TestUnfence(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		fenced   bool
	}{
		{`{"kind":"informational"}`, `{"kind":"informational"}`, false},
		{"```json\n{\"kind\":\"informational\"}\n```", `{"kind":"informational"}`, true},
		{"```\n{\"kind\":\"effectful\"}\n```", `{"kind":"effectful"}`, true},
		{"  ```JSON\n{\"kind\":\"informational\"}```  ", `{"kind":"informational"}`, true},
		{"```{\"kind\":\"informational\"}```", `{"kind":"informational"}`, true},
		// Prose around the fence is not stripped: the strict decoder
		// still refuses it.
		{"Here you go:\n```json\n{}\n```", "Here you go:\n```json\n{}\n```", false},
		{"```", "```", false},
	} {
		got, fenced := unfence(tc.in)
		if got != tc.want || fenced != tc.fenced {
			t.Errorf("unfence(%q) = %q, %v; want %q, %v", tc.in, got, fenced, tc.want, tc.fenced)
		}
	}
}
