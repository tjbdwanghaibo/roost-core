package entitysync

import (
	"bytes"
	"context"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"testing"
)

// 传输故意保留原始切片，验证之后的会话和 Flush 不会覆盖已准入字节。
func TestEncodeWorkspaceDoesNotMutateRetainedFrames(t *testing.T) {
	var retained, copies [][]byte
	transport := TransportFunc(func(_ context.Context, _ SessionID, data []byte) error {
		retained = append(retained, data)
		copies = append(copies, bytes.Clone(data))
		return nil
	})
	manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxFrameBytes: 180}})
	packs := 0
	states := make([]*entity.SubjectSyncState, 3)
	for i := range states {
		states[i] = testSubject(t, int64(i+1), &packs)
		if err := manager.Register(states[i]); err != nil {
			t.Fatal(err)
		}
	}
	open(t, manager, 1, 2)
	for _, sid := range []SessionID{1, 2} {
		for _, state := range states {
			mustSubscribe(t, manager, sid, state.SubjectID(), entity.SyncProfile{})
		}
	}
	mustFlush(t, manager)
	for range 8 {
		for _, state := range states {
			state.MarkDirty(4)
		}
		mustFlush(t, manager)
	}
	if len(retained) <= 18 {
		t.Fatalf("expected split frames, got %d", len(retained))
	}
	for i, data := range retained {
		if !bytes.Equal(data, copies[i]) {
			t.Fatalf("frame %d overwritten", i)
		}
		if _, err := frame.Decode(data, frame.DefaultLimits()); err != nil {
			t.Fatal(err)
		}
	}
	// 工作区清空活引用；容量可保留，组件载荷不能保留。
	for _, obj := range manager.encodeWork.objects[:cap(manager.encodeWork.objects)] {
		if obj.Components != nil {
			t.Fatal("object references retained")
		}
	}
	for _, component := range manager.encodeWork.components[:cap(manager.encodeWork.components)] {
		if component.Data != nil {
			t.Fatal("payload reference retained")
		}
	}
}
