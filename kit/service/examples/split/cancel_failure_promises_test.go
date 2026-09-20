package split

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/service/mail"
)

// stubMail answers the four calls the reward flow makes; CancelClaim fails
// on demand. Everything else is the embedded nil interface and must not be
// reached.
type stubMail struct {
	mail.Mail
	cancelErr error
	cancelled int
}

func (s *stubMail) Send(context.Context, mail.SendRequest) (mail.Envelope, error) {
	return mail.Envelope{ID: "m-1"}, nil
}
func (s *stubMail) ReserveClaim(context.Context, int64, string, string) (mail.Claim, error) {
	return mail.Claim{MailID: "m-1", Token: "tok-1"}, nil
}
func (s *stubMail) CancelClaim(context.Context, int64, string, string) (bool, error) {
	s.cancelled++
	return s.cancelErr == nil, s.cancelErr
}
func (s *stubMail) CommitClaim(context.Context, int64, string, string) (mail.Entry, error) {
	return mail.Entry{}, nil
}

// U-0152 · C2 · split consumer.go:78（U-0150 留待）：授予失败后取消预留也失败时，两个错误都要报出来；
// 取消成功时只报授予错误。
func TestGrantFailureReportsAFailedClaimReleaseToo(t *testing.T) {
	ctx := context.Background()
	original := grantToInventory
	t.Cleanup(func() { grantToInventory = original })
	grantErr := errors.New("inventory full")
	grantToInventory = func(context.Context, int64, string, []byte) error { return grantErr }

	build := func(t *testing.T, stub *stubMail) *RewardFlow {
		t.Helper()
		registry := app.NewRegistry(config())
		if err := registry.Register(mail.CapabilityName, stub); err != nil {
			t.Fatal(err)
		}
		flow, err := NewRewardFlow(registry)
		if err != nil {
			t.Fatal(err)
		}
		return flow
	}
	cancelErr := errors.New("redis unreachable")
	failing := &stubMail{cancelErr: cancelErr}
	err := build(t, failing).GrantSeasonReward(ctx, 7, "s1")
	if !errors.Is(err, grantErr) || !strings.Contains(err.Error(), "releasing the claim also failed") || !strings.Contains(err.Error(), cancelErr.Error()) || failing.cancelled != 1 {
		t.Fatalf("grant failure with a failing cancel = %v (cancelled %d)", err, failing.cancelled)
	}
	releasing := &stubMail{}
	err = build(t, releasing).GrantSeasonReward(ctx, 7, "s1")
	if !errors.Is(err, grantErr) || strings.Contains(err.Error(), "releasing the claim also failed") || releasing.cancelled != 1 {
		t.Fatalf("grant failure with a working cancel = %v (cancelled %d)", err, releasing.cancelled)
	}
}
