package nest

import (
	"context"
	"errors"
	"fmt"
	fctx "github.com/tjbdwanghaibo/cube-core/ctx"
	"github.com/tjbdwanghaibo/cube-core/entity"
	"log/slog"
	"sync"
	"time"
)

const NestSyncTimeout = 5 * time.Second

var (
	ErrHandlerNotFound                = errors.New("nest: handler not found")
	ErrInvalidMessage                 = errors.New("nest: invalid message")
	ErrGetterNotSet                   = errors.New("nest: entity getter not set")
	ErrQueueFull                      = errors.New("nest: dispatch queue full")
	ErrEntityNotFound                 = errors.New("nest: entity not found")
	ErrEntityTypeMismatch             = errors.New("nest: entity type mismatch")
	ErrLockTimeout                    = errors.New("nest: lock timeout")
	ErrNestTimeout                    = errors.New("nest: sync timeout")
	ErrNestCanceled                   = errors.New("nest: sync canceled")
	ErrNestStopped                    = errors.New("nest: stopped")
	ErrNestFenced                     = errors.New("nest: admission fenced")
	ErrDelayTooLong                   = errors.New("nest: delayed dispatch exceeds maximum delay")
	ErrParamMismatch                  = errors.New("nest: param mismatch")
	ErrAsyncInHandler                 = errors.New("nest: async dispatch from nest handler")
	ErrSyncInHandler                  = errors.New("nest: sync dispatch from nest handler")
	ErrEntityLockGroupMix             = errors.New("nest: mixed entity lock groups")
	ErrEntityLockGroupChanged         = errors.New("nest: entity lock group changed")
	ErrEntityGroupTransitionPending   = errors.New("nest: entity group transition pending")
	ErrEntityGroupTransitionScheduled = errors.New("nest: entity group transition scheduled")
	ErrInvalidEntityLockGroup         = errors.New("nest: invalid entity lock group")
	ErrRollbackUnsupported            = errors.New("nest: rollback is not supported by all participants")
	ErrRollbackFailed                 = errors.New("nest: rollback failed")
	ErrTransactionClosed              = errors.New("nest: transaction is already closed")
	ErrCommitterRequired              = errors.New("nest: durable transaction committer is required")
	ErrDurableRemoteWriteUnsupported  = errors.New("nest: durable remote write requires a lease-aware WAL committer")
	ErrRemoteBroadcastUnsupported     = errors.New("nest: remote-managed entities cannot use broadcast dispatch")
	ErrCommitRejected                 = errors.New("nest: transaction commit rejected")
	// ErrPipelinedCommitterRequired means a handler declared
	// DurabilityPipelined but the configured committer does not implement
	// PipelinedTransactionCommitter. This is a deployment configuration error
	// and is reported instead of silently degrading to strict commits.
	ErrPipelinedCommitterRequired = errors.New("nest: pipelined durability requires a PipelinedTransactionCommitter")
	// ErrPipelinedNotAllowed means a handler declared DurabilityPipelined but
	// the engine's pipelined allowlist does not include it. Early lock
	// release changes commit semantics, so production rollout is gated per
	// handler; see NEST_PIPELINED_COMMIT.md.
	ErrPipelinedNotAllowed = errors.New("nest: handler is not on the pipelined durability allowlist")
	// ErrCommitIndeterminate means the storage device returned an error after
	// commit bytes may have reached durable media. The process must be fenced
	// and recovered from WAL; rolling the in-memory state back could create a
	// second, conflicting history.
	ErrCommitIndeterminate = errors.New("nest: transaction commit outcome is indeterminate")
)

func NewParamCountMismatchError(handler string, got int, want int) error {
	return fmt.Errorf("%w: handler=%s got=%d want=%d", ErrParamMismatch, handler, got, want)
}

func NewEntityCountMismatchError(handler string, got int, want int) error {
	return fmt.Errorf("%w: handler=%s entities=%d want=%d", ErrEntityTypeMismatch, handler, got, want)
}

