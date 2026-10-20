package nestgame

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	"github.com/tjbdwanghaibo/roost-core/wiring/selfsync"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type syncClientValue struct {
	X         int64 `bson:"x"`
	HP        int64 `bson:"hp"`
	Planned   int64 `bson:"planned_ns"`
	Changed   int64 `bson:"changed_ns"`
	Committed int64 `bson:"committed_ns"`
}
type syncClientObject struct {
	value   syncClientValue
	version uint64
}
type syncLoadClient struct {
	mu       sync.Mutex
	receiver *frame.Receiver
	objects  map[int64]syncClientObject
	refs     map[frame.ObjectRef]int64
}
type syncLoadReport struct {
	Mode                                                string
	Frames, Updates                                     uint64
	VisibleMin, VisibleMax                              int
	ValuesVerified, LatencyPassed                       bool
	PlannedToClient, ChangedToClient, CommittedToClient latencyResult
}

// syncGameLoad 是 Sync 负载的两种形态，客户端解码、终值核对与延迟口径相同：
//   - 场景（ROOST_NEST_GAME_SYNC=1）：全部 Unit 登记在一个 Manager，Interest/AOI 决定每个玩家订阅谁（最多 49 个加自己）。
//     独立 Gate 下会话 ID 是 Game Ingress 的 receiver（gatewiring.Transport）；嵌入 TCP 下会话 ID 就是玩家 ID，
//     传输是 selfsync.PushPlayerFrame（与 game-demo 场景 sceneLane 同一条，见 embedded_load_test.go）。
//   - 自身视图（ROOST_NEST_GAME_SELF_SYNC=1，只在嵌入 TCP 下）：玩家 Unit 由 wiring/selfsync 的 SelfView 登记，
//     每个玩家只订阅自己；没有 Interest，NPC Unit 不启用 Sync。
type syncGameLoad struct {
	units    []*Unit
	config   gameConfig
	manager  *entitysync.Manager
	interest *policy.Interest // 自身视图下为 nil
	// gateTransport 只在独立 Gate 下非 nil，由 gateFrontend 接上 Game Ingress。
	gateTransport                   *gatewiring.Transport
	self                            bool
	selfSync                        *selfsync.SelfSync
	sessionsMu                      sync.RWMutex
	sessions                        map[int64]entitysync.SessionID
	clients                         []syncLoadClient
	frames, updates                 atomic.Uint64
	measuring                       atomic.Bool
	planned, changedTime, committed timings
}

func (load *syncGameLoad) position(unit *Unit) spatial.Point {
	i := entity.GetUniqueIDFromEntityID(unit.ID()) - 1
	side := int64(math.Ceil(math.Sqrt(float64(load.config.Entities))))
	return spatial.Point{X: 200 + (i%side)*30 + (unit.state.GetX()%7)*2, Y: 200 + (i/side)*30 + (unit.state.GetHP()%7)*2}
}

// enableUnitSync 打开一个 Unit 的同步内容；packer 不区分视图（场景与自身视图 "self" 都给全部同步字段）。
func enableUnitSync(unit *Unit) {
	pack := func(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
		return entity.TakeFrozenSyncPayload(1, unit.state.MarshalSync(mask)), nil
	}
	unit.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: unit.ID(), Namespace: "unit", Packer: entity.SubjectSyncPackFunc{Snapshot: func(p entity.SyncProfile) (entity.FrozenSyncPayload, error) { return pack(p, dataengine.AllFields) }, Delta: pack}})
}

