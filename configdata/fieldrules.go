package configdata

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/configdata/rules"
)

// FieldRule is one declared column rule: required / unique / min / enum are
// checked on the raw JSON rows, ref against the loaded target table. It is
// the type the generators emit (tablegen writes TableDef.Rules literals,
// RegisterAutoTable derives them from `cfg` tags) and the one the generators'
// own early checks run — see package configdata/rules.
type FieldRule = rules.Rule

// RuleError names the table, row (and its key), field and rule a load or
// reload was rejected for. Use errors.As to get it from a Load / Reload error.
type RuleError = rules.Error

// resolvedRule is a FieldRule bound to the struct field it constrains.
type resolvedRule struct {
	FieldRule
	index []int
	typ   reflect.Type
}

// resolveRules binds every rule to a field of V (matched the way
// encoding/json matches keys) and checks the rule fits the field's kind. It
// runs at registration, so a rule naming a field that does not exist — a
// typo, a renamed column — fails at startup instead of never firing.
func resolveRules[V any](declared []FieldRule, object bool) ([]resolvedRule, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	rowType := reflect.TypeOf((*V)(nil)).Elem()
	if rowType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("rules need a struct row type, got %s", rowType)
	}
	fields, err := collectAutoFields(rowType, nil, 0)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(declared))
	out := make([]resolvedRule, 0, len(declared))
	for _, rule := range declared {
		if err := rule.Validate(); err != nil {
			return nil, err
		}
		if seen[rule.Field] {
			return nil, fmt.Errorf("field %s has two rules (merge them into one)", rule.Field)
		}
		seen[rule.Field] = true
		field, ok := fieldForJSONKey(fields, rule.Field)
		if !ok {
			return nil, fmt.Errorf("rule field %s matches no field of %s", rule.Field, rowType)
		}
		base := field.field.Type
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		switch {
		case object && (rule.Unique || rule.Ref != ""):
			return nil, fmt.Errorf("field %s: unique and ref apply to table rows, not to a single object", rule.Field)
		case rule.Min != "" && !numericKind(base.Kind()):
			return nil, fmt.Errorf("field %s: min needs a numeric field, %s is %s", rule.Field, field.field.Name, field.field.Type)
		case len(rule.Enum) > 0 && !autoScalarKind(base.Kind()) && base.Kind() != reflect.Bool:
			return nil, fmt.Errorf("field %s: enum needs a string, integer or bool field, %s is %s", rule.Field, field.field.Name, field.field.Type)
		case rule.Ref != "" && !autoScalarKind(base.Kind()):
			return nil, fmt.Errorf("field %s: ref needs an integer or string field, %s is %s", rule.Field, field.field.Name, field.field.Type)
		}
		out = append(out, resolvedRule{FieldRule: rule, index: field.index, typ: field.field.Type})
	}
	return out, nil
}

// fieldForJSONKey finds the field encoding/json would fill from key: an exact
// json name first, then a case-insensitive match (json tag name, or the Go
// field name when there is no tag). Fields tagged json:"-" never match.
func fieldForJSONKey(fields []autoField, key string) (autoField, bool) {
	var folded *autoField
	for i := range fields {
		name, ok := decodedJSONName(fields[i].field)
		if !ok || !fields[i].field.IsExported() {
			continue
		}
		if name == key {
			return fields[i], true
		}
		if folded == nil && strings.EqualFold(name, key) {
			folded = &fields[i]
		}
	}
	if folded != nil {
		return *folded, true
	}
	return autoField{}, false
}

// decodedJSONName is the key encoding/json reads a field from.
func decodedJSONName(field reflect.StructField) (string, bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name, true
	}
	name, _, _ := strings.Cut(tag, ",")
	switch name {
	case "-":
		return "", false
	case "":
		return field.Name, true
	}
	return name, true
}

func numericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func hasRef(declared []FieldRule) bool {
	for _, rule := range declared {
		if rule.Ref != "" {
			return true
		}
	}
	return false
}

// checkRefs enforces the ref rules of one table once every table is loaded.
// The target must be a loaded table whose key type the field converts to
// without loss — checked even when the table is empty, so a misspelled
// target cannot hide behind an empty or all-zero column. A zero value (nil
// for a pointer field) means "no reference" unless the rule is Required: a
// required reference must point at an existing key, zero included.
func checkRefs[K comparable, V any](ctx *BuildContext, table *Table[K, V], key func(V) K, declared []FieldRule) error {
	if !hasRef(declared) {
		return nil
	}
	resolved, err := resolveRules[V](declared, false)
	if err != nil {
		return err
	}
	type boundRef struct {
		resolvedRule
		lookup refKeyLookup
	}
	var refs []boundRef
	for _, rule := range resolved {
		if rule.Ref == "" {
			continue
		}
		fail := func(detail string) error {
			return &RuleError{Table: string(table.name), Field: rule.Field, Rule: "ref", Detail: detail}
		}
		raw, ok := ctx.Snapshot.table(Name(rule.Ref))
		if !ok {
			return fail("references unknown table " + rule.Ref)
		}
		lookup, ok := raw.(refKeyLookup)
		if !ok {
			return fail("table " + rule.Ref + " does not support reference lookup")
		}
		base := rule.typ
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if !refTypeCompatible(base, lookup.refKeyType()) {
			return fail(fmt.Sprintf("type %s is not compatible with %s key type %s", rule.typ, rule.Ref, lookup.refKeyType()))
		}
		refs = append(refs, boundRef{resolvedRule: rule, lookup: lookup})
	}
	for i, row := range table.rows {
		value := reflect.ValueOf(row)
		for _, ref := range refs {
			field := value.FieldByIndex(ref.index)
			if field.Kind() == reflect.Pointer {
				if field.IsNil() {
					continue // absent / null: Required is checked on the raw row
				}
				field = field.Elem()
			}
			if field.IsZero() && !ref.Required {
				continue
			}
			if !ref.lookup.containsKeyValue(field) {
				return &RuleError{
					Table: string(table.name), Row: i + 1, Key: fmt.Sprint(key(row)),
					Field: ref.Field, Rule: "ref",
					Detail: fmt.Sprintf("references missing %s key %v", ref.Ref, field.Interface()),
				}
			}
		}
	}
	return nil
}
