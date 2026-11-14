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
		t.Skip("run ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh with isolated Mongo, Redis and NATS")
	}
	ctx := context.Background()
	var first, second *allianceOwner
	var original *alliance
	factory := func(t *testing.T, prefix string, key activity.Key) *alliance {
		if first == nil {
			first = openAllianceOwner(t, prefix, 2000, true)
			original = newAllianceWithStore(&remoteAllianceStore{first, key.String()}, key, coordinatorReservationTTL)
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
		// 共享模式下每个进程都能写（REMOTE_ENTITY“多写者：本地副本与权威”），转移只换 owner、不撤销其他写者的写许可，旧节点
		// 照样能写。要让转移把旧节点挡在外面，先退出共享回到独占，再转给 2001（独占转移把旧实例置为 Fenced）。
		exclusive, e := first.manager.LeaveRemoteSharedMode(ctx, first.id)
		check(t, e)
		if exclusive.OwnerSid != 2000 || exclusive.Shared {
			t.Fatal("leaving shared mode did not return an exclusive lease for 2000", exclusive)
		}
		next, e := first.manager.TransferRemoteOwnership(ctx, first.id, 2001)
		check(t, e)
		if next.OwnerSid != 2001 || next.Shared || next.MarkerEpoch <= exclusive.MarkerEpoch || next.RouteEpoch <= exclusive.RouteEpoch {
			t.Fatal("ownership generations did not advance", lease, exclusive, next)
		}
		// 独占转移后旧节点仍活着；即使传相同成员撤权，也不能绕过 Remote 所有权准入。
		if e := original.revoke(ctx, "alice"); e == nil {
			t.Fatal("old owner kept writing after transfer")
		} else {
			t.Logf("old owner rejected after transfer: %v", e)
		}
		second = openAllianceOwner(t, prefix, 2001, false)
		a := newAllianceWithStore(&remoteAllianceStore{second, key.String()}, key, coordinatorReservationTTL)
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
	check(t, mongo.Database(os.Getenv("ROOST_SLG_DB")).Drop(ctx))
	// dbscope=sid 的 DAO（v1.47.0 起缺省）落在 <库>_<sid>，本用例的存储没有 sid（0）。
	check(t, mongo.DatabaseForSid(os.Getenv("ROOST_SLG_DB"), 0).Drop(ctx))
	t.Log("Remote generated authority 2000 -> 2001: marker and route epochs advance, old writer rejected while alive, cold state identical before continuation; contribution replay and both Player crash-recovery deliveries remain in the same workflow")
}
