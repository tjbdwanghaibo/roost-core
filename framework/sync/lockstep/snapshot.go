package lockstep

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	flog "github.com/tjbdwanghaibo/roost-core/infra/observe/log"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 快照恢复（设计见 docs/maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md）。
//
// 落后的会话仍只发 OpCatchup(from)；走追帧还是走全量快照由服务端按 MatchConfig.Recovery 判定。
// 快照来源是业务的 SnapshotProvider，框架只负责调用、校验大小与帧号、在多个恢复中的会话之间共享、
// 分块经可靠通道下发（CA 01）、快照之后从快照帧的下一帧续发历史页，并在失败时发 C9 v2 通知。

// RecoveryMode 决定落后的会话怎样回到对局。
type RecoveryMode uint8

const (
	// RecoveryUnset 是零值：配置了 Snapshots.Provider 时解析为 RecoveryAuto，否则为 RecoveryCatchup（现行行为）。
	RecoveryUnset RecoveryMode = iota
	// RecoveryCatchup 只追帧；起点早于保留窗口时以 ErrCatchupUnservable 失败并发失败通知。
	RecoveryCatchup
	// RecoverySnapshot 对每个追帧请求都先发全量快照，再从快照帧之后追帧。
	RecoverySnapshot
	// RecoveryAuto 缺的帧都还在窗口内、且缺口不超过 AutoCatchupMaxFrames（0 表示不看缺口）时追帧，否则发快照。
	RecoveryAuto
)

func (mode RecoveryMode) String() string {
	switch mode {
	case RecoveryUnset:
		return "unset"
	case RecoveryCatchup:
		return "catchup"
	case RecoverySnapshot:
		return "snapshot"
	case RecoveryAuto:
		return "auto"
	default:
		return "invalid"
	}
}

// 快照配置的缺省与上限（维护者 2026-10-20 按设计 §12 推荐定下）。
const (
	// DefaultMaxSnapshotBytes 是 SnapshotConfig.MaxBytes 的缺省值，也是客户端缺省接受的上限。
	DefaultMaxSnapshotBytes = 4 << 20
	// MaxSnapshotBytes 是 SnapshotConfig.MaxBytes 的硬上限：客户端按首块声明的总长分配内存，上限防止恶意首块撑爆客户端。
	MaxSnapshotBytes = 64 << 20
	// DefaultSnapshotChunkBytes 是一个分块的数据字节数缺省值，远小于 TCP / KCP / QUIC 的单包上限。
	DefaultSnapshotChunkBytes = 16 << 10
	// DefaultSnapshotBytesPerTick 是“发送随链路放慢”的可靠通道上每会话每 Tick 最多发起的快照字节。
	DefaultSnapshotBytesPerTick = 64 << 10
	// DefaultSnapshotUnpacedBytesPerTick 是“只准入”的可靠通道（独立 Gate）上每会话每 Tick 的快照字节，即发送速率。
	DefaultSnapshotUnpacedBytesPerTick = 16 << 10
	// DefaultSnapshotFetchTimeout 限一次 SnapshotProvider 调用。
	DefaultSnapshotFetchTimeout = 2 * time.Second
)

// SnapshotReason 说明一次快照恢复为什么发起，供业务记录与指标标签。
type SnapshotReason uint8

const (
	// SnapshotOutOfWindow：请求起点或追帧游标已早于保留窗口。
	SnapshotOutOfWindow SnapshotReason = iota + 1
	// SnapshotOverThreshold：auto 模式下缺口超过 AutoCatchupMaxFrames。
	SnapshotOverThreshold
	// SnapshotModeSnapshot：RecoverySnapshot 模式下的每个请求。
	SnapshotModeSnapshot
	// SnapshotHostForced：宿主调用 StartSnapshot / SpectatorSnapshot。
	SnapshotHostForced
)

func (reason SnapshotReason) String() string {
	switch reason {
	case SnapshotOutOfWindow:
		return "out_of_window"
	case SnapshotOverThreshold:
		return "over_threshold"
	case SnapshotModeSnapshot:
		return "mode_snapshot"
	case SnapshotHostForced:
		return "host_forced"
	default:
		return "unknown"
	}
}

