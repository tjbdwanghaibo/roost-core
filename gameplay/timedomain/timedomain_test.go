package timedomain

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// memFields 扮演场景实体的 DAO 字段：在 Nest 事务里写入时登记逆操作，回滚即还原（生成 DAO 的 setter 也是这样）。
type memFields struct {
	state State
	sets  int
}

func (f *memFields) TimeDomain() State { return f.state }
func (f *memFields) SetTimeDomain(s State) {
	previous := f.state
	nest.RecordUndo(f, 1, func() error { f.state = previous; return nil })
	f.state = s
	f.sets++
}

type nullCommitter struct{}

func (nullCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

var errBusiness = errors.New("business refused")

// testClock 把进程业务时钟的来源换成可控时钟（fctx.Now 冻结的就是它），测试结束恢复。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func pinClock(t *testing.T) *testClock {
	t.Helper()
	c := &testClock{now: time.Unix(1_800_000_000, 0)}
	clock.SetSource(c.Now)
	t.Cleanup(func() { clock.SetSource(nil) })
	return c
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// inMessage 在一个请求上下文里执行 fn（像一条 Nest 消息：fctx.Now 在开始时冻结）。
func inMessage(fn func()) {
	_, release := fctx.NewContext()
	defer release()
	fn()
}

func newDomain(t *testing.T, fields *memFields, config Config) *Domain {
	t.Helper()
	d, err := New(fields, config)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// 没改过倍率时域时间就是业务时间；改倍率以生效时刻为新锚点：生效时刻前后的域时间相等（不跳变），之后按新倍率走，
// 时停时不动。
func TestDomainTimeIsContinuousAcrossRateChanges(t *testing.T) {
	clk := pinClock(t)
	fields := &memFields{}
	d := newDomain(t, fields, Config{})
	start := clk.Now()
	inMessage(func() {
		if got := d.Now(); !got.Equal(start) || d.Rate() != 1 {
			t.Fatalf("an untouched domain reads %v at rate %v, want the business time %v at rate 1", got, d.Rate(), start)
		}
	})

	clk.Advance(10 * time.Second) // 域时间 = start + 10s
	var before time.Time
	inMessage(func() {
		before = d.Now()
		if err := d.SetRate(2); err != nil {
			t.Fatal(err)
		}
		if after := d.Now(); !after.Equal(before) {
			t.Fatalf("the same message read %v then %v across SetRate", before, after)
		}
	})
	if want := start.Add(10 * time.Second); !before.Equal(want) {
		t.Fatalf("domain time before the change = %v, want %v", before, want)
	}
	inMessage(func() {
		if got := d.Now(); !got.Equal(before) {
			t.Fatalf("at the effective instant the domain reads %v, want the anchor %v (no jump)", got, before)
		}
	})
	clk.Advance(3 * time.Second)
	inMessage(func() {
		if got, want := d.Now(), before.Add(6*time.Second); !got.Equal(want) || d.Rate() != 2 {
			t.Fatalf("3s of business time at 2x = %v at rate %v, want %v", got, d.Rate(), want)
		}
	})

	// 再过 3 秒（域时间 +6s）时停：这条消息里先改再读，新锚点是按当前参数算出的生效时刻域时间，不是业务时间。
	clk.Advance(3 * time.Second)
	paused := before.Add(12 * time.Second)
	inMessage(func() {
		if err := d.SetRate(0); err != nil {
			t.Fatal(err)
		}
		if got := d.Now(); !got.Equal(paused) {
			t.Fatalf("the stop instant reads %v, want the domain time %v", got, paused)
		}
	})
	clk.Advance(time.Minute)
	inMessage(func() {
		if got := d.Now(); !got.Equal(paused) {
			t.Fatalf("a stopped domain moved from %v to %v", paused, got)
		}
	})
	if s := fields.TimeDomain(); s.Rate != 0 || s.AnchorDomainNano != paused.UnixNano() {
		t.Fatalf("the scene's fields hold %+v, want rate 0 anchored at %v", s, paused)
	}
}

// 一条消息里读到的域时间是同一个值：第一次读取冻结，之后别的 goroutine 发布了新倍率也不变；下一条消息才看到。
func TestOneMessageReadsOneDomainTime(t *testing.T) {
	clk := pinClock(t)
	d := newDomain(t, &memFields{}, Config{})
	inMessage(func() {
		first := d.Read()
		clk.Advance(time.Second) // 冻结的是消息开始时的业务时间，时钟走了也不变
		done := make(chan struct{})
		go func() { // 场景在另一个 goroutine 上改倍率（不在事务里，立即发布）
			defer close(done)
			inMessage(func() {
				if err := d.SetRate(5); err != nil {
					t.Error(err)
				}
			})
		}()
		<-done
		if again := d.Read(); again != first {
			t.Fatalf("one message read %+v then %+v", first, again)
		}
	})
	inMessage(func() {
		if d.Rate() != 5 {
			t.Fatalf("the next message reads rate %v, want the published 5", d.Rate())
		}
	})
}

// SetRate 在场景的事务里：回滚时字段还原、不发布；提交后才发布给成员。
func TestRateChangesFollowTheScenesTransaction(t *testing.T) {
	clk := pinClock(t)
	fields := &memFields{}
	d := newDomain(t, fields, Config{})
	clk.Advance(time.Second)
	if _, err := nest.RunIsolatedTransaction(context.Background(), nullCommitter{}, "slow", func() (any, error) {
		if err := d.SetRate(0.5); err != nil {
			t.Fatal(err)
		}
		if d.State().Initialized() {
			t.Error("a rate change was published before its transaction committed")
		}
		return nil, errBusiness
	}); !errors.Is(err, errBusiness) {
		t.Fatalf("rolled-back change = %v", err)
	}
	if fields.TimeDomain().Initialized() || d.State().Initialized() {
		t.Fatalf("after a rollback: fields %+v, published %+v; want both untouched", fields.TimeDomain(), d.State())
	}
	if _, err := nest.RunIsolatedTransaction(context.Background(), nullCommitter{}, "fast", func() (any, error) {
		return nil, d.SetRate(3)
	}); err != nil {
		t.Fatal(err)
	}
	if got := d.State(); got.Rate != 3 || got != fields.TimeDomain() {
		t.Fatalf("after a commit: published %+v, fields %+v; want the committed rate 3", got, fields.TimeDomain())
	}
}

// 倍率范围 [0, 100]，可由 Config 收窄；越界、NaN、Inf 拒绝且什么都不改。存下的越界倍率在 New 拒绝。
func TestRateRange(t *testing.T) {
	pinClock(t)
	fields := &memFields{}
	d := newDomain(t, fields, Config{})
	for _, rate := range []float64{0, 0.25, 1, 100} {
		if err := d.SetRate(rate); err != nil {
			t.Fatalf("SetRate(%v) = %v", rate, err)
		}
	}
	sets := fields.sets
	for _, rate := range []float64{-0.1, 100.5, math.NaN(), math.Inf(1)} {
		if err := d.SetRate(rate); !errors.Is(err, ErrRateOutOfRange) {
			t.Fatalf("SetRate(%v) = %v, want ErrRateOutOfRange", rate, err)
		}
	}
	if fields.sets != sets || d.State().Rate != 100 {
		t.Fatalf("a refused rate changed the domain: sets %d -> %d, rate %v", sets, fields.sets, d.State().Rate)
	}
	narrow := newDomain(t, &memFields{}, Config{MinRate: 0.5, MaxRate: 2})
	if err := narrow.SetRate(0); !errors.Is(err, ErrRateOutOfRange) {
		t.Fatalf("a domain declared [0.5, 2] accepted 0: %v", err)
	}
	if _, err := New(&memFields{}, Config{MinRate: 1, MaxRate: 200}); !errors.Is(err, ErrRateOutOfRange) {
		t.Fatalf("a declared range beyond [0, 100] = %v", err)
	}
	if _, err := New(&memFields{state: State{AnchorBusinessNano: 1, Rate: 150}}, Config{}); !errors.Is(err, ErrRateOutOfRange) {
		t.Fatalf("a stored rate of 150 = %v", err)
	}
}

// 成员看到的是发布的快照，晚于场景的提交：生效时刻之后、发布之前开始的成员消息按旧倍率读到了更大的值。发布之后
// 成员读到的域时间不回退（取已交出的最大值），之后再改倍率的新锚点也不低于它；晚到的旧发布不覆盖新的。
func TestMembersNeverSeeTheDomainGoBackwards(t *testing.T) {
	clk := pinClock(t)
	fields := &memFields{}
	d := newDomain(t, fields, Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { // 场景：在 T 时刻时停；事务提交后、发布之前停住（排在发布前面的提交后回调），等放行
		var err error
		inMessage(func() {
			_, err = nest.RunIsolatedTransaction(context.Background(), nullCommitter{}, "stop", func() (any, error) {
				nest.AfterCommit(func() { close(entered); <-release })
				return nil, d.SetRate(0)
			})
		})
		done <- err
	}()
	<-entered
	stopAt := time.Unix(0, fields.TimeDomain().AnchorDomainNano)
	clk.Advance(2 * time.Second)
	var early time.Time
	inMessage(func() { early = d.Now() }) // 还没发布：按旧倍率 1 读到 T + 2s
	if !early.After(stopAt) {
		t.Fatalf("premise: before the stop is published a member reads %v, want past the stop instant %v", early, stopAt)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if d.State().Rate != 0 {
		t.Fatalf("premise: the stop was not published: %+v", d.State())
	}
	clk.Advance(time.Second)
	inMessage(func() {
		if got := d.Now(); got.Before(early) {
			t.Fatalf("after the stop was published a member read %v, earlier than the %v it read before", got, early)
		}
	})
	stop := d.State()
	d.publish(State{}, 0) // 晚到的旧发布
	if d.State() != stop {
		t.Fatalf("a late older publication replaced the newer one: %+v", d.State())
	}
	inMessage(func() {
		if err := d.SetRate(1); err != nil {
			t.Fatal(err)
		}
	})
	if anchor := time.Unix(0, fields.TimeDomain().AnchorDomainNano); anchor.Before(early) {
		t.Fatalf("the new anchor %v is earlier than a domain time members already read (%v)", anchor, early)
	}
}

// 场景改倍率与成员读取并发（-race）：成员读到的域时间单调不减。
func TestConcurrentRateChangesAndReads(t *testing.T) {
	clk := pinClock(t)
	d := newDomain(t, &memFields{}, Config{})
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last time.Time
			for !stop.Load() {
				inMessage(func() {
					now := d.Now()
					if now.Before(last) {
						t.Errorf("a member read %v after %v", now, last)
					}
					last = now
				})
			}
		}()
	}
	var mu sync.Mutex // 场景的实体锁
	for i := range 200 {
		clk.Advance(time.Millisecond)
		mu.Lock()
		inMessage(func() {
			if err := d.SetRate(float64(i % 4)); err != nil {
				t.Error(err)
			}
		})
		mu.Unlock()
	}
	stop.Store(true)
	wg.Wait()
}
