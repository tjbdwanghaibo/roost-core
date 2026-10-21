package heartbeat

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 测试用实体种类（本测试二进制内唯一）。
const (
	kindShort  entity.EntityKind = 231
	kindLong   entity.EntityKind = 232
	kindRemote entity.EntityKind = 233
)

func init() {
	entity.MustRegisterEntityKindDefs(
		entity.EntityKindDef{Kind: kindShort, Category: entity.EntityCategoryOther, BusinessPool: entity.BusinessPoolShort},
		entity.EntityKindDef{Kind: kindLong, Category: entity.EntityCategoryOther, BusinessPool: entity.BusinessPoolLong},
	)
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category:     entity.EntityCategoryRemote,
		Kind:         kindRemote,
		RemotePolicy: entity.RemotePolicyManaged,
		NoPersist:    true,
		Builder:      func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) { return nil, nil },
		Lifetime:     entity.EntityLifetimeRemoteManaged,
	})
}

func mustID(t testing.TB, unique int64, kind entity.EntityKind) int64 {
	t.Helper()
	id, err := entity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// hbEntity 是装了一个心跳宿主的实体；OnDestroy 像组件那样撤销宿主。
type hbEntity struct {
	*entity.EntityBase
	host *Host
}

func (e *hbEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *hbEntity) OnDestroy(entity.EntityDestroyReason) {
	if e.host != nil {
		e.host.OnDestroy()
	}
}

func newHBEntity(t testing.TB, d *Driver, unique int64, kind entity.EntityKind) *hbEntity {
	t.Helper()
	e := &hbEntity{EntityBase: entity.NewEntityBase(mustID(t, unique, kind), entity.EntityCategoryOther, true, kind)}
	e.host = d.NewHost(e)
	return e
}

// remoteHBEntity 是 Remote 托管实体（嵌 RemoteEntityBase）。
type remoteHBEntity struct {
	*entity.RemoteEntityBase
	host *Host
}

func (e *remoteHBEntity) Base() *entity.EntityBase { return &e.RemoteEntityBase.EntityBase }
func (e *remoteHBEntity) OnDestroy(entity.EntityDestroyReason) {
	if e.host != nil {
		e.host.OnDestroy()
	}
}

// fakeClock 是可控的业务时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeClient 记录 Dispatch；其余方法不该被调用（嵌入的 nil 接口会 panic）。
type fakeClient struct {
	nest.Client
	mu      sync.Mutex
	sent    []sentBeat
	err     error
	block   chan struct{} // 非 nil 时第一次 Dispatch 先发 entered 再等它
	entered chan struct{}
	once    sync.Once
}

type sentBeat struct {
	frame   int64
	reg     *Reg
	handler string
}

func (c *fakeClient) Dispatch(ctx context.Context, name nest.HandlerName, id int64, params nest.Params, _ ...nest.SendOpt) error {
	if c.entered != nil {
		// 第一次 Dispatch 停在这里直到驱动停止（ctx 取消），模拟停止时正有一条投递在进行。
		c.once.Do(func() {
			close(c.entered)
			<-ctx.Done()
		})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := params[0].(*Reg)
	if r.host.id != id {
		panic("dispatch id does not match the registration's entity")
	}
	c.sent = append(c.sent, sentBeat{frame: r.host.driver.frame(), reg: r, handler: name.String()})
	return c.err
}

func (c *fakeClient) take() []sentBeat {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.sent
	c.sent = nil
	return out
}

func (d *Driver) frame() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ring.frame
}

// newTestDriver 返回接上 fakeClient 与按 map 查找的驱动（不启动循环；测试直接 step）。
func newTestDriver(t testing.TB, logger *slog.Logger) (*Driver, *fakeClient, *fakeClock, *sync.Map) {
	t.Helper()
	d := NewDriver(Config{Name: "test", Logger: logger})
	clk := newFakeClock()
	d.SetClock(clk.Now)
	client := &fakeClient{}
	var live sync.Map
	lookup := func(id int64) entity.IThreadSafeEntity {
		if v, ok := live.Load(id); ok {
			return v.(entity.IThreadSafeEntity)
		}
		return nil
	}
	if err := d.Bind(client, lookup); err != nil {
		t.Fatal(err)
	}
	return d, client, clk, &live
}

// execute 像 Nest 一样在“实体锁”内执行一拍（不经 Nest：无事务，回调侧记录不随回滚）。
func execute(t testing.TB, b sentBeat) {
	t.Helper()
	if err := b.reg.host.driver.runBeat(b.reg.host.owner, b.reg); err != nil {
		t.Fatalf("beat: %v", err)
	}
}

// countingHandler 数 Warn 记录。
type countingHandler struct {
	mu    sync.Mutex
	warns []slog.Record
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.mu.Lock()
		h.warns = append(h.warns, r)
		h.mu.Unlock()
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }
func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.warns)
}