// SnapshotRequest 描述一次取快照。快照帧号必须落在 [MinFrame, MaxFrame]：
// MinFrame = 发起时历史仍保留的最早帧 - 1（恢复期间 MinFrame+1 起的帧由钉住保护，不被自动裁剪），
// MaxFrame = 发起时已切出的最新帧（服务器参考模拟不可能超前于已切帧）；配置了 MatchConfig.CommittedFrame 时
// 不超过已提交帧。已提交帧落后于窗口起点时 MinFrame 可能大于 MaxFrame，此时没有可用快照。
type SnapshotRequest struct {
	MinFrame FrameID
	MaxFrame FrameID
	Reason   SnapshotReason
}

// Snapshot 是业务在某一帧的完整局内状态。
type Snapshot struct {
	// Frame 的含义：状态 = 依次应用帧 1..Frame 之后的结果；0 表示开局初始状态。
	Frame FrameID
	// Data 是业务编码，框架不解释、不复制：交出后业务不得再修改（多个恢复中的会话共享同一份）。
	Data []byte
	// StateHash 是业务在 Frame 处的状态 hash，算法与关键帧 ReportHash 相同；客户端恢复后核对。
	// HasStateHash 为 false 时客户端只校验字节摘要、不核对状态（不推荐）。
	StateHash    uint64
	HasStateHash bool
}

// SnapshotProvider 是快照来源，由业务实现（例如服务器参考模拟的检查点）。
//
// 调用线程：Match 在自己起的 goroutine 上调用 Snapshot——不是调用 Tick 的协程，也不是 Nest worker；
// Tick 只非阻塞地收结果，所以实现可以阻塞（读检查点存储、向房间 owner 的邮箱要一份序列化结果），
// 但必须响应 ctx（SnapshotConfig.FetchTimeout）；不响应时 Match 到期后按超时计失败、不再等它。
// 实现必须并发安全：它与房间 owner 同时运行，不得直接读 owner 独占的模拟状态（发布不可变检查点再读）。
// 同一场对局同一时刻最多一次调用在途；多个会话同时等快照时共用这一次的结果。
// panic 会被 recover 并按取快照失败处理（记 ERROR 日志），不让业务 bug 拖垮进程。
type SnapshotProvider interface {
	Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error)
}

// SnapshotProviderFunc 把函数适配成 SnapshotProvider。
type SnapshotProviderFunc func(context.Context, SnapshotRequest) (Snapshot, error)

func (f SnapshotProviderFunc) Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	return f(ctx, req)
}

var (
	// ErrSnapshotUnavailable 表示此刻没有落在请求范围内的快照（例如开局后第一个检查点还没发布）。
	ErrSnapshotUnavailable = errors.New("lockstep match: no snapshot in the requested range")
	// ErrSnapshotTooLarge 表示快照超过 SnapshotConfig.MaxBytes：确定失败，不重试，发失败通知。
	ErrSnapshotTooLarge = errors.New("lockstep match: snapshot exceeds the configured size limit")
	// ErrSnapshotInvalid 表示 Provider 返回的快照帧号越界或数据为空：计一次失败，下一 Tick 重取。
	ErrSnapshotInvalid = errors.New("lockstep match: snapshot outside the requested range or empty")
	// ErrSnapshotNotConfigured 表示没有配置 Snapshots.Provider 却要求快照恢复（StartSnapshot）。
	ErrSnapshotNotConfigured = errors.New("lockstep match: no snapshot provider configured")
	// ErrSnapshotCorrupt 表示客户端重组出的快照与首块携带的 SHA-256 摘要不符。
	ErrSnapshotCorrupt = errors.New("lockstep: snapshot digest mismatch")
)

