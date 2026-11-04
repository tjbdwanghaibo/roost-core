package cellworld

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// v2 新增的生命周期：Cell 崩溃后玩家重进按 Player 快照重建；Base 消失后 SceneActor 按租约离场；回写按版本幂等；
// 交接事务回滚不移交。

// failingStart 让 StartCell 失败：安装的最后一步，此前实体已新建、状态与区域表都已写入当前事务。
type failingStart struct{ Actor }

func (failingStart) StartCell() error { return errors.New("injected install failure") }

// Cell 崩溃：玩家在 B 挨打（HP 100 → 90）并回写给 Base；B 的进程停止（内存全丢）后重启一个空的 B'。Base 的静默看门狗按
// 最后位置重进，B' 按 Player 的快照建 SceneActor：epoch 比崩溃前大、HP 是快照的 90。
func TestCellCrashPlayerReentersFromSnapshot(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	spec.cellMod = func(cfg *NodeConfig) { cfg.WritebackEvery = 200 * time.Millisecond }
	w := startWorld(t, spec)
	b := CellID{1, 0}
	panel := Panel{MaxHP: 150, Attack: 4, Power: 23, Ver: 1}
	w.base.player(1).setCellPanel(panel)
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	w.enter(2, spatial.Point{X: 12200, Y: 5000})
	time.Sleep(3 * worldPeriod)
	for range 5 {
		w.attack(2, actorID(1), 2)
	}
	w.waitResults(5, 3*time.Second)
	waitFor(t, 3*time.Second, "writeback of hp 90", func() bool { return w.baseState(1).HP == 90 })
	_, before := w.owner(1)
	w.stopCell(b)
	w.real.forget(b)
	time.Sleep(1500 * time.Millisecond) // 超过 SilenceTimeout：第一次重进发给已停的 B，丢失
	w.startCell(b)
	v := w.waitView(1, 5*time.Second, "view after restart", func(v View) bool { return v.Epoch > before })
	if v.Cell != w.grid.SID(b) || v.Self.X != 12000 || v.Self.HP != 90 {
		t.Fatalf("after restart: view from %d at (%d,%d) hp %d, want B at (12000,5000) hp 90", v.Cell, v.Self.X, v.Self.Y, v.Self.HP)
	}
	// 面板随 Base 的 Enter 回到重建的 SceneActor（第 3 项：面板的来源是 Base 的养成输入，崩溃的 Cell 不带走它）。
	if v.Self.Panel != panel {
		t.Fatalf("after restart: panel %+v, want %+v", v.Self.Panel, panel)
	}
	if s := w.base.node.Stats(); s.Reenters == 0 {
		t.Fatalf("base stats = %+v, want re-enters", s)
	}
	w.assertSingleReal(actorID(1), b)
	// 客户端：崩溃期间自己的 mirror 过期删除（witness 停了），重进后它以新版本回来，客户端又持有自己（HP 是快照的 90）。
	// 2026-11-03 接 demo 端到端时发现：mirror 重新出现只 Show 进空间索引，玩家对自己的订阅（Enter 时建立）不会恢复。
	deadline := time.Now().Add(3 * time.Second)
	for {
		self, ok := w.base.client.held(1)[actorID(1)]
		if ok && self.HP == 90 && self.Ver.Epoch > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the client does not hold its own scene actor again: held %v (present %v), removes %v, creates %d, mirror stats %+v",
				self, ok, w.base.client.removed(1), w.base.client.createCount(1, actorID(1)), w.base.mirrors.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Base 进程消失：SceneActor 收不到输入 / 续租，LeaseTimeout 后自行离场，邻格的 Ghost 随删除消息消失。
func TestBaseGoneSceneActorExpires(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	spec.cellMod = func(cfg *NodeConfig) { cfg.LeaseTimeout = time.Second }
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	w.enter(1, spatial.Point{X: 9800, Y: 5000})
	waitFor(t, 2*time.Second, "ghost in B", func() bool { _, ok := w.roleOf(b, actorID(1)); return ok })
	time.Sleep(1500 * time.Millisecond)
	if s, ok := w.roleOf(a, actorID(1)); !ok || s.Role != "real" {
		t.Fatalf("SceneActor %+v (known %v) did not stay while the base renews its lease", s, ok)
	}
	w.base.stop()
	waitFor(t, 3*time.Second, "lease expiry", func() bool { _, ok := w.roleOf(a, actorID(1)); return !ok })
	waitFor(t, 2*time.Second, "ghost removed in B", func() bool { _, ok := w.roleOf(b, actorID(1)); return !ok })
	if got := w.cells[a].node.Stats().LeaseExpired; got != 1 {
		t.Fatalf("lease expirations %d, want 1", got)
	}
}

// 回写按版本幂等：回写消息全部重复、随机延迟（乱序）到达，Base 接受的快照版本严格递增，没有旧快照覆盖新快照。
func TestWritebackIsIdempotentAndOrdered(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	var n atomic.Int64
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind != kindWriteback {
			return false, false, 0
		}
		return false, true, time.Duration(n.Add(37)%120) * time.Millisecond
	})
	spec.cellMod = func(cfg *NodeConfig) { cfg.WritebackEvery = 100 * time.Millisecond }
	w := startWorld(t, spec)
	w.enter(1, spatial.Point{X: 9500, Y: 5000})
	w.move(1, 1000, 0) // 跨一次边界：回写的版本跨 epoch 也单调
	w.waitView(1, 5*time.Second, "crossed", func(v View) bool { return v.Self.X >= 10600 })
	w.move(1, 0, 0)
	time.Sleep(time.Second)
	w.mu.Lock()
	got := append([]EntityState(nil), w.writebacks[actorID(1)]...)
	w.mu.Unlock()
	if len(got) < 5 {
		t.Fatalf("only %d writebacks accepted", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].Ver.Newer(got[i-1].Ver) {
			t.Fatalf("writeback %d version %+v not newer than %+v", i, got[i].Ver, got[i-1].Ver)
		}
	}
	if final := w.baseState(1); final.SnapVer != got[len(got)-1].Ver {
		t.Fatalf("base snapshot version %+v, want the last accepted %+v", final.SnapVer, got[len(got)-1].Ver)
	}
}

