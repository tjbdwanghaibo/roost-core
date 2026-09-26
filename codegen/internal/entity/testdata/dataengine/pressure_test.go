package persistflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 保留 MongoStore 的批量能力。只在成功投影返回后记录耗时，不改变提交、重试和 ack。
type pressureStore struct {
	*engine.MongoStore
	active            atomic.Bool
	mu                sync.Mutex
	ages              []time.Duration
	service           time.Duration
	calls, batchCalls int
}

func (s *pressureStore) observe(records []coredata.CommitRecord, started time.Time, err error, batch bool) {
	if !s.active.Load() || err != nil {
		return
	}
	finished := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if batch {
		s.batchCalls++
	}
	s.service += finished.Sub(started)
	for _, record := range records {
		s.ages = append(s.ages, finished.Sub(time.Unix(0, record.CreatedAt)))
	}
}
func (s *pressureStore) Project(ctx context.Context, r coredata.CommitRecord) error {
	started := time.Now()
	err := s.MongoStore.Project(ctx, r)
	s.observe([]coredata.CommitRecord{r}, started, err, false)
	return err
}
func (s *pressureStore) ProjectBatch(ctx context.Context, records []coredata.CommitRecord) error {
	started := time.Now()
	err := s.MongoStore.ProjectBatch(ctx, records)
	s.observe(records, started, err, true)
	return err
}

type pressureResult struct {
	PayloadBytesPerDAO       int                   `json:"payload_bytes_per_dao"`
	BurstRequests            int                   `json:"burst_requests"`
	ColdEntities             int                   `json:"cold_entities"`
	Mixed                    *mixedResult          `json:"mixed,omitempty"`
	Shape                    string                `json:"shape"`
	Policy                   string                `json:"policy"`
	Entities                 int                   `json:"entities"`
	Writers                  int                   `json:"writers"`
	Requests                 int                   `json:"requests"`
	DAOsPerRequest           int                   `json:"daos_per_request"`
	ClientAPI                string                `json:"client_api"`
	OfferedPerSecond         int                   `json:"offered_per_second"`
	ScheduledP99MS           float64               `json:"scheduled_to_reply_p99_ms"`
	RequestSeconds           float64               `json:"request_seconds"`
	DrainSeconds             float64               `json:"drain_seconds"`
	CompletedPerSecond       float64               `json:"request_completed_per_second"`
	PersistedPerSecond       float64               `json:"persisted_per_second_including_drain"`
	RequestP95MS             float64               `json:"request_p95_ms"`
	RequestP99MS             float64               `json:"request_p99_ms"`
	ProjectionP95MS          float64               `json:"record_to_mongo_p95_ms"`
	ProjectionP99MS          float64               `json:"record_to_mongo_p99_ms"`
	UnackedAtRequestsDone    uint64                `json:"unacked_at_requests_done"`
	UnackedPeakSample        uint64                `json:"unacked_peak_after_request"`
	FinalStats               engine.ProjectorStats `json:"final_stats"`
	MongoSeconds             float64               `json:"mongo_service_seconds"`
	MongoCalls               int                   `json:"mongo_calls"`
	MongoBatchCalls          int                   `json:"mongo_batch_calls"`
	AckSeconds               float64               `json:"ack_seconds"`
	AckCalls                 uint64                `json:"ack_calls"`
	AllocatedBytesPerRequest uint64                `json:"allocated_bytes_per_request"`
	GCs                      uint32                `json:"gc_cycles"`
}

func pressureInt(t *testing.T, key string, fallback, limit int) int {
	t.Helper()
	if os.Getenv(key) == "" {
		return fallback
	}
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v < 1 || v > limit {
		t.Fatalf("%s must be in [1,%d]", key, limit)
	}
	return v
}

