package nest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

// Client 是生成的 sender 使用的实例级命令边界。实现只在派发期间持有 Params 的所有权；异步准入成功后，调用方
// 必须把可变参数值视为不可再改。Dispatch* 为异步投递，Request* 同步等待回复；在 Nest handler 或业务 worker
// 内调用分别返回 ErrAsyncInHandler / ErrSyncInHandler。
type Client interface {
	Dispatch(context.Context, HandlerName, int64, Params, ...SendOpt) error
	Request(context.Context, HandlerName, int64, Params, ...SendOpt) (any, error)
	DispatchBroadcast(context.Context, HandlerName, []int64, Params, ...SendOpt) error
	DispatchMulti(context.Context, HandlerName, []int64, Params, ...SendOpt) error
	RequestMulti(context.Context, HandlerName, []int64, Params, ...SendOpt) (any, error)
	DispatchMultiGroup(context.Context, HandlerName, [][]int64, Params, ...SendOpt) error
	RequestMultiGroup(context.Context, HandlerName, [][]int64, Params, ...SendOpt) (any, error)
}

// DispatchBroadcast 按锁组与 worker 拆批，准入一组相互独立的单实体投递。准入中途失败时，失败之前已准入的投递
// 仍可能执行；需要全有或全无语义的调用方应使用 DispatchMulti。
func (mgr *NestMgr) DispatchBroadcast(ctx context.Context, name HandlerName, ids []int64, params Params, opts ...SendOpt) error {
	if len(ids) == 0 {
		return fmt.Errorf("%w: empty entity ids", ErrInvalidMessage)
	}
	if err := validateClientDispatch(mgr, ctx, "DispatchBroadcast", name, ErrAsyncInHandler); err != nil {
		return err
	}
	fullIDs, err := normalizeClientIDs(ids)
	if err != nil {
		return err
	}
	var admissionErr error
	mgr.dispatcher.ForEachSpliceBatch(fullIDs, func(_ int, batch []int64, _ []int) {
		if admissionErr != nil {
			return
		}
		msg := GenMsg(MsgTypeBroadcast)
		msg.Tids = append(msg.Tids[:0], batch...)
		prepareClientMessage(ctx, msg, name, params, false, opts...)
		checkRemoteIds(msg, msg.Tids)
		admissionErr = mgr.admit(msg, opts...)
	})
	return admissionErr
}

var _ Client = (*NestMgr)(nil)

// Dispatch 异步投递单目标消息；返回 nil 只表示已准入。
func (mgr *NestMgr) Dispatch(ctx context.Context, name HandlerName, id int64, params Params, opts ...SendOpt) error {
	if err := validateClientDispatch(mgr, ctx, "Dispatch", name, ErrAsyncInHandler); err != nil {
		return err
	}
	fullID, err := normalizeClientID(id)
	if err != nil {
		return err
	}
	msg := GenMsg(MsgTypeSingle)
	msg.Tid = fullID
	prepareClientMessage(ctx, msg, name, params, false, opts...)
	checkRemoteId(msg, fullID)
	return mgr.admit(msg, opts...)
}

// Request 投递单目标消息并等待回复，超时取请求上下文的 SyncWait，其次引擎的同步超时。
func (mgr *NestMgr) Request(ctx context.Context, name HandlerName, id int64, params Params, opts ...SendOpt) (any, error) {
	if err := validateClientDispatch(mgr, ctx, "Request", name, ErrSyncInHandler); err != nil {
		return nil, err
	}
	fullID, err := normalizeClientID(id)
	if err != nil {
		return nil, err
	}
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Tid = fullID
	prepareClientMessage(ctx, msg, name, params, true, opts...)
	checkRemoteId(msg, fullID)
	requestTimeout := msg.Context.SyncWait
	if err := mgr.admit(msg, opts...); err != nil {
		return nil, err
	}
	return mgr.waitResult(ctx, ch, requestTimeout)
}

// DispatchMulti 异步投递多目标消息：全部目标在同一事务中按锁序加锁执行。
func (mgr *NestMgr) DispatchMulti(ctx context.Context, name HandlerName, ids []int64, params Params, opts ...SendOpt) error {
	if len(ids) == 0 {
		return fmt.Errorf("%w: empty entity ids", ErrInvalidMessage)
	}
	if err := validateClientDispatch(mgr, ctx, "DispatchMulti", name, ErrAsyncInHandler); err != nil {
		return err
	}
	fullIDs, err := normalizeClientIDs(ids)
	if err != nil {
		return err
	}
	msg := GenMsg(MsgTypeMulti)
	msg.Tids = fullIDs // 标准化已建立独立存储，直接把所有权交给消息。
	prepareClientMessage(ctx, msg, name, params, false, opts...)
	checkRemoteIds(msg, msg.Tids)
	return mgr.admit(msg, opts...)
}

