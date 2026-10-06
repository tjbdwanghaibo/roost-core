package mail

import (
	"context"
	"errors"
	"testing"
	"time"
)

// D-L3（维护者第六轮决定）：邮件的创建、过期是业务时间（Config.Now，部署时是带 time.logic_offset 的
// 业务时钟）。业务时间只许前进（docs/feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md）之后，领取租约
// ClaimDeadlineUnix 也回到业务时钟，删掉了 v1.21.0 为“偏移往回调”拆出来的 Config.SystemNow：租约只在
// mail 服务内部比较，所有实例同一个偏移。这里证明单调业务时间下的行为与拆分时相同：偏移 +24h 恒定时
// 租约恰好持续 ClaimLease，偏移前拨只让租约提前结束，重试拿到同一个 token；过期仍按业务时间。
func TestMailExpiryAndTheClaimLeaseRunOnTheMonotonicBusinessClock(t *testing.T) {
	ctx := context.Background()
	wall := &clock{now: time.Unix(1_700_000_000, 0)}
	offset := 24 * time.Hour
	business := func() time.Time { return wall.Now().Add(offset) }
	h := newHarness(t, func(cfg *Config) { cfg.Now = business })

	request := withAttachment(directTo(1), "reward")
	request.ExpiresInSeconds = 3600
	envelope := mustSend(t, h, request)
	if want := business().Add(time.Hour).Unix(); envelope.ExpiresAtUnix != want {
		t.Fatalf("the mail expires at %d, want %d: business now + 1h", envelope.ExpiresAtUnix, want)
	}
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := business().Add(DefaultClaimLease).Unix(); claim.DeadlineUnix != want {
		t.Fatalf("the claim lease ends at %d, want %d: business now + lease", claim.DeadlineUnix, want)
	}
	// 偏移恒定：租约恰好持续 ClaimLease。
	wall.advance(DefaultClaimLease - time.Second)
	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("a second before the lease ends: %v, want ErrClaimHeld", err)
	}
	wall.advance(time.Second)
	retried, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatalf("after the lease: %v", err)
	}
	if retried.Token != claim.Token {
		t.Fatalf("the retry got token %q, want the same token %q", retried.Token, claim.Token)
	}
	// 偏移前拨一分钟（下一次启动用 +24h1m）：刚续上的租约提前结束，重试拿到的仍是同一个 token，不会重复发放。
	offset += time.Minute
	again, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatalf("after moving the offset forward: %v, want the lease over", err)
	}
	if again.Token != claim.Token {
		t.Fatalf("after moving the offset forward the retry got token %q, want %q", again.Token, claim.Token)
	}
	// 过期按业务时间：前拨过了过期点，邮件过期。
	offset += 2 * time.Hour
	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("ReserveClaim after the business clock passed the expiry = %v, want ErrExpired", err)
	}
}