// SnapshotConfig 配置快照恢复；Provider 为 nil 时其余字段不生效。
type SnapshotConfig struct {
	// Provider 是业务快照来源；配置后 Recovery 零值解析为 RecoveryAuto。
	Provider SnapshotProvider
	// AutoCatchupMaxFrames 是 auto 的缺口阈值；0（缺省）= 只按窗口判断：窗口内一律追帧，越界才发快照。
	AutoCatchupMaxFrames int
	// MaxBytes 是快照大小上限；0 取 DefaultMaxSnapshotBytes，不得超过 MaxSnapshotBytes。
	MaxBytes int
	// ChunkBytes 是一个分块的数据字节数；0 取 DefaultSnapshotChunkBytes。加上分块头后不得超过可靠通道
	// 声明的单条上限（Reliable 实现 nettransport.ReliablePayloadLimiter 时核对）。
	ChunkBytes int
	// BytesPerTick 是每会话每 Tick 最多发起的快照字节（可靠通道实现 nettransport.ReliablePacer 且返回 true，
	// 即发送随链路放慢时）；0 取 DefaultSnapshotBytesPerTick。
	BytesPerTick int
	// UnpacedBytesPerTick 用于只做准入、不随链路放慢的可靠通道（独立 Gate 的 gate.Transport）；
	// 0 取 DefaultSnapshotUnpacedBytesPerTick。它就是这类通道上的快照发送速率，须低于最慢目标链路。
	UnpacedBytesPerTick int
	// FetchTimeout 限一次 Provider 调用；0 取 DefaultSnapshotFetchTimeout。
	FetchTimeout time.Duration
	// PinFrames 是恢复期间历史最多比保留窗口多留的帧数；0 取 HistoryRetainFrames（最坏两倍窗口）。
	// 超过的会话放弃恢复、收到 recovery_expired 通知。
	PinFrames int
}

// snapshotSettings 是解析后的 SnapshotConfig（只在配置了 Provider 时存在）。
type snapshotSettings struct {
	provider     SnapshotProvider
	autoMax      int
	maxBytes     int
	chunkBytes   int
	budget       int // 本局可靠通道上每会话每 Tick 的快照字节：随链路放慢取 BytesPerTick，只准入取 UnpacedBytesPerTick
	fetchTimeout time.Duration
	pinFrames    int
}

// resolveRecovery 解析恢复方式与快照配置；规则声明一次，在 NewMatch 时强制一次。
func resolveRecovery(config MatchConfig, retain int) (RecoveryMode, *snapshotSettings, error) {
	mode, snapshots := config.Recovery, config.Snapshots
	if mode > RecoveryAuto {
		return 0, nil, fmt.Errorf("%w: unknown Recovery %d", ErrMatchConfigInvalid, mode)
	}
	if mode == RecoveryUnset {
		mode = RecoveryCatchup
		if snapshots.Provider != nil {
			mode = RecoveryAuto
		}
	}
	if snapshots.Provider == nil {
		if mode != RecoveryCatchup {
			return 0, nil, fmt.Errorf("%w: Recovery %s requires Snapshots.Provider", ErrMatchConfigInvalid, mode)
		}
		return mode, nil, nil
	}
	if mode != RecoveryCatchup && config.Reliable == nil {
		return 0, nil, fmt.Errorf("%w: Recovery %s requires a Reliable sender for snapshot chunks", ErrMatchConfigInvalid, mode)
	}
	if snapshots.AutoCatchupMaxFrames < 0 || snapshots.MaxBytes < 0 || snapshots.ChunkBytes < 0 || snapshots.BytesPerTick < 0 ||
		snapshots.UnpacedBytesPerTick < 0 || snapshots.FetchTimeout < 0 || snapshots.PinFrames < 0 {
		return 0, nil, fmt.Errorf("%w: negative Snapshots setting", ErrMatchConfigInvalid)
	}
	settings := &snapshotSettings{
		provider:     snapshots.Provider,
		autoMax:      snapshots.AutoCatchupMaxFrames,
		maxBytes:     cmp.Or(snapshots.MaxBytes, DefaultMaxSnapshotBytes),
		chunkBytes:   cmp.Or(snapshots.ChunkBytes, DefaultSnapshotChunkBytes),
		fetchTimeout: cmp.Or(snapshots.FetchTimeout, DefaultSnapshotFetchTimeout),
		pinFrames:    cmp.Or(snapshots.PinFrames, retain),
	}
	if settings.maxBytes > MaxSnapshotBytes {
		return 0, nil, fmt.Errorf("%w: Snapshots.MaxBytes %d exceeds the hard limit %d", ErrMatchConfigInvalid, settings.maxBytes, MaxSnapshotBytes)
	}
	if limiter, ok := config.Reliable.(nettransport.ReliablePayloadLimiter); ok {
		if limit := limiter.MaxReliableBytes(); limit > 0 && settings.chunkBytes+MaxSnapshotChunkHeaderBytes > limit {
			return 0, nil, fmt.Errorf("%w: Snapshots.ChunkBytes %d plus %dB chunk header exceeds the reliable sender's %dB message limit", ErrMatchConfigInvalid, settings.chunkBytes, MaxSnapshotChunkHeaderBytes, limit)
		}
	}
	if pacer, ok := config.Reliable.(nettransport.ReliablePacer); ok && pacer.PacesReliableToLink() {
		settings.budget = cmp.Or(snapshots.BytesPerTick, DefaultSnapshotBytesPerTick)
	} else {
		settings.budget = cmp.Or(snapshots.UnpacedBytesPerTick, DefaultSnapshotUnpacedBytesPerTick)
	}
	return mode, settings, nil
}

