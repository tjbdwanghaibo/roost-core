package entitysync

import (
	"bytes"
	"context"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"testing"
)

func TestTickEncodingSharesOnlyIdenticalCapturedContent(t *testing.T) {

	update := entity.SubjectSyncUpdate{SubjectID: 6101, Namespace: "cache", Profile: entity.SyncProfile{Key: "near"}, Full: true, Version: 2, Payload: entity.CopyFrozenSyncPayload(1, []byte("full"))}
	var buffer []capturedUpdate
	captured := captureUpdates(&buffer, []entity.SubjectSyncUpdate{update})
	shared, ok := updateFor(captured, update.Profile)
	if !ok {
		t.Fatal("missing capture")
	}
	first, err := shared.encode(0)
	if err != nil {
		t.Fatal(err)
	}
	same, ok := updateFor(captured, update.Profile)
	if !ok || same != shared {
		t.Fatal("same profile did not share its capture")
	}
	again, err := same.encode(0)
	if err != nil {
		t.Fatal(err)
	}
	if &first[0] != &again[0] {
		t.Fatal("identical captured content was encoded twice")
	}
	for _, variant := range []string{"delta", "profile", "subject"} {
		next := update
		switch variant {
		case "delta":
			next.Full = false
			next.BaseVersion = 1
			next.Payload = entity.CopyFrozenSyncPayload(1, []byte("delta"))
		case "profile":
			next.Profile = entity.SyncProfile{Key: "far", LOD: 2}
		case "subject":
			next.SubjectID++
		}
		want, err := EncodeSubjectUpdate(next, 0)
		if err != nil {
			t.Fatal(err)
		}
		separate := captureUpdates(&buffer, []entity.SubjectSyncUpdate{next})
		got, err := separate[0].encode(0)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) || bytes.Equal(got, first) {
			t.Fatalf("%s reused a different capture", variant)
		}
	}
	// 下一个 tick 即使键相同，也必须用新的捕获内容编码。
	update.Version++
	update.Payload = entity.CopyFrozenSyncPayload(1, []byte("new tick"))
	var nextBuffer []capturedUpdate
	nextTick := captureUpdates(&nextBuffer, []entity.SubjectSyncUpdate{update})
	later, err := nextTick[0].encode(0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(later, first) {
		t.Fatal("cache escaped its tick")
	}
}

func TestSharedEncodingKeepsSnapshotsDeltasAndProfilesSeparate(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, TransportFunc(func(ctx context.Context, id SessionID, data []byte) error {
		err := transport.Push(ctx, id, data)
		// 接入层可复用已收到的独立帧，不得破坏后续会话共享的组件缓存。
		clear(data)
		return err
	}), ManagerConfig{})
	packs := 0
	state := testSubject(t, 6102, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2, 3, 4)
	for _, sid := range []SessionID{1, 2, 4} {
		profile := entity.SyncProfile{Key: "near"}
		if sid == 2 {
			profile = entity.SyncProfile{Key: "far", LOD: 2}
		}
		mustSubscribe(t, manager, sid, 6102, profile)
	}
	mustFlush(t, manager)
	for _, sid := range []SessionID{1, 2, 4} {
		transport.take(sid)
	}
	mustSubscribe(t, manager, 3, 6102, entity.SyncProfile{Key: "near"})
	state.MarkDirty(1)
	mustFlush(t, manager)
	for _, sid := range []SessionID{1, 2, 3, 4} {
		got := oneFrame(t, transport, sid).updates[6102]
		wantProfile := "near"
		if sid == 2 {
			wantProfile = "far"
		}
		if got.Full != (sid == 3) || got.Profile.Key != wantProfile || got.Version != 1 {
			t.Fatalf("session %d: %+v", sid, got)
		}
	}
}
