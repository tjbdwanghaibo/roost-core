package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func rows(t *testing.T, body string) []map[string]json.RawMessage {
	t.Helper()
	payload, err := Document([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Rows(payload, false)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCheckNamesTableRowKeyFieldAndRule(t *testing.T) {
	declared := []Rule{
		{Field: "id", Required: true, Unique: true},
		{Field: "name", Required: true},
		{Field: "level", Min: "1"},
		{Field: "kind", Enum: []string{"melee", "ranged"}},
		{Field: "rank", Enum: []string{"1", "2"}},
	}
	key := func(data []map[string]json.RawMessage) func(int) string {
		return func(i int) string { return Canonical(data[i]["id"]) }
	}
	cases := []struct {
		label, body string
		want        Error
	}{
		{"missing", `[{"id":1,"name":"a"},{"id":2}]`, Error{Table: "monster", Row: 2, Key: "2", Field: "name", Rule: "required"}},
		{"null", `[{"id":1,"name":null}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "name", Rule: "required"}},
		{"unique across spellings", `[{"id":1,"name":"a"},{"id":1.0,"name":"b"}]`, Error{Table: "monster", Row: 2, Key: "1", Field: "id", Rule: "unique"}},
		{"below min", `[{"id":1,"name":"a","level":0}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "level", Rule: "min"}},
		{"min on a string", `[{"id":1,"name":"a","level":"3"}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "level", Rule: "min"}},
		{"enum", `[{"id":1,"name":"a","kind":"magic"}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "kind", Rule: "enum"}},
		{"numeric enum", `[{"id":1,"name":"a","rank":3}]`, Error{Table: "monster", Row: 1, Key: "1", Field: "rank", Rule: "enum"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			data := rows(t, testCase.body)
			err := Check("monster", data, declared, key(data))
			var got *Error
			if !errors.As(err, &got) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if got.Table != testCase.want.Table || got.Row != testCase.want.Row || got.Key != testCase.want.Key || got.Field != testCase.want.Field || got.Rule != testCase.want.Rule {
				t.Fatalf("got %+v, want %+v", *got, testCase.want)
			}
			if !strings.Contains(err.Error(), "table monster row") || !strings.Contains(err.Error(), "field "+testCase.want.Field+": "+testCase.want.Rule) {
				t.Fatalf("message %q does not name table / row / field / rule", err)
			}
		})
	}
	// Valid rows, optional columns absent or null, numeric enum spelled 2.0,
	// keys matched case-insensitively the way encoding/json decodes them.
	ok := rows(t, `{"rows":[{"id":1,"Name":"a","level":1,"kind":"melee","rank":2.0},{"id":2,"name":"b","level":null}]}`)
	if err := Check("monster", ok, declared, nil); err != nil {
		t.Fatalf("valid rows rejected: %v", err)
	}
}

func TestCheckObjectHasNoRowNumber(t *testing.T) {
	payload, err := Document([]byte(`{"data":{"width":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Rows(payload, true)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckObject("world", data[0], []Rule{{Field: "width", Min: "1"}, {Field: "height", Required: true}})
	if err == nil || err.Error() != "table world field width: min: value 0 is below min=1" {
		t.Fatalf("err = %v", err)
	}
}

func TestDocumentRefusesVanishingAndAmbiguousData(t *testing.T) {
	for body, want := range map[string]string{
		"":                          "empty or null",
		"  null ":                   "empty or null",
		`{"rows":null}`:             `wrapper key "rows" is null`,
		`{"rows":[],"data":[]}`:     "ambiguous",
		`{"rows":[{"id":1}],"x":1}`: "", // not a wrapper: the document itself
		`[{"id":1}]`:                "",
		`{"records":[{"id":1}]}   `: "",
	} {
		_, err := Document([]byte(body))
		if want == "" {
			if err != nil {
				t.Errorf("%q: %v", body, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

func TestRuleValidateRejectsBadDeclarations(t *testing.T) {
	for _, rule := range []Rule{
		{},
		{Field: "a", Min: "one"},
		{Field: "a", Enum: []string{"x", ""}},
		{Field: "a", Enum: []string{"x", "x"}},
	} {
		if err := rule.Validate(); err == nil {
			t.Errorf("%+v accepted", rule)
		}
	}
}
