package pkgspec

import (
	"strings"
	"testing"
)

// TestValidateParamValueEnumAccepts: a value named in the enum passes,
// exactly like any other shape.
func TestValidateParamValueEnumAccepts(t *testing.T) {
	for _, v := range []string{"platform-only", "sandboxed", "host-admin"} {
		if err := ValidateParamValue("enum:platform-only|sandboxed|host-admin", v); err != nil {
			t.Errorf("value %q: unexpected error: %v", v, err)
		}
	}
}

// TestValidateParamValueEnumRejectsAndListsValues: a value outside the
// closed choice fails, and the error names every allowed value — the
// requirement H2 exists for. A household that mistypes "sandbox" for
// "sandboxed" should be told what it could have written, not just that
// what it wrote was wrong.
func TestValidateParamValueEnumRejectsAndListsValues(t *testing.T) {
	err := ValidateParamValue("enum:platform-only|sandboxed|host-admin", "sandbox")
	if err == nil {
		t.Fatal("expected an error for a value outside the enum")
	}
	for _, want := range []string{"platform-only", "sandboxed", "host-admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name allowed value %q", err.Error(), want)
		}
	}
}

// TestValidateParamValueEnumEmptyValuePasses mirrors the rule every other
// shape follows: whether a param may be empty is `required`'s question,
// not the shape's.
func TestValidateParamValueEnumEmptyValuePasses(t *testing.T) {
	if err := ValidateParamValue("enum:a|b", ""); err != nil {
		t.Errorf("empty value should pass regardless of shape: %v", err)
	}
}

// TestValidateParamValueExistingShapesUnaffected pins the pre-H2 shapes:
// adding the enum form must not change how a fixed-vocabulary shape is
// looked up or reported.
func TestValidateParamValueExistingShapesUnaffected(t *testing.T) {
	if err := ValidateParamValue("iana_timezone", "Europe/London"); err != nil {
		t.Errorf("iana_timezone regressed: %v", err)
	}
	if err := ValidateParamValue("iana_timezone", "CST"); err == nil {
		t.Error("iana_timezone stopped rejecting an abbreviation")
	}
	if err := ValidateParamValue("port", "8080"); err != nil {
		t.Errorf("port regressed: %v", err)
	}
	err := ValidateParamValue("not-a-real-shape", "x")
	if err == nil {
		t.Fatal("expected an error for an unknown shape")
	}
	if !strings.Contains(err.Error(), "enum:a|b|c") {
		t.Errorf("unknown-shape error should mention the enum form as an option: %q", err.Error())
	}
}

// TestEnumShapeValues pins the parsing helper directly: the prefix, the
// split, and that a bare "enum" (no colon) is not the enum form at all —
// it falls through to the ordinary closed-vocabulary lookup and fails
// there, the same as any other misspelling.
func TestEnumShapeValues(t *testing.T) {
	values, ok := enumShapeValues("enum:a|b|c")
	if !ok || len(values) != 3 || values[0] != "a" || values[1] != "b" || values[2] != "c" {
		t.Errorf("enumShapeValues(enum:a|b|c) = %v, %v", values, ok)
	}
	if _, ok := enumShapeValues("enum"); ok {
		t.Error(`"enum" with no colon must not parse as the enum form`)
	}
	if _, ok := enumShapeValues("port"); ok {
		t.Error(`"port" must not parse as the enum form`)
	}
}

// TestValidateEnumShapeDecl pins the package-lint-time structural checks:
// at least one value, none blank, none repeated.
func TestValidateEnumShapeDecl(t *testing.T) {
	if err := validateEnumShapeDecl([]string{"a", "b"}); err != nil {
		t.Errorf("well-formed enum rejected: %v", err)
	}
	for name, values := range map[string][]string{
		"empty (validate: enum:)": {""},
		"blank between pipes":     {"a", "", "b"},
		"duplicate value":         {"a", "b", "a"},
	} {
		if err := validateEnumShapeDecl(values); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
