package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"example.com/moba-practice/game/moba"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

// 权威与两个宿主各属独立进程（新宿主在测试父进程）。旧宿主分块在途时提升代际，
// 新宿主只能从权威已提交日志重建；下行仍用记录器，进程间提交与管理走真实 NATS。
func TestMOBARealSnapshotDuringHostHandoff(t *testing.T) {
	const testName = "TestMOBARealSnapshotDuringHostHandoff"
	switch role := os.Getenv("ROOST_MOBA_CHILD"); role {
	case "snapshot-seed":
		h := openActiveRuntime(t, "process-snapshot", true)
		r := h.createRound(t)
		setup := h.projector.Stats().WALUnacked
		for id := lockstep.FrameID(1); id <= mobaSnapshotCrashFrames; id++ {
			f := lockstep.Frame{ID: id}
			for i := range moba.ArenaSeats {
				f.Inputs = append(f.Inputs, lockstep.Input{Player: lockstep.PlayerID(i + 1), Payload: realArenaInput(id, i)})
			}
			r = h.mustApply(t, moba.Change{Op: "frame", HostSID: 2000, Epoch: 1, Frame: f})
		}
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked != setup+mobaSnapshotCrashFrames {
			t.Fatal("seed did not leave every frame in strict WAL", stats)
		}
		writeMOBAReady(t, "process-snapshot-seed", mobaSnapshotBarrier{Digest: r.Reference.Digest(), Frames: len(r.Frames), Setup: setup, Unprojected: stats.WALUnacked})
		select {}
	case "snapshot-authority":
		runSnapshotAuthority(t)
		return
	case "snapshot-old-host":
		runSnapshotOldHost(t)
		return
	}
	if err := os.MkdirAll(os.Getenv("ROOST_MOBA_WAL"), 0700); err != nil {
		t.Fatal(err)
	}
	seed := startMOBAChild(t, testName, "snapshot-seed")
	var before mobaSnapshotBarrier
	if err := json.Unmarshal(waitMOBAReady(t, seed, "process-snapshot-seed"), &before); err != nil {
		t.Fatal(err)
	}
	if err := seed.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-seed.done; err == nil {
		t.Fatal("seed did not crash")
	}
	authority := startMOBAChild(t, testName, "snapshot-authority")
	waitMOBAReady(t, authority, "process-snapshot-authority")
	admin := mobaBus(t, mobaAdminSID, "drill")
	if err := admin.Start(); err != nil {
		t.Fatal(err)
	}
	var r moba.Round
	mustMOBARPC(t, admin, mobaAuthoritySID, "authority", "round", moba.Change{Op: "read"}, &r)
	if len(r.Frames) != before.Frames || r.Reference.Digest() != before.Digest {
		t.Fatal("independent authority lost WAL state")
	}
	old := startMOBAChild(t, testName, "snapshot-old-host")
	waitMOBAReady(t, old, "process-snapshot-old-host")
	// 不是靠时间猜测并发：旧宿主已进入分块发送且仍在 release 上等，才允许 handoff。
	waitMOBAReady(t, old, "process-snapshot-inflight")
	mustMOBARPC(t, admin, mobaAuthoritySID, "authority", "round", moba.Change{Op: "handoff", HostSID: 2000, Epoch: 1, NextHost: 2001}, &r)
	if r.HostSID != 2001 || r.Epoch != 2 || len(r.Frames) <= before.Frames {
		t.Fatal("handoff changed committed checkpoint")
	}
	before.Frames, before.Digest = len(r.Frames), r.Reference.Digest()
	var out moba.RoomReply
	if err := mobaRPC(admin, 2000, "battle", "tick", true, &out); err == nil {
		t.Fatal("old host committed during snapshot handoff")
	}
	// 旧宿主的失败提交必须取消并排空阻塞发送；旧进程退出后才能启动新宿主的客户端生命周期。
	stopSnapshotChild(t, admin, old, 2000, "battle")

	host := mobaBus(t, 2001, "battle")
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	lanes := &splitLanes{committedLog: newCommittedLog(), release: make(chan struct{})}
	lanes.committed.Store(int64(len(r.Frames)))
	persist := func(ctx context.Context, c moba.Change) (moba.Round, error) {
		// 扩大“已切帧、尚未请求权威”的窗口，确保失去 CommittedFrame 的变异能被发送时核对抓住。
		select {
		case <-time.After(25 * time.Millisecond):
		case <-ctx.Done():
			return moba.Round{}, ctx.Err()
		}
		var saved moba.Round
		err := host.CallTo(ctx, "authority", mobaAuthoritySID, "round", c, &saved)
		if err == nil {
			lanes.committed.Store(int64(len(saved.Frames)))
		}
		return saved, err
	}
	checkpoints := &moba.ArenaCheckpoints{}
	room, err := moba.RecoverRoomWithSnapshots(r, persist, lanes, moba.RoomSnapshots{Reliable: lanes, Checkpoints: checkpoints})
	if err != nil {
		t.Fatal(err)
	}
	actor := moba.NewActor(room)
	defer actor.Stop(context.Background())
	bindRoster(t, actor, r, 801)
	assembler := lockstep.NewFrameAssembler(128)
	state := moba.NewArena(r.InitialSeed)
	snapshots := 0
	actorCall(t, actor, moba.RoomRequest{Op: "catchup", Epoch: 2, Session: 801, Frame: 1})
	// 先让新宿主的分块也在途，再继续 8 帧；客户端不能依靠深度 3 的实时冗余补齐。
	next := lockstep.FrameID(len(r.Frames) + 1)
	recovered := 0
	for ticks := 0; recovered < 3; ticks++ {
		if ticks == 40 {
			t.Fatal("new host client did not recover")
		}
		if ticks == 8 {
			if lanes.heldChunks.Load() == 0 {
				t.Fatal("new host did not start snapshot")
			}
			close(lanes.release)
		}
		for i := range moba.ArenaSeats {
			if i == 0 && recovered == 0 {
				continue
			}
			actorCall(t, actor, moba.RoomRequest{Op: "input", Epoch: 2, Session: nettransport.SessionID(801 + i), Frame: next, Payload: realArenaInput(next, i)})
		}
		out = actorCall(t, actor, moba.RoomRequest{Op: "tick", Epoch: 2})
		if out.Frame.ID != next {
			t.Fatal("new host skipped frame")
		}
		next++
		for _, raw := range lanes.take(801) {
			delivery, err := assembler.Receive(raw)
			if err != nil {
				t.Fatal(err)
			}
			if s := delivery.Snapshot; s != nil {
				state, err = moba.RestoreArena(s.Data, r.InitialSeed)
				if err != nil || !s.HasStateHash || state.Hash() != s.StateHash || state.Digest() != before.Digest || int(s.Frame) != before.Frames {
					t.Fatal("new host snapshot differs from durable head", err)
				}
				assembler.ResumeAfterSnapshot(s.Frame)
				snapshots++
			}
			for _, f := range delivery.Frames {
				if err := state.Apply(f); err != nil {
					t.Fatal(err)
				}
			}
		}
		if assembler.Next() > out.Frame.ID && !assembler.SnapshotInProgress() {
			recovered++
		}
	}
	if err := actor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	lanes.check(t)
	mustMOBARPC(t, admin, mobaAuthoritySID, "authority", "round", moba.Change{Op: "read"}, &r)
	if snapshots != 1 || checkpoints.Served() != 1 || lanes.pageFrames.Load() == 0 || state.Tick != lockstep.FrameID(len(r.Frames)) || state.Digest() != r.Reference.Digest() {
		t.Fatalf("handoff recovery mismatch: snapshots=%d served=%d pages=%d tick=%d/%d", snapshots, checkpoints.Served(), lanes.pageFrames.Load(), state.Tick, len(r.Frames))
	}
	stopSnapshotChild(t, admin, authority, mobaAuthoritySID, "authority")
	t.Logf("3-process NATS snapshot handoff: %d unprojected strict records recovered, old host snapshot in flight at handoff, epoch 1 -> 2, checkpoint %d, head %d, reliable page frames %d; zero uncommitted packets", before.Unprojected, before.Frames, len(r.Frames), lanes.pageFrames.Load())
}

