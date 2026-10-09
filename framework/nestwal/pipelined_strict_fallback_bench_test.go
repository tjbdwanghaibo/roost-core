package nestwal_test

// RR-20260928-11 的同机前后对照基准：真实 Nest（8 快 worker）+ 正式 engine.Projector（正常回放 / Ack）+ 真实 WAL
// （默认 GroupCommitInterval 10ms、BatchDelay 500µs），32 个闭环客户端各自轮换 2 个独立实体。
// broadcast_*：DispatchBroadcast 单目标，从派发到 AfterCommit 的延迟；request_pipelined：快路径 Request（Enqueue 两阶段）对照。
// 修复前 broadcast_pipelined 不等 fsync（async 语义），修复后应与 broadcast_strict 相同；request_pipelined 不受影响。
// 复跑：把本文件复制进基线副本的 nestwal/，两侧 go test -c 后交替运行 -test.bench BenchmarkBroadcastPipelinedCommit，benchstat 比较。

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// fallbackBenchKind 与 fallbackKind（198）、remote_codec_test.go 的 125 / 197 不撞号。
const fallbackBenchKind entity.EntityKind = 199

type fallbackBenchDao struct {
	id      int64
	Tracker coredata.Tracker
	Value   int
}

func (d *fallbackBenchDao) Id() int64                       { return d.id }
func (d *fallbackBenchDao) SetId(id int64)                  { d.id = id }
func (d *fallbackBenchDao) DbName() string                  { return "test" }
func (d *fallbackBenchDao) CollName() string                { return "rr11_bench" }
func (d *fallbackBenchDao) Dirty() entity.IDirty            { return &d.Tracker }
func (d *fallbackBenchDao) CleanDirty()                     { d.Tracker.SelfClean() }
func (d *fallbackBenchDao) DirtyTracker() *coredata.Tracker { return &d.Tracker }
func (d *fallbackBenchDao) marshal() []byte {
	raw, _ := json.Marshal(struct {
		ID    int64 `json:"id"`
		Value int   `json:"value"`
	}{d.id, d.Value})
	return raw
}
func (d *fallbackBenchDao) CaptureRollbackState() ([]byte, error) { return d.marshal(), nil }
func (d *fallbackBenchDao) RestoreRollbackState(raw []byte) error {
	var doc struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	d.Value = doc.Value
	return nil
}
func (d *fallbackBenchDao) PrepareMutation(change corenest.PersistChange) (coredata.Mutation, error) {
	version := d.Tracker.Version()
	return coredata.Mutation{
		Key:  coredata.DocumentKey{Database: "test", Resource: d.CollName(), ID: d.id},
		Kind: coredata.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "json", Data: d.marshal(),
	}, nil
}
func (d *fallbackBenchDao) AcceptMutation(mutation coredata.Mutation) error {
	return d.Tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

type fallbackBenchEntity struct {
	*entity.EntityBase
	dao *fallbackBenchDao
}

func (e *fallbackBenchEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *fallbackBenchEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

type fallbackBenchStore struct{}

func (fallbackBenchStore) Project(context.Context, coredata.CommitRecord) error { return nil }

var fallbackBenchUnique atomic.Int64

func BenchmarkBroadcastPipelinedCommit(b *testing.B) {
	const clients, perClient = 32, 2
	for _, tc := range []struct {
		name      string
		broadcast bool
		meta      corenest.HandlerMeta
	}{
		{"broadcast_strict", true, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict}},
		{"broadcast_pipelined", true, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined}},
		{"request_pipelined", false, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			opts := nestwal.DefaultOptions(b.TempDir())
			opts.SegmentBytes = 1 << 30
			wal, err := nestwal.Open(opts)
			if err != nil {
				b.Fatal(err)
			}
			projector, err := engine.NewProjector(wal, fallbackBenchStore{}, engine.ProjectorOptions{CloseWAL: true})
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = projector.Close(context.Background()) }()
			entity.MustRegisterEntityKindCategory(fallbackBenchKind, entity.EntityCategoryWorld)
			manager := entity.NewEntityManager()
			ids := make([]int64, clients*perClient)
			for i := range ids {
				id, err := entity.BuildEntityID(60000+fallbackBenchUnique.Add(1), fallbackBenchKind)
				if err != nil {
					b.Fatal(err)
				}
				e := &fallbackBenchEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryWorld, false, fallbackBenchKind), dao: &fallbackBenchDao{id: id}}
				if err := manager.TryAdd(e); err != nil {
					b.Fatal(err)
				}
				ids[i] = id
			}
			mgr := corenest.NewEngine(corenest.NestOptionWithGetter(entity.NewManagerAccess(manager)), corenest.NestOptionWithTransactionCommitter(projector),
				corenest.NestOptionWithWorkerPools(corenest.WorkerPoolConfig{Workers: 8, QueueCap: 1024}, corenest.WorkerPoolConfig{}))
			name := corenest.NewHandlerName(fmt.Sprintf("rr11_bench_%s_%d", tc.name, fallbackBenchUnique.Add(1)))
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, params []any, _ ...corenest.HandlerOption) (any, error) {
				e := es[0].(*fallbackBenchEntity)
				old := e.dao.Value
				corenest.RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
				e.dao.Value++
				done := params[0].(chan struct{})
				corenest.AfterCommit(func() { done <- struct{}{} })
				return nil, corenest.MarkPersist(e.dao, 1)
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				b.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			latencies := make([]int64, b.N)
			var next atomic.Int64
			var wg sync.WaitGroup
			b.ResetTimer()
			start := time.Now()
			for c := range clients {
				wg.Go(func() {
					done := make(chan struct{}, 1)
					for k := 0; ; k++ {
						op := next.Add(1) - 1
						if op >= int64(b.N) {
							return
						}
						id := ids[c*perClient+k%perClient]
						began := time.Now()
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						var err error
						if tc.broadcast {
							err = mgr.DispatchBroadcast(ctx, name, []int64{id}, corenest.Params{done})
						} else {
							_, err = mgr.Request(ctx, name, id, corenest.Params{done})
						}
						cancel()
						if err != nil {
							b.Error(err)
							return
						}
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							b.Error("AfterCommit never ran")
							return
						}
						latencies[op] = time.Since(began).Nanoseconds()
					}
				})
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			slices.Sort(latencies)
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "ops/s")
			b.ReportMetric(float64(latencies[(b.N-1)*50/100]), "p50-ns")
			b.ReportMetric(float64(latencies[(b.N-1)*99/100]), "p99-ns")
		})
	}
}
