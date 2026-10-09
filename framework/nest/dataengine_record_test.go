package nest

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
)

func TestPrepareCommitRecordPreservesMutationIdentity(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	tx.durability = DurabilityAsync
	if err := tx.AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: 7, Database: "game", Resource: "hero"}, Kind: dataengine.MutationPut, ExpectedVersion: 4, NextVersion: 5, Data: []byte{1}}); err != nil {
		t.Fatal(err)
	}

	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if record.Durability != DurabilityAsync.Record() {
		t.Fatalf("durability = %d", record.Durability)
	}
	if len(record.Mutations) != 1 {
		t.Fatalf("mutations = %d", len(record.Mutations))
	}
	mutation := record.Mutations[0]
	if mutation.Kind != dataengine.MutationPut || mutation.Key.ID != 7 || mutation.ExpectedVersion != 4 || mutation.NextVersion != 5 {
		t.Fatalf("mutation identity changed: %+v", mutation)
	}
}
