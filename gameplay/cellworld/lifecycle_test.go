package cellworld

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 入场身份必须来自持久计数；测试替身只省掉与此无关的心跳启动。
type entryPlayer struct{ *testPlayer }

func (*entryPlayer) StartBase() error { return nil }

func TestEntryAdvancesPersistedCounterWithoutWallClock(t *testing.T) {
	p := &entryPlayer{testPlayer: &testPlayer{dao: &undoCell[BaseState]{v: BaseState{Entry: 7}}}}
	n := &Node{cfg: NodeConfig{Grid: worldGrid(t, 1, 1)}, out: make(chan job, 4)}
	_, err := n.handleBaseCmd([]fentity.IThreadSafeEntity{p}, []any{baseCmd{kind: cmdEnter, actor: actorID(1), pos: spatial.Point{X: 100, Y: 100}, hp: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.BaseState().Entry; got != 8 {
		t.Fatalf("entry = %d, want persisted counter 8 independent of the wall clock", got)
	}
}

func TestAttackIdentitySurvivesResetSequence(t *testing.T) {
	state := BaseState{Actor: actorID(1), InWorld: true, Cell: 101, Entry: 1}
	attack := func() HitID {
		p := &entryPlayer{testPlayer: &testPlayer{dao: &undoCell[BaseState]{v: state}}}
		n := &Node{out: make(chan job, 1)}
		got, err := n.handleBaseCmd([]fentity.IThreadSafeEntity{p}, []any{baseCmd{kind: cmdAttack, target: npcID(1), amount: 1}})
		if err != nil {
			t.Fatal(err)
		}
		return got.(HitID)
	}
	if first, second := attack(), attack(); first == second {
		t.Fatalf("recreated Base reused attack identity: %+v", first)
	}
}

func TestOldInputCannotRenewOrMoveANewEntry(t *testing.T) {
	n := &Node{cfg: NodeConfig{LeaseTimeout: time.Second, MaxSpeed: 2000}}
	st := ActorState{EntityState: EntityState{ID: actorID(1), Kind: KindPlayer, Base: 1001}, Role: RoleReal, Entry: 2, LeaseUntil: 100, LastInput: 0}
	a := &plainActor{EntityBase: fentity.NewEntityBase(st.ID, fentity.EntityCategoryOther, true, kindTActor), st: st}
	old := BaseState{Actor: st.ID, Entry: 1, IntentSeq: 100, VX: 20}
	m := n.inputMsg(&old, time.Now())
	m.From = st.Base
	if _, err := n.handleInput([]fentity.IThreadSafeEntity{a}, []any{m}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.st, st) {
		t.Fatalf("old entry's input changed new actor: %+v", a.st)
	}
	current := old
	current.Entry = st.Entry
	m = n.inputMsg(&current, time.Now())
	m.From = st.Base
	if _, err := n.handleInput([]fentity.IThreadSafeEntity{a}, []any{m}); err != nil {
		t.Fatal(err)
	}
	if a.st.VX != 20 || a.st.LastInput != 100 || a.st.LeaseUntil <= st.LeaseUntil {
		t.Fatalf("current entry input was not applied: %+v", a.st)
	}
}

func TestOldEnterCannotRenewOrRebindAReal(t *testing.T) {
	n := plainNode(t, worldGrid(t, 1, 1), CellID{0, 0}, map[int64]*plainActor{})
	st := ActorState{EntityState: EntityState{ID: actorID(1), Kind: KindPlayer, Base: 1001}, Role: RoleReal, Entry: 2, LeaseUntil: 100}
	claimPlain(t, n, &st)
	a := &plainActor{EntityBase: fentity.NewEntityBase(st.ID, fentity.EntityCategoryOther, true, kindTActor), st: st}
	for _, req := range []struct {
		from  int32
		entry uint64
	}{{1001, 1}, {1002, 3}} {
		if err := n.onEnter(NewRegion(), a, req.from, &enterReq{State: st.EntityState, Entry: req.entry}, time.Now(), &txEffects{}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a.st, st) {
			t.Fatalf("stale or foreign Enter changed Real: %+v", a.st)
		}
	}
	if err := n.onEnter(NewRegion(), a, st.Base, &enterReq{State: st.EntityState, Entry: 3}, time.Now(), &txEffects{}); err != nil {
		t.Fatal(err)
	}
	resolvePlain(t, n, NewRegion(), a)
	if a.st.Entry != 3 || a.st.LeaseUntil <= st.LeaseUntil {
		t.Fatalf("current Base cannot enter: %+v", a.st)
	}
}

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
	w.enter(2, spatial.Point{X: 12200, Y: 5000}) // 同在 B，enter 已等到第一份 witness：同格攻击没有别的前提
	for range 5 {
		w.attack(2, actorID(1), 2)
	}
	w.waitResults(5, settleWithin)
	waitFor(t, settleWithin, "writeback of hp 90", func() bool { return w.baseState(1).HP == 90 })
	_, before := w.owner(1)
	reenters := w.base.node.Stats().Reenters
	w.stopCell(b)
	w.real.forget(b)
	// 超过 SilenceTimeout 后 Base 的第一次重进发给已停的 B、丢失；等它真的发出（原来 sleep 1.5s），再重启 B。
	waitFor(t, settleWithin, "the first re-enter to the stopped cell", func() bool { return w.base.node.Stats().Reenters > reenters })
	w.startCell(b)
	v := w.waitView(1, settleWithin, "view after restart", func(v View) bool { return v.Epoch > before })
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
	eventually(t, settleWithin, "the client to hold its own scene actor again", func() (bool, string) {
		self, ok := w.base.client.held(1)[actorID(1)]
		return ok && self.HP == 90 && self.Ver.Epoch > before, fmt.Sprintf("held %v (present %v), removes %v, creates %d, mirror stats %+v",
			self, ok, w.base.client.removed(1), w.base.client.createCount(1, actorID(1)), w.base.mirrors.Stats())
	})
}

// Base 进程消失：SceneActor 收不到输入 / 续租，LeaseTimeout 后自行离场，邻格的 Ghost 随删除消息消失。
func TestBaseGoneSceneActorExpires(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	spec.cellMod = func(cfg *NodeConfig) { cfg.LeaseTimeout = time.Second }
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	w.enter(1, spatial.Point{X: 9800, Y: 5000})
	waitFor(t, settleWithin, "ghost in B", func() bool { _, ok := w.roleOf(b, actorID(1)); return ok })
	time.Sleep(1500 * time.Millisecond) // 负向窗口（长于 LeaseTimeout 1s）：Base 在续租，角色不该离场
	if s, ok := w.roleOf(a, actorID(1)); !ok || s.Role != "real" {
		t.Fatalf("SceneActor %+v (known %v) did not stay while the base renews its lease", s, ok)
	}
	w.base.stop()
	waitFor(t, settleWithin, "lease expiry", func() bool { _, ok := w.roleOf(a, actorID(1)); return !ok })
	waitFor(t, settleWithin, "ghost removed in B", func() bool { _, ok := w.roleOf(b, actorID(1)); return !ok })
	if got := w.cells[a].node.Stats().LeaseExpired; got != 1 {
		t.Fatalf("lease expirations %d, want 1", got)
	}
}

// capturedMsg 是 loopback 上截下的一条已编码消息：之后原样重放，模拟传输的重复 / 迟到副本。
type capturedMsg struct {
	subject string
	data    []byte
}

// captureFirst 截下（并照常投递）第一条 kind 消息。
func captureFirst(lb *loopback, kind msgKind) <-chan capturedMsg {
	saved := make(chan capturedMsg, 1)
	lb.setFault(func(subject string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind == kind {
			if raw, err := encodeMsg(m); err == nil {
				select {
				case saved <- capturedMsg{subject: subject, data: raw}:
				default:
				}
			}
		}
		return false, false, 0
	})
	return saved
}

// leaveAndCapture 让玩家离场，返回这次 Leave 的编码消息，并等 cell 上的角色被删。
func (w *world) leaveAndCapture(lb *loopback, unique int64, cell CellID) capturedMsg {
	w.t.Helper()
	saved := captureFirst(lb, kindLeave)
	ctx, cancel := w.ctx()
	defer cancel()
	if err := w.base.node.Leave(ctx, playerEntityID(unique)); err != nil {
		w.t.Fatal(err)
	}
	var old capturedMsg
	select {
	case old = <-saved:
	case <-time.After(settleWithin):
		w.t.Fatal("leave not captured")
	}
	lb.setFault(nil)
	waitFor(w.t, settleWithin, "actor removed by its own leave", func() bool { _, ok := w.roleOf(cell, actorID(unique)); return !ok })
	return old
}

// requireStaysReal 重放 old，断言角色一直是 cell 的 Real，直到 cell 真的把它当作过期丢掉（StaleDrops 增加）且至少过了
// d。原来只看固定的 d：负载下重放的消息可能在窗口之后才被处理，窗口就什么也没看到；现在等到“处理过了”的证据。
func (w *world) requireStaysReal(lb *loopback, old capturedMsg, unique int64, cell CellID, d time.Duration, what string) {
	w.t.Helper()
	stale := w.cells[cell].node.Stats().StaleDrops
	if err := lb.Publish(old.subject, old.data); err != nil {
		w.t.Fatal(err)
	}
	start := time.Now()
	eventually(w.t, settleWithin, "the replayed message to be dropped as stale", func() (bool, string) {
		if s, ok := w.roleOf(cell, actorID(unique)); !ok || s.Role != "real" {
			w.t.Fatalf("%s: present=%v role=%q in %v", what, ok, s.Role, cell)
		}
		got := w.cells[cell].node.Stats().StaleDrops
		return got > stale && time.Since(start) >= d, fmt.Sprintf("stale drops in %v %d -> %d", cell, stale, got)
	})
}

// requireLeaveRemoves 让玩家离场，断言 cell 上的角色是被这次 Leave 删掉的（不是租约到期）。
func (w *world) requireLeaveRemoves(unique int64, cell CellID) {
	w.t.Helper()
	ctx, cancel := w.ctx()
	defer cancel()
	if err := w.base.node.Leave(ctx, playerEntityID(unique)); err != nil {
		w.t.Fatal(err)
	}
	waitFor(w.t, settleWithin, "actor removed by the current leave", func() bool { _, ok := w.roleOf(cell, actorID(unique)); return !ok })
	if got := w.cells[cell].node.Stats().LeaseExpired; got != 0 {
		w.t.Fatalf("actor in %v left by lease expiry (%d), not by the leave", cell, got)
	}
}

// RV-20261106-03：Leave 只结束发出它的那一次入场。同一玩家离场后以同 ID 重新入场，传输把第一次的 Leave 重复 / 迟到
// 送达（Transport 允许）：新角色必须留下；当前这次入场的 Leave 照常删除它。
func TestLateLeaveDoesNotRemoveReenteredActor(t *testing.T) {
	spec, lb := loopbackSpec(t, 1, 1)
	w := startWorld(t, spec)
	cell := CellID{0, 0}
	first := w.enter(1, spatial.Point{X: 5000, Y: 5000})
	old := w.leaveAndCapture(lb, 1, cell)
	w.resetViews(1)
	second := w.enter(1, spatial.Point{X: 5000, Y: 5000})
	if second.Epoch <= first.Epoch {
		t.Fatalf("epoch did not advance: %d -> %d", first.Epoch, second.Epoch)
	}
	w.requireStaysReal(lb, old, 1, cell, 300*time.Millisecond,
		fmt.Sprintf("old Leave deleted the re-entered actor (old epoch %d, new epoch %d)", first.Epoch, second.Epoch))
	w.requireLeaveRemoves(1, cell)
}

// RV-20261106-03：入场身份随交接带走。第一次入场在 B 离场（Leave 发给 B）；第二次在 A 入场再走进 B（交接装出的 Real）。
// 第一次的 Leave 迟到到达 B 不能删它；第二次的 Leave 能删——交接后的 Real 仍认得自己是哪一次入场。
func TestLateLeaveAfterHandoffKeepsTheNewEntry(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 10500, Y: 5000})
	old := w.leaveAndCapture(lb, 1, b)
	if want := Subject("w", w.grid.SID(b)); old.subject != want {
		t.Fatalf("old leave went to %s, want %s", old.subject, want)
	}
	w.resetViews(1)
	w.enter(1, spatial.Point{X: 9500, Y: 5000})
	w.move(1, 1000, 0)
	w.waitView(1, settleWithin, "handed over to B", func(v View) bool { return v.Cell == w.grid.SID(b) && v.Self.X >= 10300 })
	w.move(1, 0, 0)
	w.requireStaysReal(lb, old, 1, b, 300*time.Millisecond, "old Leave deleted the actor handed over to B")
	w.requireLeaveRemoves(1, b)
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
	w.waitView(1, settleWithin, "crossed", func(v View) bool { return v.Self.X >= 10600 })
	w.move(1, 0, 0)
	accepted := func() []EntityState {
		w.mu.Lock()
		defer w.mu.Unlock()
		return append([]EntityState(nil), w.writebacks[actorID(1)]...)
	}
	// 原来 sleep 1s 后要求至少 5 份：等到有 5 份，再让 Base 跑 5 拍（≥ 0.5s，盖过注入的最多 120ms 延迟），让晚到的重复
	// 副本都到达，再查整段的版本单调。
	eventually(t, settleWithin, "5 accepted writebacks", func() (bool, string) {
		n := len(accepted())
		return n >= 5, fmt.Sprintf("only %d writebacks accepted", n)
	})
	w.base.waitBaseBeats(5)
	got := accepted()
	for i := 1; i < len(got); i++ {
		if !got[i].Ver.Newer(got[i-1].Ver) {
			t.Fatalf("writeback %d version %+v not newer than %+v", i, got[i].Ver, got[i-1].Ver)
		}
	}
	// 角色停下后仍按 WritebackEvery 定时回写，Hooks.Writeback 在 Base 的事务提交之后才调用：读 Base 状态与读 hook 记录之间
	// 可能又接受了新的回写。所以断言 Base 当前的快照版本不旧于此前接受的最后一份，且就是 hook 记下的某一份（等 hook 跟上）。
	last := got[len(got)-1].Ver
	final := w.baseState(1)
	if last.Newer(final.SnapVer) {
		t.Fatalf("base snapshot version %+v went back behind the last accepted %+v", final.SnapVer, last)
	}
	waitFor(t, settleWithin, "the hook for the base snapshot", func() bool {
		return slices.ContainsFunc(accepted(), func(s EntityState) bool { return s.Ver == final.SnapVer })
	})
}

