package handler

import (
	"context"
	"encoding/json"
	"example.com/moba-practice/game/moba"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"os"
	"sync/atomic"
	"testing"
)

// mobaSnapshotCrashFrames 是子进程 SIGKILL 前提交的帧数：大于 64 帧窗口，重建后的 Match 历史从第 37 帧起，
// 第 1 帧与第 21 帧的追帧请求都已越过窗口。
const mobaSnapshotCrashFrames = 100

type mobaSnapshotBarrier struct {
	Digest      string
	Frames      int
	Setup       uint64 // 建局（目录预留、选英雄、开局）留下的未投影记录
	Unprojected uint64 // SIGKILL 前的未投影记录总数
}

// lockstepCounter 汇总默认注册表里一个计数器的值；reason 非空时只取该标签。用前后差值断言，不依赖进程里别的 Match。
func lockstepCounter(name, reason string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name && (reason == "" || m.Labels["reason"] == reason) {
			total += m.Value
		}
	}
	return total
}

// splitLanes 在 committedLog（发送那一刻核对不超过已提交帧）之上分开记可靠通道发出的追帧页帧数，
// 与 lockstep.catchup.frames.total 的增量对账。快照分块先挡在 release 上（模拟慢链路，在 Match 的后台发送
// goroutine 上等，不卡 Tick）：分块发完时头部已走出冗余包的覆盖范围，从检查点 +1 起的续帧必须经已提交门下的追帧页。
type splitLanes struct {
	*committedLog
	pageFrames atomic.Int64
	heldChunks atomic.Int64
	release    chan struct{}
}

