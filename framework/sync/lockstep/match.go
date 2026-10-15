package lockstep

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

var (
	ErrMatchConfigInvalid = errors.New("lockstep match: invalid config")
	ErrMatchClosed        = errors.New("lockstep match: match is closed")
	ErrPlayerDetached     = errors.New("lockstep match: player has no attached session")
	ErrSessionInUse       = errors.New("lockstep match: session already bound to another receiver")
	ErrCatchupUnavailable = errors.New("lockstep match: no reliable sender for catch-up")
	ErrCatchupFuture      = errors.New("lockstep match: catch-up start beyond the next frame")
	// ErrCatchupUnservable 表示请求的追帧起点已被裁出历史，这次追帧不可能补齐。
	// 服务端在 StartCatchup / Tick 的错误里返回它，并经可靠通道给该会话发追帧失败通知；
	// 客户端（FrameAssembler、robot、C# LockstepClient）收到通知后终止本场模拟，交由业务决定踢出或重开，
	// 框架不自动重同步。具体起点见 *CatchupUnservableError。
	ErrCatchupUnservable = errors.New("lockstep match: catch-up range trimmed from history")
	ErrHashFrameInvalid  = errors.New("lockstep match: hash report for an uncut or invalid frame")
)

// 指标名属于包契约：OBSERVABILITY.md 的告警规则以它们为键，守卫测试固定这组名字，
// 改名或丢前缀会让 desync 告警静默失效。历史：RR-20261006-61。
const (
	MetricFrames        = "lockstep.frame.total"
	MetricInputLate     = "lockstep.input.late.total"
	MetricInputRejected = "lockstep.input.rejected.total"
	MetricCatchupFrames = "lockstep.catchup.frames.total"
	// MetricCatchupUnservable 计数因起点已被裁出历史而失败的追帧（含请求时即拒绝与追帧途中被裁过）。
	MetricCatchupUnservable = "lockstep.catchup.unservable.total"
	MetricDesync            = "lockstep.desync.total"
)

// DefaultMaxDatagramBytes is the datagram PAYLOAD bound used when neither
// the config nor the datagram sender declares one: the protocol transports'
// default (nettransport.DefaultMaxDatagram — KCP / QUIC payload, and the
// AEAD UDP transport's 1232-byte packet minus its 32-byte envelope).
const DefaultMaxDatagramBytes = nettransport.DefaultMaxDatagram

// Catch-up send defaults (see MatchConfig.CatchupSendTimeout / CatchupSendWait).
const (
	DefaultCatchupSendTimeout = 2 * time.Second
	DefaultCatchupSendWait    = 5 * time.Millisecond
)

// 历史保留（MatchConfig.HistoryRetainFrames）。
//
// DefaultHistoryRetainFrames 取 3600 帧：60Hz 下 60 秒、30Hz 下 120 秒。依据是断线重连窗口按
// 60 秒计（移动网络切换、短暂掉线的常见上限），并按常见逻辑帧率上限 60Hz 换算；30Hz 的游戏因此有
// 两分钟余量。以 10 席、每帧每席 8 字节输入估算，保留 3600 帧约 1.5MB / 局，与对局长度无关。
// 帧率或允许的断线时长不同的游戏按“允许断线秒数 × 逻辑帧率”自行配置。
//
// MinHistoryRetainFrames 是自动保留的下限：Sequencer 把 ReplayHorizon 之内的迟到输入仍当作在线流量，
// 落后这么多帧的客户端仍属于“在局内”，它的追帧必须可服务；它同时等于 MaxBroadcastFrames，
// 覆盖一整页追帧与最大冗余深度。
const (
	DefaultHistoryRetainFrames = 3600
	MinHistoryRetainFrames     = int(ReplayHorizon)
	// RetainFullHistory 关闭自动裁剪：历史与 hash 报告随对局长度增长，由宿主自行调用
	// TrimHistory / TrimHashReports（关闭前的行为）。
	RetainFullHistory = -1
)

// hashTrimStride 是自动裁剪 hash 报告的步长：裁剪要遍历报告表，不必每帧做；
// 报告保留量因此不超过保留窗口再多一个步长。
const hashTrimStride = FrameID(MaxBroadcastFrames)

