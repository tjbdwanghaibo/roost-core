package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/health"
)

// 单实例锁：同一服务类型 + sid 同一时刻只有一个进程在跑 Mod（docs/feature/APP-SINGLETON-LOCK-2026-10-05.md）。
//
// 只处理同一 sid 崩溃重启时短暂出现两个进程这一种场景：旧进程卡住（SIGSTOP、长 GC、调试器）、
// 新进程已被拉起。App 在 NewRegistry 之后、第一个 Mod Init 之前获取锁（别人持有就等待），
// 持有期间由一个 goroutine 按固定节拍续期，失锁即经 RuntimeFailure fail-stop；全部 Mod 停完
// 才释放。模块不感知锁、不各自检查：Nest 由 OnFail 回调统一围栏，活性查询走 SingletonLiveness。
//
// core 只定义后端接口与状态机，不 import Redis 驱动；Redis 实现在 kit/redis（kitredis.SingletonStore）。

// SingletonStore 是单实例锁的后端：两个原子操作，语义与 redis.CompareAndSet / CompareAndDelete 相同。
// expected 为 nil 表示“键必须不存在”；applied=false 时 current 是键里现在的值（不存在为 nil）；
// ttl 为 0 表示不过期。
//
// 它同时是 App 的协调存储：业务时间高水位（<key_prefix>:business_time，不过期，只用 CompareAndSet）
// 也存在这里，见 business_time.go。
//
// 实现必须遵守：
//   - 每个方法都遵守 ctx 的截止时间：到期即返回错误，不能继续阻塞。App 给每次 CAS 一个截到 validUntil
//     的单次超时，“续期失败时 Lost 不晚于 validUntil 判定”这一时间界依赖它。
//   - 内部重试（例如 Redis Cluster 下驱动按 MaxRedirects 处理 MOVED / ASK 与网络错误）可以有，但必须受
//     同一个 ctx 限定。重复执行是安全的：续期 CAS(v, v) 幂等；获取 CAS(nil, v) 第一次已落地时重试得到
//     applied=false、current=v，App 据此认领；按值 CompareAndDelete 只删自己的值。
//   - 并发安全，而且续期不被 Live 查询拖住：二者可能同时调用，Live 并发占满连接等资源时，CAS 仍要能在
//     自己的超时内发出（kitredis 为此用两个独立客户端）。
type SingletonStore interface {
	CompareAndSet(ctx context.Context, key string, expected, next []byte, ttl time.Duration) (applied bool, current []byte, err error)
	CompareAndDelete(ctx context.Context, key string, expected []byte) (applied bool, err error)
	// Get 只读地返回每个键当前的值（不存在为 nil），顺序与 keys 相同；供 Live 查询使用。
	// 实现不得依赖单条多键命令（Redis Cluster 下跨槽会 CROSSSLOT）。
	Get(ctx context.Context, keys []string) ([][]byte, error)
	Close() error
}

// SingletonOpener 在配置读完之后、任何 Mod Init 之前调用，自己建连接（不依赖任何 Mod）。
// singleton.enabled=false 而 time.logic_offset 非 0（非生产）时，App 也用它只为业务时间高水位打开一个连接。
type SingletonOpener func(cfg *viper.Viper) (SingletonStore, error)

// SingletonLiveness 回答“同一服务类型下，这些 sid 中哪些有进程持有单实例锁”。只读，不要求本进程
// 持有锁。“活”是指进程持有锁：从拿锁（任何 Mod Init 之前）到全部 Mod 停完、Release 为止；
// 崩溃的进程最多再算 ttl，卡住的进程键过期后不算。
//
// 契约：停机中的进程仍算活（维护者决定 C5，2026-10-06）。从收到停机信号、Service.Shutdown、各 Mod
// Stop，一直到 Release 删掉键为止，键都在、值不变，Live 照常把它算进去；停机不完整（不 Release）时
// 一直算到键在 ttl 后过期。没有“停机中”这种中间值——活性只有一个事实来源，就是锁本身。代价落在
// 用 Live 决定“该等谁”的调用方：activity 开窗时的 expected 集合会包含恰在停机的服，它不会再
// NotifyPhase，这个窗口只能等到宽限期（activity.grace_window）结束才完成；只影响停机那几秒内开的窗口。
// 需要排除停机中进程的调用方自己判断，不要依赖 Live 区分。
type SingletonLiveness interface {
	Live(ctx context.Context, serverType string, sids []int32) ([]int32, error)
}

