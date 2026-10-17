package policy

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// SelfProfile 是自身视图订阅的视图：玩家实体的 packer 必须认识 Key "self"（通常给它全部同步字段）。
// 未知视图在捕获时被 packer 拒绝，不会被悄悄换成别的视图。
var SelfProfile = entity.SyncProfile{Key: "self"}

// selfStrandedPeriods 是“在线却没有打开的同步会话”持续多少个同步周期（Manager.Interval）之后算作滞留
// （Stranded）。正常的空档——登录到第一个政策阶段、丢会话到连接关闭通知送达、上一次打开的会话还在关闭——
// 都在一两个周期内结束；超过这个时长说明传输没有按约定关闭连接，或打开会话被持续拒绝。
const selfStrandedPeriods = 3

// SelfViewConfig 是 NewSelfView 的输入，三项都必填。
type SelfViewConfig struct {
	// Manager 是承载自身视图的 Manager。SelfView 拥有其中“玩家实体”这类 subject 的登记：同一 Manager 上的
	// 其他政策可以订阅它们，但不能自己 Register / Unregister 玩家实体。会话 ID 就是玩家 ID。
	Manager *entitysync.Manager
	// Subject 把玩家 ID 换成玩家实体（subject）的 ID，例如 entity.BuildEntityID(playerID, kind)。
	Subject func(playerID int64) (int64, error)
	// Lookup 返回内存里已发布的玩家实体的同步状态，没有就返回 nil。它在政策阶段调用：只读内存，
	// 不加载、不等待、不取实体锁（正式接线是 EntityManager.Get(id).Base().Sync()）。
	Lookup func(subjectID int64) *entity.SubjectSyncState
}

// SelfView 让每个在线玩家只看到自己的实体：一次登录开一个会话，用 Direct 把玩家绑定到自己（profile 固定为
// SelfProfile），全部连接断开时关会话、注销实体。它是“玩家看自己状态”的唯一生命周期实现，传输无关；
// 嵌入 TCP / 独立 Gate 的接线在 wiring/selfsync。
//
// 并发模型：Activate / Deactivate / SessionLost / EntityLoaded 只在 mu 下记事实并标记玩家待处理，
// 不调用 Manager、不等待，可以在连接协程、Gate 控制协程或业务池（快池）里调用。真正的会话、登记与绑定变化
// 全部在 Manager 的政策阶段（Flush 开头，flushGate 串行）按“此刻的净事实”一次做完：
//
//	在线（至少一条连接） → 确保会话已开（新连接到来则 HoldSession+ReadySession，从新 epoch 的全量开始）
//	                    → 实体在内存里就 Register，再 Direct.Bind（被拒绝就留在 waiting，下个阶段再看）
//	离线               → CloseSession（不欠 remove），再 Unregister 玩家实体
//
// 所以登录与断线、顶号的新旧连接、关闭与 Flush 之间不论怎样交错，政策阶段只看到连接集合的最终状态，
// 不会出现“旧连接的关闭关掉了新连接的视图”。实体不在内存里（首登新建、还没被请求加载）的在线玩家留在
// waiting 里，每个政策阶段只做一次内存查找，直到实体出现；加载有 EntityLoaded 通知加速，新建靠这次复查。
//
// 框架撤销与恢复沿用 Direct：卸载后重载不了退回 remove 时 Direct 保留绑定，实体再次加载并重新登记后由
// Direct 重新提交；会话被关闭 / 丢掉或玩家实体被注销（事务内新建被回滚）时 Direct 删除绑定并告诉
// SelfView（forgotten），会话仍开着的在线玩家在下一个政策阶段重新 Bind。
//
// Manager 因推送被拒丢掉会话（SessionLost）之后不在原连接上重开：传输的约定是“推送在活着的连接上被拒就关闭
// 该玩家的连接”（wiring/selfsync.PushPlayerFrame），所以丢会话之后等连接关闭（Deactivate）收尾；期间有新连接
// 激活就按正常激活开新会话（新 epoch、全量）。在线却超过 selfStrandedPeriods 个周期没有打开的会话的玩家数由
// Stranded 报告，正常为 0。
//
// 首个全量排在登录回包之后：会话在激活回调里只登记事实，真正的打开与第一次推送发生在之后的政策阶段；
// 激活回调由传输在鉴权 ACK 写出（嵌入 TCP）或 Bind→ACK→Activate（独立 Gate）之后调用，推送进同一条
// 先入先出的出站队列，所以客户端总是先读到 ACK。
type SelfView struct {
	manager *entitysync.Manager
	direct  *Direct
	subject func(int64) (int64, error)
	lookup  func(int64) *entity.SubjectSyncState
	unhook  func()

	mu        sync.Mutex // 只保护下面的事实；锁内不调用 Manager / Direct
	players   map[int64]*selfFacts
	bySubject map[int64]int64    // 有事实的玩家：subject ID → 玩家 ID，EntityLoaded 据此直接找到玩家
	pending   map[int64]struct{} // 自上个政策阶段以来事实有变化的玩家
	closed    bool

	// stageMu 串行化政策阶段（apply）、forgotten 回调与 Close；bindings / waiting 只在它下面读写。
	// 政策阶段在 Manager 的 Flush 里执行，不会与另一次 apply 并发；锁只为 Close 与测试的直接调用。
	stageMu  sync.Mutex
	bindings map[int64]*selfBinding
	waiting  map[int64]struct{} // 在线但视图还没就绪（实体不在内存、打开 / 登记 / 绑定被拒）的玩家，每个阶段复查
	// unopened 记在线、却没有打开的会话的玩家从何时起处于这个状态（丢会话后等连接关闭、打开被拒）；
	// 每个政策阶段末尾据此算出 stranded。
	unopened map[int64]time.Time
	stranded atomic.Int64 // 最近一个政策阶段算出的滞留玩家数，见 Stranded
}

