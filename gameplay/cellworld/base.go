package cellworld

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// ErrBaseConfig 表示 Base 配置不合法。
var ErrBaseConfig = errors.New("cellworld: invalid base config")

// ErrUnknownPlayer 表示 Base 上没有这个玩家（未 Enter 或已 Leave）。
var ErrUnknownPlayer = errors.New("cellworld: unknown player")

// BaseConfig 配置 BaseApp 侧。
type BaseConfig struct {
	// World、Grid 与 Cell 一致；SID 是本 BaseApp（Player 所在 game 进程）的 sid，不能与 Cell 的 sid 重复。
	World     string
	SID       int32
	Grid      *Grid
	Transport Transport
	// WatchPeriod 是看门狗周期；零值 100ms。
	WatchPeriod time.Duration
	// SilenceTimeout：玩家超过它没收到视野就按最后位置重新进入（Cell 崩溃的恢复路径）；零值 1s。
	SilenceTimeout time.Duration
	// LostGrace：收到交接放弃（Lost）后等这么久，仍没有更新的 owner 才重新进入；零值 500ms。
	LostGrace time.Duration
	// OnView / OnAttackResult 在传输回调 goroutine 上、Base 锁外调用，必须快速返回。
	OnView         func(View)
	OnAttackResult func(AttackResult)
}

// View 是某玩家某一拍的视野：由当前 owner Cell 算出，含邻区 Ghost。Base 只接受 epoch 不回退的视野。
type View struct {
	Player    int64
	Cell      int32 // 发出视野的 Cell sid（玩家 Real 所在格）
	Epoch     uint64
	Tick      uint64
	Self      EntityState
	Entities  []ViewEntity
	Truncated bool
	At        time.Time // Base 收到的系统时间
	SentAt    time.Time // Cell 发出的系统时间
}

// AttackResult 是一次攻击的结论（见 DamageOutcome）。
type AttackResult struct {
	Hit       HitID
	Target    int64
	Outcome   DamageOutcome
	Reason    RejectReason
	Duplicate bool
	HP        int32
}

// BaseStats 是 Base 的累计计数。
type BaseStats struct {
	ViewsAccepted, ViewsStale, OwnerChanges, Reenters, InputsResent, EnterRefusals, Lost, DecodeErrors uint64
}

type baseCounters struct {
	viewsAccepted, viewsStale, ownerChanges, reenters, inputsResent, enterRefusals, lost, decodeErrors atomic.Uint64
}

// player 是 Base 对一个玩家的记录：当前 owner（sid + epoch，只前进）、最新移动意图、最后看到的位置与 HP。
type player struct {
	id          int64
	cell        int32
	epoch       uint64
	pos         spatial.Point
	hp          int32
	intentSeq   uint64
	vx, vy      int64
	attackSeq   uint64
	lastViewAt  time.Time
	lastEnterAt time.Time
	enterHint   int32
	lostEpoch   uint64
	lostAt      time.Time
	view        View
	hasView     bool
}

// Base 是 BaseApp（Player 所在 game 进程，静态 sid）里的空间代理入口：记住每个玩家当前所在 Cell，把输入、
// 攻击转发给它，接收它算出的视野。Player 的持久状态不在这里（仍归 DataEngine），这里只有非持久的空间信息。
//
// 并发：players 由 mu 保护；传输回调与业务调用（Enter / Move / Attack）在不同 goroutine 上。发布与回调都在锁外。
type Base struct {
	cfg     BaseConfig
	mu      sync.Mutex
	players map[int64]*player
	ready   chan struct{}
	running atomic.Bool
	stats   baseCounters
}