// ModSingleton 是 SingletonLiveness 在 Registry 里的能力名；singleton.enabled=false 时不登记。
const ModSingleton ModName = "singleton"

// SingletonLiveMaxSIDs 是一次 Live 查询最多的 sid 数。
const SingletonLiveMaxSIDs = 200

var (
	// ErrSingletonHeld：启动等待到 singleton.startup_wait 仍被别人持有。对方一直在续期，说明两个
	// 健康进程配了同一个服务类型 + sid，属于部署错误，不能抢锁。
	ErrSingletonHeld = errors.New("singleton is held by a live process")
	// ErrSingletonStoreUnavailable：启动等待期间最后一次获取是报错或超时（与“被别人持有”区分）。
	ErrSingletonStoreUnavailable = errors.New("singleton store unavailable")
	// ErrSingletonLost：持有期间失锁，经 RuntimeFailure fail-stop；包裹 ErrSingletonNotHeld 或续期错误。
	ErrSingletonLost = errors.New("singleton lock lost")
	// ErrSingletonNotHeld：续期 CAS 没生效——键已过期或已是别人的值。
	ErrSingletonNotHeld = errors.New("singleton key no longer holds this process's value")
	// ErrSingletonOpenerMissing：singleton.enabled=true 但 bootstrap 没有调用 App.Singleton（fail-closed）。
	ErrSingletonOpenerMissing = errors.New("singleton.enabled=true but no SingletonStore opener is installed; call App.Singleton in the bootstrap")
)

// 默认时间参数（方案 §3.3，维护者已同意 D2）。startup_wait 缺省为 2 × ttl。
const (
	defaultSingletonTTL           = 15 * time.Second
	defaultSingletonRenewInterval = 3 * time.Second
	defaultSingletonGuard         = 5 * time.Second
	// singletonReleaseBudget 是停机时留给 Release 的时长：Mod 停机用的截止时间提前这么多，
	// 启动失败路径的 Release 也用这个超时。
	singletonReleaseBudget = 3 * time.Second
)

// Singleton 安装单实例锁的后端。是否启用由每个服务的 singleton.enabled 决定：
// 启用而没有安装 opener 时启动失败；装了 opener 但未启用时不建连接，行为与不装相同。
func (a *App) Singleton(open SingletonOpener) *App {
	a.singletonOpener = open
	return a
}

// singletonSettings 是 singleton.* 配置读出的值；未设置的时长取默认值。
type singletonSettings struct {
	enabled       bool
	keyPrefix     string
	ttl           time.Duration
	renewInterval time.Duration
	guard         time.Duration
	startupWait   time.Duration
	// readErrs 是读取时发现的类型错误（RR-20261005-NC-190）：`enabled: on` 不是布尔值、时长没写
	// 单位。validate 先报它们，不管 enabled 读成了什么——宽松读取会把 `on` 读成 false，锁静默关闭。
	readErrs []error
}

func readSingletonSettings(cfg *viper.Viper) singletonSettings {
	var s singletonSettings
	duration := func(key string, fallback time.Duration) time.Duration {
		if !cfg.IsSet(key) {
			return fallback
		}
		value, err := ConfigDuration(cfg, key)
		if err != nil {
			s.readErrs = append(s.readErrs, err)
		}
		return value
	}
	enabled, err := ConfigBool(cfg, "singleton.enabled")
	if err != nil {
		s.readErrs = append(s.readErrs, err)
	}
	s.enabled = enabled
	s.keyPrefix = cfg.GetString("singleton.key_prefix")
	s.ttl = duration("singleton.ttl", defaultSingletonTTL)
	s.renewInterval = duration("singleton.renew_interval", defaultSingletonRenewInterval)
	s.guard = duration("singleton.guard", defaultSingletonGuard)
	s.startupWait = duration("singleton.startup_wait", 2*s.ttl)
	return s
}