// snapshotTransfer 是一个会话的快照阶段：先等本局共享的取快照（image 为 nil），再按偏移分块下发。
type snapshotTransfer struct {
	reason SnapshotReason
	// fallback：auto 下只因缺口超过阈值而走快照；取快照失败且追帧仍可服务时回退追帧，不计失败。
	fallback bool
	// pin 是等快照期间钉住的最早帧（进入快照阶段时的窗口起点）；拿到快照后改钉 image.Frame+1。
	pin      FrameID
	image    *snapshotImage
	transfer uint64
	offset   int // 已确认写出的字节
}

// snapshotImage 是一份已校验的快照与它的 SHA-256 摘要，多个会话共享、只读。
type snapshotImage struct {
	snapshot Snapshot
	digest   [SnapshotDigestBytes]byte
}

func (image *snapshotImage) chunk(transfer uint64, offset, end int) []byte {
	chunk := SnapshotChunk{Transfer: transfer, Offset: offset, Data: image.snapshot.Data[offset:end]}
	if offset == 0 {
		chunk.Frame, chunk.Total = image.snapshot.Frame, len(image.snapshot.Data)
		chunk.StateHash, chunk.HasStateHash, chunk.Digest = image.snapshot.StateHash, image.snapshot.HasStateHash, image.digest
	}
	return appendSnapshotChunk(make([]byte, 0, MaxSnapshotChunkHeaderBytes+end-offset), chunk)
}

// snapshotFetch 是本局唯一在途的一次 Provider 调用。
type snapshotFetch struct {
	request  SnapshotRequest
	deadline time.Time
	done     chan snapshotFetchResult
	cancel   context.CancelFunc
}

type snapshotFetchResult struct {
	image *snapshotImage
	err   error
}

var errSnapshotProviderPanic = errors.New("lockstep match: snapshot provider panicked")

// StartSnapshot 让席位的会话不论恢复方式都走全量快照（例如 OnDesync 判出离群后，宿主用服务器参考状态把它拉回；
// 是否这样处置离群者是业务的选择）。要求配置了 Snapshots.Provider。快照已在取或在传时是空操作。
func (m *Match) StartSnapshot(player PlayerID) error {
	session, attached := m.sessions[player]
	if !attached {
		return ErrPlayerDetached
	}
	return m.startSnapshot(session)
}

// SpectatorSnapshot 是旁观者会话的 StartSnapshot。
func (m *Match) SpectatorSnapshot(session nettransport.SessionID) error {
	if _, ok := m.spectators[session]; !ok {
		return ErrPlayerDetached
	}
	return m.startSnapshot(session)
}

func (m *Match) startSnapshot(session nettransport.SessionID) error {
	switch {
	case m.closed:
		return ErrMatchClosed
	case m.snapshots == nil:
		return ErrSnapshotNotConfigured
	case m.reliable == nil:
		return ErrCatchupUnavailable
	}
	state, catching := m.catchups[session]
	switch {
	case catching && state.unservable != nil:
		return state.unservable
	case catching && state.snapshot != nil:
		return nil
	case !catching:
		state = &catchupState{next: m.sequencer.NextFrame()}
		m.catchups[session] = state
	}
	m.beginSnapshot(state, SnapshotHostForced, false)
	return nil
}

