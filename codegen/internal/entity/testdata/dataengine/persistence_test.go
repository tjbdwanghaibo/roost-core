package persistflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 本工作负载没有副作用消息；真实 Broker 故障由 kit/dataengine 集成测试覆盖。
type noEffectsPublisher struct{}

func (noEffectsPublisher) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect")
}

func TestGeneratedDataEngineProcessLifecycle(t *testing.T) {
	uri, phaseText := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_GENERATED_PHASE")
	if uri == "" || phaseText == "" {
		t.Skip("run scripts/test-dataengine-generated.sh")
	}
	database := NewStateDao().DbName()
	if !strings.HasPrefix(database, "roost_generated_it_") || database == "roost_generated_it_placeholder" {
		t.Fatal("refuse non-isolated database", database)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	if phaseText == "cleanup" {
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Fatal(err)
		}
		return
	}
	phase, err := strconv.Atoi(phaseText)
	if err != nil || phase < 1 || phase > 3 {
		t.Fatal("invalid phase", phaseText)
	}
	root := os.Getenv("ROOST_GENERATED_WAL_DIR")
	if root == "" {
		t.Fatal("missing WAL directory")
	}
	RegisterEntity()
	for policyIndex, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			const entities, updates, workers = 100, 10, 8
			store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			opts := nestwal.DefaultOptions(filepath.Join(root, policy.String()))
			opts.WriterVersion = nestwal.WriterVersionV2
			wal, err := nestwal.Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = wal.Close(context.Background()) })
			projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{CloseWAL: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = projector.Close(context.Background()) })
			outboxStore, err := engine.NewMongoOutboxStore(store)
			if err != nil {
				t.Fatal(err)
			}
			outbox, err := engine.NewOutboxWorker(outboxStore, noEffectsPublisher{}, engine.OutboxWorkerOptions{Owner: "generated-process"})
			if err != nil {
				t.Fatal(err)
			}
			access := entity.NewManagerAccess(entity.NewEntityManager())
			name := nest.NewHandlerName("generated_increment")
			rollbackName := nest.NewHandlerName("generated_rollback")
			runtime, err := engine.NewRuntime(store, wal, projector, outbox, access, nil, nil, engine.PipelinedRuntimeConfig{Allowlist: []string{name.String(), rollbackName.String()}, Async: true, AsyncWorkers: 2, AsyncQueueCap: 128})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
			ids := make([]int64, entities)
			wantBefore := int64((phase - 1) * updates)
			for i := range ids {
				id, err := entity.BuildEntityID(int64(policyIndex*entities+i+1), EntityKindAccount)
				if err != nil {
					t.Fatal(err)
				}
				ids[i] = id
				var value entity.IThreadSafeEntity
				if phase == 1 {
					value, err = access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindAccount, Id: id})
				} else {
					value, err = runtime.Repository.LoadEntity(ctx, id, EntityKindAccount)
				}
				if err != nil {
					t.Fatal(err)
				}
				account := value.(*Account)
				if account.state.GetValue() != wantBefore || account.state.DirtyTracker().Version() != uint64(wantBefore) {
					t.Fatalf("phase=%d id=%d loaded value/version=%d/%d want=%d", phase, id, account.state.GetValue(), account.state.DirtyTracker().Version(), wantBefore)
				}
			}
			options := append(runtime.NestOptions(), nest.NestOptionWithGetter(access), nest.NestOptionWithWorkerNumAndMsgCap(8, 1, 256))
			scheduler := nest.NewEngine(options...)
			scheduler.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
				state := es[0].(*Account).state
				state.SetValue(state.GetValue() + 1)
				return state.GetValue(), nil
			}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy})
			rejected := errors.New("business rejection")
			scheduler.MustRegisterHandlerWithMeta(rollbackName, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
				es[0].(*Account).state.SetValue(-1)
				return nil, rejected
			}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy})
			if err := scheduler.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = scheduler.Shutdown(context.Background()) })
			if phase < 3 {
				latencies := make([]time.Duration, entities*updates)
				failures := make(chan error, workers)
				var group sync.WaitGroup
				started := time.Now()
				for worker := range workers {
					group.Go(func() {
						for i := worker; i < entities; i += workers {
							for step := 0; step < updates; step++ {
								callStarted := time.Now()
								value, err := scheduler.Request(ctx, name, ids[i], nil)
								latencies[i*updates+step] = time.Since(callStarted)
								if err != nil || value != wantBefore+int64(step)+1 {
									failures <- fmt.Errorf("id=%d value=%v err=%v", ids[i], value, err)
									return
								}
							}
						}
					})
				}
				group.Wait()
				close(failures)
				for err := range failures {
					t.Error(err)
				}
				if t.Failed() {
					return
				}
				elapsed := time.Since(started)
				slices.Sort(latencies)
				t.Logf("phase=%d policy=%s messages=%d writers=%d elapsed=%s completed=%.0f/s p95=%s p99=%s (race enabled)", phase, policy, len(latencies), workers, elapsed, float64(len(latencies))/elapsed.Seconds(), latencies[len(latencies)*95/100], latencies[len(latencies)*99/100])
				wantBefore += updates
			}
			if _, err := scheduler.Request(ctx, rollbackName, ids[0], nil); !errors.Is(err, rejected) {
				t.Fatalf("rollback returned %v", err)
			}
			rolledBack := access.Manager().Get(ids[0]).(*Account).state
			if rolledBack.GetValue() != wantBefore || rolledBack.DirtyTracker().Version() != uint64(wantBefore) {
				t.Fatalf("rollback left value/version=%d/%d, want %d", rolledBack.GetValue(), rolledBack.DirtyTracker().Version(), wantBefore)
			}
			if err := scheduler.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				var doc struct {
					Value   int64  `bson:"value"`
					Version uint64 `bson:"_version"`
				}
				if err := client.Database(database).Collection("states").FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
					t.Fatal(err)
				}
				if doc.Value != wantBefore || doc.Version != uint64(wantBefore) {
					t.Fatalf("phase=%d id=%d persisted=%+v want=%d", phase, id, doc, wantBefore)
				}
			}
			remaining := 0
			if err := wal.Replay(ctx, func(nest.CommitFence, nest.CommitRecord) error { remaining++; return nil }); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("remaining WAL=%d", remaining)
			}
			if err := runtime.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
