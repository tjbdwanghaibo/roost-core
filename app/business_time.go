package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// 业务时间只许前进（docs/feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md）。
//
// 业务时钟 = 真实时间 + time.logic_offset（D-L3）。偏移只在启动时生效，生产强制为 0；测试环境可以
// 前拨，但同一套部署的业务时间不能往回走——活动、邮件、副本截止等业务时间戳都假定它单调，App 在这里
// 统一兜住，各模块不感知、也不再为“偏移往回调”各写一套系统钟拆分。
//
// 做法：部署级的业务时间高水位存在协调存储（单实例锁的 SingletonStore，共享 Redis）的一个不过期键里。
// 启动时（单实例锁之后、第一个 Mod Init 之前）本进程业务时间低于“高水位 − 容差”就拒绝启动；
// 否则推进高水位，之后每 businessTimeAdvanceInterval 推进一次。要回到过去只能清库重建。

// ErrBusinessTimeMovedBack：按新偏移算出的业务时间早于这套部署已经到过的业务时间（减去容差）。
var ErrBusinessTimeMovedBack = errors.New("business time would move back")

// ErrBusinessTimeGuardMissing：非生产环境配了非 0 的 time.logic_offset，却没有能存高水位的协调存储
// （bootstrap 没调用 App.Singleton，或没写 singleton.key_prefix）。没有守卫就不能用偏移。
var ErrBusinessTimeGuardMissing = errors.New("time.logic_offset needs the business time high-water mark")

const (
	// businessTimeTolerance 是启动检查放过的回退量。它吸收各主机之间的时钟偏差（NTP 同步的主机在毫秒级，
	// 没有同步的虚拟机、开发机通常在秒级）和运行中推进的间隔（进程崩溃时高水位最多落后一个间隔）。
	// 被放过的回退最多这么多，与生产里多主机之间本来就有的时钟偏差同一量级。不做成配置：调大等于放宽
	// 约束，调小会让正常的主机偏差拒绝启动。
	businessTimeTolerance = time.Minute
	// businessTimeAdvanceInterval 是运行中推进高水位的间隔，要明显小于容差。
	businessTimeAdvanceInterval = 10 * time.Second
	// businessTimeCallTimeout 是每次读写高水位的超时。
	businessTimeCallTimeout = 3 * time.Second
	// businessTimeCASAttempts 是一次推进最多的 CAS 次数。值只增不减，被别的进程抢先只会让高水位变大，
	// 几次之内就会看到不小于自己的值。
	businessTimeCASAttempts = 8
	// businessTimeKeySuffix 接在 singleton.key_prefix 后面。单实例锁的键是 <prefix>:<type>:<sid>，
	// 段数不同，不会撞上。
	businessTimeKeySuffix = ":business_time"
	// businessTimeAdvanceFailedMetric 是运行中推进高水位失败的次数（维护者第十二轮决定）。推进失败不
	// fail-stop，只靠这个计数和 Warn 日志发现；一直在涨说明协调存储不可用，下一次启动的检查会失败。
	// 导出名 app_business_time_advance_failed_total。
	businessTimeAdvanceFailedMetric = "app.business_time.advance_failed.total"
)

// businessTimeMark 是高水位键里的值：<unix 毫秒>|<写入者的偏移>|<server_type>:<sid>。只比较第一段，
// 后两段给拒绝信息点名用。
type businessTimeMark struct {
	raw    []byte // 键里的原值，CAS 的 expected；键不存在为 nil
	unixMs int64
	writer string // "<偏移>|<server_type>:<sid>"，只用于展示
}

func (m businessTimeMark) time() time.Time { return time.UnixMilli(m.unixMs) }

func parseBusinessTimeMark(raw []byte) (businessTimeMark, error) {
	if raw == nil {
		return businessTimeMark{}, nil
	}
	head, writer, _ := strings.Cut(string(raw), "|")
	unixMs, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return businessTimeMark{}, fmt.Errorf("unreadable high-water mark %q: %w", raw, err)
	}
	return businessTimeMark{raw: append([]byte(nil), raw...), unixMs: unixMs, writer: writer}, nil
}

