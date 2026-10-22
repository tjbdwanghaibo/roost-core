// Package timedomain 是场景 / 房间级的时间域：副本 2 倍速、出牌慢放、技能时停这类“一个场景里的时间流速”。
// 全局业务时间不变速，变速只在场景级（对照 ssr 的场景时间）；实现说明见 impl/10-time §2F。
//
// 域时间是业务时间的分段线性映射：
//
//	D(B) = AnchorDomain + (B − AnchorBusiness) × Rate
//
// B 是业务时间（本条消息冻结的 fctx.Now）。改倍率时以生效时刻为新锚点（新 AnchorDomain 是旧映射在
// 生效时刻的值），所以 D 连续、不跳变；倍率在 [0, 100]，0 是时停，因此 D 单调不减。
//
// 三方各读各的，不跨实体加锁：
//
//   - 参数的权威在所属实体（场景本身是实体）的 DAO 字段里（Fields）：SetRate 在场景自己的
//     Nest handler 里写，随事务提交或回滚；字段标 sync 时由 Entity Sync 下发给客户端，客户端按同一公式外推。
//     持久与否由业务选：随进程结束的副本用 nopersist,sync（缺省推荐）；要持久的场景用持久字段，重载后域时间
//     按存下的锚点与倍率接着走（停服期间同样按倍率前进，时停的保持时停）。
//   - 成员实体（场景里的玩家、怪物，在各自的锁里）读发布的快照：事务提交后（nest.AfterCommit）原子发布，不读场景
//     的 DAO。一条消息里第一次读取时冻结（fctx.Frozen），之后同一条消息读到同一个值。
//   - 心跳登记挂上时间域（heartbeat.WithTimeDomain）后 Beat.Now / Elapsed 是域时间：调度仍按真实时间，时停时
//     Elapsed 为 0，Elapsed 上限乘倍率。actionflow 的 BeatRunner 经心跳选项接上，动作 tick 的 Now / Elapsed 随之。
//
// 成员看到的是发布的快照，比场景的提交晚一个 AfterCommit：生效时刻之后、发布之前开始的成员消息仍按旧倍率算。
// 为了让成员读到的域时间单调，Domain 记下已经交出去的最大域时间，读数取它与公式值的较大者；改倍率时新锚点也
// 不低于它。代价是改倍率前后成员与客户端的域时间可能差这段发布延迟乘倍率（微秒到毫秒级），随后一致。
//
// entitytimer 不接时间域：持久定时器的离线补触发只对业务时间成立，域内的定时走心跳。
package timedomain

