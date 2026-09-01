package dataengine

import (
	"context"
	"fmt"
	"testing"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// These benchmarks measure Data Engine projection/adapter overhead with a
// deterministic fake. Replica-set latency and failover gates run in CI.
func BenchmarkMongoProjectionMatrix(b *testing.B) {
	for _, shape := range []struct {
		name               string
		mutations, effects int
	}{
		{name: "single_cas", mutations: 1},
		{name: "single_with_outbox", mutations: 1, effects: 1},
		{name: "four_document_transaction", mutations: 4},
	} {
		b.Run(shape.name, func(b *testing.B) {
			database := &mongoStoreFakeDatabase{}
			client := &mongoStoreFakeClient{db: database}
			store, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: "game"})
			if err != nil {
				b.Fatal(err)
			}
			for index := 0; index < shape.mutations; index++ {
				database.Collection(fmt.Sprintf("players_%d", index)).(*mongoStoreFakeCollection).updateResult = &fmongo.UpdateResult{MatchedCount: 1}
			}
			set, _ := bson.Marshal(bson.M{"level": 8})
			b.ReportAllocs()
			b.ResetTimer()
			for sequence := 0; sequence < b.N; sequence++ {
				var id coredata.TransactionID
				id[8], id[9], id[10], id[11] = byte(sequence>>24), byte(sequence>>16), byte(sequence>>8), byte(sequence)
				id[15] = 1
				record := coredata.CommitRecord{ID: id}
				for index := 0; index < shape.mutations; index++ {
					record.Mutations = append(record.Mutations, coredata.Mutation{
						Key:  coredata.DocumentKey{Database: "game", Resource: fmt.Sprintf("players_%d", index), ID: int64(sequence*shape.mutations + index + 1)},
						Kind: coredata.MutationPatch, ExpectedVersion: 7, NextVersion: 8, Schema: 1,
						Patch: coredata.FieldPatch{SetBSON: set},
					})
				}
				if shape.effects > 0 {
					record.Effects = []coredata.Effect{{ID: fmt.Sprintf("effect-%d", sequence), Topic: "player.changed"}}
				}
				if err := store.Project(context.Background(), record); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