// newSyncGameLoad 是场景 Sync；lane 是独立 Gate 的 gatewiring.Transport 或嵌入 TCP 的 embeddedSceneLane。
func newSyncGameLoad(t *testing.T, units []*Unit, c gameConfig, lane entitysync.Transport) *syncGameLoad {
	t.Helper()
	load := &syncGameLoad{units: units, config: c, sessions: make(map[int64]entitysync.SessionID), clients: make([]syncLoadClient, c.Players)}
	mode := entitysync.ModeOnChange
	var err error
	load.manager, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: mode, Interval: 50 * time.Millisecond, Transport: lane, SnapshotBudget: entitysync.SnapshotBudget{MaxObjects: 1000, PerSessionObjects: 50}})
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		enableUnitSync(unit)
		if err := load.manager.Register(unit.Sync()); err != nil {
			t.Fatal(err)
		}
	}
	side := int64(math.Ceil(math.Sqrt(float64(c.Entities))))
	load.interest, err = policy.NewInterest(policy.InterestConfig{Manager: load.manager, AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: side*30 + 500, Y: side*30 + 500}}, BlockSize: 150, EnterRadius: 150, LeaveRadius: 180, MaxVisible: 49},
		// 观察者编号就是会话 ID：独立 Gate 下会话是连接号而不是玩家 ID，所以事务内入口从玩家 Unit 换算到它的会话。
		ObserverOf: load.observerOf})
	if err != nil {
		t.Fatal(err)
	}
	for index := range load.clients {
		load.clients[index] = syncLoadClient{receiver: frame.NewReceiver(frame.DefaultLimits()), objects: make(map[int64]syncClientObject), refs: make(map[frame.ObjectRef]int64)}
	}
	t.Cleanup(func() {
		load.interest.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = load.manager.Close(ctx)
	})
	return load
}

// newSelfSyncGameLoad 是自身视图：与生产装配相同，由 selfsync.New → EntitySyncSetup 建 Manager 并装上 SelfView
// （wiring/nest.NewModWithEntitySync 的 initEntitySync 同一顺序：Config → Configure → NewManager → Attach），
// 推送走 selfsync 的 Transport（PushPlayerFrame）。只有玩家 Unit 启用 Sync；登记、开会话由 SelfView 在
// 连接激活（嵌入 TCP 的 AfterACK）之后的政策阶段完成，夹具不 Register。Mode / Interval 与场景 Sync 相同，便于对照。
func newSelfSyncGameLoad(t *testing.T, units []*Unit, c gameConfig, access *entity.ManagerAccess) *syncGameLoad {
	t.Helper()
	load := &syncGameLoad{units: units, config: c, self: true, sessions: make(map[int64]entitysync.SessionID), clients: make([]syncLoadClient, c.Players)}
	var err error
	load.selfSync, err = selfsync.New(selfsync.Config{Entities: access, Kind: kindUnit, MessageID: 502})
	if err != nil {
		t.Fatal(err)
	}
	setup := load.selfSync.EntitySyncSetup()
	config := setup.Config
	config.Mode, config.Interval = entitysync.ModeOnChange, 50*time.Millisecond
	if setup.Configure != nil {
		setup.Configure(&config)
	}
	load.manager, err = entitysync.NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	detach, err := setup.Attach(load.manager)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units[:c.Players] {
		enableUnitSync(unit)
	}
	for index := range load.clients {
		load.clients[index] = syncLoadClient{receiver: frame.NewReceiver(frame.DefaultLimits()), objects: make(map[int64]syncClientObject), refs: make(map[frame.ObjectRef]int64)}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = load.manager.Close(ctx)
		if detach != nil {
			detach()
		}
	})
	return load
}

// openSession 记下玩家 Unit 对应的会话并以 held 打开；start 统一 Ready，与独立 Gate 的 OnActive 同一时机。
func (load *syncGameLoad) openSession(index int, session entitysync.SessionID) error {
	if index < 0 || index >= load.config.Players {
		return gateway.ErrUnauthenticated
	}
	load.sessionsMu.Lock()
	load.sessions[load.units[index].ID()] = session
	load.sessionsMu.Unlock()
	return load.manager.OpenHeldSession(session)
}

