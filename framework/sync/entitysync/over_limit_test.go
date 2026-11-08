package entitysync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// counterValue 是一个带标签计数器的当前值（标签完全相同的那一条）；没有时为 0。
func counterValue(name string, labels metrics.Labels) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name && maps.Equal(metric.Labels, labels) {
			return metric.Value
		}
	}
	return 0
}

// underLimitGolden 是 underLimitScenario 全部帧的 SHA-256，取自超额降级之前的实现（b4699758）：
// 从不超出持有上限的会话，帧逐字节与以前相同。
const underLimitGolden = "e59a0815e7ac14be0011dcf6bf35d63921fd9244f7f5ef4a72ea92015933efd4"

// underLimitScenario 跑一段从不超出持有上限的交付：多会话、两类上限（含满额时同 tick 的 remove + create 替换）、
// 快照预算、增量、Hold/Ready、held 会话、注销、换 profile。返回按会话、按顺序拼起来的全部帧。
func underLimitScenario(t *testing.T) []byte {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{
		Limits:         frame.Limits{MaxObjects: 4},
		SnapshotBudget: SnapshotBudget{MaxObjects: 50}, // 预算走一遍、但不挡（HoldSession 按 map 顺序重排恢复请求，挡了就不确定）
		Namespaces:     map[string]NamespaceLimits{"terrain": {MaxObjects: 3, SnapshotBudget: SnapshotBudget{PerSessionObjects: 10}}},
	})
	states := map[int64]*entity.SubjectSyncState{}
	for _, id := range []int64{1, 2, 3, 4, 5, 6} {
		states[id] = nsSubject(id, "player")
	}
	for _, id := range []int64{11, 12, 13, 14} {
		states[id] = nsSubject(id, "terrain")
	}
	for _, id := range []int64{1, 2, 3, 4, 5, 6, 11, 12, 13, 14} {
		mustRegister(t, manager, states[id])
	}
	open(t, manager, 1, 2)
	if err := manager.OpenHeldSession(3); err != nil {
		t.Fatal(err)
	}
	var out []byte
	flush := func() {
		t.Helper()
		mustFlush(t, manager)
		for _, sid := range []SessionID{1, 2, 3} {
			for _, data := range transport.take(sid) {
				out = fmt.Appendf(out, "session %d frame %d:", sid, len(data))
				out = append(out, data...)
			}
		}
	}
	sub := func(sid SessionID, profile entity.SyncProfile, ids ...int64) {
		for _, id := range ids {
			mustSubscribe(t, manager, sid, id, profile)
		}
	}
	unsub := func(sid SessionID, ids ...int64) {
		for _, id := range ids {
			if err := manager.Unsubscribe(sid, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	sub(1, entity.SyncProfile{}, 1, 2, 3, 4, 11, 12, 13)
	sub(2, entity.SyncProfile{}, 3, 4, 5, 11)
	sub(3, entity.SyncProfile{}, 1, 2)
	for range 4 {
		flush()
	}
	states[1].MarkDirty(1)
	states[11].MarkDirty(1)
	flush()
	unsub(1, 4, 13)
	sub(1, entity.SyncProfile{}, 5, 14) // 满额的两类里同 tick 换一个
	flush()
	flush()
	if err := manager.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	flush()
	if err := manager.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReadySession(3); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		flush()
	}
	if err := manager.Unregister(3); err != nil {
		t.Fatal(err)
	}
	sub(2, entity.SyncProfile{Key: "far", LOD: 2}, 5)
	states[5].MarkDirty(1)
	for range 3 {
		flush()
	}
	unsub(2, 4, 5)
	sub(2, entity.SyncProfile{}, 6, 12)
	for range 2 {
		flush()
	}
	if lost := manager.Stats().SessionsLost; lost != 0 {
		t.Fatalf("the scenario lost %d session(s); it must stay within every limit", lost)
	}
	return out
}

func TestUnderTheLimitFramesAreByteIdentical(t *testing.T) {
	frames := underLimitScenario(t)
	sum := sha256.Sum256(frames)
	if got := hex.EncodeToString(sum[:]); got != underLimitGolden {
		t.Fatalf("frames of sessions that never exceed their limits changed: sha256 %s (%d bytes), want %s", got, len(frames), underLimitGolden)
	}
}

// ---- 超额降级（D17） ----

// clientTable 是接收端的对象表：按帧应用 create / remove，Full 清空旧对象。
type clientTable struct {
	refs map[frame.ObjectRef]int64
}

func newClientTable() *clientTable { return &clientTable{refs: make(map[frame.ObjectRef]int64)} }

func (c *clientTable) apply(t *testing.T, frames [][]byte) {
	t.Helper()
	for _, data := range frames {
		got := decodeFrame(t, data)
		if got.wire.Kind == frame.Full {
			clear(c.refs)
		}
		for _, object := range got.wire.Objects {
			switch object.Operation {
			case frame.ObjectCreate:
				if _, exists := c.refs[object.Ref]; exists {
					t.Fatalf("create of a ref the client already holds: %v", object.Ref)
				}
				for _, component := range object.Components {
					update, err := DecodeSubjectUpdate(component.Data, 0)
					if err != nil {
						t.Fatal(err)
					}
					c.refs[object.Ref] = update.SubjectID
				}
			case frame.ObjectRemove:
				if _, exists := c.refs[object.Ref]; !exists {
					t.Fatalf("remove of a ref the client does not hold: %v", object.Ref)
				}
				delete(c.refs, object.Ref)
			case frame.ObjectUpdate:
				if _, exists := c.refs[object.Ref]; !exists {
					t.Fatalf("update of a ref the client does not hold: %v", object.Ref)
				}
			}
		}
	}
}

func (c *clientTable) held() []int64 {
	ids := make([]int64, 0, len(c.refs))
	for _, id := range c.refs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// deferralLog 是一个带 Deferred 回调的来源收到的通知。
type deferralLog struct {
	mu    sync.Mutex
	items []DeferredSubscription
}

func (d *deferralLog) record(items []DeferredSubscription) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items = append(d.items, items...)
}

func (d *deferralLog) take() []DeferredSubscription {
	d.mu.Lock()
	defer d.mu.Unlock()
	items := d.items
	d.items = nil
	return items
}

// heldBy 是 Manager 记的会话已交付引用表（升序）。
func heldBy(manager *Manager, sid SessionID) []int64 {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	sess := manager.sessions[sid]
	if sess == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(sess.objects))
}

