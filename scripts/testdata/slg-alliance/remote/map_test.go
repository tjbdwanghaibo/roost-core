package alliance

import (
	"context"
	"encoding/json"
	"errors"
	player "example.com/slg-alliance-practice/game/entities/player"
	scoutmap "example.com/slg-alliance-practice/game/entities/scout_map"
	handler "example.com/slg-alliance-practice/game/handler"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// 三个真实 Remote Runtime 同进程；真实 Mongo/Redis/NATS，不冒充多机交接或进程崩溃验收。
func TestSLGRemoteMapReports(t *testing.T) {
	if role := os.Getenv("SLG_MANAGED_CHILD"); role != "" {
		runManagedMapChild(t, role)
		return
	}
	if os.Getenv("ROOST_SLG_REMOTE_MAP") != "1" {
		t.Skip("run ROOST_SLG_REMOTE_MAP=1 scripts/test-slg-alliance-player.sh")
	}
	// 最先注册、最后执行：所有 Bus/Runtime 关闭后才清理本次唯一库，失败保留证据。
	database := os.Getenv("ROOST_SLG_REWARD_DB")
	if !strings.HasPrefix(database, "roost_slg_reward_it_") {
		t.Fatal("isolated database required")
	}
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
		check(t, err)
		defer mongo.Close(context.Background())
		check(t, mongo.Database(database).Drop(context.Background()))
	})
	ctx := context.Background()
	home := openRewardRuntime(t, 1000, false)
	_, err := home.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: home.id, Kind: player.EntityKindPlayer})
	check(t, err)
	hs := sender.NewAllianceRewardsSender(home.nest)
	_, err = hs.Sync_AllianceHomeInit(ctx, home.id, 1000)
	check(t, err)
	wallet, err := hs.Sync_AllianceWallet(ctx, home.id)
	check(t, err)
	scoutmap.RegisterEntity()
	handler.RegisterAllianceRewardsNestHandlers()
	id, err := entity.BuildEntityID(99234, scoutmap.EntityKindScoutMap)
	check(t, err)
	prefix := os.Getenv("ROOST_SLG_REWARD_DB") + ":map"
	first := openPracticeRemoteOwner(t, prefix, 3000, true, id, scoutmap.EntityKindScoutMap)
	fs := sender.NewAllianceRewardsSender(first.nest)
	check(t, fs.Sync_AllianceMapInit(ctx, id, home.id, 1000))
	freezeBusinessTime(1700000000000)
	t.Cleanup(func() { clock.SetSource(nil) })
	_, err = fs.Sync_AllianceMarch(ctx, id, "remote-march", 1, 503, 700, 700, false)
	check(t, err)
	freezeBusinessTime(1700000010000)
	applied, err := fs.Sync_AllianceCompleteArrival(ctx, id, "remote-march", false)
	check(t, err)
	clock.SetSource(nil)
	if !applied {
		t.Fatal("first arrival not applied")
	}
	before, err := fs.Sync_AllianceMapSnapshot(ctx, id)
	check(t, err)
	report, err := fs.Sync_AllianceReport(ctx, id, "remote-march")
	check(t, err)
	resolve := func(ctx context.Context, id int64) (scout.OwnerRoute, error) {
		lease, found, err := first.manager.GetRemoteOwnership(ctx, id)
		if err != nil {
			return scout.OwnerRoute{}, err
		}
		if !found || !lease.Shared {
			return scout.OwnerRoute{}, errors.New("missing shared map owner")
		}
		return scout.OwnerRoute{SID: lease.OwnerSid, Marker: lease.MarkerEpoch, Route: lease.RouteEpoch}, nil
	}
	original, err := resolve(ctx, id)
	check(t, err)
	homeBus := crossReportBus(t, 1000, "managedreporthome")
	var unavailable atomic.Bool
	receiver := scout.RoutedReportReceiver{HomeSID: 1000, MapID: id, Recipient: home.id, Resolve: func(ctx context.Context, id int64) (scout.OwnerRoute, error) {
		if unavailable.Load() {
			return scout.OwnerRoute{}, errors.New("injected ownership authority unavailable")
		}
		return resolve(ctx, id)
	}, Receive: func(ctx context.Context, r scout.Report) error {
		_, err := hs.Sync_AllianceReceiveReport(ctx, home.id, 1000, r, false)
		return err
	}}
	check(t, homeBus.HandleRpc("receive", receiver.Handle))
	check(t, homeBus.Start())
	oldBus := crossReportBus(t, 3000, "managedmap")
	check(t, oldBus.Start())
	call := func(ctx context.Context, b *bus.Bus, r scout.RoutedReport) error {
		var accepted bool
		if err := b.CallTo(ctx, "managedreporthome", 1000, "receive", r, &accepted); err != nil {
			return err
		}
		if !accepted {
			return errors.New("missing receipt acknowledgment")
		}
		return nil
	}
	// 权威读取不可用时不得写 Player；恢复后还是同一原身份。
	unavailable.Store(true)
	if err := call(ctx, oldBus, scout.RoutedReport{Report: report, Owner: original}); err == nil {
		t.Fatal("failed authority read accepted")
	}
	empty, err := hs.Sync_AllianceInbox(ctx, home.id)
	check(t, err)
	if empty != "{}" {
		t.Fatal("failed route check wrote inbox")
	}
	unavailable.Store(false)
	// 收件已提交但地图未 ACK，正好在这个窗口交接 owner。
	check(t, call(ctx, oldBus, scout.RoutedReport{Report: report, Owner: original}))
	inbox, err := hs.Sync_AllianceInbox(ctx, home.id)
	check(t, err)
	var receipts map[string]scout.Report
	check(t, json.Unmarshal([]byte(inbox), &receipts))
	if len(receipts) != 1 || receipts[report.Identity()] != report {
		t.Fatal("first receipt missing")
	}
	next, err := first.manager.TransferRemoteOwnership(ctx, id, 3001)
	check(t, err)
	if next.OwnerSid != 3001 || next.MarkerEpoch <= original.Marker || next.RouteEpoch <= original.Route {
		t.Fatal("map generations failed to advance")
	}
	if err := fs.Sync_AllianceAckReport(ctx, id, report, false); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("old map owner expected fence, got %v", err)
	}
	if err := call(ctx, oldBus, scout.RoutedReport{Report: report, Owner: original}); err == nil {
		t.Fatal("stale source accepted")
	}
	second := openPracticeRemoteOwner(t, prefix, 3001, false, id, scoutmap.EntityKindScoutMap)
	ss := sender.NewAllianceRewardsSender(second.nest)
	cold, err := ss.Sync_AllianceMapSnapshot(ctx, id)
	check(t, err)
	if cold != before {
		t.Fatal("map handoff changed facts BEFORE retry")
	}
	current, err := resolve(ctx, id)
	check(t, err)
	newBus := crossReportBus(t, 3001, "managedmap")
	check(t, newBus.Start())
	if err := call(ctx, newBus, scout.RoutedReport{Report: report, Owner: original}); err == nil {
		t.Fatal("new source with stale epochs accepted")
	}
	if err := call(ctx, oldBus, scout.RoutedReport{Report: report, Owner: current}); err == nil {
		t.Fatal("old source with current epochs accepted")
	}
	d := scout.ReportDelivery{MapID: id, Read: func(ctx context.Context, key string) (scout.Report, error) {
		return ss.Sync_AllianceReport(ctx, id, key)
	}, Receive: func(ctx context.Context, r scout.Report) error {
		route, err := resolve(ctx, id)
		if err != nil {
			return err
		}
		return call(ctx, newBus, scout.RoutedReport{Report: r, Owner: route})
	}, Ack: func(ctx context.Context, r scout.Report) error { return ss.Sync_AllianceAckReport(ctx, id, r, true) }}
	worker := ArrivalWorker{Limit: 1, Discover: func(ctx context.Context, limit int) ([]string, error) {
		return ss.Sync_AlliancePendingReports(ctx, id, limit)
	}, Consume: d.Deliver}
	if err := worker.Tick(ctx); err == nil {
		t.Fatal("injected ACK rollback accepted")
	}
	after, err := ss.Sync_AllianceMapSnapshot(ctx, id)
	check(t, err)
	if after != cold {
		t.Fatal("failed ACK changed Remote map")
	}
	d.Ack = func(ctx context.Context, r scout.Report) error { return ss.Sync_AllianceAckReport(ctx, id, r, false) }
	worker.Consume = d.Deliver
	check(t, worker.Tick(ctx))
	check(t, worker.Tick(ctx))
	final, err := ss.Sync_AllianceMapSnapshot(ctx, id)
	check(t, err)
	_, err = second.manager.TransferRemoteOwnership(ctx, id, 3002)
	check(t, err)
	if err := ss.Sync_AllianceAckReport(ctx, id, report, false); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("second owner expected fence, got %v", err)
	}
	third := openPracticeRemoteOwner(t, prefix, 3002, false, id, scoutmap.EntityKindScoutMap)
	ts := sender.NewAllianceRewardsSender(third.nest)
	last, err := ts.Sync_AllianceMapSnapshot(ctx, id)
	check(t, err)
	if last != final {
		t.Fatal("acknowledged map changed on third owner")
	}
	pending, err := ts.Sync_AlliancePendingReports(ctx, id, 128)
	check(t, err)
	if len(pending) != 0 {
		t.Fatal("delivered report revived after transfer")
	}
	afterInbox, err := hs.Sync_AllianceInbox(ctx, home.id)
	check(t, err)
	if afterInbox != inbox {
		t.Fatal("handoff duplicated Player receipt")
	}
	afterWallet, err := hs.Sync_AllianceWallet(ctx, home.id)
	check(t, err)
	if afterWallet != wallet {
		t.Fatal("handoff changed wallet")
	}
	first.close()
	second.close()
	third.close()
	t.Run("process_handoff_crash", runManagedMapProcesses)
	t.Run("process_admission_handoff", func(t *testing.T) { t.Setenv("SLG_MANAGED_ADMISSION", "1"); runManagedMapProcesses(t) })
	t.Log("Managed Remote ScoutMap 3000 -> 3001 -> 3002: cold facts identical, stale writes/routes rejected, pending report retried and ACK durable, Player receipt unchanged")
}
