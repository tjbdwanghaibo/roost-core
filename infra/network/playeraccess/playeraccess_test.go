package playeraccess

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

type responseSession struct{ replies int }

func (*responseSession) Principal() gateway.Principal {
	return gateway.Principal{PlayerID: 1, SessionID: "test"}
}
func (s *responseSession) Reply(context.Context, any) error { s.replies++; return nil }
func (*responseSession) Close(error) error                  { return nil }

// 手动回包（handler 自己 Session.Reply、返回 nil 指针）时，派发不能再编码一次：泛型 handler 返回的
// nil 指针装进 any 后不等于 nil，Dispatch 必须先还原成“不自动回复”。
// 2026-11-13 之前这条在生成工程的 game/playeraccess 里跑（codegen add_test），随包一起搬进 core。
func TestManualReplyDoesNotEncodeAgain(t *testing.T) {
	registry := NewProtocolRegistry()
	type reply struct{}
	encoded := 0
	err := RegisterProtocol[int, *reply](registry, 1, 2,
		func([]byte) (int, error) { return 0, nil },
		func(*reply) ([]byte, error) { encoded++; return nil, nil },
		func(ctx *Context, _ int) (*reply, error) {
			return nil, ctx.Session.Reply(ctx.Context(), &Response{MessageID: 2, Sequence: ctx.Seq})
		})
	if err != nil {
		t.Fatal(err)
	}
	session := &responseSession{}
	response, err := registry.Dispatch(context.Background(), session, 1, 7, nil)
	if err != nil || response != nil || encoded != 0 || session.replies != 1 {
		t.Fatalf("response=%v err=%v encoded=%d replies=%d", response, err, encoded, session.replies)
	}
}

// Seal 之后再登记一律拒绝，不与在途派发竞争。
func TestRegisterAfterSealIsRefused(t *testing.T) {
	registry := NewProtocolRegistry()
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	err := RegisterNotify[int](registry, 3, func([]byte) (int, error) { return 0, nil }, func(*Context, int) error { return nil })
	if !errors.Is(err, ErrRegistrySealed) {
		t.Fatalf("register after seal: %v", err)
	}
}
