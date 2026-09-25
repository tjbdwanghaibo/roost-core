package nest

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"time"
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
	_, release := fctx.NewContext(fctx.WithSnapshot(call.snapshot))
	defer release()
	observeNestStage(call.msg.Name, "logic_queue", call.queuedAt)
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				call.err = err
			} else {
				call.err = fmt.Errorf("nest: local continuation panic: %v", r)
			}
		}
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

func (mgr *NestMgr) dispatchRemoteLogic(msg *Msg) (any, error) {
	return mgr.dispatchFastContinuation(msg, nil)
}
func (mgr *NestMgr) dispatchFastContinuation(msg *Msg, fn func()) (any, error) {
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
