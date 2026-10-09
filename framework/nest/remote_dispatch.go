package nest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// remoteLogicCall 是慢 worker 到逻辑 worker 的一次借用。原 Msg 由慢 worker
// 独占其引用；借用期间它只等待 done，不读写 Msg。独立信封被逻辑池释放后
// 才关闭 done，因此不会与 Msg 池回收、Guard 清理或 RefCount 更新竞争。
type remoteLogicCall struct {
	fn       func()
	msg      *Msg
	snapshot fctx.ContextSnapshot
	done     chan struct{}
	queuedAt time.Time
	ret      any
	err      error
}

func needsRemoteStage(msg *Msg) bool {
	if msg.HasRemote {
		return true
	}
	for _, param := range msg.Params {
		if _, ok := param.(RemoteAccessProvider); ok {
			return true
		}
	}
	return false
}

func (call *remoteLogicCall) run(mgr *NestMgr) {
	current, release := fctx.NewContext(fctx.WithSnapshot(call.snapshot), fctx.WithFastWorker())
	defer release()
	base := current.Base
	current.Base = entity.WithLocalExecutor(base, nil)
	observeNestStage(call.msg.Name, "logic_queue", call.queuedAt)
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				call.err = err
			} else {
				call.err = fmt.Errorf("nest: local continuation panic: %v", r)
			}
		}
		// 只返回请求数据；本地执行器仍属于等待本次续行的慢阶段。
		current.Base = base
		call.snapshot = fctx.CaptureSnapshot()
	}()
	if call.fn != nil {
		call.fn()
	} else {
		if call.msg.prepared != nil {
			defer func() { prepared := call.msg.prepared; call.msg.prepared = nil; prepared.release() }()
		}
		call.ret, call.err = runNestLogic(mgr, call.msg)
	}

}

const localTaskName = "__nest_run_local"

// RunLocal 把需要 Entity 本地锁的框架步骤交给快池执行，并同步等待它结束；fn 在快 worker 上运行，
// 自己取所需的 Entity 锁（锁等待属于 Guard/本地锁豁免）。它走续行通道，不排在任何实体的同 ID 链上，
// 所以等待它的调用方与同 ID 的慢准备不会互相等待。
//
// 只能在快池之外调用：慢 worker 或框架后台 goroutine（例如 DataEngine 驱逐被跳过的原生步骤留下的
// 实体，RR-20260926-30）。在快 worker 上调用返回 fctx.ErrBlockingInFastWorker，不会自等
// （RR-20260926-06）。Nest 未启动、已开始停机或已 fence 时返回错误，fn 不会执行；一旦投递成功就
// 等到 fn 结束，不因 ctx 提前返回——fn 仍可能正持有 Entity 锁。fn 的 panic 作为错误返回。
func (mgr *NestMgr) RunLocal(ctx context.Context, fn func()) error {
	if mgr == nil {
		return ErrNestStopped
	}
	if fn == nil {
		return nil
	}
	if err := fctx.BlockingError("nest.RunLocal"); err != nil {
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrNestCanceled, err)
		}
	}
	if err := mgr.FenceError(); err != nil {
		return err
	}
	mgr.lifecycleMu.Lock()
	var queue *dispatchQueue
	if mgr.started && !mgr.stopped {
		queue = mgr.dispatcher.queue
	}
	mgr.lifecycleMu.Unlock()
	if queue == nil {
		return ErrNestStopped
	}
	call := &remoteLogicCall{
		msg: &Msg{Name: localTaskName}, fn: fn, done: make(chan struct{}),
		queuedAt: startNestStage(mgr.stageMetrics),
	}
	envelope := msgPool.Get().(*Msg)
	envelope.remoteLogic = call
	envelope.OnSend()
	if !queue.tryContinueFast(envelope) {
		envelope.OnRelease()
		return ErrNestStopped
	}
	<-call.done
	return call.err
}

func (mgr *NestMgr) dispatchRemoteLogic(msg *Msg) (any, error) {
	return mgr.dispatchFastContinuation(msg, nil)
}
func (mgr *NestMgr) dispatchFastContinuation(msg *Msg, fn func()) (any, error) {
	fctx.AssertBlockingAllowed("nest.dispatchFastContinuation")
	call := &remoteLogicCall{
		msg: msg, fn: fn, snapshot: fctx.CaptureSnapshot(), done: make(chan struct{}),
		queuedAt: startNestStage(mgr.stageMetrics),
	}
	envelope := msgPool.Get().(*Msg)
	envelope.Tid = msg.Key()
	envelope.remoteLogic = call
	envelope.OnSend()
	mgr.dispatcher.queue.continueFast(envelope)
	// 不能在 ctx 取消时提前返回：逻辑阶段仍可能持锁并使用原 Msg。
	<-call.done
	if current := fctx.CurrentContext(); current != nil {
		current.ApplySnapshot(call.snapshot)
	}
	return call.ret, call.err
}
