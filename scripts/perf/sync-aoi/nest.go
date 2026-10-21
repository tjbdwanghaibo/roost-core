package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
)

const loadEntityKind entity.EntityKind = 190

func init() { entity.MustRegisterEntityKindCategory(loadEntityKind, entity.EntityCategory(4)) }
func workloadID(index int64, c config) int64 {
	if !c.Nest {
		return index
	}
	id, err := entity.BuildEntityID(index, loadEntityKind)
	if err != nil {
		panic(err)
	}
	return id
}
func workloadIndex(id int64, c config) int64 {
	if c.Nest {
		return entity.GetUniqueIDFromEntityID(id)
	}
	return id
}
func (s *subject) Base() *entity.EntityBase { return s.EntityBase }
func (s *subject) TakeEntitySyncChanges() uint64 {
	mask := s.tracker.TakeEntitySyncDirty()
	if mask != 0 {
		s.data.Committed = time.Now().UnixNano()
	}
	return mask
}

type loadGetter map[int64]*subject

func (g loadGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if s := g[id]; s != nil {
		return s, nil
	}
	return nil, fmt.Errorf("load entity %d missing", id)
}
func (g loadGetter) GetMany(ctx context.Context, ids []int64, cats []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	es := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		e, err := g.Get(ctx, id, 0)
		if err != nil {
			return nil, err
		}
		es[i] = e
	}
	return es, nil
}

type loadMutation struct {
	step    int
	planned time.Time
	sent    time.Time // 发生器调用 Request 之前的时刻（服务端进程单调时钟）。
}

// splitTimings 记录服务端进程里能看到的分段（只计正式测量 tick），写进 server.json 的 harness / server：
//
//	planned ─harness.admit_lag─▶ 批次开始 ─harness.client_queue─▶ sent ─server.request_to_change─▶ handler 改值（Changed）
//
// Changed 之后到客户端的分段在 client.json（server.change_to_receive_ms / harness.client_decode_ms）。
type splitTimings struct {
	mu                                     sync.Mutex
	admitLag, clientQueue, requestToChange []float64
}

func (s *splitTimings) sent(planned, begin, sent time.Time) {
	s.mu.Lock()
	s.admitLag = append(s.admitLag, milliseconds(begin.Sub(planned)))
	s.clientQueue = append(s.clientQueue, milliseconds(sent.Sub(begin)))
	s.mu.Unlock()
}
func (s *splitTimings) changed(sent time.Time) {
	d := milliseconds(time.Since(sent))
	s.mu.Lock()
	s.requestToChange = append(s.requestToChange, d)
	s.mu.Unlock()
}
func (s *splitTimings) report() (harness, server map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"admit_lag_ms": quantiles(s.admitLag), "client_queue_ms": quantiles(s.clientQueue)},
		map[string]any{"request_to_change_ms": quantiles(s.requestToChange)}
}

var loadHandler = nest.NewHandlerName("sync_aoi_state_change")

// newLoadNest 起压测的 Nest 引擎，返回引擎与实体查找（心跳驱动用同一份查找）。
func newLoadNest(states []*subject, c config, m *entitysync.Manager, in *policy.Interest, split *splitTimings) (*nest.NestMgr, loadGetter, error) {
	getter := make(loadGetter, len(states))
	for _, s := range states {
		getter[s.ID()] = s
	}
	engine := nest.NewEngine(nest.NestOptionWithGetter(getter), nest.NestOptionWithEntitySync(m), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 4096}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	// 引擎实例级注册（memory 级别、不回滚，与原包级 MustRegisterMemoryHandler 相同的 meta）：同一进程里起第二个引擎
	// （go test -count>1）不冲突。
	engine.MustRegisterHandlerWithMeta(loadHandler, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		change := params[0].(loadMutation)
		s := es[0].(*subject)
		at := position(s.data.ID, change.step, c)
		s.data.Step = int64(change.step)
		s.data.X, s.data.Y = at.X, at.Y
		s.data.HP = 100 - int64((change.step+10000)%97)
		s.data.Planned = change.planned.UnixNano()
		s.data.Changed = time.Now().UnixNano()
		if change.step > 0 {
			split.changed(change.sent)
		}
		s.tracker.MarkSync(1)
		return nil, in.QueueMove(s, at)
	}, nest.HandlerMeta{})
	if err := engine.Start(); err != nil {
		return nil, nil, err
	}
	return engine, getter, nil
}