var (
	nearProfile = entity.SyncProfile{}
	farProfile  = entity.SyncProfile{Key: "far", LOD: 2}
)

// 超额时会话不关：一次进来 5 个新对象、上限 3，按（Profile 优先级、实体 ID）留下前 3 个，其余暂缓、不截断、
// 通知来源一次（留在暂缓里不重复通知）；结果确定（两个 Manager 跑同一序列，帧逐字节相同）。空位出现后按同一排序恢复。
func TestOverLimitDefersTheLowestPriorityNewObjectsAndKeepsTheSession(t *testing.T) {
	run := func(t *testing.T) ([][]byte, *Manager, *recordingTransport, *deferralLog, *SubscriptionSource) {
		transport := newRecordingTransport()
		var lost error
		manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 3}, SessionLost: func(_ SessionID, err error) { lost = err }})
		log := &deferralLog{}
		source := manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{Deferred: log.record})
		for id := int64(10); id <= 14; id++ {
			mustRegister(t, manager, nsSubject(id, "npc"))
		}
		open(t, manager, 1)
		for id, profile := range map[int64]entity.SyncProfile{10: farProfile, 11: nearProfile, 12: farProfile, 13: nearProfile, 14: nearProfile} {
			if err := source.Subscribe(1, id, profile); err != nil {
				t.Fatal(err)
			}
		}
		mustFlush(t, manager)
		frames := transport.take(1)
		if lost != nil || manager.Stats().SessionsLost != 0 {
			t.Fatalf("session lost over its limit: %v", lost)
		}
		return frames, manager, transport, log, source
	}
	first, manager, transport, log, source := run(t)
	again, _, _, _, _ := run(t)
	if len(first) != 1 || !bytes.Equal(first[0], again[0]) {
		t.Fatalf("over-limit selection is not deterministic: %d frame(s)", len(first))
	}
	client := newClientTable()
	client.apply(t, first)
	if got := client.held(); !slices.Equal(got, []int64{11, 13, 14}) {
		t.Fatalf("client holds %v, want the three near objects [11 13 14]", got)
	}
	if got := heldBy(manager, 1); !slices.Equal(got, client.held()) {
		t.Fatalf("manager reference table %v, client %v", got, client.held())
	}
	if stats := manager.Stats(); stats.OverLimitWaiting != 2 || stats.OverLimitDeferred != 2 || stats.PendingSnapshots != 0 {
		t.Fatalf("stats %+v, want 2 waiting, none counted as pending snapshots", stats)
	}
	// 通知在下一次政策阶段送达，留在暂缓里的不再通知。
	mustFlush(t, manager)
	notices := log.take()
	if len(notices) != 2 || notices[0] != (DeferredSubscription{Session: 1, Subject: 10}) || notices[1] != (DeferredSubscription{Session: 1, Subject: 12}) {
		t.Fatalf("deferral notices %+v, want 10 then 12 on session 1", notices)
	}
	for range 3 {
		mustFlush(t, manager)
	}
	if notices := log.take(); len(notices) != 0 || len(transport.take(1)) != 0 {
		t.Fatalf("a waiting subscription was noticed again (%+v) or something was sent", notices)
	}
	// 空位：退订 13 → remove；下一次 Flush 放回排序最前的暂缓对象（10：同为 far，ID 小），12 继续等。
	if err := source.Unsubscribe(1, 13); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	client.apply(t, transport.take(1))
	if got := client.held(); !slices.Equal(got, []int64{10, 11, 14}) {
		t.Fatalf("after room appeared the client holds %v, want [10 11 14]", got)
	}
	if stats := manager.Stats(); stats.OverLimitWaiting != 1 {
		t.Fatalf("waiting %d after one slot freed, want 1", stats.OverLimitWaiting)
	}
	if notices := log.take(); len(notices) != 0 {
		t.Fatalf("recovery noticed %+v", notices)
	}
}