// businessTimeGuard 守一个进程：启动检查、运行中推进。单写者：start 在 run 的 goroutine 里，
// 之后只有 advanceLoop 一个 goroutine 读写 last。
type businessTimeGuard struct {
	store    SingletonStore
	ownStore bool // 只为高水位打开的连接，stop 时关闭；与单实例锁共用时由锁的 finish 关闭
	key      string
	offset   time.Duration
	writer   string // 写进值里的 "<偏移>|<server_type>:<sid>"
	now      func() time.Time
	interval time.Duration
	metrics  *metrics.Registry // 本 App 的指标注册表，记推进失败；nil 安全

	last businessTimeMark // 最近一次读到或写入的值

	cancel context.CancelFunc
	done   chan struct{}
}

// startBusinessTimeGuard 在单实例锁之后、任何 Mod Init 之前调用。返回 nil 表示这个进程不需要守卫。
// 谁检查见方案 §3：生产不检查（偏移强制为 0，生产行为不变）；开了单实例锁的进程用锁的存储检查；
// 偏移非 0 而没开锁的进程只为高水位打开一个连接，打不开就拒绝启动。
func (a *App) startBusinessTimeGuard(serverType ServiceName, lock *singletonLock) (*businessTimeGuard, error) {
	if isProductionServiceConfig(a.cfg) {
		return nil, nil
	}
	offset := configuredLogicOffset(a.cfg)
	guard := &businessTimeGuard{
		offset:   offset,
		writer:   fmt.Sprintf("%s|%s:%d", offset, serverType, a.cfg.GetInt32("sid")),
		now:      BusinessClock(a.registry).Now,
		interval: a.businessTimeInterval,
	}
	guard.metrics, _ = Lookup[*metrics.Registry](a.registry, ModMetrics)
	if guard.interval <= 0 {
		guard.interval = businessTimeAdvanceInterval
	}
	switch {
	case lock != nil:
		guard.store, guard.key = lock.store, lock.settings.keyPrefix+businessTimeKeySuffix
	case offset == 0:
		// 业务时间就是真实时间；同一套部署回退时，开了单实例锁的服务会拒绝启动。
		return nil, nil
	default:
		prefix := strings.TrimSpace(readSingletonSettings(a.cfg).keyPrefix)
		if a.singletonOpener == nil || prefix == "" {
			return nil, fmt.Errorf("app: %w: %s is %s, but this process has nowhere to keep the deployment's high-water mark; "+
				"install App.Singleton (kitredis.SingletonStore) in the bootstrap and set singleton.key_prefix "+
				"(singleton.enabled may stay false), or remove the offset", ErrBusinessTimeGuardMissing, logicOffsetKey, offset)
		}
		store, err := a.singletonOpener(a.cfg)
		if err != nil {
			return nil, fmt.Errorf("app: business time high-water mark: open store: %w", err)
		}
		if store == nil {
			return nil, errors.New("app: business time high-water mark: opener returned a nil store")
		}
		guard.store, guard.ownStore, guard.key = store, true, prefix+businessTimeKeySuffix
	}
	if err := guard.check(); err != nil {
		guard.closeStore()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	guard.cancel, guard.done = cancel, make(chan struct{})
	go guard.advanceLoop(ctx)
	return guard, nil
}

// check 是启动检查：读高水位（顺带首次写入），回退超过容差就拒绝，否则推进到本进程的业务时间。
// 读写失败也拒绝启动：读不到就无法证明没有回退。
func (g *businessTimeGuard) check() error {
	now := g.now()
	ctx, cancel := context.WithTimeout(context.Background(), businessTimeCallTimeout*businessTimeCASAttempts)
	defer cancel()
	// CAS(nil, now)：键不存在时直接写入（这套部署第一次启动），存在时拿到当前值、什么也不写。
	if err := g.advance(ctx, now, func(current businessTimeMark) error {
		if now.Before(current.time().Add(-businessTimeTolerance)) {
			return fmt.Errorf("app: %w: %s = %s puts business time at %s, but this deployment's business time already reached %s "+
				"(high-water mark %s, written by offset|process %s), %s behind it; business time only moves forward. "+
				"Use an offset of at least %s, or, to go back in time, wipe the deployment's data together with that key and start over",
				ErrBusinessTimeMovedBack, logicOffsetKey, g.offset, now.UTC().Format(time.RFC3339), current.time().UTC().Format(time.RFC3339),
				g.key, current.writer, current.time().Sub(now).Round(time.Second),
				(g.offset + current.time().Sub(now) - businessTimeTolerance).Round(time.Second))
		}
		return nil
	}); err != nil {
		if errors.Is(err, ErrBusinessTimeMovedBack) {
			return err
		}
		return fmt.Errorf("app: business time high-water mark %s: %w", g.key, err)
	}
	slog.Info("business time: high-water mark checked", "key", g.key, "offset", g.offset, "high_water", g.last.time())
	return nil
}

// advance 把高水位推到 at：已经不小于 at 就不写。inspect 非 nil 时，第一次读到已有的值先交给它
// （启动检查），它返回错误就不写。
func (g *businessTimeGuard) advance(ctx context.Context, at time.Time, inspect func(businessTimeMark) error) error {
	next := []byte(strconv.FormatInt(at.UnixMilli(), 10) + "|" + g.writer)
	expected := g.last // 启动时为空：先按“键不存在”写
	for range businessTimeCASAttempts {
		callCtx, cancel := context.WithTimeout(ctx, businessTimeCallTimeout)
		applied, current, err := g.store.CompareAndSet(callCtx, g.key, expected.raw, next, 0)
		cancel()
		if err != nil {
			return err
		}
		if applied {
			g.last = businessTimeMark{raw: next, unixMs: at.UnixMilli(), writer: g.writer}
			return nil
		}
		mark, err := parseBusinessTimeMark(current)
		if err != nil {
			return err
		}
		if inspect != nil && mark.raw != nil {
			if err := inspect(mark); err != nil {
				return err
			}
			inspect = nil
		}
		if mark.raw != nil && mark.unixMs >= at.UnixMilli() {
			g.last = mark
			return nil
		}
		expected = mark
	}
	return fmt.Errorf("high-water mark still changing after %d attempts", businessTimeCASAttempts)
}

// advanceLoop 运行中按固定间隔推进高水位。失败只记日志与 app.business_time.advance_failed.total、
// 下一拍再试：高水位只守下一次启动，运行中的业务不依赖它，不为它 fail-stop。
func (g *businessTimeGuard) advanceLoop(ctx context.Context) {
	defer close(g.done)
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := g.advance(ctx, g.now(), nil); err != nil && ctx.Err() == nil {
			g.metrics.IncCounter(businessTimeAdvanceFailedMetric, nil, 1)
			slog.Warn("business time: advancing the high-water mark failed; retrying on the next beat",
				"key", g.key, "err", err)
		}
	}
}

// stop 停推进 goroutine 并等它退出，再关闭只为高水位打开的连接。在单实例锁的 finish 之前调用
// （共用的存储由 finish 关闭）。nil 安全、幂等。
func (g *businessTimeGuard) stop() {
	if g == nil || g.cancel == nil {
		return
	}
	g.cancel()
	<-g.done
	g.cancel = nil
	g.closeStore()
}

func (g *businessTimeGuard) closeStore() {
	if !g.ownStore {
		return
	}
	if err := g.store.Close(); err != nil {
		slog.Warn("business time: close store failed", "key", g.key, "err", err)
	}
}