// routeRecovery 是 auto / snapshot 模式下的请求时判定（设计 §3.2）：
//
//	模式      窗口内且未超阈值   窗口内但超阈值   起点已被裁掉
//	snapshot  快照               快照             快照
//	auto      追帧               快照（可回退）   快照
//
// 已在追帧（含快照之后的续帧）的会话：起点不早于游标的请求是重试，照原样继续，不重新判定；
// 更早的起点按 catchup 的规则把游标往回拨，再按新游标判定。
func (m *Match) routeRecovery(session nettransport.SessionID, existing *catchupState, from, start FrameID) error {
	state := existing
	if state == nil {
		state = &catchupState{next: from}
	} else if from >= state.next {
		return nil
	} else {
		state.next = from
	}
	var reason SnapshotReason
	fallback := false
	switch {
	case m.recovery == RecoverySnapshot:
		reason = SnapshotModeSnapshot
	case state.next < start:
		reason = SnapshotOutOfWindow
	case m.snapshots.autoMax > 0 && int64(m.history.Latest())+1-int64(state.next) > int64(m.snapshots.autoMax):
		reason, fallback = SnapshotOverThreshold, true
	}
	if existing == nil {
		m.catchups[session] = state
	}
	if reason != 0 {
		m.beginSnapshot(state, reason, fallback)
	}
	return nil
}

// beginSnapshot 让会话进入等快照阶段：取消在途的追帧页（结果不再有用），从下一次 pumpCatchup 起参与本局共享的取快照。
func (m *Match) beginSnapshot(state *catchupState, reason SnapshotReason, fallback bool) {
	state.drop()
	state.recovered = false
	state.snapshot = &snapshotTransfer{reason: reason, fallback: fallback, pin: m.history.start()}
	metrics.IncCounter(MetricSnapshot, metrics.Labels{"reason": reason.String()}, 1)
}

// failRecovery 结束会话的恢复：取消在途发送，下一次 pumpCatchup 发出 C9 v2 失败通知，结算后删除状态。
func (m *Match) failRecovery(state *catchupState, reason RecoveryFailureReason) *CatchupUnservableError {
	state.drop()
	state.snapshot, state.recovered = nil, false
	state.unservable = &CatchupUnservableError{Reason: reason, From: state.next, Start: m.history.start()}
	return state.unservable
}

// snapshotWaiters 列出等快照的会话（升序）。
func (m *Match) snapshotWaiters() []nettransport.SessionID {
	var waiters []nettransport.SessionID
	for _, session := range m.catchupSessions() {
		if state := m.catchups[session]; state.snapshot != nil && state.snapshot.image == nil {
			waiters = append(waiters, session)
		}
	}
	return waiters
}

// releaseIdleFetch 在没有会话再等快照时取消在途的取快照，不让结果（可能是几 MiB）挂在 Match 上。
func (m *Match) releaseIdleFetch() {
	if m.fetch == nil {
		return
	}
	for _, state := range m.catchups {
		if state.snapshot != nil && state.snapshot.image == nil {
			return
		}
	}
	m.fetch.cancel()
	m.fetch = nil
}

// launchFetch 在 Match 自己起的 goroutine 上调用 Provider。请求范围按发起时的历史窗口定：等快照的会话
// 把窗口起点钉住，所以拿到的快照帧之后的帧在续帧时仍在历史里。上界 head 是 servableHead：
// 配置了 CommittedFrame 时不超过已提交帧，Provider 交出超前的检查点按无效快照处理。
func (m *Match) launchFetch(ctx context.Context, head FrameID) {
	waiters := m.snapshotWaiters()
	if len(waiters) == 0 {
		return
	}
	request := SnapshotRequest{MinFrame: m.history.start() - 1, MaxFrame: head, Reason: m.catchups[waiters[0]].snapshot.reason}
	timeout := m.snapshots.fetchTimeout
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	fetch := &snapshotFetch{request: request, deadline: time.Now().Add(timeout), done: make(chan snapshotFetchResult, 1), cancel: cancel}
	provider, maxBytes := m.snapshots.provider, m.snapshots.maxBytes
	go func() {
		fetch.done <- fetchSnapshot(fetchCtx, provider, request, maxBytes)
	}()
	m.fetch = fetch
}

