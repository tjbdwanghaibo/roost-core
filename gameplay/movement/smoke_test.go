package movement

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 端到端冒烟（不是性能测量）：真实 Nest + 心跳驱动 + 实体同步 Manager + Interest。几千个实体各自进场景、随机 MoveTo，
// 由各自的移动心跳推进，位置经 QueueMove 进 AOI、经同步帧推给观察者。观察者铺满地图（任何位置都在某个观察者的
// 进入半径内），结束后每个实体：动作成功、位置是目标、至少一个观察者最后收到的位置就是它的终点；心跳全部停下。
func TestSmokeThousandsOfEntitiesMoveOnTheirOwnHeartbeats(t *testing.T) {
	entities, bounds := 3000, int64(1280)
	if raceEnabled {
		entities = 300
	}
	f := newNestFixture(t, bounds)
	// 观察者每 160 格一个：格心到格角 113 < 进入半径 120。
	type watcher struct {
		id int64
		at spatial.Point
	}
	var watchers []watcher
	for x := int64(80); x < bounds; x += 160 {
		for y := int64(80); y < bounds; y += 160 {
			id := int64(100000 + len(watchers))
			watchers = append(watchers, watcher{id: f.watch(id, spatial.Point{X: x, Y: y}), at: spatial.Point{X: x, Y: y}})
		}
	}

	stop := make(chan struct{})
	var flushes sync.WaitGroup
	flushes.Add(1)
	go func() {
		defer flushes.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = f.sync.Flush(context.Background())
			}
		}
	}()

	rng := rand.New(rand.NewPCG(20261021, 2))
	point := func() spatial.Point {
		return spatial.Point{X: rng.Int64N(bounds), Y: rng.Int64N(bounds)}
	}
	movers := make([]*mover, entities)
	starts := make([]spatial.Point, entities)
	goals := make([]spatial.Point, entities)
	space := InterestSpace{Interest: f.interest}
	var started atomic.Int64
	for i := range entities {
		e := f.add(int64(1000+i), moverOptions{})
		movers[i], starts[i] = e, point()
		goal := spatial.Point{X: min(max(starts[i].X+rng.Int64N(301)-150, 0), bounds-1), Y: min(max(starts[i].Y+rng.Int64N(301)-150, 0), bounds-1)}
		goals[i] = goal
		f.mustRun(e, func(m *mover) error {
			if err := m.mb.Enter(1, space, starts[i]); err != nil {
				return err
			}
			_, err := m.beat.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{goal}, Speed: 150}, 0, time.Time{})
			if err == nil {
				started.Add(1)
			}
			return err
		})
	}
	waitFor(t, "every move to end", func() bool {
		f.sink.mu.Lock()
		err := f.sink.err
		f.sink.mu.Unlock()
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		for _, e := range movers {
			if len(e.endedActions()) == 0 {
				return false
			}
		}
		return true
	})
	for _, e := range movers {
		f.settle(e) // 到达那一拍的提交后回调（发布终点、停心跳）都已执行
	}
	if f.driver.Registered() != 0 {
		t.Fatalf("registered %d after every move ended", f.driver.Registered())
	}
	close(stop)
	flushes.Wait()
	f.flush()
	f.flush()

	f.sink.mu.Lock()
	sinkErr, updates := f.sink.err, f.sink.updates
	f.sink.mu.Unlock()
	if sinkErr != nil {
		t.Fatalf("decoding pushed frames: %v", sinkErr)
	}
	moved := 0
	for i, e := range movers {
		ended := e.endedActions()
		if len(ended) != 1 || ended[0].status != actionflow.ActionStatusSuccess {
			t.Fatalf("entity %d ended %+v", i, ended)
		}
		final := e.mb.Published().Pos
		if final != goals[i] {
			t.Fatalf("entity %d at %+v, want its goal %+v", i, final, goals[i])
		}
		seen := false
		for _, w := range watchers {
			if !spatial.WithinDistance(w.at, final, 120) {
				continue
			}
			if p, ok := f.sink.lastSeen(entitysync.SessionID(w.id), e.ID()); ok && p == final {
				seen = true
				break
			}
		}
		if !seen {
			t.Fatalf("no watcher next to entity %d's goal %+v received that position", i, final)
		}
		if final != starts[i] {
			moved++
		}
	}
	stats := f.driver.Stats()
	t.Logf("%d entities, %d moved, %d watchers, %d position payloads pushed; heartbeat %+v", entities, moved, len(watchers), updates, stats)
	if started.Load() != int64(entities) || stats.Executed == 0 || updates < moved {
		t.Fatalf("started %d executed %d updates %d moved %d", started.Load(), stats.Executed, updates, moved)
	}
}
