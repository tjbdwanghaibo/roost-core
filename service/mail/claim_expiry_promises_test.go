package mail

import (
	"context"
	"testing"
)

// U-0241 · C4 · RR-20260918-05：一次领取的预留必须带上"这封邮件到什么时候
// 就不能再领了"——因为发奖侧的去重身份要活到那一刻，而它没有别的办法知道。
//
// 背景：game-demo 的附件账本按固定 31 天清理，论证是"31 天 > mail.send_ttl
// 的 720h"。而 send_ttl 只要求为正数，配 40 天是合法的：账本先忘掉，信封还
// 可领，同一封邮件再发一次奖。把寿命绑在别的服务的**配置**上本来就不成立，
// 绑在它的**权威时刻**上才成立——服务这边知道这个时刻，只是没有交出去。
func TestReserveClaimReportsWhenTheMailStopsBeingClaimable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	envelope := mustSend(t, h, withAttachment(directTo(1), "reward"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if claim.ExpiresAtUnix != envelope.ExpiresAtUnix {
		t.Fatalf("the reservation reports expiry %d, want the envelope's %d — a caller that has to remember this claim until it can no longer happen has no other way to know when that is",
			claim.ExpiresAtUnix, envelope.ExpiresAtUnix)
	}
	if claim.ExpiresAtUnix <= 0 {
		t.Fatal("the envelope has no expiry, so nothing can bound a dedupe ledger against it")
	}
	// The lease deadline is a different, much shorter thing: it says when
	// somebody else may re-reserve, not when the mail stops being claimable.
	if claim.DeadlineUnix >= claim.ExpiresAtUnix {
		t.Fatalf("the lease deadline %d is not shorter than the expiry %d; the two would be confusable", claim.DeadlineUnix, claim.ExpiresAtUnix)
	}
}