// 钉住的订阅（玩家看自己）永不暂缓：类满时同一帧先 remove 优先级最低的未钉住对象再 create 它；被让位的订阅转为
// 暂缓、通知 Evicted；同类全是钉住的对象时只能暂缓（pinned_blocked）。
func TestOverLimitPinnedSubscriptionEvictsTheLowestPriority(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 3}})
	log := &deferralLog{}
	others := manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{Deferred: log.record})
	self := manager.NewSubscriptionSource()
	for _, id := range []int64{1, 2, 3, 4, 5} {
		mustRegister(t, manager, nsSubject(id, "player"))
	}
	open(t, manager, 1)
	for id, profile := range map[int64]entity.SyncProfile{1: nearProfile, 2: farProfile, 3: farProfile} {
		if err := others.Subscribe(1, id, profile); err != nil {
			t.Fatal(err)
		}
	}
	mustFlush(t, manager)
	client := newClientTable()
	client.apply(t, transport.take(1))
	evictedBefore := counterValue(overLimitDeferredMetric, metrics.Labels{"namespace": "*", "reason": "evicted"})
	if _, err := self.SubscribePinned(1, 4, nearProfile); err != nil {
		t.Fatal(err)
	}
	// 3 同时有一个增量在途：让位改写的是同一项。
	manager.subject(3).currentState().MarkDirty(1)
	mustFlush(t, manager)
	frames := transport.take(1)
	if len(frames) != 1 {
		t.Fatalf("eviction took %d frames, want one frame with remove then create", len(frames))
	}
	got := decodeFrame(t, frames[0])
	if len(got.wire.Objects) != 2 || got.wire.Objects[0].Operation != frame.ObjectRemove || got.objects[4] != frame.ObjectCreate {
		t.Fatalf("eviction frame %+v, want remove then the pinned create", got.objects)
	}
	client.apply(t, frames)
	if held := client.held(); !slices.Equal(held, []int64{1, 2, 4}) {
		t.Fatalf("client holds %v, want [1 2 4]: the far object with the larger ID makes room", held)
	}
	mustFlush(t, manager)
	if notices := log.take(); len(notices) != 1 || notices[0] != (DeferredSubscription{Session: 1, Subject: 3, Evicted: true}) {
		t.Fatalf("notices %+v, want 3 evicted", notices)
	}
	if got := counterValue(overLimitDeferredMetric, metrics.Labels{"namespace": "*", "reason": "evicted"}); got != evictedBefore+1 {
		t.Fatalf("evicted counter %d → %d", evictedBefore, got)
	}
	if stats := manager.Stats(); stats.OverLimitEvicted != 1 || stats.OverLimitWaiting != 1 || stats.SessionsLost != 0 {
		t.Fatalf("stats %+v", stats)
	}
	// 空位出现时被让位的对象回来。
	if err := others.Unsubscribe(1, 1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	client.apply(t, transport.take(1))
	if held := client.held(); !slices.Equal(held, []int64{2, 3, 4}) {
		t.Fatalf("after room appeared the client holds %v, want [2 3 4]", held)
	}
	// 同类全是钉住的：第 4 个钉住的也放不下，只能暂缓（配置错误），会话照旧。
	for _, id := range []int64{2, 3} {
		if _, err := self.SubscribePinned(1, id, nearProfile); err != nil {
			t.Fatal(err)
		}
	}
	blockedBefore := counterValue(overLimitDeferredMetric, metrics.Labels{"namespace": "*", "reason": "pinned_blocked"})
	if _, err := self.SubscribePinned(1, 5, nearProfile); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	client.apply(t, transport.take(1))
	if held := client.held(); !slices.Equal(held, []int64{2, 3, 4}) || manager.Stats().SessionsLost != 0 {
		t.Fatalf("client holds %v with every slot pinned", held)
	}
	if got := counterValue(overLimitDeferredMetric, metrics.Labels{"namespace": "*", "reason": "pinned_blocked"}); got != blockedBefore+1 {
		t.Fatalf("pinned_blocked counter %d → %d", blockedBefore, got)
	}
}

