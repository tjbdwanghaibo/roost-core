package handler

import (
	"context"
	"encoding/json"
	directoryentity "example.com/moba-practice/game/entities/battle_directory"
	sender "example.com/moba-practice/game/handler/syncsender"
	"example.com/moba-practice/game/moba"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type mobaChild struct {
	cmd  *exec.Cmd
	done chan error
	log  string
}

func startMOBAChild(t *testing.T, test, role string) *mobaChild {
	t.Helper()
	path := filepath.Join(os.Getenv("ROOST_MOBA_WAL"), "child-"+role+".log")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.v", "-test.timeout=90s")
	cmd.Env = append(os.Environ(), "ROOST_MOBA_CHILD="+role)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	c := &mobaChild{cmd: cmd, done: make(chan error, 1), log: path}
	go func() { err := cmd.Wait(); _ = out.Close(); c.done <- err; close(c.done) }()
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	})
	return c
}
func waitMOBAReady(t *testing.T, c *mobaChild, name string) []byte {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		raw, err := os.ReadFile(filepath.Join(os.Getenv("ROOST_MOBA_WAL"), name))
		if err == nil {
			return raw
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case err := <-c.done:
			raw, _ := os.ReadFile(c.log)
			t.Fatalf("child exited before readiness: %v\n%s", err, raw)
		case <-deadline.C:
			t.Fatal("child readiness deadline: " + c.log)
		case <-poll.C:
		}
	}
}
func writeMOBAReady(t *testing.T, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("ROOST_MOBA_WAL"), name), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func realArenaInput(frame lockstep.FrameID, seat int) []byte {
	c := moba.ArenaInput{Action: moba.Attack, Target: byte((seat+5)%10 + 1)}
	if frame <= 8 {
		c = moba.ArenaInput{Action: moba.Move, Step: 1, Lane: byte((seat % 5) % 3)}
		if seat >= 5 {
			c.Step = -1
		}
	} else if frame%7 == 0 {
		c.Action = moba.Cast
	}
	return c.Bytes()
}

