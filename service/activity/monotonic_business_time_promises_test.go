package activity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 业务时间只许前进（docs/feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md）：App 拒绝启动让业务时间回退的
// 部署，所以协调器的派发退避（NextAttemptAtUnix、到期判断、owed 索引）与进度凭证 ExpiresAtUnix 都回到
// 业务时钟（Config.Now），删掉了 v1.21.0 为“偏移往回调”拆出来的 Config.SystemNow。
//
// 原来的回调用例（偏移从 +24h 拨回 0 后派发多挂 24h）随之删除：那个场景现在在 App 启动时就被拒绝
// （app.TestBusinessTimeMovingBackRefusesToStart），组件不再承诺它。这里证明单调业务时间下的行为与
// 拆分时相同：同一偏移跨重启、偏移前拨，退避都恰好持续 DispatchBackoff，立即可取的照样立即可取。

func TestDispatchBackoffAndProofExpiryRunOnTheMonotonicBusinessClock(t *testing.T) {
	const gameSID = int32(1000)
	ctx := context.Background()
	wall := &activityClock{now: time.Unix(1_700_000_000, 0)}
	offset := 24 * time.Hour
	business := func() time.Time { return wall.Now().Add(offset) }

	// 第一轮：偏移 +24h。
	first, _ := newActivityService(t, func(cfg *Config) {
		cfg.Dispatches = newMemoryOwedDispatches()
		cfg.Now = business
	})
	taken := activityKey("race-taken")
	untouched := activityKey("race-untouched")
	for _, key := range []Key{taken, untouched} {
		openActivity(t, first, key, gameSID)
		if _, err := first.NotifyPhase(ctx, key, gameSID); err != nil {
			t.Fatalf("%s: notify: %v", key, err)
		}
	}
	attempt, err := first.AttemptDispatch(ctx, taken, gameSID)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if want := business().Add(first.cfg.DispatchBackoff).Unix(); attempt.NextAttemptAtUnix != want {
		t.Fatalf("NextAttemptAtUnix after an attempt = %d, want business now + backoff %d", attempt.NextAttemptAtUnix, want)
	}
	progress := activityKey("race-progress")
	openActivity(t, first, progress, gameSID)
	if _, err := first.ApplyProgress(ctx, progress, "player-1", "req-1", ProgressDelta{Score: 1}); err != nil {
		t.Fatalf("apply progress: %v", err)
	}
	proof, found, err := first.Reservation(ctx, progress, "player-1", "req-1")
	if err != nil || !found {
		t.Fatalf("reservation: found=%v err=%v", found, err)
	}
	if proof.ExpiresAtUnix != proof.CreatedAtUnix+int64(first.cfg.ReservationTTL/time.Second) {
		t.Fatalf("proof %+v: ExpiresAtUnix is not CreatedAtUnix + ReservationTTL on one clock", proof)
	}

	// 第二轮：同一份存储，同一偏移重启（业务时间继续前进）。
	restart := func(newOffset time.Duration) *Service {
		offset = newOffset
		service, err := New(first.cfg)
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	second := restart(24 * time.Hour)
	if _, err := second.AttemptDispatch(ctx, untouched, gameSID); err != nil {
		t.Fatalf("a dispatch created due immediately in the previous run: %v; want it due now", err)
	}
	// 退避恰好持续 DispatchBackoff：差一秒不可取，到点可取，owed 清单同步。
	wall.advance(first.cfg.DispatchBackoff - time.Second)
	if _, err := second.AttemptDispatch(ctx, taken, gameSID); !errors.Is(err, ErrDispatchNotDue) {
		t.Fatalf("a second before the backoff ends: %v, want ErrDispatchNotDue", err)
	}
	if owed := owedKeys(t, second, taken, gameSID); owed[taken] {
		t.Fatalf("a second before the backoff ends the game is already owed %s", taken)
	}
	wall.advance(time.Second)
	if owed := owedKeys(t, second, taken, gameSID); !owed[taken] {
		t.Fatalf("after the backoff the game is not owed %s", taken)
	}
	retried, err := second.AttemptDispatch(ctx, taken, gameSID)
	if err != nil {
		t.Fatalf("after the backoff: %v", err)
	}

	// 第三轮：偏移前拨一天。退避提前结束——只是早一点重试，不会多挂。
	third := restart(48 * time.Hour)
	if retried.NextAttemptAtUnix > business().Unix() {
		t.Fatalf("after moving the offset forward, NextAttemptAtUnix %d is still ahead of business now %d", retried.NextAttemptAtUnix, business().Unix())
	}
	if _, err := third.AttemptDispatch(ctx, taken, gameSID); err != nil {
		t.Fatalf("after moving the offset forward: %v, want the dispatch due", err)
	}
}

func owedKeys(t *testing.T, service *Service, key Key, gameSID int32) map[Key]bool {
	t.Helper()
	keys, err := service.OwedDispatches(context.Background(), key.GroupID, gameSID, 10)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[Key]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// 管理员重开派发是“立即可取”：NextAttemptAtUnix 写业务时钟的当前时间，owed 索引按它打分、按业务时钟查询。
func TestAReopenedDispatchIsOwedNow(t *testing.T) {
	const gameSID = int32(1000)
	ctx := context.Background()
	wall := &activityClock{now: time.Unix(1_700_000_000, 0)}
	business := func() time.Time { return wall.Now().Add(24 * time.Hour) }
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Dispatches = newMemoryOwedDispatches()
		cfg.DispatchMaxAttempts = 1
		cfg.Now = business
	})
	key := activityKey("race-reopen")
	openActivity(t, service, key, gameSID)
	if _, err := service.NotifyPhase(ctx, key, gameSID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AttemptDispatch(ctx, key, gameSID); err != nil {
		t.Fatal(err)
	}
	wall.advance(time.Minute)
	if _, err := service.AttemptDispatch(ctx, key, gameSID); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("second attempt = %v, want exhausted", err)
	}
	reopened, err := service.ReopenDispatch(ctx, key, gameSID, "game is back")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.NextAttemptAtUnix != business().Unix() || !reopened.Due(business().Unix()) {
		t.Fatalf("reopened dispatch %+v: want NextAttemptAtUnix = business now %d and due", reopened, business().Unix())
	}
	if owed := owedKeys(t, service, key, gameSID); !owed[key] {
		t.Fatalf("reopened dispatch is not in the owed list at business now")
	}
}
