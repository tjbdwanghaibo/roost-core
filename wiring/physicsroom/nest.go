package physicsroom

import (
	"context"
	"errors"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

// FrameCommit 是 Nest handler 的唯一入参；handler 核对 Ref，再用 AcceptTick 把这一帧（与流程的事实、回执）
// 提交给所属 Entity。
type FrameCommit struct {
	Room domain.Ref
	Tick domain.Tick
}

// AcceptTick 是房间 Entity 的提交 handler 的全部工作，在 Entity 的 Nest Guard 里调用；handler 必须登记为
// rollback=undo、durability=memory：物理帧与流程状态要么一起生效，要么一起撤回。flow 是这个房间的流程组件
// （用 Tick.Program 与 level.NewTransientLevelDAO 在首帧前建好），没有流程的房间传 nil。
//
// 固定次序：接受物理帧 → 首帧初始化并启动流程（实例名 domain.LevelInstance(ref)、Epoch 即 ref.Epoch、
// 时间用 Tick 序号）→ 逐条完成回执 → 逐条处理事实 → 按本帧的 Tick 推进定时器。流程状态变了才取待执行动作与
// 公开视图；全部是内存里的纯计算，不阻塞。流程出错时返回的错误带 domain.ErrLevel，房间停止。
func AcceptTick(state *gamephysics.StateDAO, flow *level.LevelComponent, ref domain.Ref, tick domain.Tick) (domain.Settled, error) {
	if err := state.Accept(tick.Frame); err != nil {
		return domain.Settled{}, err
	}
	if flow == nil {
		if len(tick.Facts) != 0 || len(tick.Receipts) != 0 {
			return domain.Settled{}, domain.ErrRequestInvalid
		}
		return domain.Settled{}, nil
	}
	settled, err := acceptLevel(flow, ref, tick)
	if err != nil {
		return domain.Settled{}, errcode.Wrap(domain.ErrLevel, err)
	}
	return settled, nil
}

func acceptLevel(flow *level.LevelComponent, ref domain.Ref, tick domain.Tick) (domain.Settled, error) {
	before := flow.Revision()
	now := int64(tick.Frame.Tick)
	if before == 0 {
		if err := flow.Initialize(domain.LevelInstance(ref), ref.Epoch, 0); err != nil {
			return domain.Settled{}, err
		}
		if err := flow.Start(0); err != nil {
			return domain.Settled{}, err
		}
	}
	var settled domain.Settled
	for _, receipt := range tick.Receipts {
		// 动作所在的阶段已经结束、动作被取消：回执作废，不是错误（业务动作晚到时就是这样）。
		if err := flow.Complete(receipt, now); errors.Is(err, level.ErrStale) {
			settled.StaleReceipts++
		} else if err != nil {
			return domain.Settled{}, err
		}
	}
	for _, fact := range tick.Facts {
		// 同一笔提交里前面的事实已经让流程结束：后面的不再投递。
		if phase := flow.Phase(); phase == level.Finishing || phase == level.Closing || phase == level.Closed {
			break
		}
		if err := flow.Handle(fact, now); err != nil {
			return domain.Settled{}, err
		}
	}
	if err := flow.AdvanceTick(tick.Frame.Tick); err != nil {
		return domain.Settled{}, err
	}
	if flow.Revision() == before {
		return settled, nil
	}
	view := flow.Project()
	settled.Actions, settled.View = flow.PendingActions(), &view
	return settled, nil
}

type Requester interface {
	RequestSettled(context.Context, nest.HandlerName, int64, nest.Params, ...nest.SendOpt) (any, error)
}

// NestCommit 将等待留在房间执行器上。target 必须返回已加载的房间 Entity，
// handler 应使用 rollback=undo、durability=memory，禁止先返回成功再异步提交。
// handler 用 AcceptTick 处理 FrameCommit，并把它返回的 domain.Settled 原样作为结果返回；
// 有流程的房间首帧（Tick.Program 非空）时，target 要先给这个 Entity 建好流程组件。
func NestCommit(engine Requester, handler nest.HandlerName, target func(domain.Ref) (int64, error)) domain.Commit {
	return func(ctx context.Context, ref domain.Ref, tick domain.Tick) (domain.Settled, error) {
		if engine == nil || target == nil {
			return domain.Settled{}, domain.ErrRequestInvalid
		}
		id, err := target(ref)
		if err != nil {
			return domain.Settled{}, err
		}
		result, err := engine.RequestSettled(ctx, handler, id, nest.NewParams(FrameCommit{Room: ref, Tick: tick}))
		if err != nil {
			return domain.Settled{}, err
		}
		// handler 没有返回 Settled（只接受了帧）时等同于没有流程。
		settled, _ := result.(domain.Settled)
		return settled, nil
	}
}
