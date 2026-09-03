package dataengine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-kit/internal/mongofake"
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
		{name: "batch_256_independent", mutations: 256},
	} {
		b.Run(shape.name, func(b *testing.B) {
			client := mongofake.NewClient()
			store, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: "game"})
			if err != nil {
				b.Fatal(err)
			}
			// Seed one document per collection; each iteration advances its
			// version so every projection exercises a real matching CAS.
			for index := 0; index < shape.mutations; index++ {
				coll := client.Collection("game", fmt.Sprintf("players_%d", index))
				if err := coll.Seed(bson.M{"_id": int64(index + 1), "_version": int64(7)}); err != nil {
					b.Fatal(err)
				}
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
						Key:  coredata.DocumentKey{Database: "game", Resource: fmt.Sprintf("players_%d", index), ID: int64(index + 1)},
						Kind: coredata.MutationPatch, ExpectedVersion: uint64(7 + sequence), NextVersion: uint64(8 + sequence), Schema: 1,
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

func BenchmarkMongoProjectionConflictMatrix(b *testing.B) {
	for _, conflictPercent := range []int{1, 10} {
		b.Run(fmt.Sprintf("conflict_%d_percent", conflictPercent), func(b *testing.B) {
			client := mongofake.NewClient()
			store, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: "game"})
			if err != nil {
				b.Fatal(err)
			}
			if err := client.Collection("game", "players_ok").Seed(bson.M{"_id": int64(1), "_version": int64(7)}); err != nil {
				b.Fatal(err)
			}
			if err := client.Collection("game", "players_conflict").Seed(bson.M{"_id": int64(1), "_version": int64(999), "_last_tx": "other"}); err != nil {
				b.Fatal(err)
			}
			okVersion := uint64(7)
			set, _ := bson.Marshal(bson.M{"level": 8})
			b.ReportAllocs()
			b.ResetTimer()
			for sequence := range b.N {
				conflict := sequence%100 < conflictPercent
				resource := "players_ok"
				if conflict {
					resource = "players_conflict"
				}
				var id coredata.TransactionID
				id[8], id[9], id[10], id[11], id[15] = byte(sequence>>24), byte(sequence>>16), byte(sequence>>8), byte(sequence), 2
				expected, next := uint64(7), uint64(8)
				if !conflict {
					expected, next = okVersion, okVersion+1
					okVersion++
				}
				record := coredata.CommitRecord{ID: id, Mutations: []coredata.Mutation{{
					Key:  coredata.DocumentKey{Database: "game", Resource: resource, ID: int64(1)},
					Kind: coredata.MutationPatch, ExpectedVersion: expected, NextVersion: next, Schema: 1,
					Patch: coredata.FieldPatch{SetBSON: set},
				}}}
				err := store.Project(context.Background(), record)
				if conflict && !errors.Is(err, ErrProjectionConflict) {
					b.Fatalf("conflict err=%v", err)
				}
				if !conflict && err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