// selfFacts 是一个玩家此刻的事实，由回调写、政策阶段读走。
type selfFacts struct {
	subjectID   int64
	connections map[string]struct{}
	reset       bool // 有连接新激活：它的客户端需要从全量开始
	lost        bool // Manager 丢掉了会话（推送被拒）
	loaded      bool // 实体刚从权威加载发布
}

// selfBinding 是政策阶段维护的、一个玩家在 Manager 里的实际状态。
type selfBinding struct {
	subjectID int64
	open      bool                     // 本 SelfView 打开的会话仍开着
	lost      bool                     // Manager 丢掉了会话：等连接关闭，新激活之前不再打开
	state     *entity.SubjectSyncState // 最近一次登记的状态；nil 表示还没登记过
	bound     bool                     // Direct 绑定在位（被框架删除时 forgotten 置回 false）
	failure   error                    // 上一次拒绝的原因，只在原因变化时记日志
}

// NewSelfView 创建自身视图并在 Manager 上登记政策阶段。调用方在 Manager 关闭后调用 Close。
func NewSelfView(config SelfViewConfig) (*SelfView, error) {
	if config.Manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	if config.Subject == nil || config.Lookup == nil {
		return nil, errors.New("policy: self view needs Subject and Lookup")
	}
	view := &SelfView{
		manager: config.Manager, subject: config.Subject, lookup: config.Lookup,
		players: make(map[int64]*selfFacts), bySubject: make(map[int64]int64), pending: make(map[int64]struct{}),
		bindings: make(map[int64]*selfBinding), waiting: make(map[int64]struct{}), unopened: make(map[int64]time.Time),
	}
	direct, err := newDirect(config.Manager, nil, view.forgotten)
	if err != nil {
		return nil, err
	}
	view.direct = direct
	view.unhook = config.Manager.RegisterPolicy(entitysync.PolicyHooks{Apply: view.apply, Pending: view.hasPending})
	return view, nil
}

