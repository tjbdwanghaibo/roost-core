package dataengine

import (
	"context"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type migrationDAO struct{}

func (migrationDAO) SchemaVersion() uint32 { return 2 }
func (migrationDAO) Migrate(raw []byte, from uint32) ([]byte, error) {
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["_schema"] = uint32(2)
	doc["migrated_from"] = from
	return bson.Marshal(doc)
}

type projectedSystemTicket struct{ done chan struct{} }

func (ticket projectedSystemTicket) Done() <-chan struct{} { return ticket.done }
func (projectedSystemTicket) Err() error                   { return nil }

type migrationCommitter struct{ records []coredata.CommitRecord }

func (committer *migrationCommitter) CommitSystem(_ context.Context, record coredata.CommitRecord) (coredata.ProjectionTicket, error) {
	committer.records = append(committer.records, coredata.CloneCommitRecord(record))
	done := make(chan struct{})
	close(done)
	return projectedSystemTicket{done: done}, nil
}

func TestMigrationRunnerCommitsVersionedFullMutationAndWaitsForProjection(t *testing.T) {
	committer := &migrationCommitter{}
	runner, err := NewMigrationRunner(committer)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := bson.Marshal(bson.M{"_id": int64(7), "_schema": uint32(1), "name": "old"})
	migrated, err := runner.Migrate(context.Background(), migrationDAO{}, coredata.RawDocument{
		Key: coredata.DocumentKey{Database: "game", Resource: "heroes", ID: 7}, Version: 9, Schema: 1, Data: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !migrated || len(committer.records) != 1 {
		t.Fatalf("migrated=%v records=%d", migrated, len(committer.records))
	}
	record := committer.records[0]
	mutation := record.Mutations[0]
	if record.Handler != MigrationHandler || mutation.Kind != coredata.MutationPut || mutation.ExpectedVersion != 9 || mutation.NextVersion != 10 || mutation.Schema != 2 {
		t.Fatalf("record=%+v", record)
	}
	var payload bson.M
	if err := bson.Unmarshal(mutation.Data, &payload); err != nil || payload["migrated_from"] == nil {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
}

func TestMigrationRunnerRejectsRemoteEnvelopeWithoutOwnershipLease(t *testing.T) {
	runner, _ := NewMigrationRunner(&migrationCommitter{})
	_, err := runner.Migrate(context.Background(), migrationDAO{}, coredata.RawDocument{
		Key: coredata.DocumentKey{Resource: "heroes", ID: 7}, Version: 9, Schema: 1, Data: []byte{1}, Enveloped: true,
	})
	if err != ErrRemoteMigrationLeaseRequired {
		t.Fatalf("err=%v", err)
	}
}