// MatchConfig 配置一场对局的输入序列、发送通道、追帧预算与分歧回调。
type MatchConfig struct {
	// Sequencer fixes the seat set, submit window and per-input payload cap
	// (see the Sequencer in this package).
	Sequencer SequencerConfig
	// RedundancyDepth is how many recent frames each broadcast datagram
	// carries (normalized via NormalizeRedundancyDepth: <= 0
	// selects 3, MaxBroadcastFrames is the ceiling). Depth N heals up to
	// N-1 consecutive lost datagrams without retransmission.
	RedundancyDepth int
	// MaxDatagramBytes is the datagram PAYLOAD bound the match's broadcast
	// must fit — what the sender accepts, after its own envelope (AEAD
	// header and tag, framing). Zero takes the bound the Datagrams sender
	// declares (nettransport.DatagramPayloadLimiter: the KCP, QUIC and UDP
	// transports do), else DefaultMaxDatagramBytes; a value above the
	// sender's declared bound is refused. NewMatch refuses configurations
	// whose worst-case packet — depth × players × max payload plus wire
	// overhead, i.e. every seat at full payload — exceeds it: a single
	// full-payload client must never be able to push the match's broadcast
	// past what the transport can send.
	MaxDatagramBytes int
	// HashQuorum is the minimum number of AGREEING hash reports before a
	// keyframe is judged (<= 0 derives majority-of-seats: len(players)/2+1
	// — with that choice a colluding minority reporting first can never
	// convict an honest player). When every seat has reported and no hash
	// reaches the quorum — a two-seat match with different hashes, a 2:2
	// split, all different — the frame is ruled with no majority: every
	// seat is an outlier and the verdict has NoMajority set.
	HashQuorum int
	// CatchupBatchFrames is how many history frames one catching-up session
	// receives per tick over the reliable lane (<= 0 selects 32; capped at
	// MaxBroadcastFrames; 1 is refused). The per-tick cap is the rate limit
	// that keeps a 10s reconnect from flooding the link in one burst — but
	// every tick also cuts one new frame, so a batch of 1 pages exactly as
	// fast as the head moves and the backlog never closes.
	// The net catch-up rate is batch-1 frames per tick; it must be positive.
	CatchupBatchFrames int
	// HistoryRetainFrames 是自动保留的历史帧数（含最新帧）：每次 Tick 切帧后裁掉更早的帧，
	// 并按 hashTrimStride 步长裁掉同一窗口之外的 hash 报告。0 取 DefaultHistoryRetainFrames；
	// RetainFullHistory（-1）关闭自动裁剪；其他值必须不小于 MinHistoryRetainFrames。
	// 起点早于窗口的追帧以 ErrCatchupUnservable 失败，不做全量重同步。宿主仍可调用
	// TrimHistory / TrimHashReports 裁得更多，但不能把已裁掉的帧找回来。
	HistoryRetainFrames int
	// CatchupMaxFailures abandons a catch-up after this many consecutive
	// reliable-lane send failures (<= 0 selects 8) instead of pinning the
	// session forever in a state where it receives neither live frames nor
	// history. The abandonment surfaces in Tick's joined error.
	CatchupMaxFailures int
	// CatchupSendTimeout bounds one catch-up page send on the reliable lane
	// (<= 0 selects DefaultCatchupSendTimeout). A send still blocked past
	// it — a peer that stopped acknowledging, a full KCP window — fails and
	// counts toward CatchupMaxFailures.
	CatchupSendTimeout time.Duration
	// CatchupSendWait is how long Tick waits for the pages it launched in
	// this tick (<= 0 selects DefaultCatchupSendWait). Pages are sent off
	// the match's goroutine, at most one in flight per session; a page that
	// does not finish within the wait stays in flight and is collected on
	// a later Tick, so a slow client delays only its own catch-up, never
	// the match. Tick never waits on a page launched by an
	// earlier tick.
	CatchupSendWait time.Duration
	// Datagrams broadcasts cut frames (required). Loss-tolerant lane: the
	// AEAD UDP transport, or any raw DatagramSender (see the package note).
	Datagrams nettransport.DatagramSender
	// Reliable pages catch-up frames to reconnecting sessions (optional;
	// StartCatchup fails without it). KCP or QUIC transports fit here: the
	// Match bounds every page send with CatchupSendTimeout and never blocks
	// Tick on it, so the caller's context needs no deadline.
	Reliable nettransport.ReliableSender
	// OnDesync is invoked whenever a keyframe ruling gains outliers that
	// were not surfaced before (set difference, not cardinality — an
	// equal-size flip of the outlier set fires too). Nil ignores verdicts.
	OnDesync func(DesyncVerdict)
}

// catchupState is one session's paging cursor.
type catchupState struct {
	next     FrameID
	failures int
	// unservable 非 nil 表示游标已落到保留窗口之前：这次追帧已失败，inflight 是发往客户端的
	// 失败通知，结算后删除该会话的追帧状态。期间会话仍不收实时广播（客户端已终止本场）。
	unservable *CatchupUnservableError
	// inflight is the page currently being sent, or nil. At most one page
	// per session is in flight; the cursor moves only when it succeeds.
	inflight *catchupSend
}

