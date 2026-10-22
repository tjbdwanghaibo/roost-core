package alliance

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/service/activity"
)

func TestSLGAllianceRemoteDelivery(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI") == "" || os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR") == "" || os.Getenv("ROOST_DATAENGINE_IT_NATS_URL") == "" {
		t.Skip("run scripts/test-slg-alliance-player.sh with ROOST_SLG_REMOTE=1")
	}
	ctx := context.Background()
	var first, second *allianceOwner
	var original *alliance
	factory := func(t *testing.T, prefix string, key activity.Key) *alliance {
		if first == nil {
			first = openAllianceOwner(t, prefix, 2000, true)
			original = &alliance{store: &remoteAllianceStore{first, key.String()}, key: key, reservationTTL: coordinatorReservationTTL}
			return original
		}
		before, e := original.read(ctx)
		check(t, e)
		rawBefore, e := json.Marshal(before)
		check(t, e)
		lease, found, e := first.manager.GetRemoteOwnership(ctx, first.id)
		check(t, e)
		if !found || lease.OwnerSid != 2000 || !lease.Shared {
			t.Fatal("initial shared owner missing", lease)
		}
		next, e := first.manager.TransferRemoteOwnership(ctx, first.id, 2001)
		check(t, e)
		if next.OwnerSid != 2001 || next.MarkerEpoch <= lease.MarkerEpoch || next.RouteEpoch <= lease.RouteEpoch {
			t.Fatal("ownership generations did not advance", lease, next)
		}
		// 转移后旧节点仍活着；即使传相同成员撤权，也不能绕过 Remote 所有权准入。
		if e := original.revoke(ctx, "alice"); e == nil {
			t.Fatal("old owner kept writing after transfer")
		} else {
			t.Logf("old owner rejected after transfer: %v", e)
		}
		second = openAllianceOwner(t, prefix, 2001, false)
		a := &alliance{store: &remoteAllianceStore{second, key.String()}, key: key, reservationTTL: coordinatorReservationTTL}
		after, e := a.read(ctx)
		check(t, e)
		rawAfter, e := json.Marshal(after)
		check(t, e)
		if string(rawBefore) != string(rawAfter) {
			t.Fatal("cold authority changed frozen membership or pending intents")
		}
		first.close()
		return a
	}
	runAllianceActivity(t, deliverPlayers, factory)
	if second == nil {
		t.Fatal("handoff was not exercised")
	}
	second.close()
	mongo, e := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	check(t, e)
	defer mongo.Close(ctx)
	check(t, mongo.Database(os.Getenv("ROOST_SLG_REWARD_DB")).Drop(ctx))
	t.Log("Remote generated authority 2000 -> 2001: marker and route epochs advance, old writer rejected while alive, cold state identical before continuation; contribution replay and both Player crash-recovery deliveries remain in the same workflow")
}