func (l *splitLanes) SendReliable(ctx context.Context, s nettransport.SessionID, raw []byte) error {
	if lockstep.IsSnapshotChunk(raw) {
		l.heldChunks.Add(1)
		select {
		case <-l.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else if !lockstep.IsCatchupNotice(raw) {
		if frames, err := lockstep.DecodeBroadcast(raw); err == nil {
			l.pageFrames.Add(int64(len(frames)))
		}
	}
	return l.committedLog.SendReliable(ctx, s, raw)
}

// SIGKILL 后经快照恢复（设计 FEATURE-2026-10-20 §15.6 维护者决定②）：子进程用真实 strict WAL 提交 100 帧、关掉后台投影，
// 断言每帧恰好留下一条未投影记录后被父进程 SIGKILL。父进程用同一个 WAL 与本次唯一的 Mongo 库冷加载，resume 换代，
// 用 RecoverRoomWithSnapshots 重建 Room：重建后发布的检查点就是冷加载出的已提交头部。两个客户端请求的起点都早于 64 帧窗口：
// 一个随宿主一起丢了本地状态、从第 1 帧请求，一个停在第 20 帧、从第 21 帧请求。验收：两人都经同一份第 100 帧检查点恢复
// （共用一次取快照），状态与 SIGKILL 前的权威摘要一致，从第 101 帧续到头部、与权威同摘要；可靠通道与广播在发送那一刻
// 都不超过已提交帧；快照分块在慢链路上挡 8 个 Tick，续帧必须经追帧页（页帧数 > 0），页的上界受已提交门约束；
// 快照、字节、失败、追帧页、切帧指标的增量与客户端实际收到的一致。
// 不配快照（RecoverRoom）时同一请求以 lockstep.ErrCatchupUnavailable 失败。
func TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL(t *testing.T) {
	if os.Getenv("ROOST_MOBA_CHILD") == "authority-snapshot" {
		h := openActiveRuntime(t, "active-snapshot", true)
		r := h.createRound(t)
		setup := h.projector.Stats()
		for id := lockstep.FrameID(1); id <= mobaSnapshotCrashFrames; id++ {
			f := lockstep.Frame{ID: id}
			for i := range moba.ArenaSeats {
				f.Inputs = append(f.Inputs, lockstep.Input{Player: lockstep.PlayerID(i + 1), Payload: realArenaInput(id, i)})
			}
			r = h.mustApply(t, moba.Change{Op: "frame", HostSID: 2000, Epoch: 1, Frame: f})
		}
		stats := h.projector.Stats()
		if r.Phase != "playing" || len(r.Frames) != mobaSnapshotCrashFrames || stats.Projected != 0 || setup.WALUnacked == 0 || stats.WALUnacked != setup.WALUnacked+mobaSnapshotCrashFrames {
			t.Fatalf("crash boundary: phase %s frames %d projected %d unprojected %d after setup %d, want one unprojected record per frame", r.Phase, len(r.Frames), stats.Projected, stats.WALUnacked, setup.WALUnacked)
		}
		writeMOBAReady(t, "snapshot-ready", mobaSnapshotBarrier{r.Reference.Digest(), len(r.Frames), setup.WALUnacked, stats.WALUnacked})
		select {} // 父进程在 barrier 后 SIGKILL；不会执行测试 cleanup / Runtime.Shutdown。
	}
	if err := os.MkdirAll(os.Getenv("ROOST_MOBA_WAL"), 0700); err != nil {
		t.Fatal(err)
	}
	child := startMOBAChild(t, "TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL", "authority-snapshot")
	raw := waitMOBAReady(t, child, "snapshot-ready")
	var barrier mobaSnapshotBarrier
	if err := json.Unmarshal(raw, &barrier); err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-child.done; err == nil {
		t.Fatal("snapshot child exited cleanly")
	}
	h := openActiveRuntime(t, "active-snapshot", false)
	r := h.mustApply(t, moba.Change{Op: "read"})
	if barrier.Frames != mobaSnapshotCrashFrames || len(r.Frames) != barrier.Frames || r.Reference.Digest() != barrier.Digest {
		t.Fatalf("WAL replay lost active state: %d frames, digest match %v", len(r.Frames), r.Reference.Digest() == barrier.Digest)
	}
	r = h.mustApply(t, moba.Change{Op: "resume", HostSID: 2000, Epoch: 1, NextHost: 2000})
	if r.Epoch != 2 {
		t.Fatal("restart did not fence old incarnation")
	}

	lanes := &splitLanes{committedLog: newCommittedLog(), release: make(chan struct{})}
	lanes.committed.Store(int64(len(r.Frames)))
	// 权威 Sender 返回成功后才前移已提交帧；发送那一刻的核对以它为准。
	persist := func(ctx context.Context, c moba.Change) (moba.Round, error) {
		saved, err := h.apply(ctx, c)
		if err == nil {
			lanes.committed.Store(int64(len(saved.Frames)))
		}
		return saved, err
	}
	const (
		frameMetric   = lockstep.MetricFrames
		pageMetric    = lockstep.MetricCatchupFrames
		outOfWindow   = "out_of_window"
		lostSeat      = 1
		staleSeat     = 7
		staleAt       = lockstep.FrameID(20)
		extraLiveTick = 3
		holdTicks     = 8 // 分块挡住的 Tick 数，大于冗余深度 3
	)
	before := map[string]int64{
		"frames":     lockstepCounter(frameMetric, ""),
		"snapshot":   lockstepCounter(lockstep.MetricSnapshot, outOfWindow),
		"bytes":      lockstepCounter(lockstep.MetricSnapshotBytes, ""),
		"failed":     lockstepCounter(lockstep.MetricSnapshotFailed, ""),
		"pages":      lockstepCounter(pageMetric, ""),
		"unservable": lockstepCounter(lockstep.MetricCatchupUnservable, ""),
	}
	checkpoints := &moba.ArenaCheckpoints{}
	room, err := moba.RecoverRoomWithSnapshots(r, persist, lanes, moba.RoomSnapshots{Reliable: lanes, Checkpoints: checkpoints})
	if err != nil {
		t.Fatal(err)
	}
	actor := moba.NewActor(room)
	defer actor.Stop(context.Background())
	bindRoster(t, actor, r, 501)

	type recoveringClient struct {
		seat      int
		session   nettransport.SessionID
		assembler *lockstep.FrameAssembler
		state     *moba.Arena
		restored  []lockstep.FrameID
		restoreOK bool
		bytes     int
		done      bool
	}
	clients := []*recoveringClient{
		{seat: lostSeat, assembler: lockstep.NewFrameAssembler(128), state: moba.NewArena(r.InitialSeed)},
		{seat: staleSeat, assembler: lockstep.NewFrameAssembler(128), state: moba.NewArena(r.InitialSeed)},
	}
	// 第二个客户端在宿主崩溃前已按序应用到第 20 帧。
	stale := clients[1]
	for _, f := range r.Frames[:staleAt] {
		frames, err := stale.assembler.IngestFrames([]lockstep.Frame{f})
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			if err := stale.state.Apply(frame); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range clients {
		c.session = nettransport.SessionID(501 + c.seat)
		actorCall(t, actor, moba.RoomRequest{Op: "catchup", Epoch: 2, Session: c.session, Frame: c.assembler.Next()})
	}
	drain := func(c *recoveringClient) {
		t.Helper()
		// 追帧页与分块在 Match 的后台 goroutine 上发出，晚到的包在下一个 Tick 之后取走，不影响结论。
		for _, raw := range lanes.take(c.session) {
			delivery, err := c.assembler.Receive(raw)
			if err != nil {
				t.Fatalf("seat %d: %v", c.seat, err)
			}
			if s := delivery.Snapshot; s != nil {
				state, err := moba.RestoreArena(s.Data, r.InitialSeed)
				if err != nil || !s.HasStateHash || state.Hash() != s.StateHash || state.Tick != s.Frame {
					t.Fatalf("seat %d snapshot restore: %v", c.seat, err)
				}
				c.state, c.restored, c.bytes = state, append(c.restored, s.Frame), c.bytes+len(s.Data)
				c.restoreOK = state.Digest() == barrier.Digest
				c.assembler.ResumeAfterSnapshot(s.Frame)
			}
			for _, frame := range delivery.Frames {
				if err := c.state.Apply(frame); err != nil {
					t.Fatalf("seat %d frame %d: %v", c.seat, frame.ID, err)
				}
			}
		}
	}
	recovering := func(seat int) bool {
		for _, c := range clients {
			if c.seat == seat && !c.done {
				return true
			}
		}
		return false
	}
	next := lockstep.FrameID(len(r.Frames) + 1)
	live := 0
	for ticks := 0; live < extraLiveTick; ticks++ {
		if ticks == 40 {
			t.Fatalf("clients did not recover: %+v / %+v", clients[0], clients[1])
		}
		if ticks == holdTicks {
			if lanes.heldChunks.Load() == 0 {
				t.Fatalf("no snapshot chunk was being sent after %d ticks", holdTicks)
			}
			close(lanes.release)
		}
		for i := range moba.ArenaSeats {
			if !recovering(i) {
				actorCall(t, actor, moba.RoomRequest{Op: "input", Epoch: 2, Session: nettransport.SessionID(501 + i), Frame: next, Payload: realArenaInput(next, i)})
			}
		}
		out := actorCall(t, actor, moba.RoomRequest{Op: "tick", Epoch: 2})
		if out.Frame.ID != next {
			t.Fatalf("tick cut frame %d, want %d", out.Frame.ID, next)
		}
		next++
		all := true
		for _, c := range clients {
			drain(c)
			if c.assembler.Next() > out.Frame.ID && !c.assembler.SnapshotInProgress() {
				c.done = true
			}
			all = all && c.done
		}
		if all {
			live++ // 恢复后再跑几帧：两人照常输入，最后的页与分块结算进指标
		}
	}
	lanes.check(t)
	if err := actor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := h.mustApply(t, moba.Change{Op: "read"})
	if final.Epoch != 2 || len(final.Frames) != int(next-1) {
		t.Fatalf("authority has %d frames at epoch %d, want %d", len(final.Frames), final.Epoch, next-1)
	}
	if lanes.maxSent != lockstep.FrameID(len(final.Frames)) {
		t.Fatalf("highest frame sent %d, committed %d", lanes.maxSent, len(final.Frames))
	}
	snapshotBytes := 0
	for _, c := range clients {
		if len(c.restored) != 1 || int(c.restored[0]) != barrier.Frames || !c.restoreOK {
			t.Fatalf("seat %d restored from %v (want the cold-loaded committed head %d), state equals the pre-SIGKILL authority %v", c.seat, c.restored, barrier.Frames, c.restoreOK)
		}
		if c.state.Tick != lockstep.FrameID(len(final.Frames)) || c.state.Digest() != final.Reference.Digest() {
			t.Fatalf("seat %d diverged tick=%d/%d", c.seat, c.state.Tick, len(final.Frames))
		}
		snapshotBytes += c.bytes
	}
	if lanes.pageFrames.Load() == 0 {
		t.Fatal("continuation after the snapshot never went through catch-up pages; the committed gate was not exercised")
	}
	if served := checkpoints.Served(); served != 1 {
		t.Fatalf("checkpoint served %d times; two waiting sessions should share one fetch", served)
	}
	got := map[string]int64{
		"frames":     lockstepCounter(frameMetric, "") - before["frames"],
		"snapshot":   lockstepCounter(lockstep.MetricSnapshot, outOfWindow) - before["snapshot"],
		"bytes":      lockstepCounter(lockstep.MetricSnapshotBytes, "") - before["bytes"],
		"failed":     lockstepCounter(lockstep.MetricSnapshotFailed, "") - before["failed"],
		"pages":      lockstepCounter(pageMetric, "") - before["pages"],
		"unservable": lockstepCounter(lockstep.MetricCatchupUnservable, "") - before["unservable"],
	}
	// 切帧：重建喂入 100 帧加恢复后新切的帧；快照：两次越窗发起、字节等于两人收到的检查点数据；追帧页帧数等于可靠通道上发出的页帧数。
	want := map[string]int64{
		"frames":     int64(len(final.Frames)),
		"snapshot":   2,
		"bytes":      int64(snapshotBytes),
		"failed":     0,
		"pages":      lanes.pageFrames.Load(),
		"unservable": 0,
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("metric deltas %v, want %v", got, want)
		}
	}
	h.close(t)
	if err := h.mongo.Database(h.database).Drop(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("SIGKILL snapshot recovery: %d unprojected strict records (%d after setup + %d frames); cold load epoch 1 -> 2; seats %d (from frame 1) and %d (from frame %d) restored from committed checkpoint %d via one fetch (%d bytes), continued to head %d with %d page frames; metric deltas %v",
		barrier.Unprojected, barrier.Setup, barrier.Frames, lostSeat, staleSeat, staleAt+1, barrier.Frames, snapshotBytes, len(final.Frames), lanes.pageFrames.Load(), got))
}
