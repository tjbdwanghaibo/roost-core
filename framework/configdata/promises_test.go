package configdata

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

type promiseRow struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// Two rows with the same key used to be the T-35 class of defect elsewhere:
// the later row wins silently. Here the table constructor refuses, and the
// load must surface the table name and the key.
func TestTableLoadRefusesDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "rows.json"), `[{"id":1,"name":"a"},{"id":1,"name":"b"}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, promiseRow]{Name: "rows", File: "rows.json", Key: func(v promiseRow) int32 { return v.ID }})
	_, err := NewStore(reg, dir).Load(context.Background())
	expectErr(t, err, "table rows duplicate key 1")
}

// A definition missing its identity or its key function must fail at load
// with the definition named; these used to be reachable only by hand-built
// defs and had no test.
func TestDefinitionsRefuseIncompleteShapes(t *testing.T) {
	ctx := &BuildContext{Dir: t.TempDir()}
	_, err := TableDef[int32, promiseRow]{File: "x.json", Key: func(v promiseRow) int32 { return v.ID }}.load(ctx)
	expectErr(t, err, "table name is empty")
	_, err = TableDef[int32, promiseRow]{Name: "rows", Key: func(v promiseRow) int32 { return v.ID }}.load(ctx)
	expectErr(t, err, "table rows file is empty")
	_, err = TableDef[int32, promiseRow]{Name: "rows", File: "x.json"}.load(ctx)
	expectErr(t, err, "table rows key func is nil")
	_, err = ObjectDef[promiseRow]{File: "x.json"}.load(ctx)
	expectErr(t, err, "object name is empty")
	_, err = ObjectDef[promiseRow]{Name: "world"}.load(ctx)
	expectErr(t, err, "object world file is empty")
	_, err = CustomDef[int]{Build: func(*BuildContext) (int, error) { return 0, nil }}.build(ctx)
	expectErr(t, err, "custom name is empty")
	_, err = CustomDef[int]{Name: "count"}.build(ctx)
	expectErr(t, err, "custom count build func is nil")

	// validate() is handed whatever load() produced; a wrong type is a
	// programming error that must not be swallowed as "nothing to validate".
	tableDef := TableDef[int32, promiseRow]{Name: "rows", Validate: func(*BuildContext, promiseRow) error { return nil }}
	expectErr(t, tableDef.validate(ctx, "not a table"), "table rows type mismatch")
	objectDef := ObjectDef[promiseRow]{Name: "world", Validate: func(*BuildContext, promiseRow) error { return nil }}
	expectErr(t, objectDef.validate(ctx, 42), "object world type mismatch")
	customDef := CustomDef[int]{Name: "count", Validate: func(*BuildContext, int) error { return nil }}
	expectErr(t, customDef.validate(ctx, "str"), "custom count type mismatch")

	reg := NewRegistry()
	expectErr(t, reg.RegisterTable(nil), "nil table def")
	expectErr(t, reg.RegisterObject(nil), "nil object def")
	expectErr(t, reg.RegisterCustom(nil), "nil custom def")
	expectErr(t, RegisterTable(reg, TableDef[int32, promiseRow]{File: "x.json"}), "table name is empty")
}

// Every auto-table tag rule, pinned by its message. The earlier test only
// checked that *some* error came back, so a rule could vanish while a
// neighbour kept the test green.
func TestRegisterAutoTableRefusesEachTagMistakeByMessage(t *testing.T) {
	type promoted struct {
		ID int32 `json:"id" cfg:"key"`
	}
	type taggedEmbed struct {
		promoted `cfg:"key"`
	}
	type namedEmbed struct {
		promoted `json:"inner"`
	}
	type pointerEmbed struct {
		*promoted
	}
	// No json tags here on purpose: vet's structtag check rejects the
	// duplicate-name shapes we need, and encoding/json resolves untagged
	// fields by their Go name just the same.
	type promotedByName struct {
		ID int32 `cfg:"key"`
	}
	type shadowedTag struct {
		promotedByName
		ID int32
	}
	type base1 struct {
		ID int32 `cfg:"key"`
	}
	type base2 struct {
		ID int32
	}
	type tieDropped struct {
		base1
		base2
		Key int32 `json:"key" cfg:"key"`
	}
	type unexportedTag struct {
		ID     int32 `json:"id" cfg:"key"`
		hidden int32 `cfg:"index"`
	}
	type emptyRef struct {
		ID  int32 `json:"id" cfg:"key"`
		Ref int32 `json:"ref" cfg:"ref="`
	}
	type emptyIndex struct {
		ID int32 `json:"id" cfg:"key,index="`
	}
	type doubleRef struct {
		ID  int32 `json:"id" cfg:"key"`
		Ref int32 `json:"ref" cfg:"ref=a,ref=b"`
	}
	type sliceRef struct {
		ID  int32   `json:"id" cfg:"key"`
		Ref []int32 `json:"ref" cfg:"ref=a"`
	}
	type stringKey struct {
		ID string `json:"id" cfg:"key"`
	}
	cases := []struct {
		name string
		reg  func(*Registry) error
		want string
	}{
		{"cfg tag on embedded field", func(r *Registry) error { return RegisterAutoTable[int32, taggedEmbed](r) }, "cfg tag on embedded field promoted is not supported"},
		{"json-named embed hides inner cfg tags", func(r *Registry) error { return RegisterAutoTable[int32, namedEmbed](r) }, "not promoted by encoding/json"},
		{"pointer embed with cfg tags", func(r *Registry) error { return RegisterAutoTable[int32, pointerEmbed](r) }, "cfg tags inside pointer embed promoted are not supported"},
		{"shadowed cfg field", func(r *Registry) error { return RegisterAutoTable[int32, shadowedTag](r) }, "is shadowed by"},
		{"tie-dropped cfg field", func(r *Registry) error { return RegisterAutoTable[int32, tieDropped](r) }, "tie on json name"},
		{"value type is not a struct", func(r *Registry) error { return RegisterAutoTable[int32, int](r) }, "must be a struct"},
		{"key type is not scalar", func(r *Registry) error { return RegisterAutoTable[[2]int32, promoted](r) }, "must be an integer or string"},
		{"cfg tag on unexported field", func(r *Registry) error { return RegisterAutoTable[int32, unexportedTag](r) }, "cfg tag on unexported field hidden"},
		{"empty ref target", func(r *Registry) error { return RegisterAutoTable[int32, emptyRef](r) }, "empty ref target"},
		{"empty index name", func(r *Registry) error { return RegisterAutoTable[int32, emptyIndex](r) }, "empty index name"},
		{"two ref directives", func(r *Registry) error { return RegisterAutoTable[int32, doubleRef](r) }, "multiple ref directives"},
		{"ref on a slice", func(r *Registry) error { return RegisterAutoTable[int32, sliceRef](r) }, "must be an integer or string"},
		{"validate callback of the wrong type", func(r *Registry) error {
			return RegisterAutoTable[int32, promoted](r, WithAutoValidate(func(*BuildContext, stringKey) error { return nil }))
		}, "WithAutoValidate callback must be func(*BuildContext, configdata.promoted) error"},
		{"validate-table callback of the wrong type", func(r *Registry) error {
			return RegisterAutoTable[int32, promoted](r, WithAutoValidateTable(func(*BuildContext, *Table[string, stringKey]) error { return nil }))
		}, "WithAutoValidateTable callback signature mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectErr(t, tc.reg(NewRegistry()), tc.want)
		})
	}
	if err := RegisterAutoTable[int32, promoted](NewRegistry()); err != nil {
		t.Fatalf("the plain shape must register: %v", err)
	}
}
