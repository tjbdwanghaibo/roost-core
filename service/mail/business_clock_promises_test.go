package mail

import (
	"context"
	"errors"
	"testing"
	"time"
)

// D-L3（维护者第六轮决定）：邮件的创建、过期是业务时间（Config.Now，部署时是带 time.logic_offset 的
// 业务时钟），领取租约 ClaimDeadlineUnix 是系统时间（Config.SystemNow）。业务时钟比系统时钟快一天时，
// 过期按业务时间算、租约按真实时间算；只前拨业务时间就能让邮件过期，系统时间不用动。
func TestMailExpiryIsBusinessTimeAndTheClaimLeaseIsSystemTime(t *testing.T) {
	ctx := context.Background()
	system := &clock{now: time.Unix(1_700_000_000, 0)}
	business := &clock{now: system.Now().Add(24 * time.Hour)}
	h := newHarness(t, func(cfg *Config) { cfg.Now, cfg.SystemNow = business.Now, system.Now })

	request := withAttachment(directTo(1), "reward")
	request.ExpiresInSeconds = 3600
	envelope := mustSend(t, h, request)
	if want := business.Now().Add(time.Hour).Unix(); envelope.ExpiresAtUnix != want {
		t.Fatalf("the mail expires at %d, want %d: business now + 1h", envelope.ExpiresAtUnix, want)
	}
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := system.Now().Add(30 * time.Second).Unix(); claim.DeadlineUnix != want {
		t.Fatalf("the claim lease ends at %d, want %d: system now + lease, whatever the business offset", claim.DeadlineUnix, want)
	}
	// 只前拨业务时间：邮件过期，系统时间一秒没过。
	business.advance(2 * time.Hour)
	if _, err := h.service.ReserveClaim(ctx, 1, envelope.ID, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("ReserveClaim after the business clock passed the expiry = %v, want ErrExpired", err)
	}
}