// NewBase 校验配置并构造 Base；Run 之前不收消息，但 Enter 等调用已可发出。
func NewBase(cfg BaseConfig) (*Base, error) {
	if cfg.WatchPeriod == 0 {
		cfg.WatchPeriod = 100 * time.Millisecond
	}
	if cfg.SilenceTimeout == 0 {
		cfg.SilenceTimeout = time.Second
	}
	if cfg.LostGrace == 0 {
		cfg.LostGrace = 500 * time.Millisecond
	}
	switch {
	case !validWorld(cfg.World):
		return nil, fmt.Errorf("%w: world %q", ErrBaseConfig, cfg.World)
	case cfg.Grid == nil || cfg.Transport == nil || cfg.SID == 0:
		return nil, fmt.Errorf("%w: grid, transport and sid are required", ErrBaseConfig)
	case cfg.WatchPeriod < 0 || cfg.SilenceTimeout <= cfg.WatchPeriod || cfg.LostGrace <= 0:
		return nil, fmt.Errorf("%w: silence timeout must exceed watch period", ErrBaseConfig)
	}
	if _, isCell := cfg.Grid.CellOf(cfg.SID); isCell {
		return nil, fmt.Errorf("%w: sid %d is a cell sid", ErrBaseConfig, cfg.SID)
	}
	return &Base{cfg: cfg, players: make(map[int64]*player), ready: make(chan struct{})}, nil
}

// Ready 在 Run 完成订阅后关闭。
func (b *Base) Ready() <-chan struct{} { return b.ready }

// Run 订阅收件主题并运行看门狗，直到 ctx 结束。一个 Base 只能 Run 一次。
func (b *Base) Run(ctx context.Context) error {
	if !b.running.CompareAndSwap(false, true) {
		return errors.New("cellworld: base already ran")
	}
	unsubscribe, err := b.cfg.Transport.Subscribe(Subject(b.cfg.World, b.cfg.SID), b.receive)
	if err != nil {
		return fmt.Errorf("cellworld: subscribe base %d: %w", b.cfg.SID, err)
	}
	defer func() { _ = unsubscribe() }()
	close(b.ready)
	ticker := time.NewTicker(b.cfg.WatchPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			b.watch(now)
		}
	}
}

type outgoing struct {
	sid int32
	msg *wireMsg
}

func (b *Base) publish(out []outgoing) {
	for _, o := range out {
		o.msg.From = b.cfg.SID
		data, err := encodeMsg(o.msg)
		if err != nil {
			continue
		}
		_ = b.cfg.Transport.Publish(Subject(b.cfg.World, o.sid), data) // 丢失由看门狗与重发处理
	}
}

func (b *Base) enterMsg(p *player, sid int32, hint uint64, now time.Time) outgoing {
	p.lastEnterAt = now
	return outgoing{sid: sid, msg: &wireMsg{Kind: kindEnter, Enter: &enterReq{
		State: EntityState{ID: p.id, Kind: KindPlayer, X: p.pos.X, Y: p.pos.Y, HP: p.hp}, EpochHint: hint,
	}}}
}

func (b *Base) inputMsg(p *player) outgoing {
	return outgoing{sid: p.cell, msg: &wireMsg{Kind: kindInput, Input: &inputReq{ID: p.id, Seq: p.intentSeq, VX: p.vx, VY: p.vy}}}
}

// Enter 让玩家在 pos 进入世界（发给 pos 所在格）。重复 Enter 同一玩家返回错误。
func (b *Base) Enter(id int64, pos spatial.Point, hp int32) error {
	cell, ok := b.cfg.Grid.CellAt(pos)
	if id == 0 || !ok {
		return fmt.Errorf("cellworld: enter %d at %v: outside world", id, pos)
	}
	b.mu.Lock()
	if _, exists := b.players[id]; exists {
		b.mu.Unlock()
		return fmt.Errorf("cellworld: player %d already entered", id)
	}
	p := &player{id: id, pos: pos, hp: hp}
	b.players[id] = p
	out := b.enterMsg(p, b.cfg.Grid.SID(cell), 1, time.Now())
	b.mu.Unlock()
	b.publish([]outgoing{out})
	return nil
}