// catchupSend is one page handed to the reliable lane off the match's
// goroutine. done is buffered so the sender never blocks on a match that
// stopped caring (detach, close).
type catchupSend struct {
	start    FrameID // cursor when the page was read
	end      FrameID // cursor after this page succeeds
	frames   int
	done     chan error
	cancel   context.CancelFunc
	finished bool
	err      error
}

// poll collects the send's result without blocking.
func (s *catchupSend) poll() bool {
	if !s.finished {
		select {
		case s.err = <-s.done:
			s.finished = true
		default:
		}
	}
	return s.finished
}

// drop releases a send whose result no longer matters.
func (s *catchupState) drop() {
	if s.inflight != nil {
		s.inflight.cancel()
		s.inflight = nil
	}
}

// Match 是一场对局的服务端帧同步状态，不是大厅房间或匹配服务。
type Match struct {
	sequencer    *Sequencer
	history      *History
	encoder      *RedundantEncoder
	detector     *DesyncDetector
	datagrams    nettransport.DatagramSender
	reliable     nettransport.ReliableSender
	onDesync     func(DesyncVerdict)
	depth        int
	catchupBatch int
	catchupMax   int
	sendTimeout  time.Duration
	sendWait     time.Duration
	// retain 是自动保留的帧数，0 表示关闭（RetainFullHistory）；hashTrimmed 是上次自动裁剪 hash 报告的起点。
	retain      int
	hashTrimmed FrameID
	closed      bool
	// 广播名单由串行 owner 管理，只在连接成员变化后重新排序。
	broadcastReceivers []broadcastReceiver
	// sessions binds attached seats to their transport session.
	sessions map[PlayerID]nettransport.SessionID
	// spectators are receive-only sessions: they get live broadcasts and
	// may catch up, but hold no seat and submit nothing.
	spectators map[nettransport.SessionID]struct{}
	// sessionOwners tracks which receiver (seat or spectator) holds each
	// session id, so one session can never serve two receivers.
	sessionOwners map[nettransport.SessionID]PlayerID // spectators use ownerSpectator
	// catchups holds each catching-up session's paging state.
	catchups map[nettransport.SessionID]*catchupState
	// ruled tracks the outliers already surfaced per judged frame, so
	// OnDesync fires exactly on set growth/change, not cardinality change.
	ruled map[FrameID]map[PlayerID]struct{}
}

// ownerSpectator marks a session owned by a spectator in sessionOwners.
const ownerSpectator PlayerID = -1

// NewMatch 创建一场对局的帧同步协调器；调用方负责串行驱动和结束时关闭。
func NewMatch(config MatchConfig) (*Match, error) {
	if config.Datagrams == nil {
		return nil, fmt.Errorf("%w: datagram sender is required", ErrMatchConfigInvalid)
	}
	sequencer, err := NewSequencer(config.Sequencer)
	if err != nil {
		return nil, err
	}
	depth := NormalizeRedundancyDepth(config.RedundancyDepth)
	maxDatagram, err := datagramBudget(config.MaxDatagramBytes, config.Datagrams)
	if err != nil {
		return nil, err
	}
	// Worst-case broadcast packet: header + depth frames, each carrying
	// every seat at the full payload cap plus varint overhead. Refusing the
	// configuration here is what keeps a single full-payload client from
	// blacking out the whole match's downlink at runtime. The bound is the
	// sender's PAYLOAD bound — its own envelope is already taken off.
	const packetHeader = 2 + 5  // magic+version + frame count varint
	const frameOverhead = 5 + 5 // frame id + input count varints
	const inputOverhead = 5 + 5 // player id + payload length varints
	players := len(config.Sequencer.Players)
	worst := packetHeader + depth*(frameOverhead+players*(inputOverhead+sequencer.MaxInputBytes()))
	if worst > maxDatagram {
		return nil, fmt.Errorf("%w: worst-case packet %dB (depth %d × %d players × %dB payload) exceeds datagram bound %dB — lower Sequencer.MaxInputBytes or RedundancyDepth", ErrMatchConfigInvalid, worst, depth, players, sequencer.MaxInputBytes(), maxDatagram)
	}
	batch := config.CatchupBatchFrames
	if batch <= 0 {
		batch = 32
	}
	if batch == 1 {
		return nil, fmt.Errorf("%w: catch-up batch of 1 frame per tick can never overtake the head (one frame is cut per tick); use >= 2", ErrMatchConfigInvalid)
	}
	if batch > MaxBroadcastFrames {
		batch = MaxBroadcastFrames
	}
	retain, err := historyRetention(config.HistoryRetainFrames)
	if err != nil {
		return nil, err
	}
	maxFailures := config.CatchupMaxFailures
	if maxFailures <= 0 {
		maxFailures = 8
	}
	quorum := config.HashQuorum
	if quorum <= 0 {
		quorum = players/2 + 1
	}
	detector := NewDesyncDetector(quorum)
	detector.seats = players
	sendTimeout := config.CatchupSendTimeout
	if sendTimeout <= 0 {
		sendTimeout = DefaultCatchupSendTimeout
	}
	sendWait := config.CatchupSendWait
	if sendWait <= 0 {
		sendWait = DefaultCatchupSendWait
	}
	return &Match{
		sequencer:     sequencer,
		history:       NewHistory(),
		encoder:       NewRedundantEncoder(depth),
		detector:      detector,
		datagrams:     config.Datagrams,
		reliable:      config.Reliable,
		onDesync:      config.OnDesync,
		depth:         depth,
		catchupBatch:  batch,
		catchupMax:    maxFailures,
		sendTimeout:   sendTimeout,
		sendWait:      sendWait,
		retain:        retain,
		sessions:      make(map[PlayerID]nettransport.SessionID),
		spectators:    make(map[nettransport.SessionID]struct{}),
		sessionOwners: make(map[nettransport.SessionID]PlayerID),
		catchups:      make(map[nettransport.SessionID]*catchupState),
		ruled:         make(map[FrameID]map[PlayerID]struct{}),
	}, nil
}

