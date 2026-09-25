package main

import (
	"context"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync/policy"
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
	step     int
	planned  time.Time
	observer bool
}

var loadHandler = nest.NewHandlerName("sync_aoi_state_change")

func newLoadNest(states []*subject, c config, m *entitysync.Manager, in *policy.Interest) (*nest.NestMgr, error) {
	getter := make(loadGetter, len(states))
	for _, s := range states {
		getter[s.ID()] = s
	}
	nest.MustRegisterMemoryHandler(loadHandler, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		change := params[0].(loadMutation)
		s := es[0].(*subject)
		at := position(s.data.ID, change.step, c)
		s.data.Step = int64(change.step)
		s.data.X, s.data.Y = at.X, at.Y
		s.data.HP = 100 - int64((change.step+10000)%97)
		s.data.Planned = change.planned.UnixNano()
		s.data.Changed = time.Now().UnixNano()
		s.tracker.MarkSync(1)
		return nil, in.QueueMove(s, at, change.observer)
	})
	engine := nest.NewEngine(nest.NestOptionWithGetter(getter), nest.NestOptionWithEntitySync(m), nest.NestOptionWithWorkerNumAndMsgCap(4, 1, 4096))
	if err := engine.Start(); err != nil {
		return nil, err
	}
	return engine, nil
}
