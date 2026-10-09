package entity_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// RR-20260926-59：ManagerAccess.Unload（RR-30 跳过驱逐与 RR-39 拒绝卸载共用）之后，若该 subject 仍有订阅者，
// 框架在快池之外的重载 worker 上主动从权威重载实体并 Rebind 全量；权威没有该实体或有界重试后仍失败时退回
// remove；无订阅者不重载；并发访问共用一次加载；重载有界；停机取消。

type resyncEntity struct {
	*entity.EntityBase
	label string
}

func (value *resyncEntity) Base() *entity.EntityBase { return value.EntityBase }

func newResyncEntity(id int64, label string) *resyncEntity {
	value := &resyncEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false), label: label}
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.CopyFrozenSyncPayload(1, []byte(value.label)), nil
	}
	value.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: id, Namespace: "test", Packer: entity.SubjectSyncPackFunc{
		Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) },
	}})
	return value
}

// scriptedLoader 是 AggregateLoader：按脚本返回权威状态，发布经 entity.RunLocal（与 EntityRepository 相同的执行位置约定）。
type scriptedLoader struct {
	manager  *entity.EntityManager
	label    string
	err      error
	gate     chan struct{} // 非 nil 时每次加载等它关闭或 ctx 结束
	loads    atomic.Int32
	inFlight atomic.Int32
	peak     atomic.Int32
	canceled atomic.Int32
}

func (loader *scriptedLoader) LoadEntity(ctx context.Context, id int64, _ entity.EntityKind) (entity.IThreadSafeEntity, error) {
	loader.loads.Add(1)
	current := loader.inFlight.Add(1)
	defer loader.inFlight.Add(-1)
	for {
		peak := loader.peak.Load()
		if current <= peak || loader.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	if loader.gate != nil {
		select {
		case <-loader.gate:
		case <-ctx.Done():
			loader.canceled.Add(1)
			return nil, ctx.Err()
		}
	}
	if loader.err != nil {
		return nil, loader.err
	}
	fresh := newResyncEntity(id, loader.label)
	var addErr error
	if err := entity.RunLocal(ctx, func() { addErr = loader.manager.TryAdd(fresh) }); err != nil {
		return nil, err
	}
	if errors.Is(addErr, entity.ErrEntityExists) {
		// 与 EntityRepository 相同：并发加载已先发布同一实体时返回已发布的实例。
		if existing := loader.manager.Get(id); existing != nil {
			return existing, nil
		}
	}
	if addErr != nil {
		return nil, addErr
	}
	return fresh, nil
}

type resyncHarness struct {
	t       *testing.T
	manager *entity.EntityManager
	access  *entity.ManagerAccess
	sync    *entitysync.Manager
	loader  *scriptedLoader
	frames  chan []byte
	runs    atomic.Int32
	stop    func(context.Context) error
	// unhookLoader 注销 loader（DataEngine Runtime 停机时的动作），取消在途共享加载（RR-20260926-54）。
	unhookLoader func()
}

func newResyncHarness(t *testing.T, loader *scriptedLoader, config entity.UnloadResyncConfig) *resyncHarness {
	t.Helper()
	h := &resyncHarness{t: t, manager: entity.NewEntityManager(), frames: make(chan []byte, 256), loader: loader}
	h.access = entity.NewManagerAccess(h.manager)
	loader.manager = h.manager
	unhook, err := h.access.ConfigureLoader(loader)
	if err != nil {
		t.Fatal(err)
	}
	h.unhookLoader = unhook
	h.sync, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		h.frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.sync.Close(context.Background()) })
	if err = h.sync.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	// 本地执行入口替身：Nest 构造时经 BindLocalExecutor 绑定 NestMgr.RunLocal；这里记录发布确实经过它。
	h.access.BindLocalExecutor(func(fn func()) error {
		h.runs.Add(1)
		fn()
		return nil
	})
	h.stop, err = h.access.ConfigureUnloadResync(h.sync, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.stop(context.Background()) })
	return h
}

// resident 登记一个常驻实体；subscribe 为真时会话 1 订阅并收到创建帧。
func (h *resyncHarness) resident(id int64, label string, subscribe bool) *resyncEntity {
	h.t.Helper()
	value := newResyncEntity(id, label)
	h.manager.Add(value)
	if err := h.sync.Register(value.Sync()); err != nil {
		h.t.Fatal(err)
	}
	if subscribe {
		if err := h.sync.Subscribe(1, id, entity.SyncProfile{}); err != nil {
			h.t.Fatal(err)
		}
		if err := h.sync.Flush(context.Background()); err != nil {
			h.t.Fatal(err)
		}
		h.nextFrame(time.Second, "the initial create")
	}
	return value
}

