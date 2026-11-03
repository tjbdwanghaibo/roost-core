package cellworld

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

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
	if s := w.base.node.Stats(); s.Reenters == 0 {
		t.Fatalf("base stats = %+v, want re-enters", s)
	}
	w.assertSingleReal(actorID(1), b)
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
