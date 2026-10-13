// 活动入参与配置守卫：非法参数、组大小上限、宽限期、不可读活动不拖住健康到期、开窗撞键先读胜者。

package activity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 运维重开派发拒绝非正 game sid；Redis 存储缺前缀 / 预留 ttl 不能构造；空组 id 不能列待办活动。
func TestActivityEntryPointsRefuseBlankAndNonPositiveArguments(t *testing.T) {
	ctx := context.Background()
	service, _ := newActivityService(t)
	key := Key{GroupID: "group-a", ActivityID: "a-1", Phase: PhaseClose}
	if _, err := service.ReopenDispatch(ctx, key, 0, "ops reviewed"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "game sid must be positive") {
		t.Fatalf("ReopenDispatch with game sid 0 = %v", err)
	}
	if _, err := service.PendingActivities(ctx, "", 10); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "group id is empty") {
		t.Fatalf("PendingActivities with a blank group = %v", err)
	}
	if _, err := NewRedisStores(nil, "  ", time.Minute); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
	if _, err := NewRedisStores(nil, "activity", 0); err == nil || !strings.Contains(err.Error(), "reservation ttl must be positive") {
		t.Fatalf("NewRedisStores with ttl 0 = %v", err)
	}
}

// 一组的成员不能多到一次 App.Live 查询都报错（那样每一拍开窗都失败）：一组至多 MaxExpectedGames 个成员（ParseGroups 加载时拒绝），
// 所以只要组上限不超过 app.SingletonLiveMaxSIDs 就成立；两个常量改一个不改另一个时这里先红。
func TestAGroupFitsOneLiveQuery(t *testing.T) {
	if MaxExpectedGames > app.SingletonLiveMaxSIDs {
		t.Fatalf("an activity group may hold %d game servers (MaxExpectedGames) but one App.Live query takes at most %d (app.SingletonLiveMaxSIDs); "+
			"a full group's Live query would fail on every pass and its windows would never open", MaxExpectedGames, app.SingletonLiveMaxSIDs)
	}
}

func TestGraceWindowRejectsSubsecond(t *testing.T) {
	s, _ := newActivityService(t)
	cfg := s.cfg
	cfg.GraceWindow = time.Millisecond
	if _, err := New(cfg); err == nil {
		t.Fatal("subsecond grace window accepted despite Unix-second deadline")
	}
}

type brokenActivityRead struct {
	versionstore.Store[Key, Activity]
	key Key
}

func (s brokenActivityRead) Get(ctx context.Context, key Key) (versionstore.Versioned[Activity], bool, error) {
	if key == s.key {
		return versionstore.Versioned[Activity]{}, false, versionstore.ErrMalformedRecord
	}
	return s.Store.Get(ctx, key)
}

func TestUnreadableActivityDoesNotStallHealthyExpiry(t *testing.T) {
	s, c := newActivityService(t)
	bad, good := activityKey("a-bad"), activityKey("z-good")
	for _, key := range []Key{bad, good} {
		openActivity(t, s, key, 1, 2)
		notify(t, s, key, 1)
	}
	c.advance(time.Minute)
	s.cfg.Activities = brokenActivityRead{Store: s.cfg.Activities, key: bad}
	completed, err := s.AdvanceExpired(context.Background(), good.GroupID, 10)
	if !errors.Is(err, versionstore.ErrMalformedRecord) {
		t.Fatalf("corruption not reported: %v", err)
	}
	if len(completed) != 1 || completed[0].Key != good {
		t.Fatalf("healthy expiry stalled: completed=%v err=%v", completed, err)
	}
	window, _, err := s.cfg.Windows.Get(context.Background(), good.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Value.Keys) != 1 || window.Value.Keys[0] != bad {
		t.Fatalf("bad record must stay for repair: %+v", window.Value)
	}
}

func TestSweepStillRetiresDeliveriesWhenAnotherActivityIsUnreadable(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()
	bad, good := activityKey("a-bad"), activityKey("z-good")
	openActivity(t, s, bad, 1)
	openActivity(t, s, good, 1)
	notify(t, s, good, 1)
	dispatch, err := s.AttemptDispatch(ctx, good, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AckDispatch(ctx, good, 1, dispatch.Token); err != nil {
		t.Fatal(err)
	}
	s.cfg.Activities = brokenActivityRead{Store: s.cfg.Activities, key: bad}
	(&Server{service: s}).sweepGroup(ctx, s, good.GroupID)
	if windowOf(t, s, good.GroupID).delivering(good) {
		t.Fatal("healthy terminal delivery not retired behind corrupt activity")
	}
}

type createOpeningRace struct {
	versionstore.Store[Key, Activity]
}

func (s createOpeningRace) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	// 模拟 opener 在 sweep 的 Get 与 Create 之间完成；Create 返回零值而非赢家值。
	value.Status = StatusComplete
	if _, _, err := s.Store.Create(ctx, key, value); err != nil {
		return versionstore.Versioned[Activity]{}, false, err
	}
	return s.Store.Create(ctx, key, value)
}

func TestOpeningCreateCollisionReadsWinnerBeforeClassification(t *testing.T) {
	s, c := newActivityService(t)
	key := activityKey("racing-opening")
	seedOpening(t, s, OpeningEntry{Key: key, AdmittedAtUnix: c.Now().Unix(), Intent: &Activity{Key: key, Status: StatusPending, ExpectedGameSIDs: []int32{1}}})
	c.advance(s.cfg.OpeningGrace + time.Second)
	s.cfg.Activities = createOpeningRace{s.cfg.Activities}
	if _, err := s.AdvanceExpired(context.Background(), key.GroupID, 10); err != nil {
		t.Fatal(err)
	}
	if !windowOf(t, s, key.GroupID).delivering(key) {
		t.Fatal("completed winner not moved to delivering in the same sweep")
	}
}

func TestOperatorCanRetirePermanentlyOfflineGameAndReopenSameDelivery(t *testing.T) {
	s, _ := newActivityService(t)
	ctx, key := context.Background(), activityKey("offline-game")
	openActivity(t, s, key, 1)
	notify(t, s, key, 1)
	before, _, err := s.LookupDispatch(ctx, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExhaustDispatch(ctx, key, 1, ""); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("missing note accepted: %v", err)
	}
	stopped, err := s.ExhaustDispatch(ctx, key, 1, "game permanently decommissioned")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != DispatchExhausted || stopped.Token != before.Token || stopped.Attempts != 0 || stopped.AdminNote == "" {
		t.Fatalf("operator fabricated attempts or lost identity: %+v", stopped)
	}
	again, err := s.ExhaustDispatch(ctx, key, 1, "retry after lost reply")
	if err != nil || again.AdminNote != stopped.AdminNote {
		t.Fatalf("replay changed terminal record: %+v %v", again, err)
	}
	if _, err := s.AckDispatch(ctx, key, 1, before.Token); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("late ACK accepted: %v", err)
	}
	if retired, err := s.RetireDelivered(ctx, key); err != nil || !retired {
		t.Fatalf("not retired: %v %v", retired, err)
	}
	reopened, err := s.ReopenDispatch(ctx, key, 1, "server restored")
	if err != nil || reopened.State != DispatchPending || reopened.Token != before.Token {
		t.Fatalf("reopen lost identity: %+v %v", reopened, err)
	}
}