// 已暂缓的订阅被钉住（例如 Interest 的 self 贡献到了这对 pair 上）立刻回到准入，挤掉优先级最低的对象。
func TestOverLimitPinningAWaitingSubscriptionAdmitsIt(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 2}})
	source := manager.NewSubscriptionSource()
	mustRegister(t, manager, nsSubject(1, "p"), nsSubject(2, "p"), nsSubject(3, "p"))
	open(t, manager, 1)
	for _, id := range []int64{1, 2, 3} {
		if err := source.Subscribe(1, id, nearProfile); err != nil {
			t.Fatal(err)
		}
	}
	mustFlush(t, manager)
	client := newClientTable()
	client.apply(t, transport.take(1))
	if held := client.held(); !slices.Equal(held, []int64{1, 2}) {
		t.Fatalf("client holds %v", held)
	}
	if _, err := source.SubscribePinned(1, 3, nearProfile); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	client.apply(t, transport.take(1))
	if held := client.held(); !slices.Equal(held, []int64{1, 3}) {
		t.Fatalf("client holds %v, want [1 3]: the pinned subscription evicts 2", held)
	}
	// 同一来源以普通 Subscribe 再说一次即取消钉住。
	if err := source.Subscribe(1, 3, nearProfile); err != nil {
		t.Fatal(err)
	}
	if subj := manager.subject(3); func() bool { subj.mu.Lock(); defer subj.mu.Unlock(); return subj.subscribers[1].pinned() }() {
		t.Fatal("re-subscribing without the pin kept it")
	}
}

