package syncbus

import (
	"context"
	"strings"
	"testing"
)

// U-0147 · C2 · nightly gap map core `syncbus` 5/6：nil 同步器、缺总线 / 主题 / 键函数 /
// 应用函数的配置不能启动或发布；键为零的补丁拒绝发布。
func TestPatchSyncerRefusesIncompleteConfigurationAndZeroKeys(t *testing.T) {
	ctx := context.Background()
	keyOf := func(v int64) int64 { return v }
	apply := func(context.Context, int64) error { return nil }
	var none *PatchSyncer[int64]
	if err := none.Start(); err == nil || !strings.Contains(err.Error(), "syncer is nil") {
		t.Fatalf("Start on a nil syncer = %v", err)
	}
	if err := none.Publish(ctx, 1); err == nil || !strings.Contains(err.Error(), "syncer is nil") {
		t.Fatalf("Publish on a nil syncer = %v", err)
	}
	noApply := NewPatchSyncer[int64](newPatchFakeBus(), PatchSyncerConfig[int64]{Topic: "patch", KeyOf: keyOf})
	if err := noApply.Start(); err == nil || !strings.Contains(err.Error(), "apply function are required") {
		t.Fatalf("Start without Apply = %v", err)
	}
	noTopic := NewPatchSyncer[int64](newPatchFakeBus(), PatchSyncerConfig[int64]{KeyOf: keyOf, Apply: apply})
	if err := noTopic.Publish(ctx, 1); err == nil || !strings.Contains(err.Error(), "bus, topic and key function") {
		t.Fatalf("Publish without a topic = %v", err)
	}
	bus := newPatchFakeBus()
	full := NewPatchSyncer[int64](bus, PatchSyncerConfig[int64]{Topic: "patch", KeyOf: keyOf, Apply: apply})
	if err := full.Start(); err != nil {
		t.Fatalf("Start with a full config = %v", err)
	}
	if err := full.Publish(ctx, 0); err == nil || !strings.Contains(err.Error(), "key is zero") {
		t.Fatalf("Publish with a zero key = %v", err)
	}
	if len(bus.published) != 0 {
		t.Fatal("a refused publish reached the bus")
	}
	if err := full.Publish(ctx, 7); err != nil || len(bus.published) != 1 {
		t.Fatalf("Publish with a real key = %v, published=%d", err, len(bus.published))
	}
}
