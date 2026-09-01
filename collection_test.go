package mongo

import (
	"errors"
	"testing"

	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"

	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
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

func TestStringifyIDDoesNotExposeNilSentinel(t *testing.T) {
	if got := stringifyID(nil); got != "" {
		t.Fatalf("nil id=%q, want empty", got)
	}
	if got := stringifyID(42); got != "42" {
		t.Fatalf("id=%q, want 42", got)
	}
}