// historyRetention 把配置的保留帧数解析为生效值，0 表示关闭自动裁剪。
func historyRetention(configured int) (int, error) {
	switch {
	case configured == 0:
		return DefaultHistoryRetainFrames, nil
	case configured == RetainFullHistory:
		return 0, nil
	case configured < MinHistoryRetainFrames:
		return 0, fmt.Errorf("%w: HistoryRetainFrames %d must be RetainFullHistory (-1) or at least %d (ReplayHorizon: a client that far behind is still in the match and must be able to catch up)", ErrMatchConfigInvalid, configured, MinHistoryRetainFrames)
	default:
		return configured, nil
	}
}

// datagramBudget resolves the payload bound the match's worst-case broadcast
// must fit: the sender's declared bound unless the config narrows it, and
// never more than the sender can carry.
func datagramBudget(configured int, sender nettransport.DatagramSender) (int, error) {
	declared := 0
	if limiter, ok := sender.(nettransport.DatagramPayloadLimiter); ok {
		declared = limiter.MaxDatagramPayload()
	}
	switch {
	case configured < 0:
		return 0, fmt.Errorf("%w: MaxDatagramBytes %d is negative", ErrMatchConfigInvalid, configured)
	case configured == 0 && declared > 0:
		return declared, nil
	case configured == 0:
		return DefaultMaxDatagramBytes, nil
	case declared > 0 && configured > declared:
		return 0, fmt.Errorf("%w: MaxDatagramBytes %dB exceeds the datagram sender's payload bound %dB", ErrMatchConfigInvalid, configured, declared)
	default:
		return configured, nil
	}
}

// Attach binds a seat to a transport session; the session starts receiving
// live frame broadcasts on the next Tick. Re-attaching the SAME session is
// idempotent and keeps a pending catch-up; attaching a NEW session
// (reconnect) replaces the previous one and drops its catch-up. A session
// already serving another seat or a spectator is refused — two receivers on
// one session would double-send and cross-cancel each other's catch-up.
func (m *Match) Attach(player PlayerID, session nettransport.SessionID) error {
	if m.closed {
		return ErrMatchClosed
	}
	if !m.sequencer.KnownPlayer(player) {
		return ErrPlayerUnknown
	}
	if owner, bound := m.sessionOwners[session]; bound && owner != player {
		return ErrSessionInUse
	}
	if previous, attached := m.sessions[player]; attached {
		if previous == session {
			return nil // idempotent re-attach keeps the catch-up cursor
		}
		m.dropCatchup(previous)
		delete(m.sessionOwners, previous)
	}
	m.sessions[player] = session
	m.broadcastReceivers = nil
	m.sessionOwners[session] = player
	return nil
}

// Detach unbinds a seat (disconnect). The seat stays in the match — its
// inputs simply stop arriving, which optimistic frame locking already
// tolerates as empty inputs.
func (m *Match) Detach(player PlayerID) {
	if session, attached := m.sessions[player]; attached {
		m.dropCatchup(session)
		delete(m.sessionOwners, session)
		delete(m.sessions, player)
		m.broadcastReceivers = nil
	}
}