// validate 检查启用时的配置（ValidateServiceConfig 调用）。三条时间关系的理由见方案 §3.3：
//  1. renew_interval ≤ guard：续期一直 Unknown 时，进入 [validUntil−guard, validUntil) 之后的第一拍
//     在 validUntil 之前发起；配合 cas 把单次超时截到 validUntil，Lost 不晚于 validUntil 判定。
//  2. 2 × renew_interval ≤ ttl − guard：一次续期超时之后下一次仍在窗口内发起，一次抖动不判 Lost。
//  3. startup_wait ≥ ttl + 2 × renew_interval：卡住的旧持有者最后一次续期可能在新进程启动前后才被
//     处理，键最晚约 ttl 后过期，新进程的重试间隔又是一个 renew_interval。
func (s singletonSettings) validate() []error {
	if len(s.readErrs) > 0 {
		return s.readErrs
	}
	if !s.enabled {
		return nil
	}
	var errs []error
	if strings.TrimSpace(s.keyPrefix) == "" {
		errs = append(errs, errors.New("config: singleton.key_prefix is required when singleton.enabled=true"))
	} else if strings.ContainsFunc(s.keyPrefix, unicode.IsSpace) {
		errs = append(errs, fmt.Errorf("config: singleton.key_prefix %q must not contain whitespace", s.keyPrefix))
	}
	positive := true
	for _, field := range []struct {
		key   string
		value time.Duration
	}{
		{"singleton.ttl", s.ttl},
		{"singleton.renew_interval", s.renewInterval},
		{"singleton.guard", s.guard},
		{"singleton.startup_wait", s.startupWait},
	} {
		if field.value <= 0 {
			errs = append(errs, fmt.Errorf("config: %s must be positive", field.key))
			positive = false
		}
	}
	if !positive {
		return errs
	}
	if s.renewInterval > s.guard {
		errs = append(errs, fmt.Errorf("config: singleton.renew_interval (%s) must not exceed singleton.guard (%s): an unknown renewal must reach a verdict before the key can expire", s.renewInterval, s.guard))
	}
	if 2*s.renewInterval > s.ttl-s.guard {
		errs = append(errs, fmt.Errorf("config: 2 x singleton.renew_interval (%s) must not exceed singleton.ttl - singleton.guard (%s - %s): one failed renewal must not lose the lock", 2*s.renewInterval, s.ttl, s.guard))
	}
	if s.startupWait < s.ttl+2*s.renewInterval {
		errs = append(errs, fmt.Errorf("config: singleton.startup_wait (%s) must be at least singleton.ttl + 2 x singleton.renew_interval (%s): a stalled holder's key may outlive a shorter wait", s.startupWait, s.ttl+2*s.renewInterval))
	}
	return errs
}

func singletonKey(prefix, serverType string, sid int32) string {
	return prefix + ":" + serverType + ":" + strconv.FormatInt(int64(sid), 10)
}

// newSingletonValue 生成本次进程启动的持有者值 token|hostname|pid|started_unix_ms。token 是
// crypto/rand 的 16 位十六进制串，同 sid 重启一定是另一个持有者；后三段只给运维看，CAS 比较整个值。
func newSingletonValue() ([]byte, error) {
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("app: singleton token: %w", err)
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Appendf(nil, "%s|%s|%d|%d", hex.EncodeToString(token[:]), host, os.Getpid(), time.Now().UnixMilli()), nil
}