// 子进程使用真实 strict WAL，关闭后台投影以精确制造“已确认但未进 Mongo”的恢复边界。
// 父进程 SIGKILL 只杀自己启动的子进程；不是干净停机，也不删除或修改 WAL。
func TestMOBARealActiveRoundCrashRecovery(t *testing.T) {
	if os.Getenv("ROOST_MOBA_CHILD") == "authority-crash" {
		h := openActiveRuntime(t, "active-crash", true)
		r := h.createRound(t)
		for id := lockstep.FrameID(1); id <= 20; id++ {
			f := lockstep.Frame{ID: id}
			for i := range moba.ArenaSeats {
				f.Inputs = append(f.Inputs, lockstep.Input{Player: lockstep.PlayerID(i + 1), Payload: realArenaInput(id, i)})
			}
			r = h.mustApply(t, moba.Change{Op: "frame", HostSID: 2000, Epoch: 1, Frame: f})
		}
		stats := h.projector.Stats()
		if stats.WALUnacked == 0 || stats.Projected != 0 {
			t.Fatal("crash boundary did not leave unprojected durable WAL", stats)
		}
		writeMOBAReady(t, "crash-ready", struct {
			Digest  string
			Frames  int
			Unacked uint64
		}{r.Reference.Digest(), len(r.Frames), stats.WALUnacked})
		select {} // 父进程在 barrier 后 SIGKILL；不会执行测试 cleanup / Runtime.Shutdown。
	}
	if err := os.MkdirAll(os.Getenv("ROOST_MOBA_WAL"), 0700); err != nil {
		t.Fatal(err)
	}
	child := startMOBAChild(t, "TestMOBARealActiveRoundCrashRecovery", "authority-crash")
	raw := waitMOBAReady(t, child, "crash-ready")
	var barrier struct {
		Digest  string
		Frames  int
		Unacked uint64
	}
	if err := json.Unmarshal(raw, &barrier); err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-child.done; err == nil {
		t.Fatal("crash child exited cleanly")
	}
	h := openActiveRuntime(t, "active-crash", false)
	r := h.mustApply(t, moba.Change{Op: "read"})
	if barrier.Frames != 20 || barrier.Unacked == 0 || len(r.Frames) != 20 || r.Reference.Digest() != barrier.Digest {
		t.Fatal("WAL replay lost active state")
	}
	directoryID, err := entity.BuildEntityID(31000, directoryentity.EntityKindBattleDirectory)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, err := entity.BuildEntityID(30001, entity.GetEntityKindFromID(h.roundID))
	if err != nil {
		t.Fatal(err)
	}
	initial := moba.Change{Members: r.Members, Seed: r.InitialSeed, Deadline: r.Deadline}
	reserved, err := sender.NewMobaRoundSender(h.scheduler).Sync_MOBAReserveRound(context.Background(), directoryID, r.MatchID, candidateID, moba.RoundBinding(initial))
	if err != nil || reserved != h.roundID {
		t.Fatal("cold directory allocated a second round", err)
	}
	r = h.mustApply(t, moba.Change{Op: "resume", HostSID: 2000, Epoch: 1, NextHost: 2000})
	if r.Epoch != 2 {
		t.Fatal("restart did not fence old incarnation")
	}
	if _, err := h.apply(context.Background(), moba.Change{Op: "frame", HostSID: 2000, Epoch: 1, Frame: lockstep.Frame{ID: 21}}); err == nil {
		t.Fatal("old epoch accepted after restart")
	}
	packets := 0
	wire := nettransport.TransportFunc{Datagram: func(context.Context, nettransport.SessionID, []byte) error { packets++; return nil }}
	// 真实依赖下的宿主按快照恢复的接法重建（设计 FEATURE-2026-10-20 §15.3）：可靠通道、已提交门、提交后才发布的检查点。
	// 本用例没有客户端请求追帧，可靠通道上不应出现任何包；窗口外经快照恢复的演练见 TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL。
	var reliablePackets atomic.Int64
	reliable := nettransport.TransportFunc{Reliable: func(context.Context, nettransport.SessionID, []byte) error { reliablePackets.Add(1); return nil }}
	room, err := moba.RecoverRoomWithSnapshots(r, h.apply, wire, moba.RoomSnapshots{Reliable: reliable, Checkpoints: &moba.ArenaCheckpoints{}})
	if err != nil {
		t.Fatal(err)
	}
	actor := moba.NewActor(room)
	defer actor.Stop(context.Background())
	bindRoster(t, actor, r, 501)
	client := moba.NewArena(42)
	assembler := lockstep.NewFrameAssembler(128)
	for _, f := range r.Frames {
		frames, err := assembler.IngestFrames([]lockstep.Frame{f})
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			if err := client.Apply(frame); err != nil {
				t.Fatal(err)
			}
		}
	}
	for id := lockstep.FrameID(21); id <= 40; id++ {
		for i := range moba.ArenaSeats {
			actorCall(t, actor, moba.RoomRequest{Op: "input", Epoch: 2, Session: nettransport.SessionID(501 + i), Frame: id, Payload: realArenaInput(id, i)})
		}
		out := actorCall(t, actor, moba.RoomRequest{Op: "tick", Epoch: 2})
		if err := client.Apply(out.Frame); err != nil {
			t.Fatal(err)
		}
		if out.Digest != client.Digest() {
			t.Fatal("resumed clients diverged")
		}
	}
	if packets != 200 || reliablePackets.Load() != 0 {
		t.Fatalf("resumed broadcasts=%d reliable=%d", packets, reliablePackets.Load())
	}
	if err := actor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = h.mustApply(t, moba.Change{Op: "read"})
	if len(r.Frames) != 40 || r.Members[0].SID != 1000 || r.Members[1].SID != 1001 {
		t.Fatal("resumption changed membership / sid")
	}
	h.close(t)
	if err := h.mongo.Database(h.database).Drop(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("SIGKILL recovery: WAL backlog=%d, frame 20 -> 40, epoch 1 -> 2, 10 original Player sids retained", barrier.Unacked))
}
