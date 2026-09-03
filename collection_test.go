package mongo

import (
	"errors"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"

	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestIsIndexDefinitionConflict(t *testing.T) {
	for _, err := range []error{
		drivermongo.CommandError{Code: 85},
		drivermongo.CommandError{Code: 86},
		drivermongo.CommandError{Name: "IndexOptionsConflict"},
		drivermongo.CommandError{Name: "IndexKeySpecsConflict"},
	} {
		if !isIndexDefinitionConflict(err) {
			t.Fatalf("expected index definition conflict for %v", err)
		}
	}
	if isIndexDefinitionConflict(drivermongo.CommandError{Code: 27, Name: "IndexNotFound"}) {
		t.Fatalf("index not found must not be treated as definition conflict")
	}
	if isIndexDefinitionConflict(errors.New("ordinary error")) {
		t.Fatalf("ordinary error must not be treated as definition conflict")
	}
}

func TestIsIndexNotFound(t *testing.T) {
	for _, err := range []error{
		drivermongo.CommandError{Code: 27},
		drivermongo.CommandError{Name: "IndexNotFound"},
	} {
		if !isIndexNotFound(err) {
			t.Fatalf("expected index not found for %v", err)
		}
	}
	if isIndexNotFound(drivermongo.CommandError{Code: 86, Name: "IndexKeySpecsConflict"}) {
		t.Fatalf("index definition conflict must not be treated as index not found")
	}
}

func TestIndexConflictPolicyRequiresExplicitAutoRecreate(t *testing.T) {
	idx := fmongo.IndexModel{Name: "idx_player", ConflictPolicy: fmongo.IndexConflictRecreate}
	if shouldRecreateIndexOnConflict(idx, IndexMigrationPolicy{}) {
		t.Fatal("index conflict should not recreate without explicit migration policy")
	}
	if !shouldRecreateIndexOnConflict(idx, IndexMigrationPolicy{AllowRecreate: true}) {
		t.Fatal("index conflict should recreate when model and migration policy both allow it")
	}
}

func TestMongoIndexModelDistinguishesRelativeAndAbsoluteExpiry(t *testing.T) {
	relative := resolveIndexOptions(t, fmongo.IndexModel{TTL: 60})
	if relative.ExpireAfterSeconds == nil || *relative.ExpireAfterSeconds != 60 {
		t.Fatalf("relative ttl=%v, want 60", relative.ExpireAfterSeconds)
	}
	absolute := resolveIndexOptions(t, fmongo.IndexModel{ExpireAt: true})
	if absolute.ExpireAfterSeconds == nil || *absolute.ExpireAfterSeconds != 0 {
		t.Fatalf("absolute ttl=%v, want 0", absolute.ExpireAfterSeconds)
	}
	none := resolveIndexOptions(t, fmongo.IndexModel{})
	if none.ExpireAfterSeconds != nil {
		t.Fatalf("unset ttl=%v, want nil", none.ExpireAfterSeconds)
	}
}

func resolveIndexOptions(t *testing.T, idx fmongo.IndexModel) options.IndexOptions {
	t.Helper()
	var resolved options.IndexOptions
	for _, apply := range mongoIndexModel(idx).Options.List() {
		if err := apply(&resolved); err != nil {
			t.Fatal(err)
		}
	}
	return resolved
}

func TestStringifyIDDoesNotExposeNilSentinel(t *testing.T) {
	if got := stringifyID(nil); got != "" {
		t.Fatalf("nil id=%q, want empty", got)
	}
	if got := stringifyID(42); got != "42" {
		t.Fatalf("id=%q, want 42", got)
	}
}