// singletonClock 是锁使用的单调时钟（测试注入可控时钟）。窗口、节拍和启动等待都只读它。
type singletonClock interface {
	Now() time.Time
	// Timer 返回一个在 d 之后收到时间的通道；stop 释放它（可在触发后调用）。
	Timer(d time.Duration) (c <-chan time.Time, stop func())
}

type realSingletonClock struct{}

func (realSingletonClock) Now() time.Time { return time.Now() }

func (realSingletonClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	timer := time.NewTimer(d)
	return timer.C, func() { timer.Stop() }
}

type singletonState int

const (
	// singletonWaiting：还没拿到锁（启动等待中，或启动失败）。
	singletonWaiting singletonState = iota
	// singletonHeld：最近一次 CAS 及时 Applied，validUntil 有效。
	singletonHeld
	// singletonUnknown：最近一次续期报错 / 超时 / 回复迟到，但还没到窗口末尾。
	singletonUnknown
	// singletonLost：吸收态。续期 NotHeld，或 Unknown 且已到窗口末尾；之后不再续期、不 Release。
	singletonLost
)

func (s singletonState) String() string {
	switch s {
	case singletonHeld:
		return "held"
	case singletonUnknown:
		return "unknown"
	case singletonLost:
		return "lost"
	default:
		return "waiting"
	}
}

// singletonStatus 是锁状态的快照（健康检查与测试读取）。
type singletonStatus struct {
	state      singletonState
	validUntil time.Time // 最近一次及时 Applied 的 asked + ttl
	holder     []byte    // Lost 时键里的值（别人的值；不存在为 nil）
	err        error     // 最近一次 Unknown 或 Lost 的原因
}

// singletonLock 是一个进程的单实例锁。
//
// 单写者：获取、续期及其结论都在同一个 goroutine 里顺序执行（启动阶段是 run 的 goroutine，
// 之后是续期 goroutine，二者不重叠），不存在旧回复作用到新状态上的交错，也就不需要世代号。
// mu 只保护 status 字段，供健康检查等读者并发读取；持 mu 时不做 I/O。
type singletonLock struct {
	settings singletonSettings
	store    SingletonStore
	clock    singletonClock
	key      string
	value    []byte
	failure  *RuntimeFailure

	mu     sync.Mutex
	status singletonStatus

	// 续期 goroutine 的取消与退出信号；startRenewal 之前为 nil。
	stopRenewal context.CancelFunc
	renewalDone chan struct{}
}

func (l *singletonLock) snapshot() singletonStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// casReply 是一次 CAS 调用的结果与时间点。asked 在发出请求之前读取：Redis 的 TTL 从它处理请求
// 的时刻起算（驱动内部重试时是最后落地那次），不早于 asked，所以以 asked 起算的本地窗口不会晚于
// 键过期（RR-20261004-14 的教训）。
type casReply struct {
	applied bool
	current []byte
	err     error
	asked   time.Time
	replied time.Time
}

