package skillsync

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

func TestSchemaRegistryMigratesDeterministicShortestChain(t *testing.T) {
	registry := NewSchemaRegistry()
	registerJSONField := func(from, to uint32, name string, value any) {
		t.Helper()
		if err := registry.Register(from, to, func(packet syncstream.Packet) ([]byte, error) {
			var document map[string]any
			if err := json.Unmarshal(packet.Payload, &document); err != nil {
				return nil, err
			}
			document[name] = value
			return json.Marshal(document)
		}); err != nil {
			t.Fatal(err)
		}
	}
	registerJSONField(1, 2, "v2", true)
	registerJSONField(2, 4, "path", "short")
	registerJSONField(1, 3, "unused", true)
	registerJSONField(3, 5, "unused", true)
	registerJSONField(5, 4, "path", "long")
	registry.Seal()

	path, err := registry.MigrationPath(1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 || path[0] != 1 || path[1] != 2 || path[2] != 4 {
		t.Fatalf("path=%v", path)
	}
	payload, err := registry.Migrate(syncstream.Packet{SchemaVersion: 1, Payload: []byte(`{"base":true}`)}, 4)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got["path"] != "short" || got["v2"] != true || got["unused"] != nil {
		t.Fatalf("migrated=%v", got)
	}
	if err := registry.Register(4, 6, func(packet syncstream.Packet) ([]byte, error) { return packet.Payload, nil }); !errors.Is(err, ErrSchemaRegistrySealed) {
		t.Fatalf("sealed register=%v", err)
	}
}

func TestSchemaRegistryRejectsInvalidAndMissingMigrations(t *testing.T) {
	registry := NewSchemaRegistry()
	if err := registry.Register(1, 1, func(packet syncstream.Packet) ([]byte, error) { return packet.Payload, nil }); !errors.Is(err, ErrSchemaMigrationInvalid) {
		t.Fatalf("same-version register=%v", err)
	}
	migration := func(packet syncstream.Packet) ([]byte, error) { return append(packet.Payload, '2'), nil }
	if err := registry.Register(1, 2, migration); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(1, 2, migration); !errors.Is(err, ErrSchemaMigrationExists) {
		t.Fatalf("duplicate register=%v", err)
	}
	if _, err := registry.Migrate(syncstream.Packet{SchemaVersion: 2, Payload: []byte("x")}, 3); !errors.Is(err, ErrSchemaMigrationMissing) {
		t.Fatalf("missing path=%v", err)
	}
	if err := registry.Register(2, 3, func(syncstream.Packet) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Migrate(syncstream.Packet{SchemaVersion: 1, Payload: []byte("x")}, 3); !errors.Is(err, ErrSchemaMigrationInvalid) {
		t.Fatalf("nil payload=%v", err)
	}
}

// 协商出的 schema 版本必须同时落在两边区间内：SchemaRange 的 Min 为 0 表示空区间（Contains 对任何版本都返回 false），
// 一边 Min 0 时协商失败，而不是返回一个那一边不支持的版本。
func TestNegotiateSchemaRequiresBothRangesToContainTheResult(t *testing.T) {
	cases := []struct{ server, client SchemaRange }{
		{SchemaRange{Min: 0, Max: 5}, SchemaRange{Min: 2, Max: 3}},
		{SchemaRange{Min: 2, Max: 3}, SchemaRange{Min: 0, Max: 5}},
		{SchemaRange{Min: 4, Max: 2}, SchemaRange{Min: 1, Max: 5}},
	}
	for _, test := range cases {
		version, err := NegotiateSchema(test.server, test.client)
		if err == nil {
			t.Fatalf("NegotiateSchema(%v, %v) = %d; server contains=%v client contains=%v, want ErrSchemaNegotiationFailed", test.server, test.client, version, test.server.Contains(version), test.client.Contains(version))
		}
		if !errors.Is(err, ErrSchemaNegotiationFailed) {
			t.Fatalf("err = %v, want ErrSchemaNegotiationFailed", err)
		}
	}
	// 控制：两边都是有效区间时取交集的最大值。
	if version, err := NegotiateSchema(SchemaRange{Min: 1, Max: 3}, SchemaRange{Min: 2, Max: 4}); err != nil || version != 3 {
		t.Fatalf("NegotiateSchema(valid) = %d, %v; want 3", version, err)
	}
}
