package physicsroom

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

// FrameCommit 是 Nest handler 的唯一入参；handler 核对 Ref，并向所属 Entity 的 StateDAO.Accept 提交。
type FrameCommit struct {
	Room  domain.Ref
	Frame gamephysics.Frame
}
type Requester interface {
	RequestSettled(context.Context, nest.HandlerName, int64, nest.Params, ...nest.SendOpt) (any, error)
}

// NestCommit 将等待留在房间执行器上。target 必须返回已加载的房间 Entity，
// handler 应使用 rollback=undo、durability=memory，禁止先返回成功再异步提交。
func NestCommit(engine Requester, handler nest.HandlerName, target func(domain.Ref) (int64, error)) domain.Commit {
	return func(ctx context.Context, ref domain.Ref, frame gamephysics.Frame) error {
		if engine == nil || target == nil {
			return domain.ErrRequestInvalid
		}
		id, err := target(ref)
		if err != nil {
			return err
		}
		_, err = engine.RequestSettled(ctx, handler, id, nest.NewParams(FrameCommit{Room: ref, Frame: frame}))
		return err
	}
}
