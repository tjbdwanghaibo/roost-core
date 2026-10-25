package physicsroom

import (
	"context"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

// RoomEntity 只保存已提交的瞬态物理事实。Ref 创建后不变，state 仅在 Nest Guard 中读写。
type RoomEntity struct {
	*entity.EntityBase
	Ref   domain.Ref
	state gamephysics.StateDAO
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
		if !ok || f.Room != r.Ref || f.Frame.Epoch != r.Ref.Epoch {
			return nil, domain.ErrMissing
		}
		return nil, r.state.Accept(f.Frame)
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

// Commit 在首帧前创建加载完成的 Entity，再经 RequestSettled 写入 Guard。
func (h *EntityHost) Commit(ctx context.Context, ref domain.Ref, f gamephysics.Frame) error {
	if ref.ID == "" || ref.Epoch == 0 || f.Epoch != ref.Epoch {
		return domain.ErrRequestInvalid
	}
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return domain.ErrStopping
	}
	r := h.rooms[ref]
	if r == nil {
		if len(h.rooms) >= h.maxRooms {
			h.mu.Unlock()
			return domain.ErrCapacity
		}
		h.next++
		id, err := entity.BuildEntityID(h.next, h.kind)
		if err != nil {
			h.mu.Unlock()
			return err
		}
		r = &RoomEntity{EntityBase: entity.NewEntityBase(id, h.category, true, h.kind), Ref: ref}
		h.rooms[ref] = r
		h.ids[id] = r
	}
	h.mu.Unlock()
	_, err := h.engine.RequestSettled(ctx, acceptFrame, r.ID(), nest.NewParams(FrameCommit{Room: ref, Frame: f}))
	return err
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
