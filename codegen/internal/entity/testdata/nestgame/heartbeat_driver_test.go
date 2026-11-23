package nestgame

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 心跳的两种驱动（ROOST_NEST_GAME_HEARTBEAT_DRIVER）：
//
//	ticker     原负载：唯一的 Nest 帧回调按 HeartbeatHz × 实体数的速率逐个 Dispatch 一条“心跳”业务消息（保留作对照）；
//	heartbeat  每个 Unit 在 gameplay/heartbeat 上登记一个周期 1s / HeartbeatHz 的心跳（必须是固定档位之一），
//	           驱动按相位打散逐个投递，回调在 Unit 自己的 worker、实体锁与事务内执行同一个 HeartbeatComponent.Tick。
//
// 缺省 ticker：已有报告的复跑命令保持原负载；正式对比放在 v1.31 发版前的压测之后再定缺省。
//
// heartbeat 驱动的投递方式（ROOST_NEST_GAME_HEARTBEAT_DELIVERY）：batch（Nest 批量作业）或 dispatch（每拍一条
// 单目标 Dispatch）；空串跟驱动缺省（heartbeat.DefaultDelivery，批量）。只对 heartbeat 驱动有效，生效方式进结果的
// Beats.Delivery，用来对照两种投递（sync-aoi 的 -heartbeat-delivery 同一含义）。
const (
	heartbeatDriverTicker = "ticker"
	heartbeatDriverBeat   = "heartbeat"
)

// beatPeriodFor 把 HeartbeatHz 换成心跳档位；不是档位（例如 3Hz）时报错。
func beatPeriodFor(hz int) (time.Duration, error) {
	if hz <= 0 || time.Second%time.Duration(hz) != 0 {
		return 0, fmt.Errorf("heartbeat driver needs ROOST_NEST_GAME_HZ > 0 with 1s/Hz one of %v, got %d", heartbeat.Periods, hz)
	}
	period := time.Second / time.Duration(hz)
	if !slices.Contains(heartbeat.Periods[:], period) {
		return 0, fmt.Errorf("heartbeat driver: 1s/%d = %v is not one of %v", hz, period, heartbeat.Periods)
	}
	return period, nil
}

// beatChanges 是第 index 个 Unit 前 beats 拍里改值的拍数：与 ticker 模式同一条规则（(index + 第几拍) % 100 < dirty）。
func beatChanges(index int, beats int64, dirty int) int64 {
	full, rest := beats/100, beats%100
	var n int64
	for r := range int64(100) {
		if (int64(index)+r)%100 < int64(dirty) {
			n += full
			if r < rest {
				n++
			}
		}
	}
	return n
}

// beatFlags 以回调开始时刻归窗；晚到或排空的回调照常执行，但不推进正式样本序号。
func beatFlags(began, from, end time.Time, measured *atomic.Uint64) (outside, sample bool) {
	if !inLoadWindow(began, from, end) {
		return true, false
	}
	return false, (measured.Add(1)-1)%16 == 0
}

// beatLoad 是 heartbeat 模式的心跳：一个驱动，每个 Unit 一个登记。beats 只在对应 Unit 的实体锁内写（回调里），
// 停机之后读。
type beatLoad struct {
	driver *heartbeat.Driver
	// delivery 是驱动实际使用的投递方式（空配置时为驱动缺省）。
	delivery heartbeat.Delivery
	period   time.Duration
	dirty    int
	beats    []int64
	// 同一登记的两个真实业务时刻都在 (from,end] 才记间隔；Beat.Elapsed 已截断，不能用来还原上一拍。
	from, end time.Time // driver.Start 前写定
	lastBeat  []time.Time
	interval  timings
	// onBeat 在回调里改完 DAO 之后调用（压测接计数与 Sync），返回错误时本拍回滚。
	onBeat func(u *Unit, change bool, b heartbeat.Beat) error
}