// fetchSnapshot 调用 Provider 并在调用 goroutine 内完成校验与摘要，Tick 只收结果。panic 被 recover 成错误。
func fetchSnapshot(ctx context.Context, provider SnapshotProvider, request SnapshotRequest, maxBytes int) (result snapshotFetchResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			flog.Error("lockstep: snapshot provider panicked", "panic", recovered, "min_frame", request.MinFrame, "max_frame", request.MaxFrame, "stack", string(debug.Stack()))
			result = snapshotFetchResult{err: fmt.Errorf("%w: %v", errSnapshotProviderPanic, recovered)}
		}
	}()
	snapshot, err := provider.Snapshot(ctx, request)
	switch {
	case err != nil:
		return snapshotFetchResult{err: err}
	case len(snapshot.Data) > maxBytes:
		return snapshotFetchResult{err: fmt.Errorf("%w: %d bytes, limit %d", ErrSnapshotTooLarge, len(snapshot.Data), maxBytes)}
	case len(snapshot.Data) == 0 || snapshot.Frame < request.MinFrame || snapshot.Frame > request.MaxFrame:
		return snapshotFetchResult{err: fmt.Errorf("%w: frame %d (want %d..%d), %d bytes", ErrSnapshotInvalid, snapshot.Frame, request.MinFrame, request.MaxFrame, len(snapshot.Data))}
	}
	return snapshotFetchResult{image: &snapshotImage{snapshot: snapshot, digest: sha256.Sum256(snapshot.Data)}}
}

// collectFetch 非阻塞地收取在途的取快照；Provider 不响应 ctx 时到期按超时处理，不再等它。
// 返回 true 表示本 Tick 收到了结果（成功或失败）。
func (m *Match) collectFetch(now time.Time) (bool, error) {
	fetch := m.fetch
	var result snapshotFetchResult
	select {
	case result = <-fetch.done:
	default:
		if now.Before(fetch.deadline) {
			return false, nil
		}
		result = snapshotFetchResult{err: fmt.Errorf("%w: provider did not return within %v", context.DeadlineExceeded, m.snapshots.fetchTimeout)}
	}
	fetch.cancel()
	m.fetch = nil
	return true, m.applyFetch(fetch.request, result)
}

// applyFetch 把一次取快照的结果分给所有等快照的会话（设计 §4.3）。
func (m *Match) applyFetch(request SnapshotRequest, result snapshotFetchResult) error {
	waiters := m.snapshotWaiters()
	start := m.history.start()
	if result.err == nil {
		var errs []error
		for _, session := range waiters {
			state := m.catchups[session]
			if result.image.snapshot.Frame+1 < start {
				// 只有宿主手动 TrimHistory 越过钉住点才会发生：快照之后的帧已不在，按无效快照计一次失败。
				errs = append(errs, m.snapshotAttemptFailed(session, state, fmt.Errorf("%w: frame %d precedes history start %d", ErrSnapshotInvalid, result.image.snapshot.Frame, start)))
				continue
			}
			m.lastTransfer++
			state.snapshot.image, state.snapshot.transfer, state.snapshot.offset = result.image, m.lastTransfer, 0
			state.failures = 0
		}
		return errors.Join(errs...)
	}
	label := "unavailable"
	switch {
	case errors.Is(result.err, ErrSnapshotTooLarge):
		label = "too_large"
	case errors.Is(result.err, ErrSnapshotInvalid):
		label = "invalid"
	}
	metrics.IncCounter(MetricSnapshotFailed, metrics.Labels{"reason": label}, 1)
	var errs []error
	for _, session := range waiters {
		state := m.catchups[session]
		if label == "too_large" {
			// 确定失败：业务该缩小快照或调大上限，重试没有意义。
			errs = append(errs, fmt.Errorf("catchup session %d: %w: %w", session, m.failRecovery(state, RecoverySnapshotTooLarge), result.err))
			continue
		}
		if state.snapshot.fallback && state.next >= start {
			state.snapshot = nil // auto 超阈值：追帧仍可服务，回退追帧，不计失败、不通知客户端
			continue
		}
		errs = append(errs, m.snapshotAttemptFailed(session, state, result.err))
	}
	if len(errs) != 0 {
		errs = append([]error{fmt.Errorf("snapshot fetch [%d, %d] (%s)", request.MinFrame, request.MaxFrame, request.Reason)}, errs...)
	}
	return errors.Join(errs...)
}

// snapshotAttemptFailed 计一次取快照失败；达到 CatchupMaxFailures 时发 snapshot_unavailable 通知。
func (m *Match) snapshotAttemptFailed(session nettransport.SessionID, state *catchupState, cause error) error {
	state.failures++
	if state.failures >= m.catchupMax {
		return fmt.Errorf("catchup session %d: %w after %d attempts: %w", session, m.failRecovery(state, RecoverySnapshotUnavailable), state.failures, cause)
	}
	return fmt.Errorf("catchup session %d: snapshot attempt %d: %w", session, state.failures, cause)
}

