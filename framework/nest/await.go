package nest

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

var (
	ErrAwaitContext   = errors.New("nest: Await requires a memory business handler without a remote write batch")
	ErrAwaitSignature = errors.New("nest: invalid Await callback signature")
	ErrAwaitPending   = errors.New("nest: this execution segment already has an Await")
)

// AwaitOption 仅决定恢复段的目标，不读取 Entity，不执行 I/O。
type AwaitOption[T any] func(*awaitOptions[T])
type awaitOptions[T any] struct{ targets func(T) []int64 }

// WithResumeTargets 用查询结果选择新目标。返回顺序就是恢复回调的实体参数顺序。
// work 失败时无法解析新目标，直接结束请求；不把无效实体传入回调。
func WithResumeTargets[T any](fn func(T) []int64) AwaitOption[T] {
	return func(o *awaitOptions[T]) { o.targets = fn }
}

type awaitPlan struct{ start, cancel func() }

// Await 结束当前 memory handler 段，释放 Guard 和 tail 后在 I/O 池执行 work。
// resume 形如 func(*Player, *Troop, T, error) error；实体由框架重新取得和保护。
// 必须 return Await(...)，慢闭包不得访问捕获的实体。没有跨段事务、自动重试或持久恢复。
// 默认继承当前声明目标；多组目标按参数顺序展开。首版拒绝广播与有事务/Remote 写批次的段。
func Await[T any](work func(context.Context) (T, error), resume any, options ...AwaitOption[T]) error {
	msg := currentNestDispatchMsg()
	if msg == nil || !fctx.InBusinessWorker() || !fctx.InNestHandler() || CurrentRollbackTx() != nil || !msg.txNoRollback || msg.RemoteWriteBatch != nil || msg.Type == MsgTypeBroadcast || len(msg.handlerEntities) == 0 {
		return ErrAwaitContext
	}
	if msg.awaitPlan != nil {
		return ErrAwaitPending
	}
	var opts awaitOptions[T]
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	fn := reflect.ValueOf(resume)
	typ := reflect.TypeOf((*T)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	entityType := reflect.TypeOf((*entity.IThreadSafeEntity)(nil)).Elem()
	if work == nil || !fn.IsValid() || fn.Kind() != reflect.Func || fn.IsNil() {
		return ErrAwaitSignature
	}
	ft := fn.Type()
	count := ft.NumIn() - 2
	if ft.IsVariadic() || count < 1 || ft.NumOut() != 1 || ft.Out(0) != errType || ft.In(count) != typ || ft.In(count+1) != errType {
		return ErrAwaitSignature
	}
	for i := 0; i < count; i++ {
		if !ft.In(i).Implements(entityType) {
			return ErrAwaitSignature
		}
	}
	ids := orderedMessageIDs(msg)
	if opts.targets == nil {
		if count != len(ids) || count != len(msg.handlerEntities) {
			return ErrAwaitSignature
		}
		for i, e := range msg.handlerEntities {
			if e == nil || !reflect.TypeOf(e).AssignableTo(ft.In(i)) {
				return ErrAwaitSignature
			}
		}
	}
	mgr := msg.engine
	q := mgr.dispatcher.queue
	if err := q.reserveAwait(); err != nil {
		return normalizeAdmissionError(err)
	}
	reply := msg.RetChan
	name := msg.Name
	snapshot := msg.Context // 原请求快照，不捕获当前 Guard/事务/实体。
	ctx := nestBaseContext()
	finish := func(err error) {
		if reply != nil {
			reply <- err
		} else if err != nil {
			slog.Error("nest Await failed", "handler", name, "err", err)
		}
	}
	msg.awaitPlan = &awaitPlan{
		cancel: q.cancelAwait,
		start: func() {
			q.startAwait(func() {
				defer func() {
					if r := recover(); r != nil {
						finish(joinRecoveredError(nil, r))
					}
				}()
				if err := ctx.Err(); err != nil {
					finish(err)
					return
				}
				var result T
				var workErr error
				// work 的 panic 与普通错误一样传给默认目标回调。
				func() {
					defer func() {
						if r := recover(); r != nil {
							workErr = joinRecoveredError(nil, r)
						}
					}()
					result, workErr = work(ctx)
				}()
				if err := ctx.Err(); err != nil {
					finish(err)
					return
				}
				targets := ids
				if opts.targets != nil {
					if workErr != nil {
						finish(workErr)
						return
					}
					targets = append([]int64(nil), opts.targets(result)...)
				}
				if len(targets) != count {
					finish(ErrAwaitSignature)
					return
				}
				full, err := normalizeClientIDs(targets)
				if err != nil {
					finish(err)
					return
				}
				next := GenMsg(MsgTypeMulti)
				next.Name, next.Tids, next.RetChan, next.Context = name, full, reply, snapshot
				// 冷实体经 I/O 准备；新段重新登记全体目标，不继承旧 tail。
				next.Cost = true
				checkRemoteIds(next, full)
				if next.HasRemote {
					recycleMsg(next)
					finish(ErrAwaitContext)
					return
				}
				next.resumeEntry = &handlerEntry{meta: HandlerMeta{Durability: DurabilityMemory}, handler: func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
					args := make([]reflect.Value, 0, count+2)
					for i, e := range es {
						if e == nil {
							return nil, ErrEntityNotFound
						}
						v := reflect.ValueOf(e)
						if !v.Type().AssignableTo(ft.In(i)) {
							return nil, ErrAwaitSignature
						}
						args = append(args, v)
					}
					args = append(args, reflect.ValueOf(&result).Elem())
					ev := reflect.Zero(errType)
					if workErr != nil {
						ev = reflect.ValueOf(workErr)
					}
					args = append(args, ev)
					out := fn.Call(args)[0]
					if !out.IsNil() {
						return nil, out.Interface().(error)
					}
					return nil, nil
				}}
				// TrySendMsg 接管消息引用。失败时不会回复，由原请求的完成者统一报告。
				if err := mgr.dispatcher.TrySendMsg(next); err != nil {
					finish(normalizeAdmissionError(err))
				}
			})
		},
	}
	return nil
}

func orderedMessageIDs(msg *Msg) []int64 {
	if msg.Type == MsgTypeSingle {
		return []int64{msg.Tid}
	}
	ids := append([]int64(nil), msg.Tids...)
	for _, group := range msg.GroupTIds {
		ids = append(ids, group...)
	}
	return ids
}

// 预留先于业务段结束；pending 包含尚未启动的 I/O，停机不能提前完成。
func (q *dispatchQueue) reserveAwait() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopping || !q.started {
		return worker.ErrWorkerClosed
	}
	state := &q.lanes[dispatchSlowLane]
	if q.awaitReserved+state.queued+state.running >= state.config.Workers+state.config.QueueCap {
		return worker.ErrWorkerQueueFull
	}
	q.awaitReserved++
	q.pending++
	return nil
}
func (q *dispatchQueue) cancelAwait() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.awaitReserved--
	q.pending--
	if q.stopping && q.pending == 0 {
		for i := range q.lanes {
			q.lanes[i].wake.Broadcast()
		}
	}
}
func (q *dispatchQueue) startAwait(work func()) {
	msg := &Msg{RefCount: 1, ioWork: work}
	j := &dispatchJob{msg: msg, lane: dispatchSlowLane, slow: true}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.awaitReserved--
	state := &q.lanes[dispatchSlowLane]
	j.admittedAt = time.Since(q.clockOrigin)
	state.queued++
	state.waiting.push(j)
	q.enqueueReady(dispatchSlowLane, j)
}