func NewResultTypeMismatchError(handler string, want string, got any) error {
	return fmt.Errorf("%w: handler=%s result want=%s got=%T", ErrParamMismatch, handler, want, got)
}

func NewParamTypeMismatchError(handler string, idx int, want string, got any) error {
	return fmt.Errorf("%w: handler=%s param=%d want=%s got=%T", ErrParamMismatch, handler, idx, want, got)
}

type NestMgr struct {
	dispatcher             *Dispatcher
	ticker                 *Ticker
	getter                 entity.Getter
	remoteSnapshotResolver RemoteSnapshotResolver
	remoteManager          entity.IRemoteEntityManager
	committer              TransactionCommitter
	syncTimeout            time.Duration
	// slowLockThreshold is the entity-lock hold time at which a dispatch is
	// counted and logged as slow. Zero disables the warning; the metric is
	// recorded regardless.
	slowLockThreshold time.Duration
	lifecycleMu       sync.Mutex
	started           bool
	stopped           bool
	stopDone          chan struct{}
	stopErr           error
	fenceErr          error
	groupLocks        *entityLockGroupLockManager
	handlers          map[HandlerName]handlerEntry
	// pipelinedAllow, when non-nil, is the set of handler names permitted to
	// run with DurabilityPipelined. Nil permits all handlers (development
	// default); production deployments should pin an explicit allowlist so a
	// handler cannot adopt early lock release without an operations review.
	pipelinedAllow map[string]struct{}
	// completions, when non-nil, enables Phase 2 async completion: dispatch
	// workers hand the post-durability work (AfterCommit, reply) to the pump
	// instead of parking on the commit ticket. Nil keeps the Phase 1
	// in-worker wait.
	completions *completionPump
}

func (mgr *NestMgr) groupLockManager() *entityLockGroupLockManager {
	if mgr == nil {
		return nil
	}
	mgr.lifecycleMu.Lock()
	if mgr.groupLocks == nil {
		mgr.groupLocks = newEntityLockGroupLockManager()
	}
	locks := mgr.groupLocks
	mgr.lifecycleMu.Unlock()
	return locks
}

// Fence immediately rejects new and queued dispatches without attempting to
// roll back any transaction whose durable outcome is indeterminate. It is
// idempotent and is intentionally separate from Shutdown so in-flight handlers
// can finish their fail-stop paths before the application drains workers.
func (mgr *NestMgr) Fence(cause error) {
	if mgr == nil || cause == nil {
		return
	}
	mgr.lifecycleMu.Lock()
	if mgr.fenceErr == nil {
		mgr.fenceErr = errors.Join(ErrNestFenced, cause)
	}
	fenced := mgr.fenceErr
	mgr.lifecycleMu.Unlock()
	if mgr.dispatcher != nil {
		mgr.dispatcher.Fence(fenced)
	}
}

func (mgr *NestMgr) FenceError() error {
	if mgr == nil {
		return ErrNestStopped
	}
	mgr.lifecycleMu.Lock()
	defer mgr.lifecycleMu.Unlock()
	return mgr.fenceErr
}

type HandlerName struct {
	value string
}

type Params []any

func NewHandlerName(value string) HandlerName {
	return HandlerName{value: value}
}

func NewParams(values ...any) Params {
	return Params(values)
}

func (n HandlerName) String() string {
	return n.value
}

func (mgr *NestMgr) TickDuration() time.Duration {
	if mgr == nil || mgr.ticker == nil {
		return 100 * time.Millisecond
	}
	return mgr.ticker.Duration()
}