// launchChunks 在后台 goroutine 上从已确认偏移起依次发送分块，直到本批达到每 Tick 预算或传完；
// 整批受 CatchupSendTimeout 约束，回报确认写出的字节，游标只前进到已确认处。
func (m *Match) launchChunks(ctx context.Context, session nettransport.SessionID, transfer *snapshotTransfer) *catchupSend {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.sendTimeout)
	send := &catchupSend{chunks: true, done: make(chan sendResult, 1), cancel: cancel}
	reliable, image, id, offset := m.reliable, transfer.image, transfer.transfer, transfer.offset
	chunkBytes, budget := m.snapshots.chunkBytes, m.snapshots.budget
	go func() {
		var result sendResult
		for total := len(image.snapshot.Data); offset < total && result.written < budget; {
			end := min(offset+chunkBytes, total)
			if result.err = reliable.SendReliable(sendCtx, session, image.chunk(id, offset, end)); result.err != nil {
				break
			}
			result.written += end - offset
			offset = end
		}
		send.done <- result
	}()
	return send
}

// settleChunks 结算一批分块：已确认部分保留；失败计一次（本批有进展先清零），达到 CatchupMaxFailures 放弃
// （不发通知：链路本来就坏了，客户端按重试规则再请求）；传完后从快照帧的下一帧续帧。
func (m *Match) settleChunks(session nettransport.SessionID, state *catchupState, send *catchupSend) error {
	transfer := state.snapshot
	transfer.offset += send.written
	if send.written > 0 {
		state.failures = 0
		metrics.IncCounter(MetricSnapshotBytes, nil, int64(send.written))
	}
	if send.err != nil {
		state.failures++
		if state.failures >= m.catchupMax {
			delete(m.catchups, session)
			metrics.IncCounter(MetricSnapshotFailed, metrics.Labels{"reason": "abandoned"}, 1)
			return fmt.Errorf("catchup session %d: snapshot transfer abandoned after %d failures at byte %d: %w — the client must reconnect", session, state.failures, transfer.offset, send.err)
		}
		return fmt.Errorf("catchup session %d: snapshot chunk at byte %d: %w", session, transfer.offset, send.err)
	}
	if transfer.offset < len(transfer.image.snapshot.Data) {
		return nil
	}
	state.snapshot, state.recovered = nil, true
	state.next = transfer.image.snapshot.Frame + 1
	if state.next > m.history.Latest() {
		delete(m.catchups, session) // 快照就在头部：直接回到实时广播
	}
	return nil
}

// recoveryPin 是快照恢复中的会话需要保留的最早帧；普通追帧（catchup 模式与 auto 窗口内）不钉住。
func recoveryPin(state *catchupState) (FrameID, bool) {
	switch {
	case state.unservable != nil:
		return 0, false
	case state.snapshot != nil && state.snapshot.image == nil:
		return state.snapshot.pin, true
	case state.snapshot != nil:
		return state.snapshot.image.snapshot.Frame + 1, true
	case state.recovered:
		return state.next, true
	default:
		return 0, false
	}
}

// pinnedTrim 返回考虑钉住后的裁剪点：min(正常裁剪点, 所有恢复中会话的钉住点)，但最多比正常窗口多留
// PinFrames 帧；钉住点早于这个下限的会话放弃恢复并收到 recovery_expired 通知，释放钉住。
func (m *Match) pinnedTrim(keep FrameID) (FrameID, error) {
	floor := FrameID(1)
	if keep > FrameID(m.snapshots.pinFrames) {
		floor = keep - FrameID(m.snapshots.pinFrames)
	}
	trim := keep
	var errs []error
	for _, session := range m.catchupSessions() {
		state := m.catchups[session]
		pin, pinned := recoveryPin(state)
		if !pinned || pin >= keep {
			continue
		}
		if pin < floor {
			metrics.IncCounter(MetricSnapshotFailed, metrics.Labels{"reason": "expired"}, 1)
			errs = append(errs, fmt.Errorf("catchup session %d: %w (pinned frame %d, limit %d frames beyond the window)", session, m.failRecovery(state, RecoveryExpired), pin, m.snapshots.pinFrames))
			continue
		}
		trim = min(trim, pin)
	}
	if len(errs) != 0 {
		m.releaseIdleFetch()
	}
	return trim, errors.Join(errs...)
}