import (
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

const (
	// MinRate 是倍率下限：0 是时停。
	MinRate = 0.0
	// MaxRate 是倍率上限。
	MaxRate = 100.0
)

// ErrRateOutOfRange 表示倍率不在域允许的范围里（Config 声明，缺省 [MinRate, MaxRate]），或不是有限数。
var ErrRateOutOfRange = errors.New("timedomain: rate out of range")

// State 是时间域的参数，也是存进所属实体 DAO、同步给客户端的内容：
//
//	D(B) = AnchorDomainNano + (B − AnchorBusinessNano) × Rate
//
// 时刻都是 UnixNano。零值（AnchorBusinessNano 为 0）表示从未改过倍率：恒等映射，域时间就是业务时间。
type State struct {
	AnchorBusinessNano int64
	AnchorDomainNano   int64
	Rate               float64
}

// Initialized 报告是否改过倍率（零值是恒等映射）。
func (s State) Initialized() bool { return s.AnchorBusinessNano != 0 }

// EffectiveRate 是倍率；零值 State 为 1。
func (s State) EffectiveRate() float64 {
	if !s.Initialized() {
		return 1
	}
	return s.Rate
}

// At 返回业务时间 b 对应的域时间。早于锚点的 b 不往回外推，停在锚点：生效时刻之前开始、发布之后才读的消息
// 不会读到比锚点小的值。
func (s State) At(b time.Time) time.Time {
	if !s.Initialized() {
		return b
	}
	delta := b.UnixNano() - s.AnchorBusinessNano
	if delta <= 0 {
		return time.Unix(0, s.AnchorDomainNano)
	}
	return time.Unix(0, s.AnchorDomainNano+scale(delta, s.Rate))
}

// scale 返回 delta × rate（纳秒，向下取整）。倍率 1 时精确；float64 乘法对非负倍率单调，所以 At 单调不减。
func scale(delta int64, rate float64) int64 {
	switch rate {
	case 1:
		return delta
	case 0:
		return 0
	}
	return int64(math.Floor(float64(delta) * rate))
}

// Fields 是时间域在所属实体 DAO 上的字段（业务把生成 DAO 的字段适配过来）。只在所属实体的锁内调用；写入走生成
// 的 setter，随事务回滚。字段标 sync 时随 Entity Sync 下发给客户端。
type Fields interface {
	TimeDomain() State
	SetTimeDomain(State)
}

// Config 是域的声明。
type Config struct {
	// MinRate / MaxRate 收窄这个域允许的倍率；都为 0 时取 [MinRate, MaxRate]（包常量）。必须落在包常量的范围里。
	MinRate, MaxRate float64
}

// Domain 是一个场景实例的时间域。New 在所属实体的初始化阶段（加载 / 新建）构造；SetRate 只在所属实体的锁内调用；
// Now / Rate / Read / State 任意 goroutine 都可以调用。实例随所属实体：实体卸载重载后是新的 Domain，成员要用新的。
type Domain struct {
	fields   Fields
	min, max float64

	// seq 只在所属实体锁内读写：每次 SetRate 加一，发布按它排序（AfterCommit 可能乱序到达）。
	seq uint64

	published atomic.Pointer[publication]
	// high 是已经交给读者的最大域时间（UnixNano）：读数取它与公式值的较大者，域时间对读者单调。
	high atomic.Int64
}

// publication 是一次发布：已提交的参数与它的序号。
type publication struct {
	state State
	seq   uint64
}

// Reading 是一次读取：域时间与当时的倍率。
type Reading struct {
	Now  time.Time
	Rate float64
}

// New 构造域，并发布 Fields 里已有的参数（加载时是存下的，新建时是零值：恒等映射）。存下的倍率不在范围里时返回
// ErrRateOutOfRange。
func New(fields Fields, config Config) (*Domain, error) {
	if fields == nil {
		return nil, errors.New("timedomain: fields are required")
	}
	low, high := config.MinRate, config.MaxRate
	if low == 0 && high == 0 {
		low, high = MinRate, MaxRate
	}
	if !finite(low) || !finite(high) || low < MinRate || high > MaxRate || low > high {
		return nil, fmt.Errorf("%w: domain range [%v, %v] must lie within [%v, %v]", ErrRateOutOfRange, low, high, MinRate, MaxRate)
	}
	d := &Domain{fields: fields, min: low, max: high}
	state := fields.TimeDomain()
	if state.Initialized() {
		if err := d.check(state.Rate); err != nil {
			return nil, fmt.Errorf("stored time domain: %w", err)
		}
	}
	d.published.Store(&publication{state: state})
	return d, nil
}

// Now 返回本条消息的域时间：第一次读取时用本条消息冻结的业务时间（fctx.Now）与当时发布的参数算出并冻结，同一条
// 消息里再读是同一个值。不在请求上下文里时现算。
func (d *Domain) Now() time.Time { return d.Read().Now }

// Rate 返回本条消息看到的倍率（与 Now 同一次冻结）。
func (d *Domain) Rate() float64 { return d.Read().Rate }

// Read 返回本条消息的域时间与倍率（一次冻结）。
func (d *Domain) Read() Reading {
	return fctx.Frozen(d, func() any { return d.read(fctx.Now()) }).(Reading)
}

// read 用已发布的参数换算业务时间 b，并推进已交出的最大域时间。
func (d *Domain) read(b time.Time) Reading {
	state := d.published.Load().state
	nanos := state.At(b).UnixNano()
	for {
		seen := d.high.Load()
		if nanos <= seen {
			nanos = seen
			break
		}
		if d.high.CompareAndSwap(seen, nanos) {
			break
		}
	}
	return Reading{Now: time.Unix(0, nanos), Rate: state.EffectiveRate()}
}

// State 返回最近一次发布（已提交）的参数。
func (d *Domain) State() State { return d.published.Load().state }

// SetRate 把倍率改为 rate，从本条消息冻结的业务时间起生效。只在所属实体的锁内调用（场景自己的 Nest handler，或
// 加载 / 新建的初始化阶段）。新锚点是生效时刻的域时间（按所属实体当前的参数算，含本事务里之前的修改；不低于
// 已经交给读者的域时间），所以域时间连续。写进 Fields（随事务回滚、随同步下发），提交后发布给成员；不在事务里时
// 立即发布。倍率不在范围里时返回 ErrRateOutOfRange，什么都不改。
func (d *Domain) SetRate(rate float64) error {
	if err := d.check(rate); err != nil {
		return err
	}
	at := fctx.Now()
	anchor := d.fields.TimeDomain().At(at).UnixNano()
	anchor = max(anchor, d.high.Load())
	next := State{AnchorBusinessNano: at.UnixNano(), AnchorDomainNano: anchor, Rate: rate}
	d.fields.SetTimeDomain(next)
	d.seq++
	seq := d.seq
	publish := func() { d.publish(next, seq) }
	if !nest.AfterCommit(publish) {
		publish()
	}
	return nil
}

// publish 发布一次已提交的参数；序号不大于已发布的是晚到的旧发布，丢弃。
func (d *Domain) publish(state State, seq uint64) {
	next := &publication{state: state, seq: seq}
	for {
		current := d.published.Load()
		if current.seq >= seq {
			return
		}
		if d.published.CompareAndSwap(current, next) {
			return
		}
	}
}

func (d *Domain) check(rate float64) error {
	if !finite(rate) || rate < d.min || rate > d.max {
		return fmt.Errorf("%w: %v is outside [%v, %v]", ErrRateOutOfRange, rate, d.min, d.max)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