type NestOpts struct {
	Getter                 entity.Getter
	RemoteSnapshotResolver RemoteSnapshotResolver
	RemoteManager          entity.IRemoteEntityManager
	WorkerNum              int
	HbWorkerNum            int
	MsgCap                 int
	DelayedMsgCap          int
	MaxDelay               time.Duration
	TickDuration           time.Duration
	SyncTimeout            time.Duration
	Committer              TransactionCommitter
	PipelinedAllowlist     []string
	PipelinedAsync         bool
	PipelinedAsyncWorkers  int
	PipelinedAsyncQueueCap int
	SlowLockThreshold      time.Duration
	SlowLockThresholdSet   bool
}

type NestOption func(*NestOpts)

var (
	NestOptionWithGetter = func(getter entity.Getter) NestOption {
		return func(opts *NestOpts) {
			opts.Getter = getter
		}
	}
	NestOptionWithRemoteSnapshotResolver = func(resolver RemoteSnapshotResolver) NestOption {
		return func(opts *NestOpts) {
			opts.RemoteSnapshotResolver = resolver
		}
	}
	NestOptionWithRemoteEntityManager = func(manager entity.IRemoteEntityManager) NestOption {
		return func(opts *NestOpts) {
			opts.RemoteManager = manager
		}
	}
	NestOptionWithWorkerNumAndMsgCap = func(workerNum, hbWorkerNum, msgCap int) NestOption {
		return func(opts *NestOpts) {
			opts.WorkerNum = workerNum
			opts.HbWorkerNum = hbWorkerNum
			opts.MsgCap = msgCap
		}
	}
	NestOptionWithDelayedAdmission = func(capacity int, maxDelay time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.DelayedMsgCap = capacity
			opts.MaxDelay = maxDelay
		}
	}
	NestOptionWithTickDuration = func(tickDuration time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.TickDuration = tickDuration
		}
	}
	NestOptionWithSyncTimeout = func(timeout time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.SyncTimeout = timeout
		}
	}
	NestOptionWithTransactionCommitter = func(committer TransactionCommitter) NestOption {
		return func(opts *NestOpts) {
			opts.Committer = committer
		}
	}
	// NestOptionWithPipelinedAllowlist restricts DurabilityPipelined to the
	// named handlers; dispatching any other pipelined handler fails with
	// ErrPipelinedNotAllowed. Not calling it (or passing no names) permits
	// every handler — production deployments should always pin a list.
	// NestOptionWithSlowLockThreshold sets the warn threshold for the
	// per-handler entity-lock hold time. Every dispatch records its hold in
	// the nest.handler.lock_hold metric; holds at or beyond the threshold
	// additionally count nest.handler.lock_hold.slow.total and log a
	// warning. Zero disables the warning (the metric is always recorded);
	// the default is 100ms.
	NestOptionWithSlowLockThreshold = func(threshold time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.SlowLockThreshold = threshold
			opts.SlowLockThresholdSet = true
		}
	}
	NestOptionWithPipelinedAllowlist = func(names ...string) NestOption {
		return func(opts *NestOpts) {
			opts.PipelinedAllowlist = append(opts.PipelinedAllowlist, names...)
		}
	}
	// NestOptionWithPipelinedAsyncCompletion enables Phase 2 of the pipelined
	// commit: the dispatch worker no longer parks on the commit ticket —
	// AfterCommit hooks and the reply run on a completion pool (hashed by
	// entity, so same-entity completions keep commit order) once the record
	// is durable. Hooks therefore run without the request context or entity
	// locks; see NEST_PIPELINED_COMMIT.md §10 before enabling. workers and
	// queueCap <= 0 select defaults (4, 8192); a full queue degrades single
	// transactions back to the Phase 1 in-worker wait.
	NestOptionWithPipelinedAsyncCompletion = func(workers, queueCap int) NestOption {
		return func(opts *NestOpts) {
			opts.PipelinedAsync = true
			opts.PipelinedAsyncWorkers = workers
			opts.PipelinedAsyncQueueCap = queueCap
		}
	}
)

