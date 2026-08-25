package nestwal

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corecheckpoint "github.com/tjbdwanghaibo/cube-core/checkpoint"
	"github.com/tjbdwanghaibo/cube-core/entity"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"github.com/tjbdwanghaibo/cube-core/obs"
)

// The pipelined pilot drives a real Nest engine against a real on-disk WAL
// with a game-shaped load (hot entity + uniform spread) and reports the data
// the Phase 2 decision needs: the nest.pipelined.durable_wait share of worker
// busy time, throughput, and client latency percentiles, for strict vs
// pipelined at two worker counts. It is opt-in because it is a measurement
// run, not a correctness test:
//
//	NESTWAL_PILOT=1 go test -run TestPipelinedPilot -v ./nestwal/
const pilotEnv = "NESTWAL_PILOT"

type pilotDao struct {
	id      int64
	Tracker corecheckpoint.DirtyTracker
	Value   int64
}

func (d *pilotDao) Id() int64                                  { return d.id }
func (d *pilotDao) SetId(id int64)                             { d.id = id }
func (d *pilotDao) DbName() string                             { return "pilot" }
func (d *pilotDao) CollName() string                           { return "pilot_entities" }
func (d *pilotDao) Dirty() entity.IDirty                       { return &d.Tracker }
func (d *pilotDao) CleanDirty()                                { d.Tracker.SelfClean() }
func (d *pilotDao) DirtyTracker() *corecheckpoint.DirtyTracker { return &d.Tracker }
func (d *pilotDao) Marshal() []byte {
	raw, _ := json.Marshal(struct {
		ID    int64 `json:"id"`
		Value int64 `json:"value"`
	}{ID: d.id, Value: d.Value})
	return raw
}
func (d *pilotDao) CaptureRollbackState() ([]byte, error) { return d.Marshal(), nil }
func (d *pilotDao) RestoreRollbackState(raw []byte) error {
	var doc struct {
		ID    int64 `json:"id"`
		Value int64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	d.id = doc.ID
	d.Value = doc.Value
	return nil
}
func (d *pilotDao) PrepareCommit(tx *corenest.RollbackTx) error {
	if !d.Tracker.Dirty() {
		return nil
	}
	return tx.AddMutation(corenest.EntityMutation{
		EntityID: d.id,
		Resource: d.CollName(),
		Mask:     d.Tracker.Snapshot().PersistDirty,
		Codec:    "json",
		Data:     d.Marshal(),
	})
}

type pilotEntity struct {
	*entity.EntityBase
	dao *pilotDao
}

func (e *pilotEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *pilotEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

type pilotGetter struct {
	entities map[int64]entity.IThreadSafeEntity
}

func (g *pilotGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	e, ok := g.entities[id]
	if !ok {
		return nil, corenest.ErrEntityNotFound
	}
	return e, nil
}

func (g *pilotGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	ret := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		ret[i] = g.entities[id]
	}
	return ret, nil
}

const pilotEntityKind entity.EntityKind = 237

var pilotKindOnce sync.Once

func pilotEntityID(t *testing.T, unique int64) int64 {
	t.Helper()
	pilotKindOnce.Do(func() {
		entity.MustRegisterEntityKindCategory(pilotEntityKind, entity.EntityCategory(1))
	})
	id, err := entity.BuildEntityID(unique, pilotEntityKind)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var pilotHandlersOnce sync.Once

func registerPilotHandlers() {
	handler := func(es []entity.IThreadSafeEntity, _ []any, _ ...corenest.HandlerOption) (any, error) {
		e := es[0].(*pilotEntity)
		old := e.dao.Value
		if !corenest.RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value++
		e.dao.Tracker.MarkPersist(1)
		return nil, nil
	}
	corenest.MustRegisterHandlerWithMeta(corenest.NewHandlerName("pilot_strict"), handler,
		corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict})
	corenest.MustRegisterHandlerWithMeta(corenest.NewHandlerName("pilot_pipelined"), handler,
		corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined})
}

type pilotResult struct {
	scenario        string
	requests        int64
	throughput      float64 // requests/s
	hotThroughput   float64 // hot-entity requests/s: all serialized on one worker+lock chain
	p50, p95, p99   time.Duration
	hotP50, hotP99  time.Duration
	waitShare       float64 // durable_wait total / dispatch cost total
	durableWaitMean time.Duration
	durableWaitMax  time.Duration
}

