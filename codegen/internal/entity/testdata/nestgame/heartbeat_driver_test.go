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

// beatLoad 是 heartbeat 模式的心跳：一个驱动，每个 Unit 一个登记。beats 只在对应 Unit 的实体锁内写（回调里），
// 停机之后读。
type beatLoad struct {
	driver *heartbeat.Driver
	period time.Duration
	dirty  int
	beats  []int64
	// interval 是同一登记相邻两拍的业务时间差（Beat.Elapsed，非首拍），只在测量窗口内记。
	interval  timings
	measuring atomic.Bool
	// onBeat 在回调里改完 DAO 之后调用（压测接计数与 Sync），返回错误时本拍回滚。
	onBeat func(u *Unit, change bool, b heartbeat.Beat) error
}

// newBeatLoad 给每个 Unit 登记心跳并启动（加载阶段，不在事务里，立即生效；驱动 Start 之后才开始跳）。
func newBeatLoad(units []*Unit, period time.Duration, dirty int, onBeat func(*Unit, bool, heartbeat.Beat) error) (*beatLoad, error) {
	load := &beatLoad{driver: heartbeat.NewDriver(heartbeat.Config{Name: "nest-game"}), period: period, dirty: dirty, beats: make([]int64, len(units)), onBeat: onBeat}
	for i, u := range units {
		index, unit := i, u
		reg, err := load.driver.NewHost(unit).Register("perf.game.heartbeat", period, func(b heartbeat.Beat) error {
			change := (int64(index)+load.beats[index])%100 < int64(load.dirty)
			unit.heartbeat.Tick(change)
			load.beats[index]++
			if !b.First && load.measuring.Load() {
				load.interval.add(b.Elapsed, true)
			}
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

// start 接上 Nest 并启动驱动。
func (l *beatLoad) start(client nest.Client, access *entity.ManagerAccess) error {
	manager := access.Manager()
	if err := l.driver.Bind(client, func(id int64) entity.IThreadSafeEntity { return manager.Get(id) }); err != nil {
		return err
	}
	l.measuring.Store(true)
	return l.driver.Start()
}

// stop 停驱动：Stop 返回后不再投递，已在 Nest 队列里的拍由随后的 engine.Shutdown 执行完。
func (l *beatLoad) stop(ctx context.Context) error {
	l.measuring.Store(false)
	return l.driver.Stop(ctx)
}

// beatReport 是 heartbeat 模式的心跳结果（ticker 模式为 nil）。
type beatReport struct {
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
	r := beatReport{PeriodMS: float64(l.period) / float64(time.Millisecond), Driver: l.driver.Stats(), Interval: l.interval.result(), BeatsMin: -1}
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
	if r.Interval.Samples > 0 && r.Interval.P99MS > r.PeriodMS+50 {
		problems = append(problems, fmt.Errorf("beat interval p99 %.2fms over period %.0fms + 50ms", r.Interval.P99MS, r.PeriodMS))
	}
	if err := errors.Join(problems...); err != nil {
		r.Problem = err.Error()
	} else {
		r.Verified = true
	}
	return r
}

// 心跳驱动接线的短测（几百毫秒、64 个 Unit，不是性能测量）：nest-game.sh --check-heartbeat 在生成工程里跑。
// 每个 Unit 的心跳经正式生成的组件与 DAO setter 执行，拍数、DAO 与“按拍数算出的改值次数”一致，没有跳拍。
func TestHeartbeatDriverTicksGeneratedUnits(t *testing.T) {
	RegisterEntity()
	access := entity.NewManagerAccess(entity.NewEntityManager())
	units := make([]*Unit, 64)
	for i := range units {
		id, err := entity.BuildEntityID(int64(100000+i+1), kindUnit)
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
	load, err := newBeatLoad(units, period, 50, func(*Unit, bool, heartbeat.Beat) error {
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
	if err := load.start(engine, access); err != nil {
		t.Fatal(err)
	}
	const run = 500 * time.Millisecond
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
	// (3 + r) % 100 < 10 的 r 是 0..6 与 97..99：250 拍 = 两整轮 20 次 + 前 50 拍里的 7 次。
	if beatChanges(3, 250, 10) != 27 || beatChanges(95, 10, 10) != 5 {
		t.Fatalf("beatChanges: %d / %d", beatChanges(3, 250, 10), beatChanges(95, 10, 10))
	}
}
