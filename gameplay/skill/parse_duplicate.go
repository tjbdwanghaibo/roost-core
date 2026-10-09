package skill

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
)

func rejectDuplicateKeys(data []byte) error {
	return rejectDuplicateKeysWithLimits(data, DefaultParseLimits())
}

type jsonScanBudget struct {
	limits ParseLimits
	tokens int
}

func rejectDuplicateKeysWithLimits(data []byte, limits ParseLimits) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	budget := &jsonScanBudget{limits: limits}
	if err := scanJSONValue(decoder, "$", nil, 1, budget); err != nil {
		return err
	}
	token, err := decoder.Token()
	if err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected second JSON value starting with %v", token)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string, first json.Token, depth int, budget *jsonScanBudget) error {
	if depth > budget.limits.MaxDepth {
		return fmt.Errorf("%w: depth %d > %d at %s", ErrParseLimitExceeded, depth, budget.limits.MaxDepth, path)
	}
	token := first
	var err error
	if token == nil {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
	}
	budget.tokens++
	if budget.tokens > budget.limits.MaxTokens {
		return fmt.Errorf("%w: tokens %d > %d", ErrParseLimitExceeded, budget.tokens, budget.limits.MaxTokens)
	}
	if value, ok := token.(string); ok && len(value) > budget.limits.MaxStringBytes {
		return fmt.Errorf("%w: string bytes %d > %d at %s", ErrParseLimitExceeded, len(value), budget.limits.MaxStringBytes, path)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		entries := 0
		for decoder.More() {
			entries++
			if entries > budget.limits.MaxContainerEntries {
				return fmt.Errorf("%w: object entries %d > %d at %s", ErrParseLimitExceeded, entries, budget.limits.MaxContainerEntries, path)
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s: object key is not a string", path)
			}
			budget.tokens++
			if budget.tokens > budget.limits.MaxTokens || len(key) > budget.limits.MaxStringBytes {
				return fmt.Errorf("%w: object key or token budget exceeded at %s", ErrParseLimitExceeded, path)
			}
			childPath := jsonObjectPath(path, key)
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate key at %s", childPath)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, childPath, nil, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("%s: malformed object", path)
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if index >= budget.limits.MaxContainerEntries {
				return fmt.Errorf("%w: array entries exceed %d at %s", ErrParseLimitExceeded, budget.limits.MaxContainerEntries, path)
			}
			if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index), nil, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("%s: malformed array", path)
		}
	default:
		return fmt.Errorf("%s: unexpected delimiter %q", path, delimiter)
	}
	return nil
}

func jsonObjectPath(path, key string) string {
	if path == "$" {
		return "$." + key
	}
	return path + "." + key
}

var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// exactFieldNames caches, per struct type, the canonical JSON name of every
// field and the type its value decodes into.
var exactFieldNames sync.Map // reflect.Type -> map[string]reflect.Type

// requireExactFieldNames walks data along destination's type and rejects any
// object key that names a struct field only case-insensitively. Duplicate keys
// that differ only in case therefore fail too: at most one of them can be the
// canonical spelling. Types with their own UnmarshalJSON (json.RawMessage,
// SkillPresentation, VisualRef, RuntimeValue) are skipped because they decode
// through decodeStrictSingle again; map keys are names chosen by the author
// (memory, persistent_state, attribute_overrides…) and stay case-sensitive.
// It runs after a successful strict Decode, so shape errors are already
// reported and any re-decode failure here is unreachable.
func requireExactFieldNames(data []byte, typ reflect.Type) error {
	if typ == nil {
		return nil
	}
	for typ.Kind() == reflect.Pointer {
		if typ.Implements(jsonUnmarshalerType) {
			return nil
		}
		typ = typ.Elem()
	}
	if typ.Implements(jsonUnmarshalerType) || reflect.PointerTo(typ).Implements(jsonUnmarshalerType) {
		return nil
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		fields := structFieldNames(typ)
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fieldType, ok := fields[key]
			if !ok {
				return fmt.Errorf("json: unknown field %q (field names are case-sensitive)", key)
			}
			if err := requireExactFieldNames(object[key], fieldType); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		for _, item := range items {
			if err := requireExactFieldNames(item, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := requireExactFieldNames(object[key], typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func structFieldNames(typ reflect.Type) map[string]reflect.Type {
	if cached, ok := exactFieldNames.Load(typ); ok {
		return cached.(map[string]reflect.Type)
	}
	fields := make(map[string]reflect.Type)
	for _, field := range reflect.VisibleFields(typ) {
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct {
			continue // promoted fields are listed by VisibleFields themselves
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	exactFieldNames.Store(typ, fields)
	return fields
}
