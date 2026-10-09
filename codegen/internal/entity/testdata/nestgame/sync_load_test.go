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
type syncGameLoad struct {
	units                           []*Unit
	config                          gameConfig
	manager                         *entitysync.Manager
	interest                        *policy.Interest
	transport                       *gatewiring.Transport
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
func newSyncGameLoad(t *testing.T, units []*Unit, c gameConfig) *syncGameLoad {
	t.Helper()
	load := &syncGameLoad{units: units, config: c, transport: &gatewiring.Transport{SyncMessageID: 501}, sessions: make(map[int64]entitysync.SessionID), clients: make([]syncLoadClient, c.Players)}
	mode := entitysync.ModeOnChange
	var err error
	load.manager, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: mode, Interval: 50 * time.Millisecond, Transport: load.transport, SnapshotBudget: entitysync.SnapshotBudget{MaxObjects: 1000, PerSessionObjects: 50}})
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		pack := func(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
			return entity.TakeFrozenSyncPayload(1, unit.state.MarshalSync(mask)), nil
		}
		unit.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: unit.ID(), Namespace: "unit", Packer: entity.SubjectSyncPackFunc{Snapshot: func(p entity.SyncProfile) (entity.FrozenSyncPayload, error) { return pack(p, dataengine.AllFields) }, Delta: pack}})
		if err := load.manager.Register(unit.Sync()); err != nil {
			t.Fatal(err)
		}
	}
	side := int64(math.Ceil(math.Sqrt(float64(c.Entities))))
	load.interest, err = policy.NewInterest(policy.InterestConfig{Manager: load.manager, AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: side*30 + 500, Y: side*30 + 500}}, BlockSize: 150, EnterRadius: 150, LeaveRadius: 180, MaxVisible: 49}, Session: func(observer int64) entitysync.SessionID {
		load.sessionsMu.RLock()
		defer load.sessionsMu.RUnlock()
		return load.sessions[observer]
	}})
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
func (load *syncGameLoad) active(binding gateway.Binding, id uint64) error {
	index := int(binding.PlayerID) - 1
	if index < 0 || index >= load.config.Players {
		return gateway.ErrUnauthenticated
	}
	receiver := entitysync.SessionID(id)
	load.sessionsMu.Lock()
	load.sessions[load.units[index].ID()] = receiver
	load.sessionsMu.Unlock()
	return load.manager.OpenHeldSession(receiver)
}
func (load *syncGameLoad) closed(_ gateway.Binding, id uint64) {
	load.manager.CloseSession(entitysync.SessionID(id))
}
func (load *syncGameLoad) changed(unit *Unit, in invocation) error {
	unit.state.SetPlannedNS(loadTimestamp(in.planned))
	unit.state.SetChangedNS(loadTimestamp(time.Now()))
	return load.interest.QueueMove(unit, load.position(unit), entity.GetUniqueIDFromEntityID(unit.ID()) <= int64(load.config.Players))
}
func (load *syncGameLoad) start(t *testing.T, network *gateFrontend) {
	t.Helper()
	for index, unit := range load.units {
		var err error
		if index < load.config.Players {
			err = load.interest.Enter(unit.ID(), load.position(unit))
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
				}
			}
		}
		load.frames.Add(1)
		return nil
	})
	return err
}
func (load *syncGameLoad) valuesMatch() bool {
	for index := range load.clients {
		wanted := load.interest.Visible(load.units[index].ID())
		wanted = append(wanted, load.units[index].ID())
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
func (load *syncGameLoad) awaitValues(t *testing.T, network *gateFrontend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for !load.valuesMatch() {
		network.mu.Lock()
		failure := network.failure
		network.mu.Unlock()
		if failure != nil {
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
func (load *syncGameLoad) finish(t *testing.T, network *gateFrontend) *syncLoadReport {
	load.awaitValues(t, network)
	load.measuring.Store(false)
	report := &syncLoadReport{Mode: load.manager.Mode().String(), Frames: load.frames.Load(), Updates: load.updates.Load(), ValuesVerified: true, PlannedToClient: load.planned.result(), ChangedToClient: load.changedTime.result(), CommittedToClient: load.committed.result(), VisibleMin: math.MaxInt}
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
		wanted := append(load.interest.Visible(load.units[index].ID()), load.units[index].ID())
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