// Hold/Ready 从零计：暂缓的订阅全部回到普通待快照，Ready 后按优先级重新准入；计数不重复。
func TestOverLimitHoldReadyStartsOverFromZero(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 2}})
	mustRegister(t, manager, nsSubject(1, "p"), nsSubject(2, "p"), nsSubject(3, "p"))
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 3, nearProfile)
	mustFlush(t, manager)
	mustSubscribe(t, manager, 1, 2, nearProfile)
	mustFlush(t, manager)
	mustSubscribe(t, manager, 1, 1, farProfile) // 来得最晚、优先级最低
	mustFlush(t, manager)
	client := newClientTable()
	client.apply(t, transport.take(1))
	if held := client.held(); !slices.Equal(held, []int64{2, 3}) || manager.Stats().OverLimitWaiting != 1 {
		t.Fatalf("client holds %v waiting %d", held, manager.Stats().OverLimitWaiting)
	}
	if err := manager.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	if waiting := manager.Stats().OverLimitWaiting; waiting != 0 {
		t.Fatalf("waiting %d after Hold, want 0: a new epoch counts from zero", waiting)
	}
	if err := manager.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	frames := transport.take(1)
	if len(frames) != 1 || decodeFrame(t, frames[0]).wire.Kind != frame.Full {
		t.Fatalf("Ready sent %d frame(s), want one Full", len(frames))
	}
	client.apply(t, frames)
	if held := client.held(); !slices.Equal(held, []int64{2, 3}) {
		t.Fatalf("after Ready the client holds %v, want [2 3] by priority", held)
	}
	stats, audit := manager.Stats(), manager.AuditStats()
	if stats.OverLimitWaiting != 1 || audit.OverLimitWaiting != 1 || stats.PendingSnapshots != audit.PendingSnapshots {
		t.Fatalf("stats %+v audit %+v", stats, audit)
	}
}

// 暂缓的订阅不进快照预算、不让 Drain 等待；两种预算只给放得下的新对象。让位的 remove 不受发送预算限制。
func TestOverLimitWaitingDoesNotHoldDrainOrTheBudgets(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{
		Limits:         frame.Limits{MaxObjects: 2},
		SnapshotBudget: SnapshotBudget{PerSessionObjects: 1},
		SendBudget:     SendBudget{MaxBytes: 1}, // 每个窗口只放第一个对象
		Interval:       time.Millisecond,        // 发送预算按 Interval 开新窗口
	})
	flush := func() {
		t.Helper()
		time.Sleep(2 * time.Millisecond)
		mustFlush(t, manager)
	}
	for id := int64(1); id <= 5; id++ {
		mustRegister(t, manager, nsSubject(id, "p"))
	}
	open(t, manager, 1)
	for id := int64(1); id <= 4; id++ {
		mustSubscribe(t, manager, 1, id, nearProfile)
	}
	client := newClientTable()
	for range 8 {
		flush()
		client.apply(t, transport.take(1))
	}
	if held := client.held(); len(held) != 2 || manager.Stats().OverLimitWaiting != 2 {
		t.Fatalf("client holds %v waiting %d, want 2 + 2", held, manager.Stats().OverLimitWaiting)
	}
	if stats := manager.Stats(); stats.PendingSnapshots != 0 || stats.WaitingSnapshotSubjects != 0 {
		t.Fatalf("waiting subscriptions sit in the snapshot budget: %+v", stats)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Drain(ctx); err != nil {
		t.Fatalf("drain with over-limit subscriptions waiting: %v", err)
	}
	// 钉住的对象到来：让位的 remove 与普通 remove 一样不受 1 字节的发送预算限制。
	if _, err := manager.NewSubscriptionSource().SubscribePinned(1, 5, nearProfile); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		flush()
		client.apply(t, transport.take(1))
	}
	if held := client.held(); len(held) != 2 || !slices.Contains(held, 5) || manager.Stats().SessionsLost != 0 {
		t.Fatalf("client holds %v, want the pinned 5 among 2", held)
	}
	if got := heldBy(manager, 1); !slices.Equal(got, client.held()) {
		t.Fatalf("manager %v client %v", got, client.held())
	}
}