func runPilotScenario(t *testing.T, name string, handler string, workers int, async bool, duration time.Duration) pilotResult {
	t.Helper()
	// Fresh metrics registry, WAL, committer, and engine per scenario: an
	// engine is single-use and the measurements must not bleed across runs.
	registry := obs.NewRegistry()
	obs.SetDefaultRegistry(registry)
	defer obs.SetDefaultRegistry(obs.NewRegistry())

	walOpts := DefaultOptions(t.TempDir())
	w, err := Open(walOpts)
	if err != nil {
		t.Fatal(err)
	}
	committer, err := NewCommitter(w, MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error { return nil }), nil, DefaultCommitterOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close(context.Background())

	const entityCount = 256
	getter := &pilotGetter{entities: make(map[int64]entity.IThreadSafeEntity, entityCount)}
	ids := make([]int64, entityCount)
	for i := range ids {
		id := pilotEntityID(t, int64(9000+i))
		ids[i] = id
		getter.entities[id] = &pilotEntity{
			EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, pilotEntityKind),
			dao:        &pilotDao{id: id},
		}
	}
	hotID := ids[0]

	engineOpts := []corenest.NestOption{
		corenest.NestOptionWithGetter(getter),
		corenest.NestOptionWithTransactionCommitter(committer),
		corenest.NestOptionWithWorkerNumAndMsgCap(workers, 1, 4096),
		corenest.NestOptionWithTickDuration(100 * time.Millisecond),
	}
	if async {
		engineOpts = append(engineOpts, corenest.NestOptionWithPipelinedAsyncCompletion(0, 0))
	}
	engine := corenest.NewEngine(engineOpts...)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Shutdown(ctx)
	}()

	// Game-shaped load: 25% of requests hit one hot entity (guild/alliance
	// pattern), the rest spread uniformly. Client goroutines model gateway
	// sessions issuing serial requests.
	const clients = 32
	var requests, hotRequests atomic.Int64
	latencies := make([][]time.Duration, clients)
	hotLatencies := make([][]time.Duration, clients)
	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(c), 42))
			samples := make([]time.Duration, 0, 4096)
			hotSamples := make([]time.Duration, 0, 1024)
			for time.Now().Before(deadline) {
				id := hotID
				hot := rng.IntN(4) == 0
				if !hot {
					id = ids[rng.IntN(entityCount)]
				}
				start := time.Now()
				if _, err := engine.Request(context.Background(), corenest.NewHandlerName(handler), id, nil); err != nil {
					t.Errorf("pilot request: %v", err)
					return
				}
				elapsed := time.Since(start)
				samples = append(samples, elapsed)
				requests.Add(1)
				if hot {
					hotSamples = append(hotSamples, elapsed)
					hotRequests.Add(1)
				}
			}
			latencies[c] = samples
			hotLatencies[c] = hotSamples
		}(c)
	}
	wg.Wait()

	merge := func(buckets [][]time.Duration) []time.Duration {
		all := make([]time.Duration, 0, requests.Load())
		for _, samples := range buckets {
			all = append(all, samples...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
		return all
	}
	percentile := func(sorted []time.Duration, p float64) time.Duration {
		if len(sorted) == 0 {
			return 0
		}
		return sorted[int(float64(len(sorted)-1)*p)]
	}
	all := merge(latencies)
	hot := merge(hotLatencies)

	result := pilotResult{
		scenario:      name,
		requests:      requests.Load(),
		throughput:    float64(requests.Load()) / duration.Seconds(),
		hotThroughput: float64(hotRequests.Load()) / duration.Seconds(),
		p50:           percentile(all, 0.50),
		p95:           percentile(all, 0.95),
		p99:           percentile(all, 0.99),
		hotP50:        percentile(hot, 0.50),
		hotP99:        percentile(hot, 0.99),
	}
	var waitTotal, dispatchTotal int64
	for _, metric := range registry.Snapshot() {
		switch metric.Name {
		case "nest.pipelined.durable_wait":
			waitTotal += metric.TotalNanos
			if metric.Count > 0 {
				result.durableWaitMean = time.Duration(metric.TotalNanos / metric.Count)
			}
			if time.Duration(metric.MaxNanos) > result.durableWaitMax {
				result.durableWaitMax = time.Duration(metric.MaxNanos)
			}
		case "nest.dispatch.cost":
			dispatchTotal += metric.TotalNanos
		}
	}
	// The "share of worker busy time" reading only holds while the worker
	// actually parks on the ticket (Phase 1). In async mode dispatch cost no
	// longer contains the wait, so the ratio is meaningless — durable_wait
	// then reads as the commit pipeline latency distribution instead.
	if dispatchTotal > 0 && !async {
		result.waitShare = float64(waitTotal) / float64(dispatchTotal)
	}
	return result
}

func TestPipelinedPilot(t *testing.T) {
	if os.Getenv(pilotEnv) == "" {
		t.Skipf("measurement run; set %s=1 to execute", pilotEnv)
	}
	pilotHandlersOnce.Do(registerPilotHandlers)
	duration := 3 * time.Second
	if raw := os.Getenv("NESTWAL_PILOT_DURATION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
		duration = parsed
	}

	type pilotScenario struct {
		name    string
		handler string
		workers int
		async   bool
	}
	scenarios := []pilotScenario{
		{"strict/w4", "pilot_strict", 4, false},
		{"pipelined/w4", "pilot_pipelined", 4, false},
		{"strict/w16", "pilot_strict", 16, false},
		{"pipelined/w16", "pilot_pipelined", 16, false},
		// w32 exists to answer one question: does adding workers keep lifting
		// the hot-entity chain, or is it pinned to one worker's serial
		// 1/durable_wait ceiling (the Phase 2 decision criterion)?
		{"strict/w32", "pilot_strict", 32, false},
		{"pipelined/w32", "pilot_pipelined", 32, false},
	}
	// Phase 2 async completion: the worker no longer parks on the ticket,
	// so the hot-entity chain should break through 1/durable_wait.
	scenarios = append(scenarios,
		pilotScenario{"async/w4", "pilot_pipelined", 4, true},
		pilotScenario{"async/w16", "pilot_pipelined", 16, true},
	)
	results := make([]pilotResult, 0, len(scenarios))
	for _, scenario := range scenarios {
		results = append(results, runPilotScenario(t, scenario.name, scenario.handler, scenario.workers, scenario.async, duration))
	}

	t.Log("scenario | req/s | hot req/s | p50 | p95 | p99 | hot p50 | hot p99 | wait share | wait mean | wait max")
	for _, r := range results {
		t.Logf("%-13s | %7.0f | %7.0f | %10s | %10s | %10s | %10s | %10s | %5.1f%% | %10s | %10s",
			r.scenario, r.throughput, r.hotThroughput, r.p50, r.p95, r.p99, r.hotP50, r.hotP99, r.waitShare*100, r.durableWaitMean, r.durableWaitMax)
	}
}
