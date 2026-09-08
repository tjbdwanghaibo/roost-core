package dataengine

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-kit/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type benchmarkProjectionStore struct{ projected atomic.Uint64 }

func (store *benchmarkProjectionStore) Project(context.Context, coredata.CommitRecord) error {
	store.projected.Add(1)
	return nil
}

type benchmarkReplayStore struct {
	projectCalls uint64
	batchCalls   uint64
	projected    uint64
}

func (store *benchmarkReplayStore) Project(context.Context, coredata.CommitRecord) error {
	store.projectCalls++
	store.projected++
	return nil
}

func (store *benchmarkReplayStore) ProjectBatch(_ context.Context, records []coredata.CommitRecord) error {
	store.batchCalls++
	store.projected += uint64(len(records))
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

// BenchmarkProjectorWALReplayAckMatrix measures the real file WAL replay and
// checkpoint-ack path with an in-memory BatchProjectionStore. It deliberately
// does not claim MongoDB throughput; the integration workload covers Mongo.
func BenchmarkProjectorWALReplayAckMatrix(b *testing.B) {
	const recordCount = 256
	for _, workload := range []struct {
		name         string
		specialEvery int
	}{
		{name: "ordinary_only"},
		{name: "special_1_percent", specialEvery: 100},
		{name: "special_10_percent", specialEvery: 10},
		{name: "all_special", specialEvery: 1},
	} {
		b.Run(workload.name, func(b *testing.B) {
			records := benchmarkReplayRecords(b, recordCount, workload.specialEvery)
			var totalAcks, totalProjectCalls, totalBatchCalls uint64
			for range b.N {
				b.StopTimer()
				options := nestwal.DefaultOptions(b.TempDir())
				options.WriterVersion = nestwal.WriterVersionV2
				options.GroupCommitInterval = time.Millisecond
				wal, err := nestwal.Open(options)
				if err != nil {
					b.Fatal(err)
				}
				store := &benchmarkReplayStore{}
				projector, err := NewProjector(wal, store, ProjectorOptions{
					ReplayBatchRecords: recordCount, ReplayBatchBytes: 64 << 20,
					CloseWAL: false, IdlePoll: time.Hour,
				})
				if err != nil {
					b.Fatal(err)
				}
				projector.cancel()
				<-projector.done
				for i := range records {
					if _, err := wal.Append(context.Background(), records[i]); err != nil {
						b.Fatal(err)
					}
				}
				if err := wal.Sync(context.Background()); err != nil {
					b.Fatal(err)
				}
				ack := wal.Ack
				var ackCalls uint64
				projector.ack = func(ctx context.Context, fence corenest.CommitFence) error {
					ackCalls++
					return ack(ctx, fence)
				}

				b.StartTimer()
				processed, replayErr := projector.replayPass(context.Background())
				b.StopTimer()
				if replayErr != nil || processed != recordCount {
					b.Fatalf("processed=%d err=%v", processed, replayErr)
				}
				if store.projected != recordCount || ackCalls == 0 {
					b.Fatalf("projected=%d checkpoint acks=%d", store.projected, ackCalls)
				}
				remaining := 0
				if err := wal.Replay(context.Background(), func(corenest.CommitFence, coredata.CommitRecord) error {
					remaining++
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				if remaining != 0 {
					b.Fatalf("WAL retained %d acknowledged records", remaining)
				}
				totalAcks += ackCalls
				totalProjectCalls += store.projectCalls
				totalBatchCalls += store.batchCalls
				if err := wal.Close(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(totalAcks)/float64(b.N), "acks/op")
			b.ReportMetric(float64(totalProjectCalls)/float64(b.N), "project_calls/op")
			b.ReportMetric(float64(totalBatchCalls)/float64(b.N), "batch_calls/op")
		})
	}
}

func benchmarkReplayRecords(b *testing.B, count, specialEvery int) []coredata.CommitRecord {
	b.Helper()
	const entityCount = 32
	versions := make([]uint64, entityCount)
	records := make([]coredata.CommitRecord, count)
	for i := range records {
		entityIndex := i % entityCount
		entityID := int64(entityIndex + 1)
		expected := versions[entityIndex]
		next := expected + 1
		var mutation coredata.Mutation
		if expected == 0 {
			data, err := bson.Marshal(bson.M{"_id": entityID, "value": int64(i)})
			if err != nil {
				b.Fatal(err)
			}
			mutation = coredata.Mutation{
				Key:  coredata.DocumentKey{Database: "benchmark", Resource: "entities", ID: entityID},
				Kind: coredata.MutationPut, ExpectedVersion: expected, NextVersion: next,
				Mask: coredata.AllFields, Schema: 1, Codec: "bson-v2", Data: data,
			}
		} else {
			set, err := bson.Marshal(bson.D{{Key: "value", Value: int64(i)}})
			if err != nil {
				b.Fatal(err)
			}
			mutation = coredata.Mutation{
				Key:  coredata.DocumentKey{Database: "benchmark", Resource: "entities", ID: entityID},
				Kind: coredata.MutationPatch, ExpectedVersion: expected, NextVersion: next,
				Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: set},
			}
		}
		var transactionID coredata.TransactionID
		binary.BigEndian.PutUint64(transactionID[8:], uint64(i+1))
		record := coredata.CommitRecord{
			ID: transactionID, Handler: "benchmark-wal-replay", Durability: corenest.DurabilityAsync,
			Mutations: []coredata.Mutation{mutation},
		}
		if specialEvery > 0 && (i+1)%specialEvery == 0 {
			record.Effects = []coredata.Effect{{
				ID: fmt.Sprintf("benchmark-effect-%d", i+1), Topic: "benchmark.changed",
			}}
		}
		records[i] = record
		versions[entityIndex] = next
	}
	return records
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