// 固定请求数、有界并发的闭环压力。停止发送后仍计入 Flush 排空耗时，避免把排队伪装为吞吐。
func TestGeneratedDataEnginePressure(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_PERF") != "1" {
		t.Skip("use scripts/perf/dataengine.sh")
	}
	shape, policyText := os.Getenv("ROOST_PERF_SHAPE"), os.Getenv("ROOST_PERF_POLICY")
	if shape == "" {
		shape = "pair"
	}
	policy := nest.DurabilityAsync
	switch policyText {
	case "", "async":
	case "strict":
		policy = nest.DurabilityStrict
	case "pipelined":
		policy = nest.DurabilityPipelined
	default:
		t.Fatal("invalid policy")
	}
	daos := 1
	switch shape {
	case "single":
	case "dual":
		daos = 2
	case "pair", "hot", "cold":
		daos = 4
	default:
		t.Fatal("invalid shape")
	}
	payloadBytes := pressureInt(t, "ROOST_PERF_PAYLOAD_BYTES", 0, 1<<20)
	burst := pressureInt(t, "ROOST_PERF_BURST", 1, 10000)
	padding := strings.Repeat("p", payloadBytes)
	clientAPI := "Request"
	if daos == 4 {
		clientAPI = "RequestMulti"
	}
	n := pressureInt(t, "ROOST_PERF_REQUESTS", 1000, 100000)
	rate := pressureInt(t, "ROOST_PERF_RATE", 0, 100000)
	writers := pressureInt(t, "ROOST_PERF_WRITERS", 32, 256)
	count := pressureInt(t, "ROOST_PERF_ENTITIES", 128, 10000)
	if shape == "hot" {
		count = 2
	}
	if count < 2 || count%2 != 0 {
		t.Fatal("entities must be even and at least 2")
	}
	uri, root := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_GENERATED_WAL_DIR")
	database := NewWalletDao().DbName()
	if uri == "" || root == "" || !strings.HasPrefix(database, "roost_generated_it_") || database == "roost_generated_it_placeholder" {
		t.Fatal("isolated environment required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	RegisterEntity()
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	observed := &pressureStore{MongoStore: store, ages: make([]time.Duration, 0, n)}
	opts := nestwal.DefaultOptions(filepath.Join(root, "pressure"))
	opts.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	projector, err := engine.NewProjector(wal, observed, engine.ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	var ackN, ackNS atomic.Uint64
	projector.OverrideAck(func(ctx context.Context, fence nest.CommitFence) error {
		started := time.Now()
		err := wal.Ack(ctx, fence)
		if observed.active.Load() {
			ackN.Add(1)
			ackNS.Add(uint64(time.Since(started)))
		}
		return err
	})
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, noEffectsPublisher{}, engine.OutboxWorkerOptions{Owner: "pressure"})
	if err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	rt, err := engine.NewRuntime(store, wal, projector, outbox, access, nil, nil, engine.PipelinedRuntimeConfig{Allowlist: []string{"trade_seed", "pressure_update"}, Async: true, AsyncWorkers: 2, AsyncQueueCap: 128})
	if err != nil {
		t.Fatal(err)
	}
	if err = rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Shutdown(context.Background()) })
	options := append(rt.NestOptions(), nest.NestOptionWithGetter(access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 8, QueueCap: 4096}, nest.WorkerPoolConfig{Workers: 128, QueueCap: 256}))
	var mixed *mixedLoad
	if os.Getenv("ROOST_PERF_MIXED") == "1" {
		if shape == "cold" {
			t.Fatal("mixed AOI cannot retain evicted entity pointers")
		}
		mixed = newMixedLoad(t, ctx, count)
		options = append(options, nest.NestOptionWithEntitySync(mixed.manager))
	}
	scheduler := nest.NewEngine(options...)
	meta := nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy}
	scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_seed"), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		e := es[0].(*Trader)
		e.wallet.SetCoins(10000)
		e.inventory.SetItems(1000)
		if payloadBytes > 0 {
			e.wallet.SetPayload(padding)
			e.inventory.SetPayload(padding)
		}
		return nil, nil
	}, meta)
	scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("pressure_update"), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		for _, value := range es {
			e := value.(*Trader)
			e.wallet.SetCoins(e.wallet.GetCoins() + 1)
			e.wallet.SetTransfers(e.wallet.GetTransfers() + 1)
			if payloadBytes > 0 {
				e.wallet.SetPayload(fmt.Sprintf("%08d", e.wallet.GetTransfers()) + padding)
			}
			if daos > 1 {
				e.inventory.SetItems(e.inventory.GetItems() + 1)
				e.inventory.SetTransfers(e.inventory.GetTransfers() + 1)
				if payloadBytes > 0 {
					e.inventory.SetPayload(fmt.Sprintf("%08d", e.inventory.GetTransfers()) + padding)
				}
			}
		}
		return nil, nil
	}, meta)
	if mixed != nil {
		mixed.installHandler(scheduler)
	}
	if err = scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Shutdown(context.Background()) })
	h := &tradeFixture{ctx: ctx, client: client, database: database, runtime: rt, scheduler: scheduler, access: access, wal: wal}
	ids := h.seed(t, 30000, count)
	cold := 0
	if shape == "cold" {
		for i, id := range ids {
			if i%2 == 0 {
				if err := access.Destroy(ctx, access.Manager().Get(id), entity.EntityDestroyReason(0), false); err != nil {
					t.Fatal(err)
				}
				cold++
			}
		}
	}
	if mixed != nil {
		mixed.start(t, scheduler, access, ids)
	}

	beforeStats := projector.Stats()
	latencies := make([]time.Duration, n)
	scheduledLatencies := make([]time.Duration, n)
	counts := make([]atomic.Int64, count)
	var next atomic.Uint64
	var peak atomic.Uint64
	failures := make(chan error, writers)
	var group sync.WaitGroup
	runtime.GC()
	var beforeMem, afterMem runtime.MemStats
	runtime.ReadMemStats(&beforeMem)
	if path := os.Getenv("ROOST_PERF_CPU_PROFILE"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := pprof.StartCPUProfile(file); err != nil {
			t.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}
	observed.active.Store(true)
	started := time.Now()
	for range writers {
		group.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= n {
					return
				}
				scheduled := started
				if rate > 0 {
					scheduled = started.Add(time.Duration(index/burst*burst) * time.Second / time.Duration(rate))
					if delay := time.Until(scheduled); delay > 0 {
						timer := time.NewTimer(delay)
						select {
						case <-timer.C:
						case <-ctx.Done():
							timer.Stop()
							failures <- ctx.Err()
							return
						}
					}
				}
				first := index % count
				entityIDs := []int64{ids[first]}
				if daos == 4 {
					first = 2 * (index % (count / 2))
					entityIDs = []int64{ids[first], ids[first+1]}
				}
				callStarted := time.Now()
				var err error
				if daos == 4 {
					var opts []nest.SendOpt
					if shape == "cold" {
						opts = append(opts, nest.SendOptionSlow())
					}
					_, err = scheduler.RequestMulti(ctx, nest.NewHandlerName("pressure_update"), entityIDs, nil, opts...)
				} else {
					_, err = scheduler.Request(ctx, nest.NewHandlerName("pressure_update"), entityIDs[0], nil)
				}
				latencies[index] = time.Since(callStarted)
				if rate > 0 {
					scheduledLatencies[index] = time.Since(scheduled)
				}
				if err != nil {
					failures <- err
					return
				}
				counts[first].Add(1)
				if daos == 4 {
					counts[first+1].Add(1)
				}
				pending := projector.Stats().WALUnacked
				for old := peak.Load(); pending > old; old = peak.Load() {
					if peak.CompareAndSwap(old, pending) {
						break
					}
				}
			}
		})
	}
	group.Wait()
	requestElapsed := time.Since(started)
	var mixedStats *mixedResult
	if mixed != nil {
		mixedStats = mixed.stop(t)
	}
	unacked := projector.Stats().WALUnacked
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	drainStarted := time.Now()
	if err = rt.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	drainElapsed := time.Since(drainStarted)
	totalElapsed := time.Since(started)
	observed.active.Store(false)
	if os.Getenv("ROOST_PERF_CPU_PROFILE") != "" {
		pprof.StopCPUProfile()
	}
	runtime.ReadMemStats(&afterMem)
	stats := projector.Stats()
	if stats.Committed-beforeStats.Committed != uint64(n) || stats.Projected-beforeStats.Projected != uint64(n) || stats.WALUnacked != 0 || stats.ProjectionFailures != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if remaining := h.records(t); len(remaining) != 0 {
		t.Fatalf("remaining WAL=%d", len(remaining))
	}
	// 每份 DAO 都验证，而非只核对事务计数。单 DAO 场景还检查未修改的背包版本。
	for i, id := range ids {
		updates := counts[i].Load()
		value, loadErr := access.Get(ctx, id, entity.EntityCategoryNone)
		if loadErr != nil || value == nil {
			t.Fatalf("final load: %v", loadErr)
		}
		e := value.(*Trader)
		if e.wallet.GetCoins() != 10000+updates || e.wallet.GetTransfers() != updates || e.wallet.DirtyTracker().Version() != uint64(1+updates) {
			t.Fatalf("wallet memory mismatch: %d", id)
		}
		inventoryUpdates := updates
		if daos == 1 {
			inventoryUpdates = 0
		}
		if e.inventory.GetItems() != 1000+inventoryUpdates || e.inventory.GetTransfers() != inventoryUpdates || e.inventory.DirtyTracker().Version() != uint64(1+inventoryUpdates) {
			t.Fatalf("inventory memory mismatch: %d", id)
		}
		for _, resource := range []string{"trade_wallets", "trade_inventories"} {
			var doc struct {
				Payload   string `bson:"payload"`
				Coins     int64  `bson:"coins"`
				Items     int64  `bson:"items"`
				Transfers int64  `bson:"transfers"`
				Version   uint64 `bson:"_version"`
			}
			if err := client.Database(database).Collection(resource).FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
				t.Fatal(err)
			}
			want, value, base := updates, doc.Coins, int64(10000)
			if resource == "trade_inventories" {
				want, value, base = inventoryUpdates, doc.Items, 1000
			}
			expectedPayload := padding
			if payloadBytes > 0 && want > 0 {
				expectedPayload = fmt.Sprintf("%08d", want) + padding
			}
			if doc.Payload != expectedPayload {
				t.Fatalf("payload mismatch: %s/%d size=%d want=%d", resource, id, len(doc.Payload), len(expectedPayload))
			}
			if doc.Transfers != want || doc.Version != uint64(1+want) || value != base+want {
				t.Fatalf("Mongo mismatch: %s/%d=%+v want=%d", resource, id, doc, want)
			}
		}
	}
	slices.Sort(latencies)
	slices.Sort(scheduledLatencies)
	observed.mu.Lock()
	defer observed.mu.Unlock()
	if len(observed.ages) != n {
		t.Fatalf("projection samples=%d want=%d", len(observed.ages), n)
	}
	slices.Sort(observed.ages)
	ms := func(v time.Duration) float64 { return float64(v) / float64(time.Millisecond) }
	result := pressureResult{PayloadBytesPerDAO: payloadBytes, BurstRequests: burst, ColdEntities: cold, Mixed: mixedStats, Shape: shape, Policy: policy.String(), Entities: count, Writers: writers, Requests: n, DAOsPerRequest: daos,
		ClientAPI:        clientAPI,
		OfferedPerSecond: rate, ScheduledP99MS: ms(scheduledLatencies[(n-1)*99/100]),
		RequestSeconds: requestElapsed.Seconds(), DrainSeconds: drainElapsed.Seconds(), CompletedPerSecond: float64(n) / requestElapsed.Seconds(), PersistedPerSecond: float64(n) / totalElapsed.Seconds(),
		RequestP95MS: ms(latencies[(n-1)*95/100]), RequestP99MS: ms(latencies[(n-1)*99/100]), ProjectionP95MS: ms(observed.ages[(n-1)*95/100]), ProjectionP99MS: ms(observed.ages[(n-1)*99/100]),
		UnackedAtRequestsDone: unacked, UnackedPeakSample: peak.Load(), FinalStats: stats, MongoSeconds: observed.service.Seconds(), MongoCalls: observed.calls, MongoBatchCalls: observed.batchCalls,
		AckSeconds: time.Duration(ackNS.Load()).Seconds(), AckCalls: ackN.Load(), AllocatedBytesPerRequest: (afterMem.TotalAlloc - beforeMem.TotalAlloc) / uint64(n), GCs: afterMem.NumGC - beforeMem.NumGC}
	h.close(t, nil)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("ROOST_PERF_OUTPUT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println(string(data))
}