// observerOf 是玩家 Unit 的观察者编号（会话 ID）；NPC 没有会话，不是观察者。
func (load *syncGameLoad) observerOf(entityID int64) (int64, bool) {
	load.sessionsMu.RLock()
	defer load.sessionsMu.RUnlock()
	session, ok := load.sessions[entityID]
	return int64(session), ok
}
func (load *syncGameLoad) active(binding gateway.Binding, id uint64) error {
	return load.openSession(int(binding.PlayerID)-1, entitysync.SessionID(id))
}
func (load *syncGameLoad) closed(_ gateway.Binding, id uint64) {
	load.manager.CloseSession(entitysync.SessionID(id))
}
func (load *syncGameLoad) changed(unit *Unit, in invocation) error {
	if load.self && unit.Sync() == nil {
		return nil // 自身视图只看玩家 Unit，NPC 没有同步内容
	}
	unit.state.SetPlannedNS(loadTimestamp(in.planned))
	unit.state.SetChangedNS(loadTimestamp(time.Now()))
	if load.self {
		return nil
	}
	return load.interest.QueueMove(unit, load.position(unit), entity.GetUniqueIDFromEntityID(unit.ID()) <= int64(load.config.Players))
}
func (load *syncGameLoad) start(t *testing.T, network loadFrontend) {
	t.Helper()
	if load.self {
		// 会话、登记与绑定由 SelfView 在 Manager 的政策阶段完成；这里只启动并等每个玩家收到自己的全量。
		if err := load.manager.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		load.awaitValues(t, network)
		load.measuring.Store(true)
		return
	}
	for index, unit := range load.units {
		var err error
		if index < load.config.Players {
			observer, ok := load.observerOf(unit.ID())
			if !ok {
				t.Fatalf("player unit %d has no session", unit.ID())
			}
			err = load.interest.Enter(observer, unit.ID(), load.position(unit), policy.ObserverView{})
		} else {
			err = load.interest.Show(unit.ID(), load.position(unit))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if refused := load.interest.Apply(); len(refused) != 0 {
		t.Fatalf("Interest refuses=%+v", refused)
	}
	load.sessionsMu.RLock()
	for _, id := range load.sessions {
		if err := load.manager.ReadySession(id); err != nil {
			t.Fatal(err)
		}
	}
	load.sessionsMu.RUnlock()
	if err := load.manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	load.awaitValues(t, network)
	load.measuring.Store(true)
}
func (load *syncGameLoad) apply(index int, raw []byte) error {
	client := &load.clients[index]
	client.mu.Lock()
	defer client.mu.Unlock()
	_, err := client.receiver.Receive(raw, func(f frame.Frame) error {
		if f.Kind == frame.Full {
			clear(client.objects)
			clear(client.refs)
		}
		for _, object := range f.Objects {
			if object.Operation == frame.ObjectRemove {
				// Ref 是本会话的短编号与代次，不能当作全局 EntityID。
				delete(client.objects, client.refs[object.Ref])
				delete(client.refs, object.Ref)
				continue
			}
			for _, component := range object.Components {
				update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
				if err != nil {
					return err
				}
				previous, exists := client.objects[update.SubjectID]
				if !update.Full && (!exists || previous.version != update.BaseVersion) {
					return fmt.Errorf("subject %d missing baseline %d", update.SubjectID, update.BaseVersion)
				}
				if update.Full {
					previous = syncClientObject{}
				}
				if err := bson.Unmarshal(update.Payload.BytesCopy(), &previous.value); err != nil {
					return err
				}
				previous.version = update.Version
				client.objects[update.SubjectID] = previous
				client.refs[object.Ref] = update.SubjectID
				load.updates.Add(1)
				// 新进入 AOI 的全量携带历史状态，不是当时已订阅的变化交付样本。
				if load.measuring.Load() && !update.Full && previous.value.Changed > 0 {
					now := time.Now()
					load.planned.add(now.Sub(loadTime(previous.value.Planned)), true)
					load.changedTime.add(now.Sub(loadTime(previous.value.Changed)), true)
					load.committed.add(now.Sub(loadTime(previous.value.Committed)), true)
					if c := tail.Load(); c != nil {
						planned, changed, committed := loadTime(previous.value.Planned), loadTime(previous.value.Changed), loadTime(previous.value.Committed)
						c.observe("sync", now.Sub(planned), tailPart{"business", changed.Sub(planned)}, tailPart{"commit", committed.Sub(changed)}, tailPart{"deliver", now.Sub(committed)})
					}
				}
			}
		}
		load.frames.Add(1)
		return nil
	})
	return err
}

// wanted 是玩家 index 的客户端此刻应该持有的对象：场景是 AOI 可见集合加自己，自身视图只有自己。
func (load *syncGameLoad) wanted(index int) []int64 {
	if load.self {
		return []int64{load.units[index].ID()}
	}
	observer, _ := load.observerOf(load.units[index].ID())
	return append(load.interest.Visible(observer), load.units[index].ID())
}
func (load *syncGameLoad) valuesMatch() bool {
	for index := range load.clients {
		wanted := load.wanted(index)
		client := &load.clients[index]
		client.mu.Lock()
		matches := len(client.objects) == len(wanted)
		for _, id := range wanted {
			value, exists := client.objects[id]
			u := load.units[entity.GetUniqueIDFromEntityID(id)-1]
			if !exists || value.value.X != u.state.GetX() || value.value.HP != u.state.GetHP() {
				matches = false
				break
			}
		}
		client.mu.Unlock()
		if !matches {
			return false
		}
	}
	return true
}

// 仅在业务未启动或已 Shutdown 时读 DAO；不会在客户端网络 goroutine 读 Entity。
func (load *syncGameLoad) awaitValues(t *testing.T, network loadFrontend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for !load.valuesMatch() {
		if failure := network.currentFailure(); failure != nil {
			t.Logf("Sync mismatch: %s", load.mismatch())
			client := &load.clients[0]
			client.mu.Lock()
			t.Logf("Sync diagnostic frames=%d updates=%d first-client=%v", load.frames.Load(), load.updates.Load(), client.objects)
			client.mu.Unlock()
			t.Fatal(failure)
		}
		if err := load.manager.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("Sync final view/DAO values did not converge")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (load *syncGameLoad) finish(t *testing.T, network loadFrontend) *syncLoadReport {
	load.awaitValues(t, network)
	load.measuring.Store(false)
	mode := load.manager.Mode().String()
	if load.self {
		mode = "self:" + mode
	}
	report := &syncLoadReport{Mode: mode, Frames: load.frames.Load(), Updates: load.updates.Load(), ValuesVerified: true, PlannedToClient: load.planned.result(), ChangedToClient: load.changedTime.result(), CommittedToClient: load.committed.result(), VisibleMin: math.MaxInt}
	for index := range load.clients {
		client := &load.clients[index]
		client.mu.Lock()
		report.VisibleMin = min(report.VisibleMin, len(client.objects))
		report.VisibleMax = max(report.VisibleMax, len(client.objects))
		client.mu.Unlock()
	}
	report.LatencyPassed = report.CommittedToClient.Samples > 0 && report.CommittedToClient.P99MS <= 50 && report.CommittedToClient.NegativeDurations == 0 && report.ChangedToClient.NegativeDurations == 0 && report.PlannedToClient.NegativeDurations == 0
	return report
}

func (load *syncGameLoad) mismatch() string {
	for index := range load.clients {
		wanted := load.wanted(index)
		client := &load.clients[index]
		client.mu.Lock()
		if len(wanted) != len(client.objects) {
			got := len(client.objects)
			client.mu.Unlock()
			return fmt.Sprintf("client=%d want count=%d got=%d wanted=%v", index, len(wanted), got, wanted)
		}
		for _, id := range wanted {
			v, exists := client.objects[id]
			u := load.units[entity.GetUniqueIDFromEntityID(id)-1]
			if !exists || v.value.X != u.state.GetX() || v.value.HP != u.state.GetHP() {
				message := fmt.Sprintf("client=%d subject=%d exists=%v want X/HP=%d/%d got=%d/%d", index, id, exists, u.state.GetX(), u.state.GetHP(), v.value.X, v.value.HP)
				client.mu.Unlock()
				return message
			}
		}
		client.mu.Unlock()
	}
	return "no mismatch"
}
