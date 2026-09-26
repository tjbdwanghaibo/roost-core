package persistflow

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 与 pressure 的真实文件 WAL/Mongo 共用 Nest 和生成 DAO。持久写入频率由 RATE 决定，
// Sync 的 20Hz 与 1%/5% 内存变化独立。接收端在进程内严格解码，延迟不是网络 SLO。
type mixedResult struct {
	Players, Entities, VisibleLimit, DirtyPercent, Hz int
	MovementTicks, StateChanges                       int
	StateAgeAtDecodeP99MS                             float64
	ExistingObjectChangeToDecodeP99MS                 float64
	ExistingObjectChangeSamples                       int
	Sync                                              entitysync.ManagerStats
	Nest                                              nest.DispatcherStats
}
type mixedClient struct {
	tick, epoch uint32
	objects     map[frame.ObjectRef]traderSnapshot
	versions    map[frame.ObjectRef]uint64
}
type mixedLoad struct {
	ctx                   context.Context
	manager               *entitysync.Manager
	interest              *policy.Interest
	scheduler             *nest.NestMgr
	access                *entity.ManagerAccess
	ids                   []int64
	clients               map[entitysync.SessionID]*mixedClient
	players, dirty, count int
	stopMoves             context.CancelFunc
	done                  chan error
	ticks, changes        int
	ages                  []time.Duration
	existingAges          []time.Duration
	once                  sync.Once
}

