package lockstep

import "testing"

func TestBroadcastCacheTracksConnectionLifecycle(t *testing.T) {
	transport := newRecordingTransport()
	match := newTestMatch(t, transport, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.AttachSpectator(300); err != nil {
		t.Fatal(err)
	}
	before := match.broadcastOrder()
	if len(before) != 2 || before[0].session != 101 || before[1].session != 300 {
		t.Fatal(before)
	}
	if err := match.Attach(1, 102); err != nil {
		t.Fatal(err)
	}
	if match.DetachSession(1, 101) {
		t.Fatal("stale disconnect removed new connection")
	}
	after := match.broadcastOrder()
	if after[0].session != 102 || before[0].session != 101 {
		t.Fatal("rebind cache invalidation/snapshot failed")
	}
	match.DetachSpectator(300)
	if len(match.broadcastOrder()) != 1 {
		t.Fatal("spectator remains")
	}
	match.Detach(1)
	if len(match.broadcastOrder()) != 0 {
		t.Fatal("player remains")
	}
	if err := match.Attach(2, 202); err != nil {
		t.Fatal(err)
	}
	if len(match.broadcastOrder()) != 1 {
		t.Fatal("empty cache did not invalidate")
	}
	match.Close()
	if len(match.broadcastOrder()) != 0 {
		t.Fatal("closed cache retains receiver")
	}
}
