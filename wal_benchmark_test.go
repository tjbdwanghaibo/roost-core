package nestwal

import (
	"context"
	"sync/atomic"
	"testing"

	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

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