// 整轮重试（ErrRetryLater）中断一次带暂缓的 tick 之后，持有、暂缓与客户端照样一致。
func TestOverLimitSurvivesARetriedTick(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 2}})
	for id := int64(1); id <= 4; id++ {
		mustRegister(t, manager, nsSubject(id, "p"))
	}
	open(t, manager, 1)
	client := newClientTable()
	mustSubscribe(t, manager, 1, 1, nearProfile)
	mustFlush(t, manager)
	client.apply(t, transport.take(1))
	for _, id := range []int64{2, 3, 4} {
		mustSubscribe(t, manager, 1, id, nearProfile)
	}
	transport.setRetry(ErrRetryLater)
	if err := manager.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("flush = %v, want ErrRetryLater", err)
	}
	transport.setRetry(nil)
	for range 3 {
		mustFlush(t, manager)
		client.apply(t, transport.take(1))
	}
	if held := client.held(); !slices.Equal(held, []int64{1, 2}) || !slices.Equal(heldBy(manager, 1), held) {
		t.Fatalf("client %v manager %v, want [1 2]", held, heldBy(manager, 1))
	}
	if stats, audit := manager.Stats(), manager.AuditStats(); stats.OverLimitWaiting != 2 || audit.OverLimitWaiting != 2 || stats.SessionsLost != 0 {
		t.Fatalf("stats %+v audit %+v", stats, audit)
	}
}

// 有订阅处于超额暂缓时 CheckHealth 降级并给出数量；恢复后回到 OK。
func TestOverLimitDegradesHealthUntilRecovered(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 1}})
	mustRegister(t, manager, nsSubject(1, "p"), nsSubject(2, "p"))
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 1, nearProfile)
	mustSubscribe(t, manager, 1, 2, nearProfile)
	mustFlush(t, manager)
	if result := manager.CheckHealth(context.Background()); result.Status != health.StatusDegraded || !strings.Contains(result.Message, "over_limit_waiting=1") {
		t.Fatalf("health %+v, want degraded naming over_limit_waiting=1", result)
	}
	everyNamespace := metrics.Labels{"namespace": "*"}
	if got := counterValue(overLimitWaitingMetric, everyNamespace); got != 1 {
		t.Fatalf("%s = %d, want 1", overLimitWaitingMetric, got)
	}
	// 同一进程的第二个 Manager（自身视图与场景常同时存在）：指标是两者之和，关闭后扣掉它的份额，不互相覆盖。
	other := newTestManager(t, newRecordingTransport(), ManagerConfig{Limits: frame.Limits{MaxObjects: 1}})
	mustRegister(t, other, nsSubject(1, "p"), nsSubject(2, "p"))
	open(t, other, 1)
	mustSubscribe(t, other, 1, 1, nearProfile)
	mustSubscribe(t, other, 1, 2, nearProfile)
	mustFlush(t, other)
	if got := counterValue(overLimitWaitingMetric, everyNamespace); got != 2 {
		t.Fatalf("%s = %d with two managers each waiting on one object, want 2", overLimitWaitingMetric, got)
	}
	if err := other.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := counterValue(overLimitWaitingMetric, everyNamespace); got != 1 {
		t.Fatalf("%s = %d after closing the second manager, want 1", overLimitWaitingMetric, got)
	}
	if err := manager.Unsubscribe(1, 1); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		mustFlush(t, manager)
	}
	if result := manager.CheckHealth(context.Background()); result.Status != health.StatusOK {
		t.Fatalf("health %+v after recovery", result)
	}
	if got := counterValue(overLimitWaitingMetric, everyNamespace); got != 0 {
		t.Fatalf("%s = %d after recovery, want 0", overLimitWaitingMetric, got)
	}
}