// 交接以事务为边界：目标格第一次安装到最后一步失败（实体已新建、状态与 seen 已写，StartCell 报错 → handler 返回错误 →
// 事务回滚：新建的实体撤销、seen 与区域索引还原、不回复），旧格重传同一 epoch 的 Offer，第二次安装成功。如果回滚没有还原
// seen，第二次会被当成“已接受过”只回复不安装，实体就丢了。
func TestHandoffRollbackDoesNotHandOver(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	var failures atomic.Int32
	spec.cellMod = func(cfg *NodeConfig) {
		if cfg.SID != 102 {
			return
		}
		create := cfg.CreateActor
		cfg.CreateActor = func(id int64, kind EntityKind) (Actor, error) {
			a, err := create(id, kind)
			if err != nil || id != actorID(1) || failures.Add(1) != 1 {
				return a, err
			}
			return failingStart{a}, nil
		}
	}
	w := startWorld(t, spec)
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 9950, Y: 5000})
	w.move(1, 1000, 0)
	w.waitView(1, 5*time.Second, "view from B", func(v View) bool { return v.Cell == w.grid.SID(b) })
	w.move(1, 0, 0)
	if failures.Load() < 2 {
		t.Fatalf("install attempted %d times, want the failed one and a retry", failures.Load())
	}
	if sid, epoch := w.owner(1); sid != w.grid.SID(b) || epoch != 2 {
		t.Fatalf("owner sid %d epoch %d, want B at the same epoch 2", sid, epoch)
	}
	if aborted := w.sumStats(func(s NodeStats) uint64 { return s.HandoffsAborted }); aborted != 0 {
		t.Fatalf("%d aborted handoffs", aborted)
	}
	time.Sleep(3 * worldPeriod)
	w.assertSingleReal(actorID(1), b)
}