// Activate 记录玩家的一条连接已激活（鉴权 ACK 已写出）。connection 在该玩家的连接里唯一，
// 例如嵌入 TCP 的连接 nonce、独立 Gate 的 receiver ID。不阻塞，可在任何协程调用。
func (v *SelfView) Activate(playerID int64, connection string) {
	if v == nil || playerID == 0 {
		return
	}
	subjectID, err := v.subject(playerID)
	if err != nil {
		slog.Warn("self view: player has no subject id; its own view stays off", "player_id", playerID, "err", err)
		return
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	facts := v.players[playerID]
	if facts == nil {
		facts = &selfFacts{subjectID: subjectID, connections: make(map[string]struct{})}
		v.players[playerID] = facts
		v.bySubject[subjectID] = playerID
	}
	facts.connections[connection] = struct{}{}
	facts.reset = true
	v.pending[playerID] = struct{}{}
	v.mu.Unlock()
	v.manager.WakeSync()
}

// Deactivate 记录这条连接已关闭。不认识的连接（从未激活、已关闭过）被忽略。不阻塞。
func (v *SelfView) Deactivate(playerID int64, connection string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	facts := v.players[playerID]
	if v.closed || facts == nil {
		return
	}
	if _, known := facts.connections[connection]; !known {
		return
	}
	delete(facts.connections, connection)
	v.pending[playerID] = struct{}{}
}

// SessionLost 接 ManagerConfig.SessionLost：Manager 因推送被拒丢掉了玩家会话。SelfView 不在原连接上重开：
// 传输拒收时已经关闭了这名玩家的连接（或玩家本来就没有连接），关闭通知随后到达，由 Deactivate 收尾；
// 在那之前有新连接激活，就按正常激活开新会话。它在 Flush 的推送阶段被调用，这里只记事实。
//
// “连接活着、同步会话已死”不是由政策自己恢复的状态：跟不上的连接由传输断开（慢消费者规则只在传输一处），
// 恢复走客户端重连与新激活这一条正常路径。
func (v *SelfView) SessionLost(session entitysync.SessionID, _ error) {
	if v == nil {
		return
	}
	playerID := int64(session)
	v.mu.Lock()
	defer v.mu.Unlock()
	if facts := v.players[playerID]; facts != nil && !v.closed {
		facts.lost = true
		v.pending[playerID] = struct{}{}
	}
}

// EntityLoaded 记录一个实体刚从权威加载发布（DataEngine 的 OnEntityLoaded，在业务池上调用）。
// 是在线玩家的实体时，下一个政策阶段登记新状态；其他实体忽略。不阻塞。
func (v *SelfView) EntityLoaded(subjectID int64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	playerID, known := v.bySubject[subjectID]
	facts := v.players[playerID]
	woke := known && facts != nil && len(facts.connections) > 0
	if woke {
		facts.loaded = true
		v.pending[playerID] = struct{}{}
	}
	v.mu.Unlock()
	if woke {
		v.manager.WakeSync()
	}
}

// Online 报告当前有连接的玩家数。
func (v *SelfView) Online() int {
	if v == nil {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	count := 0
	for _, facts := range v.players {
		if len(facts.connections) > 0 {
			count++
		}
	}
	return count
}

// Stranded 报告最近一个政策阶段时在线（至少一条连接）、却已超过 selfStrandedPeriods 个同步周期没有打开的
// 同步会话的玩家数。正常为 0：丢会话之后连接应被传输关闭，打开被拒（上一次的会话还在关闭）应很快成功。
// 持续不为 0 说明传输没有关闭拒收的连接，或会话打开被持续拒绝（例如 Manager 的会话数上限）。
func (v *SelfView) Stranded() int {
	if v == nil {
		return 0
	}
	return int(v.stranded.Load())
}

// Close 撤下政策阶段并停止接收事实；幂等。在 Manager 关闭之后调用：会话、订阅与登记随 Manager 一起释放。
func (v *SelfView) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	already := v.closed
	v.closed = true
	clear(v.players)
	clear(v.bySubject)
	clear(v.pending)
	v.mu.Unlock()
	if already {
		return
	}
	v.unhook()
	v.stageMu.Lock()
	clear(v.bindings)
	clear(v.waiting)
	clear(v.unopened)
	v.stranded.Store(0)
	v.stageMu.Unlock()
}

func (v *SelfView) hasPending() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.pending) > 0
}

