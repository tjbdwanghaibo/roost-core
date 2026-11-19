package physicsroom

import (
	"context"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

// RoomEntity 只保存已提交的瞬态事实：物理帧，以及（有流程的房间）关卡流程的状态。Ref 创建后不变，
// state 与 flow 仅在 Nest Guard 中读写，由同一个 handler 在同一笔 undo 事务里修改。
// flow 在 Entity 创建时用首帧带来的 Program 建好，之后不换；没有流程的房间为 nil。
type RoomEntity struct {
	*entity.EntityBase
	Ref   domain.Ref
	state gamephysics.StateDAO
	flow  *level.LevelComponent
}

func (r *RoomEntity) Base() *entity.EntityBase { return r.EntityBase }

// EntityHost 拥有房间 Entity 与专用 Nest 引擎；外部先排空 Service，再关闭 Host。
// kind/category 由部署方分配并注册，不在库里占用固定的业务 Kind。
type EntityHost struct {
	mu       sync.RWMutex
	engine   *nest.NestMgr
	kind     entity.EntityKind
	category entity.EntityCategory
	maxRooms int
	next     int64
	closing  bool
	rooms    map[domain.Ref]*RoomEntity
	ids      map[int64]*RoomEntity
}

var acceptFrame = nest.NewHandlerName("physicsroom.entity.accept")
var readFrame = nest.NewHandlerName("physicsroom.entity.snapshot")
var readLevel = nest.NewHandlerName("physicsroom.entity.level")

func NewEntityHost(kind entity.EntityKind, category entity.EntityCategory, maxRooms int, options ...nest.NestOption) (*EntityHost, error) {
	if kind == 0 || category == 0 || maxRooms < 1 || maxRooms > 1024 {
		return nil, domain.ErrRequestInvalid
	}
	registered, err := entity.ResolveEntityKindCategory(kind)
	if err != nil {
		return nil, err
	}
	if registered != category {
		return nil, domain.ErrRequestInvalid
	}
	h := &EntityHost{kind: kind, category: category, maxRooms: maxRooms, rooms: make(map[domain.Ref]*RoomEntity), ids: make(map[int64]*RoomEntity)}
	options = append(options, nest.NestOptionWithGetter(h))
	h.engine = nest.NewEngine(options...)
	err = h.engine.RegisterHandlerWithMeta(acceptFrame, func(es []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
		if len(es) != 1 || len(args) != 1 {
			return nil, domain.ErrRequestInvalid
		}
		r, ok := es[0].(*RoomEntity)
		if !ok {
			return nil, domain.ErrRequestInvalid
		}
		f, ok := args[0].(FrameCommit)
		if !ok || f.Room != r.Ref || f.Tick.Frame.Epoch != r.Ref.Epoch {
			return nil, domain.ErrMissing
		}
		return AcceptTick(&r.state, r.flow, r.Ref, f.Tick)
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	if err != nil {
		return nil, err
	}
	err = h.engine.RegisterHandlerWithMeta(readLevel, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		r := es[0].(*RoomEntity)
		if r.flow == nil {
			return nil, domain.ErrMissing
		}
		return r.flow.Snapshot(), nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	if err != nil {
		return nil, err
	}
	err = h.engine.RegisterHandlerWithMeta(readFrame, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return es[0].(*RoomEntity).state.Snapshot(), nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	if err != nil {
		return nil, err
	}
	if err = h.engine.Start(); err != nil {
		return nil, err
	}
	return h, nil
}

// Commit 在首帧前创建加载完成的 Entity（有流程的房间同时建好流程组件），再经 RequestSettled 写入 Guard。
// 返回的 Settled 来自已经提交成功的流程状态。
func (h *EntityHost) Commit(ctx context.Context, ref domain.Ref, tick domain.Tick) (domain.Settled, error) {
	if ref.ID == "" || ref.Epoch == 0 || tick.Frame.Epoch != ref.Epoch {
		return domain.Settled{}, domain.ErrRequestInvalid
	}
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return domain.Settled{}, domain.ErrStopping
	}
	r := h.rooms[ref]
	if r == nil {
		if len(h.rooms) >= h.maxRooms {
			h.mu.Unlock()
			return domain.Settled{}, domain.ErrCapacity
		}
		h.next++
		id, err := entity.BuildEntityID(h.next, h.kind)
		if err != nil {
			h.mu.Unlock()
			return domain.Settled{}, err
		}
		r = &RoomEntity{EntityBase: entity.NewEntityBase(id, h.category, true, h.kind), Ref: ref}
		if tick.Program != nil {
			// 流程是瞬态的：物理世界不可恢复，流程落库后也对不上世界。
			if r.flow, err = level.NewLevelComponent(level.NewTransientLevelDAO(id), tick.Program); err != nil {
				h.mu.Unlock()
				return domain.Settled{}, err
			}
			// 重生这类动作在房间的一生里会发生成千上万次：账本只留未完成的，否则它填满 Limits.Effects 之后房间就停了。
			// 房间对每个动作只给一次回执（service/physicsroom 的 flow.known），晚到的按 ErrStale 记成作废。
			r.flow.ForgetSettledActions()
		}
		h.rooms[ref] = r
		h.ids[id] = r
	}
	h.mu.Unlock()
	result, err := h.engine.RequestSettled(ctx, acceptFrame, r.ID(), nest.NewParams(FrameCommit{Room: ref, Tick: tick}))
	if err != nil {
		return domain.Settled{}, err
	}
	settled, _ := result.(domain.Settled)
	return settled, nil
}

// Forget 只由 Service 的 Closed 回调调用：此时 Run 及在途提交均已结束。
func (h *EntityHost) Forget(ref domain.Ref) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r := h.rooms[ref]; r != nil {
		delete(h.rooms, ref)
		delete(h.ids, r.ID())
	}
}
func (h *EntityHost) Snapshot(ctx context.Context, ref domain.Ref) (gamephysics.Frame, error) {
	h.mu.RLock()
	r := h.rooms[ref]
	h.mu.RUnlock()
	if r == nil {
		return gamephysics.Frame{}, domain.ErrMissing
	}
	result, err := h.engine.RequestSettled(ctx, readFrame, r.ID(), nil)
	if err != nil {
		return gamephysics.Frame{}, err
	}
	return result.(gamephysics.Frame), nil
}

// Level 返回房间流程已提交状态的副本；房间不存在或没有流程时返回 ErrMissing。
func (h *EntityHost) Level(ctx context.Context, ref domain.Ref) (level.State, error) {
	h.mu.RLock()
	r := h.rooms[ref]
	h.mu.RUnlock()
	if r == nil {
		return level.State{}, domain.ErrMissing
	}
	result, err := h.engine.RequestSettled(ctx, readLevel, r.ID(), nil)
	if err != nil {
		return level.State{}, err
	}
	return result.(level.State), nil
}
func (h *EntityHost) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r := h.ids[id]
	if r == nil {
		return nil, domain.ErrMissing
	}
	return r, nil
}
func (h *EntityHost) GetMany(ctx context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		r, err := h.Get(ctx, id, 0)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}
func (h *EntityHost) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()
	return h.engine.Shutdown(ctx)
}