func runSnapshotAuthority(t *testing.T) {
	h := openActiveRuntime(t, "process-snapshot", false)
	b := mobaBus(t, mobaAuthoritySID, "authority")
	stop := registerStop(t, b)
	if err := b.HandleRpc("round", func(c *bus.RpcContext) (any, error) {
		var change moba.Change
		if err := c.Decode(&change); err != nil {
			return nil, err
		}
		switch change.Op {
		case "read":
		case "frame":
			if c.FromSid != change.HostSID || c.FromSid != 2000 && c.FromSid != 2001 {
				return nil, moba.ErrRule
			}
		case "handoff":
			if c.FromSid != mobaAdminSID {
				return nil, moba.ErrRule
			}
		default:
			return nil, moba.ErrRule
		}
		return h.apply(c.Context(), change)
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	writeMOBAReady(t, "process-snapshot-authority", true)
	select {
	case <-stop:
	case <-time.After(70 * time.Second):
		t.Fatal("authority stop deadline")
	}
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	if err := h.mongo.Database(h.database).Drop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.mongo.DatabaseForSid(h.database, 0).Drop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func runSnapshotOldHost(t *testing.T) {
	b := mobaBus(t, 2000, "battle")
	stop := registerStop(t, b)
	var r moba.Round
	mustMOBARPC(t, b, mobaAuthoritySID, "authority", "round", moba.Change{Op: "read"}, &r)
	lanes := &splitLanes{committedLog: newCommittedLog(), release: make(chan struct{})}
	lanes.committed.Store(int64(len(r.Frames)))
	persist := func(ctx context.Context, c moba.Change) (moba.Round, error) {
		var saved moba.Round
		err := b.CallTo(ctx, "authority", mobaAuthoritySID, "round", c, &saved)
		if err == nil {
			lanes.committed.Store(int64(len(saved.Frames)))
		}
		return saved, err
	}
	room, err := moba.RecoverRoomWithSnapshots(r, persist, lanes, moba.RoomSnapshots{Reliable: lanes, Checkpoints: &moba.ArenaCheckpoints{}})
	if err != nil {
		t.Fatal(err)
	}
	actor := moba.NewActor(room)
	defer actor.Stop(context.Background())
	bindRoster(t, actor, r, 701)
	actorCall(t, actor, moba.RoomRequest{Op: "catchup", Epoch: 1, Session: 701, Frame: 1})
	actorCall(t, actor, moba.RoomRequest{Op: "tick", Epoch: 1})
	if err := b.HandleRpc("tick", func(c *bus.RpcContext) (any, error) {
		if c.FromSid != mobaAdminSID {
			return nil, moba.ErrRule
		}
		return actor.Call(c.Context(), moba.RoomRequest{Op: "tick", Epoch: 1})
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	writeMOBAReady(t, "process-snapshot-old-host", true)
	deadline := time.After(5 * time.Second)
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for lanes.heldChunks.Load() == 0 {
		select {
		case <-poll.C:
			actorCall(t, actor, moba.RoomRequest{Op: "tick", Epoch: 1})
		case <-deadline:
			t.Fatal("old host snapshot not in flight")
		}
	}
	writeMOBAReady(t, "process-snapshot-inflight", true)
	select {
	case <-stop:
	case <-time.After(70 * time.Second):
		t.Fatal("old host stop deadline")
	}
	if err := actor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	lanes.check(t)
	for _, raw := range lanes.take(701) {
		if lockstep.IsSnapshotChunk(raw) {
			t.Fatal("old snapshot escaped the blocked lifecycle")
		}
	}
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func stopSnapshotChild(t *testing.T, b *bus.Bus, child *mobaChild, sid int32, kind string) {
	t.Helper()
	var stopped bool
	mustMOBARPC(t, b, sid, kind, "stop", true, &stopped)
	select {
	case err := <-child.done:
		if err != nil {
			raw, _ := os.ReadFile(child.log)
			t.Fatal(fmt.Sprintf("snapshot child %d failed: %v\n%s", sid, err, raw))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("snapshot child failed to drain")
	}
}