// NewEngine constructs an instance-scoped Nest engine. Callers inject its
// Client into generated senders. An engine is single-use and cannot be
// restarted after Shutdown.
func NewEngine(opts ...NestOption) *NestMgr {
	params := &NestOpts{}
	for _, opt := range opts {
		if opt != nil {
			opt(params)
		}
	}
	if !params.SlowLockThresholdSet {
		params.SlowLockThreshold = 100 * time.Millisecond
	}
	ret := &NestMgr{
		getter:                 params.Getter,
		remoteSnapshotResolver: params.RemoteSnapshotResolver,
		remoteManager:          params.RemoteManager,
		committer:              params.Committer,
		syncTimeout:            params.SyncTimeout,
		slowLockThreshold:      params.SlowLockThreshold,
		stopDone:               make(chan struct{}),
		groupLocks:             newEntityLockGroupLockManager(),
		handlers:               snapshotHandlerEntries(),
	}
	if len(params.PipelinedAllowlist) > 0 {
		ret.pipelinedAllow = make(map[string]struct{}, len(params.PipelinedAllowlist))
		for _, name := range params.PipelinedAllowlist {
			ret.pipelinedAllow[name] = struct{}{}
		}
	}
	if params.PipelinedAsync {
		ret.completions = newCompletionPump(params.PipelinedAsyncWorkers, params.PipelinedAsyncQueueCap)
		ret.completions.fence = ret.Fence
	}
	if ret.syncTimeout <= 0 {
		ret.syncTimeout = NestSyncTimeout
	}
	ret.dispatcher = NewDispatcher("nest", params.WorkerNum, params.HbWorkerNum, params.MsgCap, func(msg *Msg) {
		NestDispatch(ret, msg)
	})
	ret.dispatcher.ConfigureDelayedAdmission(params.DelayedMsgCap, params.MaxDelay)
	ret.ticker = NewTicker(params.TickDuration)
	return ret
}

// Start starts the worker pools and frame ticker. Repeated calls before
// Shutdown are harmless.
func (mgr *NestMgr) Start() error {
	if mgr == nil {
		return ErrNestStopped
	}
	mgr.lifecycleMu.Lock()
	defer mgr.lifecycleMu.Unlock()
	if mgr.stopped {
		return ErrNestStopped
	}
	if mgr.started {
		return nil
	}
	if mgr.getter == nil {
		return ErrGetterNotSet
	}
	mgr.dispatcher.OnInit()
	mgr.dispatcher.OnRun()
	if mgr.completions != nil {
		mgr.completions.start()
	}
	mgr.ticker.Start()
	mgr.started = true
	return nil
}

// Shutdown stops admission, drains accepted work and stops the ticker. It is
// safe for concurrent callers; all callers observe the same result.
func (mgr *NestMgr) Shutdown(ctx context.Context) error {
	if mgr == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mgr.lifecycleMu.Lock()
	if mgr.stopped {
		done := mgr.stopDone
		mgr.lifecycleMu.Unlock()
		return mgr.waitStopped(ctx, done)
	}
	mgr.stopped = true
	started := mgr.started
	done := mgr.stopDone
	mgr.lifecycleMu.Unlock()

	if !started {
		mgr.finishShutdown(nil)
		return nil
	}
	// Once shutdown starts, accepted work must finish even if one caller's
	// deadline expires. This prevents a second engine from starting while old
	// workers still mutate entities. The caller's context bounds only its wait.
	go func() {
		mgr.ticker.Stop()
		err := mgr.dispatcher.OnDestroyWithContext(context.Background())
		// Deferred completions are accepted work: every reply and AfterCommit
		// promised to a caller must be delivered before shutdown finishes.
		// The dispatcher is drained, so no new submissions can arrive.
		if mgr.completions != nil {
			err = errors.Join(err, mgr.completions.stop(context.Background()))
		}
		mgr.finishShutdown(err)
	}()
	return mgr.waitStopped(ctx, done)
}

