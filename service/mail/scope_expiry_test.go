package mail

import (
	"context"
	"testing"
	"time"
)

func TestCloseBroadcastScopeFencesRetriesAndAcceptsCommitOrAbort(t *testing.T) {
	for _, result := range []string{"commit", "abort"} {
		t.Run(result, func(t *testing.T) {
			ctx := context.Background()
			h, _ := lazyHarness(t, LazyBroadcastConfig{})
			bus := newFakeBus()
			if err := RegisterHandlers(bus, h.service); err != nil {
				t.Fatal(err)
			}
			m, err := NewBusClient(bus, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			const player = int64(7)
			profile := func(sid int32, epoch, seq uint64) BroadcastProfile {
				p := veteran(h)
				p.Binding = BroadcastBinding{ServerID: sid, Epoch: epoch, Seq: seq}
				p.Scopes = []string{ServerScope(sid)}
				return p
			}
			a := profile(1, 0, 0)
			if _, err := m.PullBroadcasts(ctx, player, a); err != nil {
				t.Fatal(err)
			}
			send := func(sid int32, key string) string { return mustSend(t, h, broadcastTo(ServerScope(sid), key)).ID }
			old := send(1, "old-A")
			before := send(2, "early-B")
			cut := BroadcastBinding{ServerID: 1, Seq: 1}
			closeScope := func() {
				s, err := m.CloseBroadcastScope(ctx, player, cut)
				if err != nil || s.BroadcastsBehind {
					t.Fatal(s, err)
				}
			}
			closeScope()
			late := send(1, "after-A")
			fresh := send(2, "after-B")
			closeScope()
			if _, err := m.PullBroadcasts(ctx, player, a); codeOfRemoteOrLocal(err) != CodeBroadcastProfileRequired {
				t.Fatalf("stale pull: %v", err)
			}
			if _, err := m.List(ctx, player, "", 10); codeOfRemoteOrLocal(err) != CodeBroadcastProfileRequired {
				t.Fatalf("list without current scope: %v", err)
			}
			if s, err := m.Summary(ctx, player); err != nil || s.Unread != 1 {
				t.Fatal(s, err)
			}
			box, _, err := h.service.Mailbox(ctx, player)
			if err != nil || box.BroadcastWatermark != 2 || box.BroadcastProfile != nil || len(box.Entries) != 1 {
				t.Fatalf("cut moved: %+v %v", box, err)
			}
			target := profile(2, 1, 1)
			want, exclude := fresh, late
			if result == "abort" {
				target = profile(1, 0, 2)
				want, exclude = late, fresh
			}
			if _, err := m.PullBroadcasts(ctx, player, target); err != nil {
				t.Fatal(err)
			}
			closeScope()
			if _, err := m.PullBroadcasts(ctx, player, a); codeOfRemoteOrLocal(err) != CodeBroadcastProfileRequired {
				t.Fatalf("delayed old connection: %v", err)
			}
			p, err := m.List(ctx, player, "", 10)
			if err != nil || p.Unread != 2 {
				t.Fatal(p, err)
			}
			seen := map[string]bool{}
			for _, item := range p.Items {
				seen[item.Envelope.ID] = true
			}
			if !seen[old] || !seen[want] || seen[before] || seen[exclude] || len(seen) != 2 {
				t.Fatalf("mail after %s: %v", result, seen)
			}
		})
	}
}

// No prior PullBroadcasts means there is no audience to drain. Such a move must not
// disable direct mail, even if other players have pending broadcasts. Its fence still
// rejects a delayed pull with the source binding.
func TestNeverPulledMoveKeepsDirectMailAvailable(t *testing.T) {
	ctx := context.Background()
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	mustSend(t, h, broadcastTo(ServerScope(1), "other"))
	e := mustSend(t, h, directTo(7))
	if _, err := h.service.CloseBroadcastScope(ctx, 7, BroadcastBinding{ServerID: 1, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	p, err := h.service.List(ctx, 7, "", 10)
	if err != nil || len(p.Items) != 1 || p.Items[0].Envelope.ID != e.ID {
		t.Fatal(p, err)
	}
	a := veteran(h)
	a.Binding = BroadcastBinding{ServerID: 1}
	if _, err = h.service.PullBroadcasts(ctx, 7, a); codeOfRemoteOrLocal(err) != CodeBroadcastProfileRequired {
		t.Fatal(err)
	}
}

func TestSummaryExpiresWithoutNewBroadcastOrList(t *testing.T) {
	for _, mode := range []string{"direct", "broadcast"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var h *harness
			if mode == "direct" {
				h = newHarness(t)
				r := directTo(7)
				r.ExpiresInSeconds = 10
				mustSend(t, h, r)
			} else {
				h, _ = lazyHarness(t, LazyBroadcastConfig{})
				r := broadcastTo("", "all")
				r.ExpiresInSeconds = 10
				mustSend(t, h, r)
				if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
					t.Fatal(err)
				}
			}
			check := func(unread int32, evicted uint64) {
				s, err := h.service.Summary(ctx, 7)
				if err != nil || s.Unread != unread || s.Evicted != evicted {
					t.Fatal(s, err)
				}
			}
			h.clock.advance(9 * time.Second)
			check(1, 0)
			h.clock.advance(time.Second)
			check(0, 1)
			h.clock.advance(time.Hour)
			check(0, 1)
			p, err := h.service.List(ctx, 7, "", 10)
			if err != nil || len(p.Items) != 0 || p.Unread != 0 {
				t.Fatal(p, err)
			}
		})
	}
}

func TestSummaryRetainsExpiredUnsettledClaim(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	r := withAttachment(directTo(7), "reward")
	r.ExpiresInSeconds = 10
	e := mustSend(t, h, r)
	c, err := h.service.ReserveClaim(ctx, 7, e.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Hour)
	if _, err = h.service.Summary(ctx, 7); err != nil {
		t.Fatal(err)
	}
	box, _, err := h.service.Mailbox(ctx, 7)
	if err != nil || box.Entries[e.ID].ClaimToken != c.Token || box.Evicted != 0 {
		t.Fatalf("lost pending receipt: %+v %v", box, err)
	}
	if _, err = h.service.CommitClaim(ctx, 7, e.ID, c.Token); err != nil {
		t.Fatal(err)
	}
}