// DetachSession 在 Match 的串行 owner 中处理断线通知，仅移除通知所属的生命周期。
// Gate 的 receiver ID 在进程内不复用；旧连接的迟到关闭不能拆掉已经重绑的新连接。
// 完整 Gate/Game/BindID 校验仍由接入层完成，Match 不重复持有连接身份表。
func (m *Match) DetachSession(player PlayerID, session nettransport.SessionID) bool {
	current, attached := m.sessions[player]
	if !attached || current != session {
		return false
	}
	m.Detach(player)
	return true
}

// AttachSpectator binds a receive-only session: it gets live broadcasts and
// may catch up via SpectatorCatchup, but holds no seat.
func (m *Match) AttachSpectator(session nettransport.SessionID) error {
	if m.closed {
		return ErrMatchClosed
	}
	if owner, bound := m.sessionOwners[session]; bound && owner != ownerSpectator {
		return ErrSessionInUse
	}
	m.spectators[session] = struct{}{}
	m.broadcastReceivers = nil
	m.sessionOwners[session] = ownerSpectator
	return nil
}

// DetachSpectator unbinds a spectator session.
func (m *Match) DetachSpectator(session nettransport.SessionID) {
	if _, ok := m.spectators[session]; ok {
		delete(m.spectators, session)
		m.broadcastReceivers = nil
		delete(m.sessionOwners, session)
		m.dropCatchup(session)
	}
}

// dropCatchup ends a session's catch-up and cancels its in-flight page.
func (m *Match) dropCatchup(session nettransport.SessionID) {
	if state, ok := m.catchups[session]; ok {
		state.drop()
		delete(m.catchups, session)
	}
}

// NextFrame is the id the next Tick will cut.
func (m *Match) NextFrame() FrameID { return m.sequencer.NextFrame() }

// SubmitInput feeds one player's input into the sequencer and returns the
// frame it was folded into. Late inputs (frame already cut) are folded
// forward and metered as lockstep.input.late.total; rejected inputs are
// metered as lockstep.input.rejected.total{reason} — the first signal of a
// malicious or version-skewed client.
func (m *Match) SubmitInput(player PlayerID, frame FrameID, payload []byte) (FrameID, error) {
	if m.closed {
		return 0, ErrMatchClosed
	}
	late := frame != 0 && frame < m.sequencer.NextFrame()
	folded, err := m.sequencer.SubmitInput(player, frame, payload)
	if err != nil {
		metrics.IncCounter(MetricInputRejected, metrics.Labels{"reason": rejectReason(err)}, 1)
		return 0, err
	}
	if late {
		metrics.IncCounter(MetricInputLate, nil, 1)
	}
	return folded, nil
}

func rejectReason(err error) string {
	switch {
	case errors.Is(err, ErrPlayerUnknown):
		return "unknown_player"
	case errors.Is(err, ErrFrameTooEarly):
		return "too_early"
	case errors.Is(err, ErrPayloadTooBig):
		return "payload_too_big"
	default:
		return "other"
	}
}

// Tick cuts the next frame, records it in history, and broadcasts the
// redundant packet to every attached session (seats and spectators, in
// deterministic order); it then pages pending catch-ups over the reliable
// lane. Broadcast errors don't stop the frame — the frame is cut and
// history is authoritative regardless of delivery — but they are joined and
// returned so the caller can drop dead sessions.
//
// Catch-up pages are sent off the match's goroutine (one in flight per
// session, each bounded by CatchupSendTimeout) and Tick waits at most
// CatchupSendWait for the pages it launched: a client whose reliable lane
// stalls slows only its own catch-up, never the match.
func (m *Match) Tick(ctx context.Context) (Frame, error) {
	if m.closed {
		return Frame{}, ErrMatchClosed
	}
	frame := m.sequencer.Advance()
	m.history.Append(frame)
	packet := m.encoder.Push(frame)
	metrics.IncCounter(MetricFrames, nil, 1)

	var errs []error
	// Pages that finished since the last tick first: a session whose
	// remaining backlog rides in this tick's redundant packet goes live now.
	if err := m.collectCatchups(); err != nil {
		errs = append(errs, err)
	}
	for _, receiver := range m.broadcastOrder() {
		if _, catching := m.catchups[receiver.session]; catching {
			continue // live frames resume once the catch-up pages reach the head
		}
		if err := m.datagrams.SendDatagram(ctx, receiver.session, packet); err != nil {
			errs = append(errs, fmt.Errorf("receiver %d session %d: %w", receiver.owner, receiver.session, err))
		}
	}
	if err := m.pumpCatchup(ctx); err != nil {
		errs = append(errs, err)
	}
	// 裁剪放在追帧之后：页面在发出前已编码成独立字节，裁剪不影响在途页；
	// 从当前最早帧开始的追帧因此还能先发出第一页，不会被本次 Tick 刚裁掉。
	m.retainWindow(frame.ID)
	return frame, errors.Join(errs...)
}