func newMixedLoad(t *testing.T, ctx context.Context, count int) *mixedLoad {
	t.Helper()
	l := &mixedLoad{ctx: ctx, count: count, players: min(1000, count), dirty: pressureInt(t, "ROOST_PERF_MIXED_DIRTY", 1, 100), clients: make(map[entitysync.SessionID]*mixedClient), done: make(chan error, 1)}
	var err error
	l.manager, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModeOnChange, Interval: 50 * time.Millisecond, Transport: entitysync.TransportFunc(l.receive)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.manager.Close(context.Background()) })
	side := int64(math.Ceil(math.Sqrt(float64(count))))
	l.interest, err = policy.NewInterest(policy.InterestConfig{Manager: l.manager, AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: side*30 + 500, Y: side*30 + 500}}, BlockSize: 150, EnterRadius: 150, LeaveRadius: 180, MaxVisible: 49}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.interest.Close)
	return l
}
func (l *mixedLoad) point(index, step int) spatial.Point {
	side := int64(math.Ceil(math.Sqrt(float64(l.count))))
	return spatial.Point{X: 200 + int64(index)%side*30 + int64(step%16)*2, Y: 200 + int64(index)/side*30}
}
func (l *mixedLoad) start(t *testing.T, scheduler *nest.NestMgr, access *entity.ManagerAccess, ids []int64) {
	t.Helper()
	l.scheduler, l.access, l.ids = scheduler, access, ids
	for i, id := range ids {
		value := access.Manager().Get(id).(*Trader)
		if err := l.manager.Register(value.Sync()); err != nil {
			t.Fatal(err)
		}
		var err error
		if i%max(1, len(ids)/l.players) == 0 && len(l.clients) < l.players {
			sid := entitysync.SessionID(id)
			l.clients[sid] = &mixedClient{objects: make(map[frame.ObjectRef]traderSnapshot), versions: make(map[frame.ObjectRef]uint64)}
			if err = l.manager.OpenSession(sid); err != nil {
				t.Fatal(err)
			}
			err = l.interest.Enter(id, l.point(i, 0))
		} else {
			err = l.interest.Show(id, l.point(i, 0))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if failures := l.interest.Apply(); len(failures) != 0 {
		t.Fatal(failures)
	}
	if err := l.manager.Flush(l.ctx); err != nil {
		t.Fatal(err)
	}

	if err := l.manager.Start(l.ctx); err != nil {
		t.Fatal(err)
	}
	moveCtx, cancel := context.WithCancel(l.ctx)
	l.stopMoves = cancel
	t.Cleanup(func() { l.once.Do(func() { cancel(); <-l.done }) })
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-moveCtx.Done():
				l.done <- nil
				return
			case <-ticker.C:
			}
			l.ticks++
			count := max(1, len(ids)*l.dirty/100)
			batch := make([]int64, 0, min(50, count))
			for offset := 0; offset < count; {
				batch = batch[:0]
				for len(batch) < cap(batch) && offset < count {
					batch = append(batch, ids[((l.ticks-1)*count+offset)%len(ids)])
					offset++
				}
				if _, err := scheduler.RequestMulti(l.ctx, nest.NewHandlerName("pressure_move"), batch, []any{l.ticks}); err != nil {
					l.done <- err
					return
				}
				l.changes += len(batch)
			}
		}
	}()
}
func (l *mixedLoad) receive(_ context.Context, sid entitysync.SessionID, raw []byte) error {
	f, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
	if err != nil {
		return err
	}
	c := l.clients[sid]
	if c == nil {
		return fmt.Errorf("unknown client %d", sid)
	}
	if c.epoch != f.Epoch {
		clear(c.objects)
		clear(c.versions)
		c.epoch, c.tick = f.Epoch, 0
	}
	if f.BaseTick != c.tick {
		return fmt.Errorf("client %d broken tick %d -> %d", sid, c.tick, f.BaseTick)
	}
	c.tick = f.Tick
	for _, object := range f.Objects {
		old, exists := c.objects[object.Ref]
		switch object.Operation {
		case frame.ObjectRemove:
			if !exists {
				return fmt.Errorf("remove without create")
			}
			delete(c.objects, object.Ref)
			delete(c.versions, object.Ref)
			continue
		case frame.ObjectCreate:
			if exists {
				return fmt.Errorf("duplicate create")
			}
		case frame.ObjectUpdate:
			if !exists {
				return fmt.Errorf("update without create")
			}
		default:
			return fmt.Errorf("invalid operation")
		}
		if len(object.Components) != 1 {
			return fmt.Errorf("component count")
		}
		update, err := entitysync.DecodeSubjectUpdate(object.Components[0].Data, 0)
		if err != nil {
			return err
		}
		if exists && old.ID != update.SubjectID {
			return fmt.Errorf("reference mismatch")
		}
		if !update.Full && c.versions[object.Ref] != update.BaseVersion {
			return fmt.Errorf("version gap")
		}
		var value traderSnapshot
		if err = bson.Unmarshal(update.Payload.BytesCopy(), &value); err != nil {
			return err
		}
		if value.ID != update.SubjectID {
			return fmt.Errorf("payload subject mismatch")
		}
		if value.Changed > 0 && value.Changed != old.Changed {
			age := time.Since(time.Unix(0, value.Changed))
			l.ages = append(l.ages, age)
			if exists {
				l.existingAges = append(l.existingAges, age)
			}
		}
		c.objects[object.Ref], c.versions[object.Ref] = value, update.Version
	}
	return nil
}
func (l *mixedLoad) stop(t *testing.T) *mixedResult {
	t.Helper()
	l.once.Do(func() {
		l.stopMoves()
		if err := <-l.done; err != nil {
			t.Error(err)
		}
	})
	if err := l.manager.Stop(l.ctx); err != nil {
		t.Fatal(err)
	}
	for tries := 0; l.manager.Stats().Pending > 0 && tries < 100; tries++ {
		if err := l.manager.Flush(l.ctx); err != nil {
			t.Fatal(err)
		}
	}
	stats := l.manager.Stats()
	if stats.Pending != 0 || stats.FlushFailures != 0 || stats.SessionsLost != 0 {
		t.Fatalf("mixed sync did not converge: %+v error=%v", stats, l.manager.LastError())
	}
	expected := make(map[entitysync.SessionID]map[int64]bool, len(l.clients))
	for _, id := range l.ids {
		for _, sid := range l.manager.Subscribers(id) {
			if expected[sid] == nil {
				expected[sid] = make(map[int64]bool)
			}
			expected[sid][id] = true
		}
	}
	for sid, client := range l.clients {
		if len(client.objects) != len(expected[sid]) || len(client.objects) > 50 {
			t.Fatalf("client %d AOI count=%d expected=%d", sid, len(client.objects), len(expected[sid]))
		}
		for _, got := range client.objects {
			if !expected[sid][got.ID] {
				t.Fatal("client retained obsolete AOI")
			}
			e := l.access.Manager().Get(got.ID).(*Trader)
			if got.Coins != e.wallet.GetCoins() || got.Transfers != e.wallet.GetTransfers() || got.Items != e.inventory.GetItems() || got.X != e.wallet.GetX() || got.Y != e.wallet.GetY() {
				t.Fatalf("client %d stale subject %d", sid, got.ID)
			}
		}
	}
	slices.Sort(l.ages)
	p99 := float64(0)
	if len(l.ages) > 0 {
		p99 = float64(l.ages[(len(l.ages)-1)*99/100]) / float64(time.Millisecond)
	}
	slices.Sort(l.existingAges)
	existingP99 := float64(0)
	if len(l.existingAges) > 0 {
		existingP99 = float64(l.existingAges[(len(l.existingAges)-1)*99/100]) / float64(time.Millisecond)
	}
	return &mixedResult{ExistingObjectChangeToDecodeP99MS: existingP99, ExistingObjectChangeSamples: len(l.existingAges), Players: len(l.clients), Entities: l.count, VisibleLimit: 50, DirtyPercent: l.dirty, Hz: 20, MovementTicks: l.ticks, StateChanges: l.changes, StateAgeAtDecodeP99MS: p99, Sync: stats, Nest: l.scheduler.Stats()}
}

func (l *mixedLoad) installHandler(scheduler *nest.NestMgr) {
	scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("pressure_move"), func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		step := params[0].(int)
		for _, value := range es {
			e := value.(*Trader)
			index := int(entity.GetUniqueIDFromEntityID(e.ID())) - 30000
			at := l.point(index, step)
			e.wallet.SetX(at.X)
			e.wallet.SetY(at.Y)
			e.wallet.SetChanged(time.Now().UnixNano())
			_, observer := l.clients[entitysync.SessionID(e.ID())]
			if err := l.interest.QueueMove(e, at, observer); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory})
}
