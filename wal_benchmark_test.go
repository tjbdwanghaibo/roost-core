package nestwal

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func BenchmarkRecordEncodingMatrix(b *testing.B) {
	type shape struct {
		name        string
		kind        dataengine.MutationKind
		payloadSize int
		patchFields int
	}
	shapes := []shape{
		{name: "full_1KiB", kind: dataengine.MutationPut, payloadSize: 1 << 10},
		{name: "full_16KiB", kind: dataengine.MutationPut, payloadSize: 16 << 10},
		{name: "full_64KiB", kind: dataengine.MutationPut, payloadSize: 64 << 10},
		{name: "patch_1", kind: dataengine.MutationPatch, patchFields: 1},
		{name: "patch_8", kind: dataengine.MutationPatch, patchFields: 8},
		{name: "patch_32", kind: dataengine.MutationPatch, patchFields: 32},
	}
	for _, shape := range shapes {
		for _, mutationCount := range []int{1, 4} {
			for _, effectCount := range []int{0, 1} {
				name := fmt.Sprintf("%s/mutations_%d/effects_%d", shape.name, mutationCount, effectCount)
				b.Run(name, func(b *testing.B) {
					record := benchmarkRecordShape(b, shape.kind, shape.payloadSize, shape.patchFields, mutationCount, effectCount)
					encoded, err := encodeRecordVersion(record, WriterVersionV2)
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if _, err := encodeRecordVersion(record, WriterVersionV2); err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(len(encoded)), "record_bytes")
				})
			}
		}
	}
}

func benchmarkRecordShape(b *testing.B, kind dataengine.MutationKind, payloadSize, patchFields, mutations, effects int) corenest.CommitRecord {
	b.Helper()
	var id dataengine.TransactionID
	id[15] = 1
	record := corenest.CommitRecord{ID: id, Durability: corenest.DurabilityAsync}
	for index := range mutations {
		mutation := dataengine.Mutation{
			Key:  dataengine.DocumentKey{Database: "game", Resource: "players", ID: int64(index + 1)},
			Kind: kind, ExpectedVersion: 7, NextVersion: 8, Schema: 1, Codec: "bson-v2",
		}
		if kind == dataengine.MutationPut {
			mutation.Data = make([]byte, payloadSize)
		} else {
			set := make(bson.D, 0, patchFields)
			for field := range patchFields {
				set = append(set, bson.E{Key: fmt.Sprintf("field_%02d", field), Value: field})
			}
			mutation.Patch.SetBSON, _ = bson.Marshal(set)
		}
		record.Mutations = append(record.Mutations, mutation)
	}
	if effects > 0 {
		record.Effects = []dataengine.Effect{{ID: "effect-1", Topic: "player.changed", Payload: []byte("event")}}
	}
	return record
}

func BenchmarkWALAppendAsyncParallel(b *testing.B) {
	opts := DefaultOptions(b.TempDir())
	opts.SegmentBytes = 1 << 30
	w, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close(context.Background())
	var sequence atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			seq := sequence.Add(1)
			var id corenest.TransactionID
			for i := 0; i < 8; i++ {
				id[15-i] = byte(seq >> (i * 8))
			}
			_, err := w.Append(context.Background(), corenest.CommitRecord{
				ID: id, Durability: corenest.DurabilityAsync,
				Mutations: []corenest.EntityMutation{{
					EntityID: int64(seq), Database: "game", Resource: "players",
					Version: seq, Codec: "bson-full-v1", Data: []byte("small-after-image"),
				}},
			})
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkWALAppendV2AsyncParallel(b *testing.B) {
	opts := DefaultOptions(b.TempDir())
	opts.WriterVersion = WriterVersionV2
	opts.SegmentBytes = 1 << 30
	w, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close(context.Background())
	var sequence atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			seq := sequence.Add(1)
			var id corenest.TransactionID
			for i := 0; i < 8; i++ {
				id[15-i] = byte(seq >> (i * 8))
			}
			_, err := w.Append(context.Background(), corenest.CommitRecord{
				ID: id, Durability: corenest.DurabilityAsync,
				Mutations: []corenest.EntityMutation{{
					Key:             dataengine.DocumentKey{Database: "game", Resource: "players", ID: int64(seq)},
					Kind:            dataengine.MutationPut,
					ExpectedVersion: seq - 1,
					NextVersion:     seq,
					Codec:           "bson-v2",
					Data:            []byte("small-after-image"),
				}},
			})
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkWALAppendStrict(b *testing.B) {
	opts := DefaultOptions(b.TempDir())
	opts.SegmentBytes = 1 << 30
	w, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close(context.Background())
	record := testRecord(1, corenest.DurabilityStrict)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		record.ID[8]++
		if _, err := w.Append(context.Background(), record); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWALAppendStrictParallel(b *testing.B) {
	opts := DefaultOptions(b.TempDir())
	opts.SegmentBytes = 1 << 30
	w, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close(context.Background())
	var sequence atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			seq := sequence.Add(1)
			var id corenest.TransactionID
			for i := 0; i < 8; i++ {
				id[15-i] = byte(seq >> (i * 8))
			}
			_, err := w.Append(context.Background(), corenest.CommitRecord{
				ID: id, Durability: corenest.DurabilityStrict,
				Mutations: []corenest.EntityMutation{{
					EntityID: int64(seq), Database: "game", Resource: "players",
					Version: seq, Codec: "bson-full-v1", Data: []byte("small-after-image"),
				}},
			})
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}
