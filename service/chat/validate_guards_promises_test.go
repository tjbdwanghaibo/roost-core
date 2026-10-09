package chat

import (
	"errors"
	"strings"
	"testing"
)

// U-0150 · C2 · gap map kit `service/chat` 4/20：频道种类与规则不符、成对作用域缺参与者、发送者名非 UTF-8、
// 空消息类型各以对应哨兵拒绝。
func TestChannelSenderAndBodyTypeValidation(t *testing.T) {
	if err := (Channel{Kind: "world"}).validate(ChannelRule{Kind: "group"}, 1); !errors.Is(err, ErrChannelInvalid) || !strings.Contains(err.Error(), "does not match rule") {
		t.Fatalf("channel kind mismatch = %v", err)
	}
	if err := (Channel{Kind: "pair", Target: 5}).validate(ChannelRule{Kind: "pair", RequiresTarget: true, Scope: ScopePair}, 0); !errors.Is(err, ErrChannelInvalid) || !strings.Contains(err.Error(), "needs a participant") {
		t.Fatalf("pair channel without a participant = %v", err)
	}
	if err := (Sender{RoleID: 1, Name: "\xff\xfe"}).validate(); !errors.Is(err, ErrSenderInvalid) || !strings.Contains(err.Error(), "not valid utf-8") {
		t.Fatalf("sender with an invalid name = %v", err)
	}
	if err := NewBodyRegistry().Register("  ", BodySpec{}); !errors.Is(err, ErrTypeUnknown) || !strings.Contains(err.Error(), "message type is empty") {
		t.Fatalf("Register with a blank type = %v", err)
	}
}