// newBeatLoad 给每个 Unit 登记心跳并启动（加载阶段，不在事务里，立即生效；驱动 Start 之后才开始跳）。
// delivery 是 ROOST_NEST_GAME_HEARTBEAT_DELIVERY 的值（空串 = 驱动缺省），不认识的值报错。
func newBeatLoad(units []*Unit, period time.Duration, dirty int, delivery string, onBeat func(*Unit, bool, heartbeat.Beat) error) (*beatLoad, error) {
	parsed, err := heartbeat.ParseDelivery(delivery)
	if err != nil {
		return nil, fmt.Errorf("ROOST_NEST_GAME_HEARTBEAT_DELIVERY: %w", err)
	}
	driver := heartbeat.NewDriver(heartbeat.Config{Name: "nest-game", Delivery: parsed})
	load := &beatLoad{driver: driver, delivery: driver.Delivery(), period: period, dirty: dirty, beats: make([]int64, len(units)), lastBeat: make([]time.Time, len(units)), onBeat: onBeat}
	for i, u := range units {
		index, unit := i, u
		reg, err := load.driver.NewHost(unit).Register("perf.game.heartbeat", period, func(b heartbeat.Beat) error {
			change := (int64(index)+load.beats[index])%100 < int64(load.dirty)
			unit.heartbeat.Tick(change)
			load.beats[index]++
			load.observeInterval(index, b)
			if load.onBeat != nil {
				return load.onBeat(unit, change, b)
			}
			return nil
		}, heartbeat.WithDurability(nest.DurabilityMemory))
		if err != nil {
			return nil, err
		}
		if err := reg.Start(); err != nil {
			return nil, err
		}
	}
	return load, nil
}

// 普通 10µs 桶到 100ms 封顶，会把超时截成合格值。仅 interval 用 200µs 桶到 2s，
// 高于所有可配档位的判决线（最高 1s+50ms），每条判决线都正好是桶界。
const beatIntervalBucket = 200 * time.Microsecond

func (l *beatLoad) observeInterval(index int, b heartbeat.Beat) {
	prev := l.lastBeat[index]
	l.lastBeat[index] = b.Now // 只由对应 Unit 的实体锁内访问。
	if !b.First && inLoadWindow(prev, l.from, l.end) && inLoadWindow(b.Now, l.from, l.end) {
		l.interval.addWithBucket(b.Now.Sub(prev), true, beatIntervalBucket)
	}
}

// start 接上 Nest 并启动驱动。
func (l *beatLoad) start(client nest.Client, access *entity.ManagerAccess, from, end time.Time) error {
	manager := access.Manager()
	if err := l.driver.Bind(client, func(id int64) entity.IThreadSafeEntity { return manager.Get(id) }); err != nil {
		return err
	}
	l.from, l.end = from, end
	return l.driver.Start()
}

// stop 停驱动：Stop 返回后不再投递，已在 Nest 队列里的拍由随后的 engine.Shutdown 执行完。
func (l *beatLoad) stop(ctx context.Context) error {
	return l.driver.Stop(ctx)
}

// beatReport 是 heartbeat 模式的心跳结果（ticker 模式为 nil）。
type beatReport struct {
	// Delivery 是驱动实际使用的投递方式（batch / dispatch）。
	Delivery                     heartbeat.Delivery
	PeriodMS                     float64
	Driver                       heartbeat.Stats
	Interval                     latencyResult
	BeatsMin, BeatsMax, BeatsSum int64
	ExpectedPerUnit              float64
	Verified                     bool
	Problem                      string `json:",omitempty"`
}

// verify 在停机之后核对：没有跳拍、卡住与准入失败；投递 = 执行；每个 Unit 的拍数在 duration / 档位 ± 2 以内
// （相位与启停时刻差一拍以内，另留一拍余量）；每个 Unit 的 DAO 与“按拍数算出的改值次数”一致；间隔 P99 不超过
// 档位 + 50ms。duration 为 0 时不核对拍数。
func (l *beatLoad) verify(units []*Unit, duration time.Duration) beatReport {
	r := beatReport{Delivery: l.delivery, PeriodMS: float64(l.period) / float64(time.Millisecond), Driver: l.driver.Stats(), Interval: l.interval.resultWithBucket(beatIntervalBucket), BeatsMin: -1}
	var problems []error
	s := r.Driver
	if s.SkippedInflight+s.SkippedQueueFull+s.SkippedRejected+s.Stalled > 0 {
		problems = append(problems, fmt.Errorf("skipped beats: inflight %d queue_full %d rejected %d stalled %d", s.SkippedInflight, s.SkippedQueueFull, s.SkippedRejected, s.Stalled))
	}
	if s.Delivered != s.Executed || s.Executed == 0 {
		problems = append(problems, fmt.Errorf("delivered %d executed %d", s.Delivered, s.Executed))
	}
	if duration > 0 {
		r.ExpectedPerUnit = float64(duration) / float64(l.period)
	}
	for i, u := range units {
		n := l.beats[i]
		r.BeatsSum += n
		if r.BeatsMin < 0 || n < r.BeatsMin {
			r.BeatsMin = n
		}
		r.BeatsMax = max(r.BeatsMax, n)
		want := beatChanges(i, n, l.dirty)
		if u.state.GetHeartbeats() != want || u.state.GetHP() != want%100 {
			problems = append(problems, fmt.Errorf("unit %d: %d beats should change %d times, DAO heartbeats %d hp %d", i, n, want, u.state.GetHeartbeats(), u.state.GetHP()))
			break
		}
	}
	if duration > 0 && (float64(r.BeatsMin) < r.ExpectedPerUnit-2 || float64(r.BeatsMax) > r.ExpectedPerUnit+2) {
		problems = append(problems, fmt.Errorf("beats per unit %d..%d, want %.1f ± 2", r.BeatsMin, r.BeatsMax, r.ExpectedPerUnit))
	}
	// 短窗中每个 Unit 可能只有一拍，没有相邻正式拍时不凭空要求 interval 样本。
	if !r.Interval.passes(r.PeriodMS+50, false) {
		problems = append(problems, fmt.Errorf("beat interval p99 %.2fms (limit %.0fms), negative %d", r.Interval.P99MS, r.PeriodMS+50, r.Interval.NegativeDurations))
	}
	if err := errors.Join(problems...); err != nil {
		r.Problem = err.Error()
	} else {
		r.Verified = true
	}
	return r
}