// forgotten 是 Direct 删除绑定（会话被关闭 / 丢掉、玩家实体被注销）时的通知，在政策阶段的释放交付里调用。
func (v *SelfView) forgotten(session entitysync.SessionID, _ int64) {
	v.stageMu.Lock()
	if binding := v.bindings[int64(session)]; binding != nil {
		binding.bound = false
	}
	v.stageMu.Unlock()
	v.mu.Lock()
	if _, known := v.players[int64(session)]; known {
		v.pending[int64(session)] = struct{}{}
	}
	v.mu.Unlock()
}

// selfStep 是政策阶段对一个玩家读走的事实。
type selfStep struct {
	player                      int64
	online, reset, lost, loaded bool
}

// apply 是政策阶段：取走待处理玩家（加上仍在 waiting 的）的事实，逐个对齐 Manager 里的状态。
func (v *SelfView) apply() error {
	v.stageMu.Lock()
	defer v.stageMu.Unlock()
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	candidates := make(map[int64]struct{}, len(v.pending)+len(v.waiting))
	maps.Copy(candidates, v.pending)
	maps.Copy(candidates, v.waiting)
	clear(v.pending)
	steps := make([]selfStep, 0, len(candidates))
	for _, playerID := range slices.Sorted(maps.Keys(candidates)) {
		step := selfStep{player: playerID}
		if facts := v.players[playerID]; facts != nil {
			step.online, step.reset, step.lost, step.loaded = len(facts.connections) > 0, facts.reset, facts.lost, facts.loaded
			facts.reset, facts.lost, facts.loaded = false, false, false
			if !step.online {
				delete(v.players, playerID)
				delete(v.bySubject, facts.subjectID)
			}
		}
		steps = append(steps, step)
	}
	v.mu.Unlock()
	now := time.Now()
	for _, step := range steps {
		v.align(step, now)
	}
	v.countStranded(now)
	return nil
}

// countStranded 在政策阶段末尾数出滞留玩家（见 Stranded）。只在 stageMu 下调用。
func (v *SelfView) countStranded(now time.Time) {
	limit := time.Duration(selfStrandedPeriods) * v.manager.Interval()
	stranded := 0
	for _, since := range v.unopened {
		if now.Sub(since) > limit {
			stranded++
		}
	}
	v.stranded.Store(int64(stranded))
}

// align 把一个玩家在 Manager 里的状态对齐到它此刻的事实。只在 stageMu 下、政策阶段调用；
// 所有 Manager / Direct 调用都不等待，也不会回调 SelfView（释放与重新提交在下个政策阶段交付）。
func (v *SelfView) align(step selfStep, now time.Time) {
	session := entitysync.SessionID(step.player)
	binding := v.bindings[step.player]
	defer v.trackUnopened(step, now)
	if !step.online {
		delete(v.waiting, step.player)
		if binding == nil {
			return
		}
		delete(v.bindings, step.player)
		// 先关会话：订阅随之移除、不欠 remove；subject 没有订阅者，Unregister 立即忘掉它。
		if binding.open {
			v.manager.CloseSession(session)
		}
		if binding.state != nil {
			if err := v.manager.Unregister(binding.subjectID); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
				slog.Debug("self view: player subject not retired", "player_id", step.player, "subject_id", binding.subjectID, "err", err)
			}
		}
		return
	}
	if binding == nil {
		subjectID, err := v.subject(step.player)
		if err != nil {
			slog.Warn("self view: player has no subject id", "player_id", step.player, "err", err)
			delete(v.waiting, step.player)
			return
		}
		binding = &selfBinding{subjectID: subjectID}
		v.bindings[step.player] = binding
	}
	if step.lost {
		// Manager 已删掉会话与订阅；Direct 的绑定在本阶段之前的释放交付里已删（forgotten）。
		binding.open, binding.bound, binding.lost = false, false, true
	}
	if step.reset {
		// 新连接激活（可能与丢会话在同一阶段到达）：它的客户端从全量开始，按正常激活打开新会话。
		binding.lost = false
	}
	if binding.lost {
		// 传输拒收时已关闭这名玩家的连接：等 Deactivate 收尾（关闭通知随后就到），不在原连接上重开。
		delete(v.waiting, step.player)
		slog.Debug("self view: session lost; waiting for the player's connections to close", "player_id", step.player, "subject_id", binding.subjectID)
		return
	}
	if !v.ensureSession(step, binding, session) || !v.ensureSubject(step, binding) {
		v.waiting[step.player] = struct{}{}
		return
	}
	if !binding.bound {
		if err := v.direct.Bind(step.player, binding.subjectID, SelfProfile); err != nil {
			if errors.Is(err, entitysync.ErrSubjectNotRegistered) {
				// 登记之后 subject 被撤回（事务内新建被回滚）：下个阶段按内存里那时的状态重新登记。
				binding.state = nil
			}
			v.refused(step.player, binding, "bind", err)
			v.waiting[step.player] = struct{}{}
			return
		}
		binding.bound = true
	}
	binding.failure = nil
	delete(v.waiting, step.player)
}

