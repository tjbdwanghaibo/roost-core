package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// U-0191 · C8 · RR-20260914-02:OpenActivity 与 sweep 交错不能让一个成功打开的活动失去窗口索引。
// 旧实现:窗口条目先进、Activities.Create 后进;sweep 对"窗口里有 key、活动不存在"的判据是"死条目,
// 删掉"——它分不清"还没建"与"已经没了"。交错:admitToWindow → sweep 读到 key、Get 不存在、prune →
// Create 成功 → 首个 notify 进入 collecting → 宽限期过了,窗口里没有它,没人再看它:
// `status=collecting window=[]`。承诺:窗口条目有 opening / 确认两段生命周期,清理只能删它观察到
// 的恢复责任。OpeningGrace 后帮助持久计划创建,确认只消费原来的名额。

type openingStoreHook struct {
	versionstore.Store[Key, Activity]
	beforeCreate func()
	createErr    error
}

func (s *openingStoreHook) Create(ctx context.Context, key Key, value Activity) (versionstore.Versioned[Activity], bool, error) {
	if s.beforeCreate != nil {
		s.beforeCreate()
	}
	if s.createErr != nil {
		return versionstore.Versioned[Activity]{}, false, s.createErr
	}
	return s.Store.Create(ctx, key, value)
}

func TestOpenSweepInterleaveKeepsTheActivityInTheWindow(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	s.cfg.Activities = &openingStoreHook{Store: s.cfg.Activities, beforeCreate: func() {
		// 一次真实的 sweep 恰好落在窗口条目已在、活动记录未建之间。
		if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
			t.Fatal(err)
		}
	}}
	k := activityKey("opening")
	openActivity(t, s, k, 1, 2)
	notify(t, s, k, 1)
	c.advance(time.Minute)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	a, found, err := s.LookupActivity(ctx, k)
	if err != nil || !found {
		t.Fatalf("lookup %v %v", found, err)
	}
	keys, err := s.PendingActivities(ctx, "group-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusComplete {
		t.Fatalf("open succeeded but expired activity stranded: status=%s window=%v", a.Status, keys)
	}
}

// Create 与确认之间进程退出:活动存在、窗口里只有 opening 条目。下一次 sweep 必须把它认领进窗口,
// 之后照常按宽限期完成。
func TestSweepHealsAnOpeningEntryWhoseActivityExists(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	k := activityKey("half-open")
	if _, err := s.admitToWindow(ctx, Activity{Key: k, ExpectedGameSIDs: []int32{1, 2}, Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	now := s.cfg.Now().Unix()
	if _, created, err := s.cfg.Activities.Create(ctx, k, Activity{Key: k, ExpectedGameSIDs: []int32{1, 2}, Status: StatusPending, OpenedAtUnix: now, UpdatedAtUnix: now}); err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	notify(t, s, k, 1)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.LookupActivity(ctx, k)
	if err != nil || a.Status != StatusComplete {
		t.Fatalf("half-open activity not completed by the sweep: status=%s err=%v", a.Status, err)
	}
}

// Create 返回错误不能证明未提交。Grace 后继续帮助相同计划,失败时仍保留名额,
// 后端恢复后成功创建;不能靠超时重新承诺这个名额。
func TestSweepRetainsAndRecoversAnUnknownOpeningAfterGrace(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	hook := &openingStoreHook{Store: s.cfg.Activities, createErr: errors.New("activities: connection reset")}
	s.cfg.Activities = hook
	k := activityKey("abandoned")
	if _, err := s.OpenActivity(ctx, k, []int32{1}); err == nil {
		t.Fatal("expected the injected create failure")
	}
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.PendingActivities(ctx, "group-a", 10); len(keys) != 1 {
		t.Fatalf("an opening entry inside its grace was reclaimed: window=%v", keys)
	}
	c.advance(s.cfg.OpeningGrace + time.Second)
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err == nil {
		t.Fatal("expected the still-unavailable create to be reported")
	}
	if keys, _ := s.PendingActivities(ctx, "group-a", 10); len(keys) != 1 {
		t.Fatalf("unknown create lost its capacity promise: window=%v", keys)
	}
	hook.createErr = nil
	if _, err := s.AdvanceExpired(ctx, "group-a", 10); err != nil {
		t.Fatal(err)
	}
	if activity, found, err := s.LookupActivity(ctx, k); err != nil || !found || activity.Status != StatusPending {
		t.Fatalf("creation plan was not recovered: %+v %v %v", activity, found, err)
	}
}