// fillDistinct 给 v（指向结构体）的每个导出字段填互不相同的非零值（递归进嵌入与嵌套结构体），跳过切片与指针。
// 往返测试用它：以后新增的字段自动被覆盖。
func fillDistinct(v reflect.Value, next *int64) {
	for i := range v.NumField() {
		f, sf := v.Field(i), v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		*next++
		switch f.Kind() {
		case reflect.Struct:
			fillDistinct(f, next)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			f.SetInt(*next)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			f.SetUint(uint64(*next))
		}
	}
}

// RV-20261106-04：BaseState / ActorState 是接入方会整份保存或传递的公开状态，MsgPack 往返必须保留每个字段（曾经 X/Y、
// VX/VY 共用一个标签，编码丢了 X 与 VX）。交接 Offer 带走 ActorState 的一部分（含位置零头），同样逐字段往返。
func TestStateMsgpackRoundTrip(t *testing.T) {
	for _, want := range []any{&BaseState{}, &ActorState{}, &handoffOffer{}} {
		var next int64
		fillDistinct(reflect.ValueOf(want).Elem(), &next)
		raw, err := msgpack.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		got := reflect.New(reflect.TypeOf(want).Elem())
		if err := msgpack.Unmarshal(raw, got.Interface()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Interface(), want) {
			t.Fatalf("%T round trip lost fields:\nwant %+v\ngot  %+v", want, want, got.Interface())
		}
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
	w.waitView(1, settleWithin, "view from B", func(v View) bool { return v.Cell == w.grid.SID(b) })
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
	w.assertSingleReal(actorID(1), b) // 等到结论成立（有期限）
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
	retire := attachTestOwners(t, grid, &cfg)
	defer retire()
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
	time.Sleep(200 * time.Millisecond) // 负向窗口：CloseIngress 之后到达的消息不再投递（回环同步投递，窗口只防异步漏网）
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

func TestUncommittedWitnessDoesNotPublishMirrors(t *testing.T) {
	spec, _ := loopbackSpec(t, 1, 1)
	w := startWorld(t, spec)
	n := w.base.node
	actor := actorID(7881)
	st := BaseState{Actor: actor, InWorld: true, Cell: w.cells[CellID{0, 0}].sid, Epoch: 1}
	view := &witnessMsg{Observer: actor, Epoch: 1, Self: EntityState{ID: actor, Kind: KindPlayer, X: 5000, Y: 5000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}}
	_, err := nest.RunDetachedTransaction(context.Background(), nullCommitter{}, "rejected witness", func() (any, error) {
		fx := &txEffects{}
		n.onWitness(&st, st.Cell, 0, view, time.Now(), fx)
		n.registerCommit(fx)
		return nil, errors.New("reject before commit")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if _, _, ok := n.cfg.Mirrors.Get(actor); ok {
		t.Fatal("uncommitted witness escaped through mirror")
	}
}

// 静止内容不推进版本，但每份已接受的视野仍证明对象存活，不能在 TTL 后被反复删除重建。
func TestUnchangedWitnessKeepsMirrorAliveWithoutDirtying(t *testing.T) {
	spec, _ := loopbackSpec(t, 1, 1)
	w := startWorld(t, spec)
	n := w.base.node
	actor := actorID(7882)
	st := BaseState{Actor: actor, InWorld: true, Cell: w.cells[CellID{}].sid, Epoch: 1}
	view := &witnessMsg{Observer: actor, Epoch: 1, Self: EntityState{ID: actor, Kind: KindPlayer, X: 5000, Y: 5000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}}
	start := time.Now()
	for i := range 100 {
		n.feedMirrors(&st, view, start.Add(time.Duration(i)*50*time.Millisecond))
		if _, _, ok := n.cfg.Mirrors.Get(actor); !ok {
			t.Fatalf("live static mirror expired at beat %d", i)
		}
	}
	if stats := n.cfg.Mirrors.Stats(); stats.Created != 1 || stats.Updated != 0 || stats.Expired != 0 {
		t.Fatalf("static witness dirtied or recreated mirror: %+v", stats)
	}
	if got := n.cfg.Mirrors.Expire(start.Add(7 * time.Second)); got != 1 {
		t.Fatalf("silent mirror did not expire: %d", got)
	}
}