// retainWindow 是自动保留：切出 latest 后只留最近 retain 帧，hash 报告按步长跟随同一窗口。
// 游标落到窗口之前的追帧，在下一次 Tick 的 pumpCatchup 里以 ErrCatchupUnservable 结束并通知客户端。
func (m *Match) retainWindow(latest FrameID) {
	if m.retain == 0 || latest <= FrameID(m.retain) {
		return
	}
	keep := latest - FrameID(m.retain) + 1
	m.history.TrimBefore(keep)
	if keep-m.hashTrimmed >= hashTrimStride {
		m.TrimHashReports(keep)
		m.hashTrimmed = keep
	}
}

type broadcastReceiver struct {
	owner   PlayerID
	session nettransport.SessionID
}

// broadcastOrder returns seats (by ascending player) then spectators (by
// ascending session): deterministic iteration keeps error text and delivery
// order reproducible.
func (m *Match) broadcastOrder() []broadcastReceiver {
	if m.broadcastReceivers != nil {
		return m.broadcastReceivers
	}
	receivers := make([]broadcastReceiver, 0, len(m.sessions)+len(m.spectators))
	players := make([]PlayerID, 0, len(m.sessions))
	for player := range m.sessions {
		players = append(players, player)
	}
	slices.Sort(players)
	for _, player := range players {
		receivers = append(receivers, broadcastReceiver{owner: player, session: m.sessions[player]})
	}
	specs := make([]nettransport.SessionID, 0, len(m.spectators))
	for session := range m.spectators {
		specs = append(specs, session)
	}
	slices.Sort(specs)
	for _, session := range specs {
		receivers = append(receivers, broadcastReceiver{owner: ownerSpectator, session: session})
	}
	m.broadcastReceivers = receivers
	return receivers
}

// StartCatchup begins paging history to the player's attached session,
// starting at from (0 = frame 1; values beyond the next uncut
// frame are refused). A start already trimmed from history fails at once with
// ErrCatchupUnservable (*CatchupUnservableError); the session still gets the
// catch-up-failed notice over the reliable lane on the next Tick, so a client
// whose request arrived as a fire-and-forget command learns it too. Pages of CatchupBatchFrames go out over the reliable
// lane once per Tick until the cursor reaches the head, then the session
// switches back to live datagram broadcasts. Calling again while already
// catching up moves the cursor to min(current, from) — it never re-pages
// forward past history the client is still missing.
func (m *Match) StartCatchup(player PlayerID, from FrameID) error {
	session, attached := m.sessions[player]
	if !attached {
		return ErrPlayerDetached
	}
	return m.startCatchup(session, from)
}

// SpectatorCatchup begins paging history to an attached spectator session.
func (m *Match) SpectatorCatchup(session nettransport.SessionID, from FrameID) error {
	if _, ok := m.spectators[session]; !ok {
		return ErrPlayerDetached
	}
	return m.startCatchup(session, from)
}

func (m *Match) startCatchup(session nettransport.SessionID, from FrameID) error {
	if m.closed {
		return ErrMatchClosed
	}
	if m.reliable == nil {
		return ErrCatchupUnavailable
	}
	if from > m.sequencer.NextFrame() {
		return fmt.Errorf("%w: from %d, next %d", ErrCatchupFuture, from, m.sequencer.NextFrame())
	}
	// 帧号从 1 开始；0 表示从头追，必须和其他起点一样核对是否已被裁掉，
	// 否则 ReadRange 会从保留的第一帧开始，客户端拿到一段有洞的帧流。
	from = max(from, 1)
	existing, catching := m.catchups[session]
	if catching && existing.unservable != nil {
		return existing.unservable // 失败通知还在途：这次追帧已经结束
	}
	start := m.history.start()
	if catching {
		if from < existing.next {
			existing.next = from // 游标落到窗口之前时由 pumpCatchup 统一结束并通知
		}
	} else {
		m.catchups[session] = &catchupState{next: from}
	}
	if from < start {
		return &CatchupUnservableError{From: from, Start: start}
	}
	return nil
}