// Move 设定玩家移动意图（速度，单位 / 秒）。发给当前 owner；owner 未知或交接中丢失时，由视野里的 LastInput
// 落后触发重发。
func (b *Base) Move(id int64, vx, vy int64) error {
	b.mu.Lock()
	p, ok := b.players[id]
	if !ok {
		b.mu.Unlock()
		return ErrUnknownPlayer
	}
	p.intentSeq++
	p.vx, p.vy = vx, vy
	var out []outgoing
	if p.cell != 0 {
		out = append(out, b.inputMsg(p))
	}
	b.mu.Unlock()
	b.publish(out)
	return nil
}

// Attack 让玩家攻击 target，结论经 OnAttackResult 回来。玩家的 owner 未知时返回错误。
func (b *Base) Attack(id, target int64, amount int32) (HitID, error) {
	b.mu.Lock()
	p, ok := b.players[id]
	if !ok || p.cell == 0 {
		b.mu.Unlock()
		return HitID{}, ErrUnknownPlayer
	}
	p.attackSeq++
	hit := HitID{Attacker: id, Seq: p.attackSeq}
	out := outgoing{sid: p.cell, msg: &wireMsg{Kind: kindAttack, Attack: &attackReq{Hit: hit, Target: target, Amount: amount}}}
	b.mu.Unlock()
	b.publish([]outgoing{out})
	return hit, nil
}

// Leave 让玩家离开（尽力而为：消息丢失时 owner 上的 Real 会留到该 Cell 退出；原型简化）。
func (b *Base) Leave(id int64) error {
	b.mu.Lock()
	p, ok := b.players[id]
	if !ok {
		b.mu.Unlock()
		return ErrUnknownPlayer
	}
	delete(b.players, id)
	var out []outgoing
	if p.cell != 0 {
		out = append(out, outgoing{sid: p.cell, msg: &wireMsg{Kind: kindLeave, Leave: &leaveReq{ID: id}}})
	}
	b.mu.Unlock()
	b.publish(out)
	return nil
}

// LastView 返回玩家最近一次被接受的视野。
func (b *Base) LastView(id int64) (View, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.players[id]
	if !ok || !p.hasView {
		return View{}, false
	}
	return p.view, true
}

// Owner 返回玩家当前所知的 owner Cell sid 与 epoch。
func (b *Base) Owner(id int64) (int32, uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.players[id]
	if !ok || p.cell == 0 {
		return 0, 0, false
	}
	return p.cell, p.epoch, true
}

// Stats 返回计数快照。
func (b *Base) Stats() BaseStats {
	s := &b.stats
	return BaseStats{
		ViewsAccepted: s.viewsAccepted.Load(), ViewsStale: s.viewsStale.Load(), OwnerChanges: s.ownerChanges.Load(),
		Reenters: s.reenters.Load(), InputsResent: s.inputsResent.Load(), EnterRefusals: s.enterRefusals.Load(),
		Lost: s.lost.Load(), DecodeErrors: s.decodeErrors.Load(),
	}
}

// switchOwner 在看到更大的 epoch 时更新 owner；owner 已应用的输入落后于最新意图就重发。
func (b *Base) switchOwner(p *player, sid int32, epoch, lastInput uint64, now time.Time, out []outgoing) []outgoing {
	if epoch <= p.epoch {
		return out
	}
	p.cell, p.epoch = sid, epoch
	p.enterHint = 0
	p.lastViewAt = now // 新 owner 的第一份视野最多一拍后到；静默看门狗从这里重新计时
	b.stats.ownerChanges.Add(1)
	if p.intentSeq > lastInput {
		b.stats.inputsResent.Add(1)
		out = append(out, b.inputMsg(p))
	}
	return out
}

