package mirror

import (
	"context"
	"strings"
	"testing"
)

// U-0149 · C2 · gap map core `mirror` 4/14：nil 复制器不能启动；未知操作码拒绝发布；未初始化的
// 复制器 / 键为零的删除拒绝。
func TestReplicatorRefusesNilUninitialisedAndMalformedPublishes(t *testing.T) {
	ctx := context.Background()
	var none *Replicator
	if err := none.Start(); err == nil || !strings.Contains(err.Error(), "replicator is nil") {
		t.Fatalf("Start on a nil replicator = %v", err)
	}
	if err := (&Replicator{}).PublishDelete(ctx, 1, 1); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("PublishDelete on an uninitialised replicator = %v", err)
	}
	bus := newFakeBus()
	rep := New(bus, "topic", &fakeStore{})
	if err := rep.Publish(ctx, Envelope{Key: 1, Version: 1, Op: Op(99)}); err == nil || !strings.Contains(err.Error(), "unsupported operation 99") {
		t.Fatalf("Publish with an unknown op = %v", err)
	}
	if err := rep.PublishDelete(ctx, 0, 1); err == nil || !strings.Contains(err.Error(), "delete key is zero") {
		t.Fatalf("PublishDelete with key 0 = %v", err)
	}
	if err := rep.PublishDelete(ctx, 7, 1); err != nil {
		t.Fatalf("PublishDelete with a real key = %v", err)
	}
}
