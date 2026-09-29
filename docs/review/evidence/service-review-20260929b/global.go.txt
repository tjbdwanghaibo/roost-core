package global

import (
	"context"
	"testing"
)

func TestReviewLeaseCannotRenewAgainstChangedRoute(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	binding, err := s.Bind(ctx, 100, "g", 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	moving, err := s.BeginMigration(ctx, 100, 2, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.CompleteMigration(ctx, 100, moving.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := s.RenewLease(ctx, 100, lease.Incarnation, map[string]string{"load": "1"})
	if err == nil {
		t.Fatalf("stale routing lease renewed: route=(sid=%d epoch=%d) lease=(sid=%d epoch=%d)", current.GlobalSID, current.Epoch, renewed.GlobalSID, renewed.RouteEpoch)
	}
}

func TestReviewLeaseSnapshotsOwnLoadMaps(t *testing.T) {
	for _, entry := range []string{"renew", "lookup", "live"} {
		t.Run(entry, func(t *testing.T) {
			s, _ := newService(t)
			ctx := context.Background()
			if _, err := s.Bind(ctx, 100, "g", 1); err != nil {
				t.Fatal(err)
			}
			lease, err := s.AcquireLease(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			renewed, err := s.RenewLease(ctx, 100, lease.Incarnation, map[string]string{"load": "1"})
			if err != nil {
				t.Fatal(err)
			}
			var snapshot GameLease
			switch entry {
			case "renew":
				snapshot = renewed
			case "lookup":
				snapshot, _, err = s.Lease(ctx, 100)
			case "live":
				var list []GameLease
				list, err = s.LiveGames(ctx, "g", []int32{100}, 1)
				if len(list) != 1 {
					t.Fatal("missing live game")
				}
				snapshot = list[0]
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _, _ := s.cfg.Leases.Get(ctx, 100)
			snapshot.Load["load"] = "forged"
			after, _, err := s.cfg.Leases.Get(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			if after.Value.Load["load"] != "1" {
				t.Fatalf("returned %s snapshot edited storage without CAS: load=%s version=%d->%d", entry, after.Value.Load["load"], before.Version, after.Version)
			}
		})
	}
}