// 并发 churn（-race）：订阅 / 退订 / 钉住 / Hold / Ready / 标脏与 Flush 并发。每次 Flush 后会话的各类持有数不超过
// 上限、Manager 的引用表与客户端一致、从不丢会话；停下之后暂缓计数、审计与会话暂缓索引一致，退订到上限以内后全部恢复。
func TestOverLimitChurnKeepsTablesConsistent(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{
		Limits:     frame.Limits{MaxObjects: 3},
		Namespaces: map[string]NamespaceLimits{"terrain": {MaxObjects: 2}},
	})
	const sessions, subjects = 4, 12
	states := make(map[int64]*entity.SubjectSyncState)
	for id := int64(1); id <= subjects; id++ {
		namespace := "p"
		if id > 8 {
			namespace = "terrain"
		}
		states[id] = nsSubject(id, namespace)
		mustRegister(t, manager, states[id])
	}
	for sid := SessionID(1); sid <= sessions; sid++ {
		open(t, manager, sid)
	}
	pinned := manager.NewSubscriptionSource()
	clients := make(map[SessionID]*clientTable)
	for sid := SessionID(1); sid <= sessions; sid++ {
		clients[sid] = newClientTable()
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for worker := range 3 {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(seed), 7))
			for {
				select {
				case <-stop:
					return
				default:
				}
				sid := SessionID(1 + r.IntN(sessions))
				id := int64(1 + r.IntN(subjects))
				switch r.IntN(10) {
				case 0, 1, 2:
					_ = manager.Subscribe(sid, id, []entity.SyncProfile{nearProfile, farProfile}[r.IntN(2)])
				case 3, 4:
					_ = manager.Unsubscribe(sid, id)
				case 5:
					_, _ = pinned.SubscribePinned(sid, id, nearProfile)
				case 6:
					_ = pinned.Unsubscribe(sid, id)
				case 7:
					states[id].MarkDirty(1)
				case 8:
					if r.IntN(8) == 0 {
						_ = manager.HoldSession(sid)
						_ = manager.ReadySession(sid)
					}
				default:
					runtime.Gosched()
				}
			}
		}(int64(worker))
	}
	check := func() {
		t.Helper()
		for sid := SessionID(1); sid <= sessions; sid++ {
			clients[sid].apply(t, transport.take(sid))
			checkClassHeld(t, manager, sid)
			manager.mu.RLock()
			sess := manager.sessions[sid]
			for class := range manager.overLimitWaiting {
				if held, limit := sess.heldIn(uint8(class), manager.wire), manager.wire.classLimit(uint8(class)); held > limit {
					manager.mu.RUnlock()
					t.Fatalf("session %d holds %d of class %d, limit %d", sid, held, class, limit)
				}
			}
			manager.mu.RUnlock()
		}
		if lost := manager.Stats().SessionsLost; lost != 0 {
			t.Fatalf("%d session(s) lost under churn", lost)
		}
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		mustFlush(t, manager)
		check()
	}
	close(stop)
	wg.Wait()
	if stats := manager.Stats(); stats.OverLimitDeferred == 0 || stats.OverLimitEvicted == 0 {
		t.Fatalf("the churn never went over a limit (deferred %d, evicted %d); it proves nothing", stats.OverLimitDeferred, stats.OverLimitEvicted)
	}
	for range 4 {
		mustFlush(t, manager)
		check()
	}
	for sid := SessionID(1); sid <= sessions; sid++ {
		if got, want := heldBy(manager, sid), clients[sid].held(); !slices.Equal(got, want) {
			t.Fatalf("session %d: manager %v client %v", sid, got, want)
		}
	}
	// 静止时：计数 = 审计；每个暂缓的订阅都在会话的暂缓索引里。
	stats, audit := manager.Stats(), manager.AuditStats()
	if stats.OverLimitWaiting != audit.OverLimitWaiting || stats.PendingSnapshots != audit.PendingSnapshots {
		t.Fatalf("stats %+v audit %+v", stats, audit)
	}
	for id := int64(1); id <= subjects; id++ {
		subj := manager.subject(id)
		subj.mu.Lock()
		for sid, sub := range subj.subscribers {
			if sub.waitsForRoom() {
				manager.mu.RLock()
				indexed := sub.lifetime.overLimit[id] == subj
				manager.mu.RUnlock()
				if !indexed {
					subj.mu.Unlock()
					t.Fatalf("waiting subscription %d → %d is missing from the session's over-limit index", sid, id)
				}
			}
		}
		subj.mu.Unlock()
	}
	// 退订到上限以内：全部恢复，不剩暂缓。
	for sid := SessionID(1); sid <= sessions; sid++ {
		for id := int64(1); id <= subjects; id++ {
			if id != 1 && id != 9 {
				_ = manager.Unsubscribe(sid, id)
				_ = pinned.Unsubscribe(sid, id)
			}
		}
	}
	for range 4 {
		mustFlush(t, manager)
		check()
	}
	if stats := manager.Stats(); stats.OverLimitWaiting != 0 || manager.AuditStats().OverLimitWaiting != 0 {
		t.Fatalf("waiting %d after shrinking under every limit", stats.OverLimitWaiting)
	}
}

