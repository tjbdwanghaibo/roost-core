package chat

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
