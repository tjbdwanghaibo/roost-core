// RR-20261004-NC-03：JSON schema 的输入和输出容器不得共享可变引用。
package admin

import (
	"fmt"
	"sync"
	"testing"
)

func TestMetadataOwnership(t *testing.T) {
	for _, mode := range []string{"empty_input", "empty_get", "empty_list", "array_input", "array_get", "array_list", "ordinary_map_control"} {
		t.Run(mode, func(t *testing.T) {
			registry := NewMetadataRegistry()
			input := map[string]any{}
			if mode == "array_input" || mode == "array_get" || mode == "array_list" {
				input["oneOf"] = []any{map[string]any{"type": "string"}}
			}
			if mode == "ordinary_map_control" {
				input["properties"] = map[string]any{"kind": "string"}
			}
			if err := registry.Register(CommandMeta{Name: "test", PayloadSchema: input}); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "empty_input":
				input["type"] = "injected"
			case "empty_get":
				meta, _ := registry.Get("test")
				meta.PayloadSchema["type"] = "injected"
			case "empty_list":
				registry.List()[0].PayloadSchema["type"] = "injected"
			case "array_input":
				input["oneOf"].([]any)[0].(map[string]any)["type"] = "injected"
			case "array_get":
				meta, _ := registry.Get("test")
				meta.PayloadSchema["oneOf"].([]any)[0].(map[string]any)["type"] = "injected"
			case "array_list":
				registry.List()[0].PayloadSchema["oneOf"].([]any)[0].(map[string]any)["type"] = "injected"
			case "ordinary_map_control":
				meta, _ := registry.Get("test")
				meta.PayloadSchema["properties"].(map[string]any)["kind"] = "injected"
			}
			again, _ := registry.Get("test")
			t.Logf("stored_schema=%v", again.PayloadSchema)
			if len(mode) >= 5 && mode[:5] == "empty" {
				if len(again.PayloadSchema) != 0 {
					t.Fatalf("external mutation changed empty stored schema: %v", again.PayloadSchema)
				}
			}
			if len(mode) >= 5 && mode[:5] == "array" {
				if got := again.PayloadSchema["oneOf"].([]any)[0].(map[string]any)["type"]; got != "string" {
					t.Fatalf("external mutation changed schema inside array: %v", got)
				}
			}
			if mode == "ordinary_map_control" && again.PayloadSchema["properties"].(map[string]any)["kind"] != "string" {
				t.Fatal(again.PayloadSchema)
			}
		})
	}
}

func TestSchemaPreservesNilAndEmptyContainers(t *testing.T) {
	for _, mode := range []string{"nil_root", "empty_root", "nil_map", "empty_map", "nil_array", "empty_array", "nil_strings", "empty_strings"} {
		t.Run(mode, func(t *testing.T) {
			var schema map[string]any
			if mode != "nil_root" {
				schema = map[string]any{}
			}
			switch mode {
			case "nil_map":
				schema["value"] = map[string]any(nil)
			case "empty_map":
				schema["value"] = map[string]any{}
			case "nil_array":
				schema["value"] = []any(nil)
			case "empty_array":
				schema["value"] = []any{}
			case "nil_strings":
				schema["value"] = []string(nil)
			case "empty_strings":
				schema["value"] = []string{}
			}
			registry := NewMetadataRegistry()
			if err := registry.Register(CommandMeta{Name: "test", PayloadSchema: schema}); err != nil {
				t.Fatal(err)
			}
			meta, _ := registry.Get("test")
			if (schema == nil) != (meta.PayloadSchema == nil) {
				t.Fatal("root nilness changed")
			}
			if mode == "nil_root" || mode == "empty_root" {
				return
			}
			wantNil := mode[:3] == "nil"
			var gotNil bool
			switch value := meta.PayloadSchema["value"].(type) {
			case map[string]any:
				gotNil = value == nil
			case []any:
				gotNil = value == nil
			case []string:
				gotNil = value == nil
			default:
				t.Fatalf("container type changed: %T", value)
			}
			if gotNil != wantNil {
				t.Fatalf("nilness=%v want=%v", gotNil, wantNil)
			}
		})
	}
}

func TestSchemaNestedArraysAndStringsHaveIndependentOwnership(t *testing.T) {
	input := map[string]any{"nested": []any{[]any{map[string]any{"type": "string"}}}, "enum": []string{"safe"}}
	registry := NewMetadataRegistry()
	if err := registry.Register(CommandMeta{Name: "test", PayloadSchema: input}); err != nil {
		t.Fatal(err)
	}
	input["nested"].([]any)[0].([]any)[0].(map[string]any)["type"] = "input changed"
	input["enum"].([]string)[0] = "input changed"
	meta, _ := registry.Get("test")
	if meta.PayloadSchema["nested"].([]any)[0].([]any)[0].(map[string]any)["type"] != "string" || meta.PayloadSchema["enum"].([]string)[0] != "safe" {
		t.Fatal(meta.PayloadSchema)
	}
	meta.PayloadSchema["nested"].([]any)[0].([]any)[0].(map[string]any)["type"] = "output changed"
	meta.PayloadSchema["enum"].([]string)[0] = "output changed"
	again := registry.List()[0]
	if again.PayloadSchema["nested"].([]any)[0].([]any)[0].(map[string]any)["type"] != "string" || again.PayloadSchema["enum"].([]string)[0] != "safe" {
		t.Fatal(again.PayloadSchema)
	}
}

func TestConcurrentMetadataConsumersOnlyMutateTheirCopies(t *testing.T) {
	registry := NewMetadataRegistry()
	if err := registry.Register(CommandMeta{Name: "test", PayloadSchema: map[string]any{"oneOf": []any{map[string]any{"type": "string"}}}}); err != nil {
		t.Fatal(err)
	}
	const consumers = 16
	failures := make(chan error, consumers)
	var group sync.WaitGroup
	for range consumers {
		group.Go(func() {
			for range 20 {
				meta, _ := registry.Get("test")
				meta.PayloadSchema["oneOf"].([]any)[0].(map[string]any)["type"] = "local change"
				item := registry.List()[0]
				if got := item.PayloadSchema["oneOf"].([]any)[0].(map[string]any)["type"]; got != "string" {
					failures <- fmt.Errorf("stored type=%v", got)
					return
				}
				item.PayloadSchema["oneOf"].([]any)[0].(map[string]any)["type"] = "another local change"
			}
		})
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
