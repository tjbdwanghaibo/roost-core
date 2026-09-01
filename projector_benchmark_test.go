package dataengine

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"github.com/tjbdwanghaibo/cube-kit/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type benchmarkProjectionStore struct{ projected atomic.Uint64 }

func (store *benchmarkProjectionStore) Project(context.Context, coredata.CommitRecord) error {
	store.projected.Add(1)
	return nil
}

func BenchmarkProjectionSegmentPlanner(b *testing.B) {
	for _, specialEvery := range []int{0, 100, 10, 1} {
		name := fmt.Sprintf("special_every_%d", specialEvery)
		b.Run(name, func(b *testing.B) {
			records := make([]coredata.CommitRecord, 1024)
			for i := range records {
				records[i] = projectorRecord(byte(i%255+1), specialEvery > 0 && i%specialEvery == 0)
			}
			fences := projectionTestFences(records)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := planProjectionSegments(records, fences, 1024, 4<<20); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkProjectorAdmissionMatrix(b *testing.B) {
	for _, durability := range []corenest.DurabilityPolicy{corenest.DurabilityAsync, corenest.DurabilityStrict, corenest.DurabilityPipelined} {
		for _, writers := range []int{1, 8, 32} {
			b.Run(fmt.Sprintf("%s/writers_%d", durability.String(), writers), func(b *testing.B) {
				options := nestwal.DefaultOptions(b.TempDir())
				options.WriterVersion = nestwal.WriterVersionV2
				options.SegmentBytes = 1 << 30
				options.GroupCommitInterval = time.Millisecond
				wal, err := nestwal.Open(options)
				if err != nil {
					b.Fatal(err)
				}
				projector, err := NewProjector(wal, &benchmarkProjectionStore{}, ProjectorOptions{CloseWAL: true, IdlePoll: time.Hour})
				if err != nil {
					b.Fatal(err)
				}
				defer projector.Close(context.Background())
				set, _ := bson.Marshal(bson.M{"level": 7})
				var sequence atomic.Uint64
				previousProcs := runtime.GOMAXPROCS(writers)
				defer runtime.GOMAXPROCS(previousProcs)
				b.SetParallelism(1)
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(iterator *testing.PB) {
					for iterator.Next() {
						value := sequence.Add(1)
						var id coredata.TransactionID
						for index := 0; index < 8; index++ {
							id[15-index] = byte(value >> (index * 8))
						}
						record := coredata.CommitRecord{ID: id, Durability: durability, Mutations: []coredata.Mutation{{
							Key:  coredata.DocumentKey{Database: "game", Resource: "players", ID: int64(value)},
							Kind: coredata.MutationPatch, ExpectedVersion: 7, NextVersion: 8, Schema: 1,
							Patch: coredata.FieldPatch{SetBSON: set},
						}}}
						if durability == corenest.DurabilityPipelined {
							ticket, err := projector.Enqueue(context.Background(), record)
							if err == nil {
								<-ticket.Done()
								err = ticket.Err()
							}
							projector.TransactionReleased(id)
							if err != nil {
								b.Error(err)
								return
							}
							continue
						}
						if err := projector.Commit(context.Background(), record); err != nil {
							b.Error(err)
							return
						}
						projector.TransactionReleased(id)
					}
				})
			})
		}
	}
}