// CatchingUp reports whether the player's session is still paging history.
func (m *Match) CatchingUp(player PlayerID) bool {
	session, attached := m.sessions[player]
	if !attached {
		return false
	}
	_, catching := m.catchups[session]
	return catching
}

// catchupSessions lists catching-up sessions in ascending order, so error
// text and send order are reproducible.
func (m *Match) catchupSessions() []nettransport.SessionID {
	sessions := make([]nettransport.SessionID, 0, len(m.catchups))
	for session := range m.catchups {
		sessions = append(sessions, session)
	}
	slices.Sort(sessions)
	return sessions
}

// collectCatchups settles pages that finished since the previous tick. A
// session whose cursor now falls inside this tick's redundant packet (the
// frames from Latest-depth+1 on) is caught up: the broadcast that follows
// carries the rest. Without this a page that always outlives the wait
// would trail the head by one frame forever.
func (m *Match) collectCatchups() error {
	if len(m.catchups) == 0 {
		return nil
	}
	var errs []error
	for _, session := range m.catchupSessions() {
		state := m.catchups[session]
		if state.inflight == nil || !state.inflight.poll() {
			continue
		}
		if err := m.settleCatchup(session, state); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, catching := m.catchups[session]; catching && state.inflight == nil && state.next+FrameID(m.depth) > m.history.Latest() {
			delete(m.catchups, session)
		}
	}
	return errors.Join(errs...)
}

// settleCatchup applies a finished page's result: success moves the cursor
// (and ends the catch-up once it passes the head); failure keeps the cursor
// and counts toward CatchupMaxFailures.
func (m *Match) settleCatchup(session nettransport.SessionID, state *catchupState) error {
	send := state.inflight
	state.inflight = nil
	send.cancel()
	if state.unservable != nil {
		delete(m.catchups, session)
		if send.err != nil {
			return fmt.Errorf("catchup session %d: catch-up-failed notice not delivered (%w): %w", session, ErrCatchupUnservable, send.err)
		}
		return nil
	}
	if send.err != nil {
		state.failures++
		if state.failures >= m.catchupMax {
			delete(m.catchups, session)
			return fmt.Errorf("catchup session %d abandoned after %d failures: %w — the client must reconnect", session, state.failures, send.err)
		}
		return fmt.Errorf("catchup session %d: %w", session, send.err)
	}
	state.failures = 0
	metrics.IncCounter(MetricCatchupFrames, nil, int64(send.frames))
	if state.next >= send.start {
		state.next = send.end
	} // else StartCatchup rewound the cursor while the page was in flight: keep it
	if state.next > m.history.Latest() {
		delete(m.catchups, session)
	}
	return nil
}

