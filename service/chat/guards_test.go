// 聊天入参守卫与边角：频道与发送者校验、淘汰后的请求键仍识别发送者、策略变化后的重放、旧账本格式拒绝、自定义共享频道读不到私聊。

package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 频道种类与规则不符、成对作用域缺参与者、发送者名非 UTF-8、空消息类型各以对应哨兵拒绝。
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

func TestEvictedRequestKeyStillIdentifiesSender(t *testing.T) {
	h := newHarness(t, withRules(ChannelRule{Kind: ChannelWorld, Scope: ScopeShared, Retain: 1}))
	mustPublish(t, h, role(1), text("first", "key", world()))
	mustPublish(t, h, role(1), text("second", "other", world()))
	if _, err := h.service.Publish(context.Background(), role(2), text("collision", "key", world())); !errors.Is(err, ErrConflict) {
		t.Fatalf("evicted foreign key returned %v instead of conflict", err)
	}
}
func TestReplaySurvivesPublishPolicyChange(t *testing.T) {
	policy := &recordingPolicy{}
	h := newHarness(t, withPolicy(policy))
	req := text("first", "key", world())
	first := mustPublish(t, h, role(1), req)
	policy.publishErr = errors.New("muted")
	if replay, err := h.service.Publish(context.Background(), role(1), req); err != nil || replay.Seq != first.Seq {
		t.Fatalf("committed replay refused after policy change: %v", err)
	}
	if _, err := h.service.Publish(context.Background(), role(1), text("new", "new", world())); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("new publish bypassed policy: %v", err)
	}
}
func TestZeroRetentionAgeDisablesAgePruning(t *testing.T) {
	h := newHarness(t, withRetentionAge(0))
	mustPublish(t, h, role(1), text("keep", "key", world()))
	h.clock.advance(365 * 24 * time.Hour)
	ref, err := h.store.Resolve(world(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.Prune(context.Background(), ref, 10); err != nil || n != 0 {
		t.Fatalf("zero retention unexpectedly removed %d: %v", n, err)
	}
}

func TestChannelCodecRejectsOldUnidentifiedLedger(t *testing.T) {
	if _, err := (channelStateCodec{}).Decode([]byte(`{"version":1,"last_seq":1}`)); err == nil {
		t.Fatal("legacy channel state accepted")
	}
	current := channelState{Requests: map[string]requestReceipt{"request": {Sequence: 1, Origin: OriginRole, RoleID: 5}}}
	raw, err := (channelStateCodec{}).Encode(current)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := (channelStateCodec{}).Decode(raw)
	if err != nil || restored.Requests["request"].RoleID != 5 {
		t.Fatalf("ledger sender lost: %+v %v", restored, err)
	}
}

func TestCustomSharedChannelCannotReadPrivatePair(t *testing.T) {
	bodies := NewBodyRegistry()
	bodies.MustRegister("text", BodySpec{Validator: TextValidator(100)})
	policy := PolicyFuncs{
		Publish: func(context.Context, Sender, Channel) error { return nil },
		Read: func(_ context.Context, viewer Sender, ch Channel) error {
			if ch.Kind == ChannelPrivate && viewer.RoleID != 1 && viewer.RoleID != 2 {
				return fmt.Errorf("not a participant")
			}
			return nil
		},
	}
	s, err := NewStore(versionstore.NewMemoryStore[string, channelState](), Config{
		Policy: policy, Bodies: bodies, Rules: []ChannelRule{{Kind: "private:1", Scope: ScopeShared, RequiresTarget: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = s.Append(ctx, Sender{RoleID: 1}, PublishRequest{Channel: Channel{Kind: ChannelPrivate, Target: 2}, Type: "text", Body: []byte("secret"), RequestID: "private-msg"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.History(ctx, Sender{RoleID: 3}, HistoryQuery{Channel: Channel{Kind: "private:1", Target: 2}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) > 0 {
		t.Fatalf("shared channel disclosed private message: from=%d body=%s", page.Messages[0].From.RoleID, page.Messages[0].Body)
	}
}