// 仍在暂缓的订阅隔 overLimitRenotifyInterval 再通知一次：政策上次让出的位置不够（或退订的是还没捕获的订阅）时，
// 只通知一次会停在“想要的比放得下的多”上。恢复了的不再通知。
func TestOverLimitRenotifiesWhileStillWaiting(t *testing.T) {
	previous := overLimitRenotifyInterval
	overLimitRenotifyInterval = 20 * time.Millisecond
	t.Cleanup(func() { overLimitRenotifyInterval = previous })
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 1}})
	log := &deferralLog{}
	source := manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{Deferred: log.record})
	mustRegister(t, manager, nsSubject(1, "p"), nsSubject(2, "p"))
	open(t, manager, 1)
	for _, id := range []int64{1, 2} {
		if err := source.Subscribe(1, id, nearProfile); err != nil {
			t.Fatal(err)
		}
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	if notices := log.take(); len(notices) != 1 || notices[0].Subject != 2 {
		t.Fatalf("first notices %+v, want subject 2", notices)
	}
	mustFlush(t, manager)
	if notices := log.take(); len(notices) != 0 {
		t.Fatalf("renotified inside the interval: %+v", notices)
	}
	time.Sleep(3 * overLimitRenotifyInterval)
	mustFlush(t, manager) // 扫描、排队
	mustFlush(t, manager) // 政策阶段交付
	if notices := log.take(); len(notices) != 1 || notices[0] != (DeferredSubscription{Session: 1, Subject: 2}) {
		t.Fatalf("notices after the interval %+v, want subject 2 again", notices)
	}
	if err := source.Unsubscribe(1, 1); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		mustFlush(t, manager)
	}
	log.take()
	time.Sleep(3 * overLimitRenotifyInterval)
	mustFlush(t, manager)
	mustFlush(t, manager)
	if notices := log.take(); len(notices) != 0 || manager.Stats().OverLimitWaiting != 0 {
		t.Fatalf("a recovered subscription was renotified: %+v (waiting %d)", notices, manager.Stats().OverLimitWaiting)
	}
}