func (h *resyncHarness) nextFrame(timeout time.Duration, what string) frame.Frame {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case raw := <-h.frames:
			decoded, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
			if err != nil {
				h.t.Fatal(err)
			}
			return decoded
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no frame within %v: %s", timeout, what)
		}
		if err := h.sync.Flush(context.Background()); err != nil {
			h.t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *resyncHarness) unload(value entity.IThreadSafeEntity) {
	h.t.Helper()
	if err := h.access.Unload(context.Background(), value); err != nil {
		h.t.Fatal(err)
	}
}

func payloadOf(t *testing.T, object frame.ObjectDelta) string {
	t.Helper()
	if len(object.Components) != 1 {
		t.Fatalf("object carries %d components", len(object.Components))
	}
	update, err := entitysync.DecodeSubjectUpdate(object.Components[0].Data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !update.Full {
		t.Fatalf("update is not full: %+v", update)
	}
	return string(update.Payload.BytesCopy())
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUnloadReloadsSubscribedSubjectFromAuthority(t *testing.T) {
	h := newResyncHarness(t, &scriptedLoader{label: "authority"}, entity.UnloadResyncConfig{})
	stale := h.resident(5001, "rejected", true)
	h.unload(stale)
	got := h.nextFrame(3*time.Second, "unloaded with a subscriber, but no authoritative full reached it")
	if len(got.Objects) != 1 || got.Objects[0].Operation != frame.ObjectUpdate {
		t.Fatalf("frame after unload=%+v, want one full ObjectUpdate of the held object", got.Objects)
	}
	if payload := payloadOf(t, got.Objects[0]); payload != "authority" {
		t.Fatalf("subscriber got %q after the unload, want the authority state", payload)
	}
	if loads := h.loader.loads.Load(); loads != 1 {
		t.Fatalf("loads=%d, want 1", loads)
	}
	if h.runs.Load() == 0 {
		t.Fatal("reload did not publish through the injected local executor")
	}
	fresh := h.manager.Get(5001)
	if fresh == nil || fresh == entity.IThreadSafeEntity(stale) {
		t.Fatal("reloaded instance is not resident")
	}
	if stats := h.access.UnloadResyncStats(); stats.Reloaded != 1 || stats.Retracted != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestUnloadWithoutSubscribersDoesNotReload(t *testing.T) {
	h := newResyncHarness(t, &scriptedLoader{label: "authority"}, entity.UnloadResyncConfig{})
	h.unload(h.resident(5002, "rejected", false))
	h.unload(newResyncEntity(5003, "never registered with sync"))
	if stats := h.access.UnloadResyncStats(); stats.Scheduled != 0 {
		t.Fatalf("unload without subscribers scheduled a reload: %+v", stats)
	}
	if loads := h.loader.loads.Load(); loads != 0 {
		t.Fatalf("loads=%d, want none", loads)
	}
	if h.manager.Get(5002) != nil {
		t.Fatal("entity without subscribers was reloaded")
	}
}

func TestUnloadRetractsWhenAuthorityHasNoEntity(t *testing.T) {
	h := newResyncHarness(t, &scriptedLoader{err: errors.Join(entity.ErrAuthorityEntityNotFound, errors.New("aggregate missing"))}, entity.UnloadResyncConfig{RetryMin: time.Millisecond})
	h.unload(h.resident(5004, "rejected", true))
	got := h.nextFrame(3*time.Second, "authority has no such entity, but the subscriber got no remove")
	if len(got.Objects) != 1 || got.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("frame after reload found no authority=%+v, want ObjectRemove", got.Objects)
	}
	if loads := h.loader.loads.Load(); loads != 1 {
		t.Fatalf("absent authority was retried: loads=%d", loads)
	}
	// remove-before-create：退役完成后，同 ID 的新实例重新登记是新 subject。
	if err := h.sync.Register(newResyncEntity(5004, "created later").Sync()); err != nil {
		t.Fatalf("register after the remove: %v", err)
	}
}

func TestUnloadRetractsAfterBoundedReloadFailures(t *testing.T) {
	h := newResyncHarness(t, &scriptedLoader{err: errors.New("mongo unavailable")}, entity.UnloadResyncConfig{Attempts: 3, RetryMin: time.Millisecond, RetryMax: 2 * time.Millisecond})
	h.unload(h.resident(5005, "rejected", true))
	got := h.nextFrame(3*time.Second, "reload kept failing, but the subscriber got no remove")
	if len(got.Objects) != 1 || got.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("frame after persistent reload failure=%+v, want ObjectRemove", got.Objects)
	}
	if loads := h.loader.loads.Load(); loads != 3 {
		t.Fatalf("loads=%d, want exactly the 3 bounded attempts", loads)
	}
	if stats := h.access.UnloadResyncStats(); stats.Failures != 3 || stats.Retracted != 1 || stats.Reloaded != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

// 业务并发访问与重复登记：同一实体重复卸载触发只登记一次；业务在重载期间访问同一实体，与重载共用一次加载。
func TestUnloadResyncSharesTheLoadWithConcurrentAccess(t *testing.T) {
	gate := make(chan struct{})
	h := newResyncHarness(t, &scriptedLoader{label: "authority", gate: gate}, entity.UnloadResyncConfig{})
	stale := h.resident(5006, "rejected", true)
	h.unload(stale)
	h.unload(stale) // 已卸载：幂等，不重复登记
	waitFor(t, "the reload to reach the loader", func() bool { return h.loader.inFlight.Load() == 1 })
	business := make(chan entity.IThreadSafeEntity, 1)
	go func() {
		value, err := h.access.Get(context.Background(), 5006, entity.EntityCategoryNone)
		if err != nil {
			t.Error(err)
		}
		business <- value
	}()
	close(gate)
	got := h.nextFrame(3*time.Second, "the shared reload")
	if len(got.Objects) != 1 || payloadOf(t, got.Objects[0]) != "authority" {
		t.Fatalf("frame=%+v", got.Objects)
	}
	value := <-business
	if value == nil || value != h.manager.Get(5006) {
		t.Fatal("business access and the reload got different instances")
	}
	if loads := h.loader.loads.Load(); loads != 1 {
		t.Fatalf("loads=%d, want one shared load", loads)
	}
	if stats := h.access.UnloadResyncStats(); stats.Scheduled != 1 {
		t.Fatalf("repeated unload registered %d reloads", stats.Scheduled)
	}
}

// 重载风暴：大量被卸载实体同时有订阅者。并发加载不超过 Workers，等待队列不超过 QueueCapacity，
// 放不下的立即退回 remove（订阅者不停留在错误内容上），其余在放行后全部重载。
func TestUnloadResyncStormIsBounded(t *testing.T) {
	const entities, workers, queue = 40, 2, 8
	gate := make(chan struct{})
	h := newResyncHarness(t, &scriptedLoader{label: "authority", gate: gate}, entity.UnloadResyncConfig{Workers: workers, QueueCapacity: queue})
	stale := make([]*resyncEntity, 0, entities)
	for i := 0; i < entities; i++ {
		value := h.resident(int64(6000+i), "rejected", false)
		if err := h.sync.Subscribe(1, value.ID(), entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, value)
	}
	if err := h.sync.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for len(h.frames) > 0 {
		<-h.frames
	}
	for _, value := range stale {
		h.unload(value)
	}
	stats := h.access.UnloadResyncStats()
	if stats.Scheduled+stats.Overflow != entities || stats.Scheduled > workers+queue {
		t.Fatalf("storm stats=%+v, want at most %d accepted and the rest overflowed", stats, workers+queue)
	}
	if stats.Retracted != stats.Overflow {
		t.Fatalf("overflowed subjects were not retracted: %+v", stats)
	}
	close(gate)
	waitFor(t, "the accepted reloads", func() bool { return h.access.UnloadResyncStats().Reloaded == stats.Scheduled })
	if peak := h.loader.peak.Load(); peak > workers {
		t.Fatalf("peak concurrent reloads=%d, want <= %d", peak, workers)
	}
}

// 停机：Stop 让 worker 离开在途等待并退出；取消不是“重载失败”，不向订阅者发 remove。重载就是一次共享冷加载
// （RR-20260926-54）：worker 离开不取消加载本身，loader 注销（DataEngine 停机）才取消它。
func TestUnloadResyncStopCancelsInflightReload(t *testing.T) {
	h := newResyncHarness(t, &scriptedLoader{label: "authority", gate: make(chan struct{})}, entity.UnloadResyncConfig{})
	h.unload(h.resident(5007, "rejected", true))
	waitFor(t, "the reload to reach the loader", func() bool { return h.loader.inFlight.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.stop(ctx); err != nil {
		t.Fatalf("stop did not cancel the in-flight reload: %v", err)
	}
	h.unhookLoader()
	waitFor(t, "loader unregistration to cancel the shared load", func() bool { return h.loader.canceled.Load() == 1 })
	if stats := h.access.UnloadResyncStats(); stats.Retracted != 0 {
		t.Fatalf("shutdown retracted subjects: %+v", stats)
	}
	h.unload(h.resident(5008, "after stop", true))
	if stats := h.access.UnloadResyncStats(); stats.Scheduled != 1 {
		t.Fatalf("stopped resync scheduled new work: %+v", stats)
	}
}
