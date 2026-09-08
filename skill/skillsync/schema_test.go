package skillsync

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
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