// trackUnopened 记下玩家是否处于“在线、却没有打开的会话”及其开始时刻（align 收尾时调用）。
func (v *SelfView) trackUnopened(step selfStep, now time.Time) {
	binding := v.bindings[step.player]
	if !step.online || binding == nil || binding.open {
		delete(v.unopened, step.player)
		return
	}
	if _, already := v.unopened[step.player]; !already {
		v.unopened[step.player] = now
	}
}

// ensureSession 让玩家有一个打开的会话。新连接到来而会话已开时 HoldSession + ReadySession：新 epoch、
// 所有订阅回到全量，正在用的连接也收到全量（它的接收端按更大的 epoch 接受）。返回 false 表示本阶段没开成。
func (v *SelfView) ensureSession(step selfStep, binding *selfBinding, session entitysync.SessionID) bool {
	if binding.open {
		if step.reset && !step.lost {
			if err := v.manager.HoldSession(session); err == nil {
				_ = v.manager.ReadySession(session)
			} else if errors.Is(err, entitysync.ErrSessionUnknown) {
				// 会话在本阶段之前被丢掉、丢失通知还没处理到：按新会话打开。
				binding.open, binding.bound = false, false
			}
		}
		if binding.open {
			return true
		}
	}
	err := v.manager.OpenSession(session)
	if err != nil {
		v.refused(step.player, binding, "open session", err)
		return false
	}
	binding.open, binding.bound = true, false
	return true
}

// ensureSubject 让玩家实体以内存里当前的状态登记。返回 false 表示实体不在内存或登记被拒，留到下个阶段。
func (v *SelfView) ensureSubject(step selfStep, binding *selfBinding) bool {
	state := v.lookup(binding.subjectID)
	if state == nil || !state.Enabled() {
		return false
	}
	if state == binding.state && !step.loaded {
		return true
	}
	// 同一状态重复登记是空操作；旧状态已关闭（卸载 / 驱逐后重新加载）时 Manager 接到新状态上并强制全量；
	// subject 已被撤回时这是新的登记，Direct 的重新提交随之交付。
	switch err := v.manager.Register(state); {
	case err == nil, errors.Is(err, entitysync.ErrSubjectRegistered):
		binding.state = state
		return true
	default:
		v.refused(step.player, binding, "register", err)
		return false
	}
}

// refused 记录一次拒绝；同一原因只记一次，避免 waiting 里的玩家每个 tick 刷日志。
func (v *SelfView) refused(player int64, binding *selfBinding, step string, err error) {
	if binding.failure != nil && errors.Is(err, binding.failure) {
		return
	}
	binding.failure = err
	level := slog.LevelWarn
	if errors.Is(err, entitysync.ErrSubjectRetiring) || errors.Is(err, entitysync.ErrSubjectNotRegistered) || entitysync.SessionOpenRetryable(err) {
		level = slog.LevelDebug
	}
	slog.Log(context.Background(), level, "self view: "+step+" refused; retrying at the next sync tick", "player_id", player, "subject_id", binding.subjectID, "err", err)
}
