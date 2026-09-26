package engine

import (
	"context"
	"sync/atomic"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// BenchmarkProjectorReserveDiscard 只测 WAL 准入的锁内簿记（reserve + discard），不含 WAL 写入：
// RR-20260926-30 在这里加了原生步骤的实体屏障检查，用它做同机前后对照。
func BenchmarkProjectorReserveDiscard(b *testing.B) {
	options := nestwal.DefaultOptions(b.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		b.Fatal(err)
	}
	defer wal.Close(context.Background())
	projector, err := NewProjector(wal, &benchmarkProjectionStore{}, ProjectorOptions{CloseWAL: false, ManualReplay: true})
	if err != nil {
		b.Fatal(err)
	}
	defer projector.Close(context.Background())
	set, _ := bson.Marshal(bson.M{"level": 7})
	var sequence atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			value := sequence.Add(1)
			var id coredata.TransactionID
			for index := 0; index < 8; index++ {
				id[15-index] = byte(value >> (index * 8))
			}
			record := coredata.CommitRecord{ID: id, Mutations: []coredata.Mutation{{
				Key:  coredata.DocumentKey{Database: "game", Resource: "players", ID: int64(value)},
				Kind: coredata.MutationPatch, ExpectedVersion: 7, NextVersion: 8, Schema: 1,
				Patch: coredata.FieldPatch{SetBSON: set},
			}}}
			if err := projector.reserve(record, true); err != nil {
				b.Error(err)
				return
			}
			projector.discard(id)
		}
	})
}