// cas 发起一次 CompareAndSet(key, expected, value, ttl)。单次超时等于 renew_interval，持有期间
// 还不越过 validUntil：固定节拍下，前一拍的报错可能很快回来（结论仍在窗口内），下一拍于是在
// [validUntil−guard, validUntil) 里才发起，若再给它一整个 renew_interval，超时结论最晚落在
// validUntil−guard+2×renew_interval，可能晚于键过期。截到 validUntil，Unknown→Lost 就一定不晚于
// validUntil 判定。asked 已不早于 validUntil（卡住后恢复）时不截：那次续期 Applied 仍证明键一直是
// 自己的（方案 §3.4），报错则按窗口末尾判 Lost。启动获取时 validUntil 为零值，不受影响。
func (l *singletonLock) cas(ctx context.Context, expected []byte) casReply {
	asked := l.clock.Now()
	timeout := l.settings.renewInterval
	if validUntil := l.snapshot().validUntil; asked.Before(validUntil) {
		timeout = min(timeout, validUntil.Sub(asked))
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	applied, current, err := l.store.CompareAndSet(callCtx, l.key, expected, l.value, l.settings.ttl)
	cancel()
	return casReply{applied: applied, current: current, err: err, asked: asked, replied: l.clock.Now()}
}

// timely 判断一次 Applied 是否作数：回复必须在 asked + ttl − guard 之前到达。整个进程被 SIGSTOP
// 时在途的续期可能已被处理，SIGCONT 后读到的是停之前的 Applied，那时键可能早已是别人的；
// 迟到的 Applied 当作 Unknown，并立即再续一次（方案 §3.4）。
func (l *singletonLock) timely(reply casReply) bool {
	return reply.replied.Before(reply.asked.Add(l.settings.ttl - l.settings.guard))
}

func (l *singletonLock) hold(asked time.Time) {
	l.mu.Lock()
	l.status = singletonStatus{state: singletonHeld, validUntil: asked.Add(l.settings.ttl)}
	l.mu.Unlock()
}

func (l *singletonLock) markUnknown(cause error) {
	l.mu.Lock()
	l.status.state = singletonUnknown
	l.status.err = cause
	l.mu.Unlock()
}

// errSingletonLateReply 标记一次迟到的 Applied（当作 Unknown，不延长窗口）。
var errSingletonLateReply = errors.New("singleton: applied reply arrived after the validity window")

// errSingletonWaitInterrupted：启动等待期间收到退出信号。
var errSingletonWaitInterrupted = errors.New("singleton: interrupted while waiting for the lock")

// acquire 在任何 Mod Init 之前获取锁（方案 §3.4 启动获取）：
//  1. Acquire Applied（及时）→ Held。
//  2. Applied=false 且 current 是自己的值：上一次 Acquire 已落地、只是回复丢了。这个值只有本进程
//     设过、而且还没启动任何 Mod，直接认领，随后立即 Renew 一次，用那次的 asked 起算窗口。
//     迟到的 Applied 同样走这一步。
//  3. current 是别人的值：持有者变化时记 Info 日志，renew_interval 后重试。
//  4. 报错或超时（Unknown）：同样 renew_interval 后重试，计入 startup_wait。
//  5. 超过 startup_wait：返回 ErrSingletonHeld（最后一次是报错则 ErrSingletonStoreUnavailable），不抢锁。
//
// signals 非空时等待期间监听退出信号（测试注入的 signalSource）；生产上等待期间不注册信号，
// SIGTERM 按默认处置直接终止进程——此时什么都没启动，也没持有锁。
func (l *singletonLock) acquire(signals <-chan os.Signal) error {
	deadline := l.clock.Now().Add(l.settings.startupWait)
	var holder []byte
	for {
		reply := l.cas(context.Background(), nil)
		lastErr := reply.err
		switch {
		case reply.err != nil:
		case reply.applied && l.timely(reply):
			l.hold(reply.asked)
			return nil
		case reply.applied || bytes.Equal(reply.current, l.value):
			confirmed, current, err := l.confirmOwnValue()
			if confirmed {
				return nil
			}
			lastErr = err
			if current != nil {
				holder = l.noteHolder(holder, current)
			}
		default:
			holder = l.noteHolder(holder, reply.current)
		}

		if !l.clock.Now().Before(deadline) {
			if lastErr != nil {
				return fmt.Errorf("app: singleton %s: %w: %w", l.key, ErrSingletonStoreUnavailable, lastErr)
			}
			return fmt.Errorf("app: singleton %s: %w (holder %s)", l.key, ErrSingletonHeld, holder)
		}
		if !l.waitUntil(reply.asked.Add(l.settings.renewInterval), nil, signals) {
			return errSingletonWaitInterrupted
		}
	}
}

// confirmOwnValue 用一次 Renew 确认键里是自己的值，并以这次的 asked 起算窗口。
// 返回 current 非空表示键已是别人的值。
func (l *singletonLock) confirmOwnValue() (bool, []byte, error) {
	reply := l.cas(context.Background(), l.value)
	switch {
	case reply.err != nil:
		return false, nil, reply.err
	case reply.applied && l.timely(reply):
		l.hold(reply.asked)
		return true, nil, nil
	case reply.applied:
		return false, nil, errSingletonLateReply
	default:
		return false, reply.current, nil
	}
}

func (l *singletonLock) noteHolder(previous, current []byte) []byte {
	if current != nil && !bytes.Equal(previous, current) {
		slog.Info("singleton: waiting for the current holder to release or expire", "key", l.key, "holder", string(current))
	}
	if current == nil {
		return previous
	}
	return current
}

// waitUntil 等到 at（可控时钟）。ctx 取消或收到信号时返回 false。
func (l *singletonLock) waitUntil(at time.Time, done <-chan struct{}, signals <-chan os.Signal) bool {
	fire, stop := l.clock.Timer(at.Sub(l.clock.Now()))
	defer stop()
	select {
	case <-fire:
		return true
	case <-done:
		return false
	case sig := <-signals:
		slog.Info("singleton: received signal while waiting for the lock, exiting", "key", l.key, "signal", sig)
		return false
	}
}

// startRenewal 在拿到锁之后启动续期 goroutine，节拍从上一次 Applied 的 asked 起算。
func (l *singletonLock) startRenewal() {
	ctx, cancel := context.WithCancel(context.Background())
	l.stopRenewal = cancel
	l.renewalDone = make(chan struct{})
	asked := l.snapshot().validUntil.Add(-l.settings.ttl)
	go l.renewLoop(ctx, asked.Add(l.settings.renewInterval))
}

// renewLoop 是持有期间唯一的写者。调度按固定节拍：节拍点是上一次 Applied 的 asked + k × renew_interval，
// 单次超时等于 renew_interval，超时或报错就在下一个节拍点重试、不额外再睡一个间隔。最后一个结论落在
// validUntil−guard 之前的节拍之后，下一拍在 validUntil−guard+renew_interval ≤ validUntil 之前发起，
// 它的单次超时又截到 validUntil（见 cas），所以 Lost 一定不晚于 validUntil 判定。
//
// 结论：
//   - 及时 Applied → Held，validUntil = asked + ttl。
//   - 迟到 Applied → 不作数（Unknown），立即再续一次，以那次的结论为准。
//   - Applied=false → NotHeld → Lost。
//   - 报错 / 超时 → Unknown；已到 validUntil − guard 则 Lost。只看时间不够：卡住后恢复的进程窗口按时间
//     已过，但键可能仍是自己的，所以“窗口末尾”只在续期失败时才判 Lost，恢复后那次续期 Applied 就照常继续。
//
// 停止（ctx 取消）时在途调用的结果不做判断，直接退出。
func (l *singletonLock) renewLoop(ctx context.Context, next time.Time) {
	defer close(l.renewalDone)
	for {
		if !l.waitUntil(next, ctx.Done(), nil) {
			return
		}
		reply := l.cas(ctx, l.value)
		if ctx.Err() != nil {
			return
		}
		switch {
		case reply.err == nil && reply.applied && l.timely(reply):
			l.hold(reply.asked)
			next = reply.asked.Add(l.settings.renewInterval)
		case reply.err == nil && reply.applied:
			l.markUnknown(errSingletonLateReply)
			slog.Warn("singleton: renewal reply arrived after the window; renewing again now", "key", l.key,
				"asked", reply.asked, "replied", reply.replied)
			next = reply.replied
		case reply.err == nil:
			l.lose(fmt.Errorf("%w (current holder %q)", ErrSingletonNotHeld, reply.current), reply.current)
			return
		default:
			validUntil := l.snapshot().validUntil
			if !reply.replied.Before(validUntil.Add(-l.settings.guard)) {
				l.lose(fmt.Errorf("renewal outcome unknown at the end of the validity window: %w", reply.err), nil)
				return
			}
			l.markUnknown(reply.err)
			slog.Warn("singleton: renewal outcome unknown; retrying on the next beat", "key", l.key,
				"err", reply.err, "valid_until_in", validUntil.Sub(reply.replied))
			next = next.Add(l.settings.renewInterval)
		}
	}
}

// lose 进入吸收态 Lost 并 fail-stop。RuntimeFailure 先执行 OnFail 回调（Nest 围栏）再唤醒 run。
func (l *singletonLock) lose(cause error, holder []byte) {
	l.mu.Lock()
	l.status.state = singletonLost
	l.status.holder = holder
	l.status.err = cause
	l.mu.Unlock()
	slog.Error("singleton lock lost; fail-stop", "key", l.key, "holder", string(holder), "err", cause)
	l.failure.Fail(fmt.Errorf("%w: %w", ErrSingletonLost, cause))
}

// finish 是 run 的统一收尾：先停续期并等它退出，再决定是否 Release，最后在没有组件还会用它时关闭 store。
//
// mayRelease 只在“全部 Mod 都停完”的路径上为 true（方案 §3.5）：Service.Shutdown 超时、服务专属或
// 共享 Mod 停机不完整时还有组件可能在用依赖，不能让新进程提前拿锁，只停续期、等键自然过期。
// Lost 时键已不是自己的，也不 Release。releaseDeadline 是停机路径 shutdownCtx 的截止时间，Release
// 用 min(剩余, singletonReleaseBudget)，剩余为零就跳过；零值表示启动失败路径，固定 singletonReleaseBudget。
//
// store 同时承载 Live 能力：停机不完整时还在跑的组件（与 run 不等它们、保留它们依赖的理由相同）
// 可能继续调用 Live，这时不关闭 store，留到进程退出，否则它们会读到 client closed。没拿到锁
// （还没启动任何 Mod）或全部 Mod 已停完才关闭。
func (l *singletonLock) finish(mayRelease bool, releaseDeadline time.Time) {
	if l.stopRenewal != nil {
		l.stopRenewal()
		<-l.renewalDone
	}
	state := l.snapshot().state
	switch {
	case state == singletonWaiting:
	case state == singletonLost:
		slog.Warn("singleton: lock was lost; not releasing", "key", l.key)
	case !mayRelease:
		slog.Warn("singleton: shutdown incomplete; leaving the key to expire", "key", l.key, "ttl", l.settings.ttl)
	default:
		l.release(releaseDeadline)
	}
	if state != singletonWaiting && !mayRelease {
		slog.Warn("singleton: shutdown incomplete; keeping the store open for components still running", "key", l.key)
		return
	}
	if err := l.store.Close(); err != nil {
		slog.Warn("singleton: close store failed", "key", l.key, "err", err)
	}
}

func (l *singletonLock) release(deadline time.Time) {
	budget := singletonReleaseBudget
	if !deadline.IsZero() {
		budget = min(time.Until(deadline), singletonReleaseBudget)
		if budget <= 0 {
			slog.Warn("singleton: no shutdown time left to release; leaving the key to expire", "key", l.key, "ttl", l.settings.ttl)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	applied, err := l.store.CompareAndDelete(ctx, l.key, l.value)
	switch {
	case err != nil:
		slog.Warn("singleton: release failed; the key will expire", "key", l.key, "err", err, "ttl", l.settings.ttl)
	case !applied:
		slog.Warn("singleton: key no longer held at release", "key", l.key)
	default:
		slog.Info("singleton: released", "key", l.key)
	}
}

// checkHealth 是 singleton 健康检查（只进 /readyz）：Held 为 OK，窗口内的 Unknown 为 Degraded，
// Lost 与尚未持有为 Fail。Degraded 算就绪（维护者决定 D1）：续期结果未知的窗口里 /readyz 仍返回
// 200，响应体 degraded_dependencies 列出本项与剩余窗口；失锁 / 未持有的 Fail 才让它 503。
func (l *singletonLock) checkHealth(context.Context) health.Result {
	status := l.snapshot()
	switch status.state {
	case singletonHeld:
		return health.Result{Status: health.StatusOK, Message: "held " + string(l.value)}
	case singletonUnknown:
		return health.Result{Status: health.StatusDegraded,
			Message: fmt.Sprintf("renewal outcome unknown; window ends in %s", status.validUntil.Sub(l.clock.Now())), Err: status.err}
	case singletonLost:
		return health.Result{Status: health.StatusFail, Message: fmt.Sprintf("lost; current holder %q", status.holder), Err: status.err}
	default:
		return health.Result{Status: health.StatusFail, Message: "not acquired"}
	}
}

// singletonLiveness 是 ModSingleton 能力的实现：用本进程的 key_prefix 拼键、逐键读，值非空即活。
type singletonLiveness struct {
	store  SingletonStore
	prefix string
}

func (s singletonLiveness) Live(ctx context.Context, serverType string, sids []int32) ([]int32, error) {
	if strings.TrimSpace(serverType) == "" {
		return nil, errors.New("app: singleton live: server type is required")
	}
	if len(sids) > SingletonLiveMaxSIDs {
		return nil, fmt.Errorf("app: singleton live: %d sids exceeds the limit of %d", len(sids), SingletonLiveMaxSIDs)
	}
	if len(sids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(sids))
	for i, sid := range sids {
		keys[i] = singletonKey(s.prefix, serverType, sid)
	}
	values, err := s.store.Get(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("app: singleton live: %w", err)
	}
	if len(values) != len(keys) {
		return nil, fmt.Errorf("app: singleton live: store returned %d values for %d keys", len(values), len(keys))
	}
	live := make([]int32, 0, len(sids))
	for i, value := range values {
		if len(value) > 0 {
			live = append(live, sids[i])
		}
	}
	return live, nil
}

// openSingleton 在 NewRegistry 之后、任何 Mod 之前打开后端、登记 Live 能力与健康检查；
// 未启用时返回 nil（不调用 opener）。获取由调用方随后执行。
func (a *App) openSingleton(serverType ServiceName) (*singletonLock, error) {
	settings := readSingletonSettings(a.cfg)
	if !settings.enabled {
		return nil, nil
	}
	if a.singletonOpener == nil {
		return nil, fmt.Errorf("app: %w", ErrSingletonOpenerMissing)
	}
	failure, ok := Lookup[*RuntimeFailure](a.registry, ModRuntimeFailure)
	if !ok || failure == nil {
		return nil, fmt.Errorf("app: capability %q not found", ModRuntimeFailure)
	}
	healthRegistry, ok := Lookup[*health.Registry](a.registry, ModHealth)
	if !ok || healthRegistry == nil {
		return nil, fmt.Errorf("app: capability %q not found", ModHealth)
	}
	value, err := newSingletonValue()
	if err != nil {
		return nil, err
	}
	store, err := a.singletonOpener(a.cfg)
	if err != nil {
		return nil, fmt.Errorf("app: singleton: open store: %w", err)
	}
	if store == nil {
		return nil, errors.New("app: singleton: opener returned a nil store")
	}
	clock := a.singletonClock
	if clock == nil {
		clock = realSingletonClock{}
	}
	lock := &singletonLock{
		settings: settings,
		store:    store,
		clock:    clock,
		key:      singletonKey(settings.keyPrefix, string(serverType), a.cfg.GetInt32("sid")),
		value:    value,
		failure:  failure,
	}
	if err := a.registry.Register(ModSingleton, SingletonLiveness(singletonLiveness{store: store, prefix: settings.keyPrefix})); err != nil {
		_ = store.Close()
		return nil, err
	}
	healthRegistry.Register("singleton", health.CheckerFunc(lock.checkHealth))
	a.singleton = lock
	return lock, nil
}