// E3：handler 名单先于 Node 注册（服务在引擎启动前经 NestHandlerInstaller 注册），同一引擎上不能注册两遍；
// 一份名单只接一个 Node。harness 的每个进程都按这个顺序起（先注册、引擎 Start、再 NewNode）。
func TestHandlersBindToExactlyOneNode(t *testing.T) {
	h := NewHandlers()
	engine := nest.NewEngine(nest.NestOptionWithGetter(fentity.NewManagerAccess(fentity.NewEntityManager())))
	if err := h.Register(engine); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(engine); err == nil {
		t.Fatal("registering the same names twice on one engine succeeded")
	}
	grid, err := NewGrid(GridConfig{Cols: 1, Rows: 1, CellSize: 1000, GhostBand: 100, Hysteresis: 10, Owners: map[CellID]int32{{}: 1}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := NodeConfig{World: "w", SID: 1, Grid: grid, Transport: newLoopback(), Nest: engine, Handlers: h,
		Destroy: func(context.Context, fentity.IThreadSafeEntity) error { return nil },
		Cell:    &CellID{}, Scene: mustID(t, 1, kindTScene), ViewRadius: 50,
		CreateActor: func(int64, EntityKind) (Actor, error) { return nil, errors.New("unused") }}
	if _, err := NewNode(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNode(cfg); err == nil {
		t.Fatal("a second node bound the same handlers")
	}
}

// blockingNest 把一种 handler 的投递挡在准入里，用来造“传输回调正在处理中”的状态；其余照常交给引擎。
type blockingNest struct {
	nest.Client
	handler    nest.HandlerName
	entered    chan struct{}
	release    chan struct{}
	dispatched atomic.Int64
}

func (b *blockingNest) Dispatch(ctx context.Context, h nest.HandlerName, id int64, p nest.Params, opts ...nest.SendOpt) error {
	if h == b.handler {
		b.dispatched.Add(1)
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-b.release
	}
	return b.Client.Dispatch(ctx, h, id, p, opts...)
}

// App 停机第一步关外部输入（App 生命周期方案，main edd1ccb7）：cellworld 的收件（别的进程经 NATS 发来的 Enter / 攻击 /
// 交接 / witness / 队伍快照）是外部输入。CloseIngress 之后到达的消息不再投进 Nest、不再喂 mirror；调用时正在处理的那一条
// （传输回调里的准入）处理完才返回，ctx 先到期返回 ctx 错误、用新 ctx 再调继续等；出站照常（Run 不退出）。
func TestCloseIngressStopsTakingMessagesAndWaitsForTheOneInFlight(t *testing.T) {
	lb := newLoopback()
	grid := worldGrid(t, 2, 1)
	cell := CellID{0, 0}
	var gate *blockingNest
	p := startProc(t, procSpec{sid: grid.SID(cell), cell: &cell, grid: grid, transport: lb, mod: func(cfg *NodeConfig) {
		gate = &blockingNest{Client: cfg.Nest, handler: hInput, entered: make(chan struct{}, 1), release: make(chan struct{})}
		cfg.Nest = gate
	}})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer p.stop()
	defer release() // 失败退出时先放开挡住的回调，否则停传输会一直等它
	send := func(seq uint64) {
		data, err := encodeMsg(&wireMsg{Kind: kindInput, From: 999, Input: &inputReq{ID: actorID(7), Seq: seq, VX: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if err := lb.Publish(Subject("w", p.sid), data); err != nil {
			t.Fatal(err)
		}
	}
	send(1)
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first input never reached the dispatch")
	}
	// 第一条还在准入里：CloseIngress 不能先返回；ctx 到期答 ctx 错误。
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := p.node.CloseIngress(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseIngress with a message in flight = %v, want the ctx error (it must wait for the one in flight)", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- p.node.CloseIngress(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("CloseIngress returned %v while the first message was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseIngress after the one in flight finished = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseIngress did not return after the message in flight finished")
	}
	in := p.node.Stats().MsgsIn
	send(2)
	send(3)
	time.Sleep(200 * time.Millisecond)
	if got := gate.dispatched.Load(); got != 1 {
		t.Fatalf("inputs dispatched after CloseIngress: %d in all, want only the first one", got)
	}
	if s := p.node.Stats(); s.MsgsIn != in {
		t.Fatalf("messages taken after CloseIngress: %d → %d, want none", in, s.MsgsIn)
	}
	// 出站与 Run 不受影响：Run 只在 ctx 结束时返回。
	select {
	case <-p.done:
		t.Fatal("Run returned after CloseIngress; only the receiving side may stop")
	default:
	}
	if err := p.node.CloseIngress(context.Background()); err != nil {
		t.Fatalf("a second CloseIngress = %v, want nil (idempotent)", err)
	}
}