// RequestMulti 是 DispatchMulti 的同步版本。
func (mgr *NestMgr) RequestMulti(ctx context.Context, name HandlerName, ids []int64, params Params, opts ...SendOpt) (any, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: empty entity ids", ErrInvalidMessage)
	}
	if err := validateClientDispatch(mgr, ctx, "RequestMulti", name, ErrSyncInHandler); err != nil {
		return nil, err
	}
	fullIDs, err := normalizeClientIDs(ids)
	if err != nil {
		return nil, err
	}
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Tids = fullIDs // 不再重复复制；调用方原切片仍与消息隔离。
	prepareClientMessage(ctx, msg, name, params, true, opts...)
	checkRemoteIds(msg, msg.Tids)
	requestTimeout := msg.Context.SyncWait
	if err := mgr.admit(msg, opts...); err != nil {
		return nil, err
	}
	return mgr.waitResult(ctx, ch, requestTimeout)
}

// DispatchMultiGroup 异步投递分组多目标消息；组边界经 HandlerOptionWithGroup 交给 handler。
func (mgr *NestMgr) DispatchMultiGroup(ctx context.Context, name HandlerName, groups [][]int64, params Params, opts ...SendOpt) error {
	if !validGroups(groups) {
		return fmt.Errorf("%w: empty entity groups", ErrInvalidMessage)
	}
	if err := validateClientDispatch(mgr, ctx, "DispatchMultiGroup", name, ErrAsyncInHandler); err != nil {
		return err
	}
	fullGroups, err := normalizeClientGroups(groups)
	if err != nil {
		return err
	}
	msg := GenMsg(MsgTypeMultiGroup)
	msg.GroupTIds = fullGroups
	prepareClientMessage(ctx, msg, name, params, false, opts...)
	checkRemoteGroups(msg, msg.GroupTIds)
	return mgr.admit(msg, opts...)
}

// RequestMultiGroup 是 DispatchMultiGroup 的同步版本。
func (mgr *NestMgr) RequestMultiGroup(ctx context.Context, name HandlerName, groups [][]int64, params Params, opts ...SendOpt) (any, error) {
	if !validGroups(groups) {
		return nil, fmt.Errorf("%w: empty entity groups", ErrInvalidMessage)
	}
	if err := validateClientDispatch(mgr, ctx, "RequestMultiGroup", name, ErrSyncInHandler); err != nil {
		return nil, err
	}
	fullGroups, err := normalizeClientGroups(groups)
	if err != nil {
		return nil, err
	}
	msg, ch := GenSyncMsg(MsgTypeMultiGroup)
	msg.GroupTIds = fullGroups
	prepareClientMessage(ctx, msg, name, params, true, opts...)
	checkRemoteGroups(msg, msg.GroupTIds)
	requestTimeout := msg.Context.SyncWait
	if err := mgr.admit(msg, opts...); err != nil {
		return nil, err
	}
	return mgr.waitResult(ctx, ch, requestTimeout)
}

// validateClientDispatch 检查投递前置条件：引擎运行且未 fence、handler 名与 ctx 合法且未取消、不在 Nest handler
// 或业务 worker 内（在其中调用返回 nestedError）。
func validateClientDispatch(mgr *NestMgr, ctx context.Context, api string, name HandlerName, nestedError error) error {
	if mgr == nil {
		return ErrNestStopped
	}
	if err := mgr.FenceError(); err != nil {
		return err
	}
	if !mgr.Running() {
		return ErrNestStopped
	}
	if name.String() == "" {
		return fmt.Errorf("%w: empty handler name", ErrInvalidMessage)
	}
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidMessage)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrNestCanceled, err)
	}
	if fctx.InNestHandler() || fctx.InBusinessWorker() {
		current := fctx.CurrentContext()
		return fmt.Errorf("%w: api=%s caller=%s target=%s", nestedError, api, current.Meta.Handler, name.String())
	}
	return nil
}