func (b *Base) receive(data []byte) {
	m, err := decodeMsg(data)
	if err != nil {
		b.stats.decodeErrors.Add(1)
		return
	}
	now := time.Now()
	var out []outgoing
	var view *View
	var result *AttackResult
	b.mu.Lock()
	switch m.Kind {
	case kindOwner:
		if p, ok := b.players[m.Owner.ID]; ok {
			out = b.switchOwner(p, m.From, m.Owner.Epoch, m.Owner.LastInput, now, out)
		}
	case kindView:
		v := m.View
		p, ok := b.players[v.Observer]
		switch {
		case !ok || v.Epoch < p.epoch || v.Epoch == p.epoch && m.From != p.cell:
			b.stats.viewsStale.Add(1)
		default:
			out = b.switchOwner(p, m.From, v.Epoch, v.LastInput, now, out)
			if p.intentSeq > v.LastInput && len(out) == 0 {
				b.stats.inputsResent.Add(1)
				out = append(out, b.inputMsg(p))
			}
			p.pos, p.hp = v.Self.Pos(), v.Self.HP
			p.lastViewAt = now
			p.view = View{Player: v.Observer, Cell: m.From, Epoch: v.Epoch, Tick: v.Tick, Self: v.Self,
				Entities: v.Entities, Truncated: v.Truncated, At: now, SentAt: time.Unix(0, m.SentAt)}
			p.hasView = true
			b.stats.viewsAccepted.Add(1)
			cp := p.view
			view = &cp
		}
	case kindAttackResult:
		r := m.AttackResult
		result = &AttackResult{Hit: r.Hit, Target: r.Target, Outcome: r.Outcome, Reason: r.Reason, Duplicate: r.Duplicate, HP: r.HP}
	case kindLost:
		st := m.Lost.State
		if p, ok := b.players[st.ID]; ok && st.Ver.Epoch > p.epoch && st.Ver.Epoch > p.lostEpoch {
			b.stats.lost.Add(1)
			p.lostEpoch, p.lostAt = st.Ver.Epoch, now
			p.pos, p.hp = st.Pos(), st.HP
		}
	case kindEnterRefused:
		if p, ok := b.players[m.EnterRefused.ID]; ok {
			b.stats.enterRefusals.Add(1)
			p.enterHint = m.EnterRefused.Hint
		}
	default:
		b.stats.decodeErrors.Add(1)
	}
	b.mu.Unlock()
	b.publish(out)
	if view != nil && b.cfg.OnView != nil {
		b.cfg.OnView(*view)
	}
	if result != nil && b.cfg.OnAttackResult != nil {
		b.cfg.OnAttackResult(*result)
	}
}

// watch 是看门狗：初次 Enter 丢失则重发；交接放弃（Lost）后宽限期内没有更新的 owner 就按最后位置重进；
// 玩家视野静默超过 SilenceTimeout（owner Cell 崩溃或不可达）也重进。重进的 epoch 提示比已知的大，
// 目标 Cell 已有 Real 时幂等返回，有存活 Ghost 时拒绝并提示 owner（见 Cell.onEnter）。
func (b *Base) watch(now time.Time) {
	var out []outgoing
	b.mu.Lock()
	for _, p := range b.players {
		target := p.enterHint
		if target == 0 {
			if cell, ok := b.cfg.Grid.CellAt(p.pos); ok {
				target = b.cfg.Grid.SID(cell)
			}
		}
		sinceEnter := now.Sub(p.lastEnterAt)
		switch {
		case target == 0:
		case p.enterHint != 0 && sinceEnter >= b.cfg.WatchPeriod:
			out = append(out, b.enterMsg(p, target, max(p.epoch, p.lostEpoch)+1, now))
			p.enterHint = 0
		case p.lostEpoch > p.epoch && now.Sub(p.lostAt) >= b.cfg.LostGrace && sinceEnter >= b.cfg.LostGrace:
			b.stats.reenters.Add(1)
			out = append(out, b.enterMsg(p, target, p.lostEpoch+1, now))
		case p.epoch == 0 && sinceEnter >= b.cfg.SilenceTimeout:
			out = append(out, b.enterMsg(p, target, 1, now))
		case p.epoch > 0 && now.Sub(p.lastViewAt) >= b.cfg.SilenceTimeout && sinceEnter >= b.cfg.SilenceTimeout:
			b.stats.reenters.Add(1)
			out = append(out, b.enterMsg(p, target, max(p.epoch, p.lostEpoch)+1, now))
		}
	}
	b.mu.Unlock()
	b.publish(out)
}