// pumpCatchup launches the next page for every catching-up session that has
// none in flight, then waits up to CatchupSendWait for those pages. A page
// still in flight after the wait is collected by a later Tick; Tick never
// waits on it again.
func (m *Match) pumpCatchup(ctx context.Context) error {
	if len(m.catchups) == 0 {
		return nil
	}
	var errs []error
	var launched []nettransport.SessionID
	for _, session := range m.catchupSessions() {
		state := m.catchups[session]
		if state.inflight != nil {
			continue // still sending: only this session waits
		}
		// History trimmed past the cursor: the gap can never be served —
		// abandon loudly instead of paging a stream with a hole in it, and
		// tell the client over the reliable lane (the notice is the session's
		// last catch-up send; settleCatchup removes the state once it is done).
		if start := m.history.start(); state.next < start {
			state.unservable = &CatchupUnservableError{From: state.next, Start: start}
			metrics.IncCounter(MetricCatchupUnservable, nil, 1)
			errs = append(errs, fmt.Errorf("catchup session %d: %w", session, state.unservable))
			state.inflight = m.launchSend(ctx, session, EncodeCatchupUnservable(state.unservable.From, state.unservable.Start), catchupSend{start: state.next, end: state.next})
			launched = append(launched, session)
			continue
		}
		page := m.history.ReadRange(state.next, m.catchupBatch)
		if len(page) == 0 {
			delete(m.catchups, session) // caught up: live broadcasts take over
			continue
		}
		state.inflight = m.launchSend(ctx, session, EncodeBroadcast(page), catchupSend{start: state.next, end: page[len(page)-1].ID + 1, frames: len(page)})
		launched = append(launched, session)
	}
	if len(launched) == 0 {
		return errors.Join(errs...)
	}
	wait := time.NewTimer(m.sendWait)
	defer wait.Stop()
	expired := false
	for _, session := range launched {
		send := m.catchups[session].inflight
		if !expired {
			select {
			case send.err = <-send.done:
				send.finished = true
			case <-wait.C:
				expired = true
			case <-ctx.Done():
				expired = true
			}
		}
		if !send.poll() {
			continue
		}
		if err := m.settleCatchup(session, m.catchups[session]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// launchSend sends one catch-up payload (a page, or the catch-up-failed
// notice) on its own goroutine. The send keeps the caller's context values
// but not its cancellation (it may outlive the Tick), and is bounded by
// CatchupSendTimeout; dropping the catch-up cancels it.
func (m *Match) launchSend(ctx context.Context, session nettransport.SessionID, payload []byte, cursor catchupSend) *catchupSend {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.sendTimeout)
	send := &catchupSend{
		start:  cursor.start,
		end:    cursor.end,
		frames: cursor.frames,
		done:   make(chan error, 1),
		cancel: cancel,
	}
	reliable := m.reliable
	go func() {
		send.done <- reliable.SendReliable(sendCtx, session, payload)
	}()
	return send
}

// ReportHash records one player's keyframe simulation hash. Reports are
// validated (seat must exist, the frame must already be cut) so a client
// cannot inflate detector state with forged seats or future frames. Once a
// hash gains quorum agreeing reports the ruling runs; OnDesync fires
// whenever the outlier SET changes (new members counted in
// lockstep.desync.total), not merely when it grows in size.
func (m *Match) ReportHash(player PlayerID, frame FrameID, hash uint64) error {
	if m.closed {
		return ErrMatchClosed
	}
	if !m.sequencer.KnownPlayer(player) {
		metrics.IncCounter(MetricInputRejected, metrics.Labels{"reason": "hash_unknown_player"}, 1)
		return ErrPlayerUnknown
	}
	if frame == 0 || frame > m.history.Latest() {
		metrics.IncCounter(MetricInputRejected, metrics.Labels{"reason": "hash_invalid_frame"}, 1)
		return fmt.Errorf("%w: frame %d, latest %d", ErrHashFrameInvalid, frame, m.history.Latest())
	}
	verdict, ready := m.detector.Report(player, frame, hash)
	if !ready || len(verdict.Outliers) == 0 {
		return nil
	}
	surfaced := m.ruled[frame]
	changed := len(verdict.Outliers) != len(surfaced)
	newOutliers := 0
	for _, outlier := range verdict.Outliers {
		if _, seen := surfaced[outlier]; !seen {
			changed = true
			newOutliers++
		}
	}
	if !changed {
		return nil
	}
	next := make(map[PlayerID]struct{}, len(verdict.Outliers))
	for _, outlier := range verdict.Outliers {
		next[outlier] = struct{}{}
	}
	m.ruled[frame] = next
	if newOutliers > 0 {
		metrics.IncCounter(MetricDesync, nil, int64(newOutliers))
	}
	if m.onDesync != nil {
		m.onDesync(verdict)
	}
	return nil
}

// TrimHashReports drops hash-report state for frames before the given id
// (already judged and acted on). Trimmed frames are tombstoned in the
// detector: late reports cannot rebuild a forgeable report set for them.
func (m *Match) TrimHashReports(before FrameID) {
	m.detector.Trim(before)
	for frame := range m.ruled {
		if frame < before {
			delete(m.ruled, frame)
		}
	}
}

// TrimHistory drops stored frames before keep. With automatic retention
// (MatchConfig.HistoryRetainFrames) the match already trims every Tick; this
// is for hosts that trim further, or that disabled automatic retention.
// Catch-ups older than keep are abandoned on their next pump with
// ErrCatchupUnservable and the catch-up-failed notice.
func (m *Match) TrimHistory(keep FrameID) {
	m.history.TrimBefore(keep)
}

// History exposes the match's frame history (catch-up source and replay
// artifact). The returned structure is single-owner state: use it only from
// the same serial handler that drives the Match, and do not mutate frames.
func (m *Match) History() *History { return m.history }

// Close ends the match: all receivers and pending catch-ups are released
// and every subsequent operation fails with ErrMatchClosed. One Match serves
// exactly one match — do not reuse it.
func (m *Match) Close() {
	for _, state := range m.catchups {
		state.drop()
	}
	m.closed = true
	m.broadcastReceivers = nil
	m.sessions = make(map[PlayerID]nettransport.SessionID)
	m.spectators = make(map[nettransport.SessionID]struct{})
	m.sessionOwners = make(map[nettransport.SessionID]PlayerID)
	m.catchups = make(map[nettransport.SessionID]*catchupState)
	m.ruled = make(map[FrameID]map[PlayerID]struct{})
}
