package activity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 发版前审查观察（D-L3 收尾）：派发的重试退避与进度凭证的有效期是系统时间。
//
// D-L3 规则写明“重试与退避属系统钟”，但协调器当初整个划成了业务时间：派发的 NextAttemptAtUnix（创建时“立即可取”
// 与每次尝试后的退避）和进度凭证的 ExpiresAtUnix 都读 Config.Now（业务时钟）。测试环境两次运行之间把
// time.logic_offset 往回拨 D，上一轮按业务时钟打下的“下次可取”时间就整体晚了 D：欠下的派发要多挂 D 才交得出去。
// 修后这两处读 Config.SystemNow（Mod 注入 time.Now），活动窗口、宽限、开关窗仍是业务时钟。
func TestDispatchBackoffAndProofExpiryAreSystemTime(t *testing.T) {
	const offset = 24 * time.Hour
	const gameSID = int32(1000)
	ctx := context.Background()
	system := &activityClock{now: time.Unix(1_700_000_000, 0)}
	business := func() time.Time { return system.Now().Add(offset) }

	// 第一轮：偏移 +24h。
	first, _ := newActivityService(t, func(cfg *Config) {
		cfg.Dispatches = newMemoryOwedDispatches()
		cfg.Now, cfg.SystemNow = business, system.Now
	})
	taken := activityKey("race-taken")
	untouched := activityKey("race-untouched")
	for _, key := range []Key{taken, untouched} {
		openActivity(t, first, key, gameSID)
		if _, err := first.NotifyPhase(ctx, key, gameSID); err != nil {
			t.Fatalf("%s: notify: %v", key, err)
		}
	}
	if _, err := first.AttemptDispatch(ctx, taken, gameSID); err != nil {
		t.Fatalf("first attempt: %v", err)
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
	if want := system.Now().Add(first.cfg.ReservationTTL).Unix(); proof.ExpiresAtUnix != want {
		t.Errorf("proof ExpiresAtUnix = %d, %v from system time + ReservationTTL; want the system clock (the ledger TTL is relative system time)",
			proof.ExpiresAtUnix, time.Duration(proof.ExpiresAtUnix-want)*time.Second)
	}

	// 第二轮：同一份存储，偏移拨回 0。
	cfg := first.cfg
	cfg.Now, cfg.SystemNow = system.Now, system.Now
	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 没取过的派发创建时就该立即可取。
	if _, err := second.AttemptDispatch(ctx, untouched, gameSID); err != nil {
		t.Errorf("a dispatch created due immediately in the previous run: %v; want it due now, not an offset later", err)
	}
	// 取过一次的派发，退避（5s）过后就该可取，不多挂一个偏移。
	system.advance(first.cfg.DispatchBackoff)
	owed, err := second.OwedDispatches(ctx, taken.GroupID, gameSID, 10)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, key := range owed {
		listed = listed || key == taken
	}
	if !listed {
		t.Errorf("after the backoff the game is owed %v; want %s listed", owed, taken)
	}
	if _, err := second.AttemptDispatch(ctx, taken, gameSID); err != nil {
		if errors.Is(err, ErrDispatchNotDue) {
			t.Fatalf("after the %v backoff, moving the offset back by %v still holds the owed dispatch: %v", first.cfg.DispatchBackoff, offset, err)
		}
		t.Fatalf("second attempt: %v", err)
	}
}

// 管理员重开派发是“立即可取”。owed 索引（Redis 有序集合）按 NextAttemptAtUnix 打分、用系统时钟查询；
// 重开把它写成系统时钟的当前时间，偏移非 0 时分数也不会落在系统时钟的未来。
func TestAReopenedDispatchIsOwedNowOnTheSystemClock(t *testing.T) {
	const offset = 24 * time.Hour
	const gameSID = int32(1000)
	ctx := context.Background()
	system := &activityClock{now: time.Unix(1_700_000_000, 0)}
	service, _ := newActivityService(t, func(cfg *Config) {
		cfg.Dispatches = newMemoryOwedDispatches()
		cfg.DispatchMaxAttempts = 1
		cfg.Now = func() time.Time { return system.Now().Add(offset) }
		cfg.SystemNow = system.Now
	})
	key := activityKey("race-reopen")
	openActivity(t, service, key, gameSID)
	if _, err := service.NotifyPhase(ctx, key, gameSID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AttemptDispatch(ctx, key, gameSID); err != nil {
		t.Fatal(err)
	}
	system.advance(time.Minute)
	if _, err := service.AttemptDispatch(ctx, key, gameSID); !errors.Is(err, ErrDispatchExhausted) {
		t.Fatalf("second attempt = %v, want exhausted", err)
	}
	reopened, err := service.ReopenDispatch(ctx, key, gameSID, "game is back")
	if err != nil {
		t.Fatal(err)
	}
	score, owed := owedDispatchEntry(reopened)
	if !owed || int64(score) > system.Now().Unix() {
		t.Fatalf("reopened dispatch scores %v in the owed index (owed=%v), system now %d: the game would not find it until the score passes",
			int64(score), owed, system.Now().Unix())
	}
	if !reopened.Due(system.Now().Unix()) {
		t.Fatalf("reopened dispatch %+v is not due at system now", reopened)
	}
}
