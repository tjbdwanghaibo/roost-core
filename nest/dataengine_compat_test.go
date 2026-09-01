package nest

import (
	"testing"

	"github.com/tjbdwanghaibo/cube-core/dataengine"
)

func TestPrepareCommitRecordCanonicalizesLegacyMutation(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	tx.durability = DurabilityAsync
	if err := tx.AddMutation(EntityMutation{
		EntityID: 7,
		Database: "game",
		Resource: "hero",
		Version:  5,
		Data:     []byte{1},
	}); err != nil {
		t.Fatal(err)
	}

	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if record.Durability != DurabilityAsync {
		t.Fatalf("durability = %d", record.Durability)
	}
	if len(record.Mutations) != 1 {
		t.Fatalf("mutations = %d", len(record.Mutations))
	}
	mutation := record.Mutations[0]
	if mutation.Kind != dataengine.MutationPut || mutation.Key.ID != 7 || mutation.ExpectedVersion != 4 || mutation.NextVersion != 5 {
		t.Fatalf("mutation was not canonicalized: %+v", mutation)
	}
	if mutation.EntityID != 0 || mutation.Resource != "" || mutation.Version != 0 {
		t.Fatalf("legacy mutation fields escaped prepare: %+v", mutation)
	}
}