// 心跳驱动接线的短测（每种投递几百毫秒、64 个 Unit，不是性能测量）：nest-game.sh --check-heartbeat 在生成工程里跑。
// 每个 Unit 的心跳经正式生成的组件与 DAO setter 执行，拍数、DAO 与“按拍数算出的改值次数”一致，没有跳拍；
// ROOST_NEST_GAME_HEARTBEAT_DELIVERY 的三种取值（空 = 驱动缺省 batch、batch、dispatch）都到达驱动并记进结果。
func TestHeartbeatDriverTicksGeneratedUnits(t *testing.T) {
	RegisterEntity()
	if _, err := newBeatLoad(nil, 50*time.Millisecond, 50, "fanout", nil); err == nil {
		t.Fatal("an unknown ROOST_NEST_GAME_HEARTBEAT_DELIVERY was accepted")
	}
	for i, tc := range []struct {
		value string
		want  heartbeat.Delivery
	}{{"", heartbeat.DefaultDelivery}, {"batch", heartbeat.DeliveryBatch}, {"dispatch", heartbeat.DeliveryDispatch}} {
		t.Run("delivery="+string(tc.want)+"/"+tc.value, func(t *testing.T) {
			r := runHeartbeatDriverCheck(t, int64(100000+1000*i), tc.value)
			if r.Delivery != tc.want {
				t.Fatalf("ROOST_NEST_GAME_HEARTBEAT_DELIVERY=%q: the driver delivered by %q, want %q", tc.value, r.Delivery, tc.want)
			}
		})
	}
	// (3 + r) % 100 < 10 的 r 是 0..6 与 97..99：250 拍 = 两整轮 20 次 + 前 50 拍里的 7 次。
	if beatChanges(3, 250, 10) != 27 || beatChanges(95, 10, 10) != 5 {
		t.Fatalf("beatChanges: %d / %d", beatChanges(3, 250, 10), beatChanges(95, 10, 10))
	}
}

// runHeartbeatDriverCheck 用一种投递方式跑一轮 500ms 的接线短测并核对，返回结果。
func runHeartbeatDriverCheck(t *testing.T, firstUnique int64, delivery string) beatReport {
	t.Helper()
	access := entity.NewManagerAccess(entity.NewEntityManager())
	units := make([]*Unit, 64)
	for i := range units {
		id, err := entity.BuildEntityID(firstUnique+int64(i)+1, kindUnit)
		if err != nil {
			t.Fatal(err)
		}
		v, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: kindUnit, Id: id})
		if err != nil {
			t.Fatal(err)
		}
		units[i] = v.(*Unit)
	}
	period, err := beatPeriodFor(20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beatPeriodFor(3); err == nil {
		t.Fatal("3Hz is not a heartbeat tier and must be refused")
	}
	var inTx atomic.Int64
	load, err := newBeatLoad(units, period, 50, delivery, func(*Unit, bool, heartbeat.Beat) error {
		if nest.CurrentRollbackTx() != nil {
			inTx.Add(1)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := nest.NewEngine(nest.NestOptionWithGetter(access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{Workers: 2}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	const run = 500 * time.Millisecond
	from := time.Now()
	if err := load.start(engine, access, from, from.Add(run)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(run)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := load.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	r := load.verify(units, run)
	t.Logf("%+v; beats in a transaction %d", r, inTx.Load())
	if !r.Verified {
		t.Fatal(r.Problem)
	}
	if inTx.Load() != r.BeatsSum {
		t.Fatalf("%d of %d beats ran inside a Nest transaction", inTx.Load(), r.BeatsSum)
	}
	return r
}
