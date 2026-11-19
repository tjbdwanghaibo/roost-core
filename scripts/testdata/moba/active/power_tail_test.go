package handler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"example.com/moba-practice/game/modules/moba"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
)

// The client receives two committed datagrams. Model power loss (not mere process death)
// by keeping the fsynced prefix through frame 1 and removing the complete async tail.
func TestMOBARealClientAheadAfterTailLoss(t *testing.T) {
	const scope = "cut-tail"
	ctx := context.Background()
	type cut struct {
		Offset  int64
		Packets [][]byte
	}
	segment := func() string {
		return filepath.Join(os.Getenv("ROOST_MOBA_WAL"), scope, "segment-00000000000000000001.wal")
	}
	if os.Getenv("ROOST_MOBA_CHILD") == scope {
		h := openActiveRuntime(t, scope, true)
		r := h.createRound(t)
		var mark cut
		var baseSyncs uint64
		wire := nettransport.TransportFunc{Datagram: func(_ context.Context, s nettransport.SessionID, b []byte) error {
			if s == 501 {
				mark.Packets = append(mark.Packets, append([]byte(nil), b...))
			}
			return nil
		}}
		room, err := moba.RecoverRoom(r, h.apply, wire)
		if err != nil {
			t.Fatal(err)
		}
		actor := moba.NewActor(room)
		bindRoster(t, actor, r, 501)
		for id := lockstep.FrameID(1); id <= 2; id++ {
			actorCall(t, actor, moba.RoomRequest{Op: "input", Epoch: r.Epoch, Session: 501, Frame: id, Payload: moba.ArenaInput{Action: moba.Move, Step: 1, Lane: 1}.Bytes()})
			actorCall(t, actor, moba.RoomRequest{Op: "tick", Epoch: r.Epoch})
			if id == 1 {
				if err := h.runtime.WAL.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				st, err := os.Stat(segment())
				if err != nil {
					t.Fatal(err)
				}
				mark.Offset = st.Size()
				baseSyncs = h.runtime.WAL.Stats().Syncs
			}
		}
		if len(mark.Packets) != 2 || h.projector.Stats().Projected != 0 || h.runtime.WAL.Stats().Syncs != baseSyncs {
			t.Fatalf("wrong boundary %+v", h.projector.Stats())
		}
		writeMOBAReady(t, "power-cut", mark)
		select {}
	}
	// Isolate both Mongo and WAL from the existing SIGKILL-only drill.
	t.Setenv("ROOST_MOBA_DB", os.Getenv("ROOST_MOBA_DB")+"_power")
	t.Setenv("ROOST_MOBA_WAL", t.TempDir())
	child := startMOBAChild(t, t.Name(), scope)
	var mark cut
	if err := json.Unmarshal(waitMOBAReady(t, child, "power-cut"), &mark); err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-child.done
	client := moba.NewArena(42)
	old := lockstep.NewFrameAssembler(128)
	for _, b := range mark.Packets {
		fs, err := old.Ingest(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if err := client.Apply(f); err != nil {
				t.Fatal(err)
			}
		}
	}
	if client.Tick != 2 {
		t.Fatal(client.Tick)
	}
	if err := os.Truncate(segment(), mark.Offset); err != nil {
		t.Fatal(err)
	}
	h := openActiveRuntime(t, scope, false)
	r := h.mustApply(t, moba.Change{Op: "resume", HostSID: 2000, Epoch: 1, NextHost: 2000})
	if r.Reference.Tick != 1 || r.Epoch != 2 {
		t.Fatal(r)
	}
	fresh, err := moba.RestoreArena(r.Reference.Snapshot(), r.InitialSeed)
	if err != nil {
		t.Fatal(err)
	}
	rebased := lockstep.NewFrameAssembler(128)
	rebased.ResumeAfterSnapshot(fresh.Tick)
	var packet []byte
	wire := nettransport.TransportFunc{Datagram: func(_ context.Context, s nettransport.SessionID, b []byte) error {
		if s == 501 {
			packet = append([]byte(nil), b...)
		}
		return nil
	}}
	room, err := moba.RecoverRoom(r, h.apply, wire)
	if err != nil {
		t.Fatal(err)
	}
	actor := moba.NewActor(room)
	defer actor.Stop(ctx)
	bindRoster(t, actor, r, 501)
	out := actorCall(t, actor, moba.RoomRequest{Op: "tick", Epoch: r.Epoch})
	skipped, err := old.Ingest(packet)
	if err != nil || len(skipped) != 0 || client.Digest() == out.Digest {
		t.Fatalf("old assembler: frames=%v err=%v", skipped, err)
	}
	fs, err := rebased.Ingest(packet)
	if err != nil || len(fs) != 1 {
		t.Fatal(fs, err)
	}
	if err := fresh.Apply(fs[0]); err != nil || fresh.Digest() != out.Digest {
		t.Fatalf("rebase failed: %v", err)
	}
	if err := actor.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.mongo.DatabaseForSid(h.database, 0).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("old client 2; retained authority 1; epoch 2 reused frame 2: stale digest differs, rebased digest matches")
}