// prepareClientMessage 填写消息名、参数副本、发送选项与上下文快照。同步请求携带调用方 ctx 作为 Base；异步消息只带
// 可跨异步边界的部分。
func prepareClientMessage(ctx context.Context, msg *Msg, name HandlerName, params Params, wait bool, opts ...SendOpt) {
	msg.Name = name.String()
	msg.Params = append(msg.Params[:0], params...)
	opt := resolveSendOptions(opts)
	msg.Cost = opt.Cost
	var snapshot fctx.ContextSnapshot
	if wait {
		snapshot = fctx.CaptureSnapshot()
		if !snapshot.Valid {
			snapshot.Valid = true
			snapshot.Config = fctx.RuntimeConfig()
		}
		snapshot.Base = ctx
	} else {
		// 异步准入只用 ctx 检查取消 / 截止。固定当前运行期配置代际，保留请求身份与 trace，但从不携带 Base 上的值、
		// 任意 KV 或事务状态；业务数据必须显式放在 Params 中。
		snapshot = asyncMessageContextSnapshot(fctx.CaptureSnapshot())
	}
	msg.Context = snapshot
}

// resolveSendOptions 合并发送选项。
func resolveSendOptions(opts []SendOpt) sendOptParam {
	var ret sendOptParam
	for _, opt := range opts {
		if opt != nil {
			opt(&ret)
		}
	}
	return ret
}

// admit 把消息交给派发器：带延时选项时进入延时队列，否则立即准入。返回的错误已经归一化（ErrQueueFull /
// ErrNestStopped），调用方直接返回，不要再归一化一次。
func (mgr *NestMgr) admit(msg *Msg, opts ...SendOpt) error {
	opt := resolveSendOptions(opts)
	var err error
	if opt.Delay > 0 {
		err = mgr.dispatcher.TryDelaySendMsg(opt.Delay, msg)
	} else {
		err = mgr.dispatcher.TrySendMsg(msg)
	}
	return normalizeAdmissionError(err)
}

// normalizeAdmissionError 把 worker 层的准入错误换成 Nest 的哨兵（ErrQueueFull / ErrNestStopped），保留原因链。
func normalizeAdmissionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, worker.ErrWorkerQueueFull) {
		return errors.Join(ErrQueueFull, err)
	}
	if errors.Is(err, worker.ErrWorkerClosed) {
		return errors.Join(ErrNestStopped, err)
	}
	return err
}

// waitResult 等待同步请求的回复：回复是 error 时作为错误返回；ctx 取消返回 ErrNestCanceled，超时返回 ErrNestTimeout。
// 取消或超时只结束等待，不撤销已准入的业务。
func (mgr *NestMgr) waitResult(ctx context.Context, ch <-chan any, requestTimeout time.Duration) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := mgr.syncTimeout
	if requestTimeout > 0 {
		timeout = requestTimeout
	}
	if timeout <= 0 {
		timeout = NestSyncTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ret := <-ch:
		if err, ok := ret.(error); ok {
			return nil, err
		}
		return ret, nil
	case <-ctx.Done():
		return nil, errors.Join(ErrNestCanceled, ctx.Err())
	case <-timer.C:
		return nil, ErrNestTimeout
	}
}

// validGroups 报告分组非空且每组都非空。
func validGroups(groups [][]int64) bool {
	if len(groups) == 0 {
		return false
	}
	for _, ids := range groups {
		if len(ids) == 0 {
			return false
		}
	}
	return true
}

// normalizeClientID 把调用方传入的 ID 规范为完整 ID，非法时返回 ErrInvalidMessage。
func normalizeClientID(id int64) (int64, error) {
	fullID, err := entity.NormalizeFullID(id, entity.EntityKindNone)
	if err != nil {
		return 0, fmt.Errorf("%w: entity id %d: %v", ErrInvalidMessage, id, err)
	}
	return fullID, nil
}

// normalizeClientIDs 规范一组 ID，结果使用独立存储，与调用方切片隔离。
func normalizeClientIDs(ids []int64) ([]int64, error) {
	ret := make([]int64, len(ids))
	for i, id := range ids {
		fullID, err := normalizeClientID(id)
		if err != nil {
			return nil, fmt.Errorf("entity index %d: %w", i, err)
		}
		ret[i] = fullID
	}
	return ret, nil
}

// normalizeClientGroups 规范分组 ID，结果使用独立的连续存储。
func normalizeClientGroups(groups [][]int64) ([][]int64, error) {
	count := 0
	for _, ids := range groups {
		count += len(ids)
	}
	ret := make([][]int64, len(groups))
	// 一份消息独占连续存储；各组限制 cap，append 不会覆盖相邻组。
	storage := make([]int64, count)
	offset := 0
	for i, ids := range groups {
		end := offset + len(ids)
		ret[i] = storage[offset:end:end]
		for j, id := range ids {
			fullID, err := normalizeClientID(id)
			if err != nil {
				return nil, fmt.Errorf("entity group %d: entity index %d: %w", i, j, err)
			}
			ret[i][j] = fullID
		}
		offset = end
	}
	return ret, nil
}