func (mgr *NestMgr) waitStopped(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		mgr.lifecycleMu.Lock()
		err := mgr.stopErr
		mgr.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (mgr *NestMgr) finishShutdown(err error) {
	mgr.lifecycleMu.Lock()
	mgr.stopErr = err
	close(mgr.stopDone)
	mgr.lifecycleMu.Unlock()
}

// Running reports whether Start completed and Shutdown has not begun.
func (mgr *NestMgr) Running() bool {
	if mgr == nil {
		return false
	}
	mgr.lifecycleMu.Lock()
	defer mgr.lifecycleMu.Unlock()
	return mgr.started && !mgr.stopped && mgr.fenceErr == nil
}

type sendOptParam struct {
	Delay time.Duration
	Cost  bool
}

type SendOpt func(*sendOptParam)

var (
	SendOptionWithDelay = func(delay time.Duration) SendOpt {
		return func(opt *sendOptParam) {
			opt.Delay = delay
		}
	}
	SendOptionIsCost = func() SendOpt {
		return func(opt *sendOptParam) {
			opt.Cost = true
		}
	}
)

// checkRemoteId checks whether a target ID is remote-capable and marks the
// message for remote preparation. The preparer later decides marked remote vs
// local fast path from the runtime marker store.
func checkRemoteId(msg *Msg, id int64) {
	if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
		msg.HasRemote = true
		msg.Cost = true
	}
}

// checkRemoteIds checks if any target ID in the slice is remote-capable and
// marks the message for remote preparation.
func checkRemoteIds(msg *Msg, ids []int64) {
	for _, id := range ids {
		if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
			msg.HasRemote = true
			msg.Cost = true
			return
		}
	}
}

// checkRemoteGroups checks if any target ID in grouped slices is
// remote-capable and marks the message for remote preparation.
func checkRemoteGroups(msg *Msg, groups [][]int64) {
	for _, g := range groups {
		for _, id := range g {
			if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
				msg.HasRemote = true
				msg.Cost = true
				return
			}
		}
	}
}

func shouldPrepareRemoteID(meta entity.EntityIDMeta) bool {
	if meta.Kind == entity.EntityKindNone || !meta.RemoteCapable {
		return false
	}
	if !entity.IsEntityKindRemoteCapable(meta.Kind) {
		return false
	}
	return entity.IsEntityKindRemoteManaged(meta.Kind)
}

func bindMsgContext(msg *Msg, carryBase bool) {
	if msg == nil {
		return
	}
	snapshot := fctx.CaptureSnapshot()
	if !carryBase {
		snapshot = asyncMessageContextSnapshot(snapshot)
	}
	msg.Context = snapshot
}

// asyncMessageContextSnapshot keeps the immutable framework envelope needed
// for tracing, request identity and config-generation consistency, while
// dropping execution-local state. In particular Base values, arbitrary KV
// (which may contain the active rollback transaction), frame and sync wait
// never cross the asynchronous boundary.
func asyncMessageContextSnapshot(snapshot fctx.ContextSnapshot) fctx.ContextSnapshot {
	if !snapshot.Valid {
		snapshot.Valid = true
		snapshot.Config = fctx.RuntimeConfig()
	}
	return fctx.ContextSnapshot{
		Valid:  true,
		Config: snapshot.Config,
		Meta:   snapshot.Meta,
		Trace:  snapshot.Trace.Clone(),
	}
}

func ensureAsyncDispatchAllowed(api string, name HandlerName) {
	if !fctx.InNestHandler() {
		return
	}
	c := fctx.CurrentContext()
	err := fmt.Errorf("%w: api=%s caller=%s target=%s", ErrAsyncInHandler, api, c.Meta.Handler, name.String())
	slog.Error("nest async dispatch from nest handler rejected",
		"err", err,
		"api", api,
		"caller", c.Meta.Handler,
		"target", name.String(),
		"player", c.Meta.PlayerID,
		"frame", c.Frame,
	)
	panic(err)
}
