// 数据模型：空记录判定、效果复制、数据库作用域与安全的嵌套补丁路径、投影等待、命名同步视图。

package dataengine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

func TestCommitRecordEmptyIncludesReceipts(t *testing.T) {
	if (CommitRecord{Receipts: []Receipt{{Namespace: "saga", ID: "step-1"}}}).Empty() {
		t.Fatal("record containing a receipt must not be empty")
	}
}

func TestCloneEffectPreservesAndDetachesHeaders(t *testing.T) {
	original := Effect{Headers: map[string]string{"trace": "abc"}}
	cloned := CloneEffect(original)
	original.Headers["trace"] = "changed"
	if cloned.Headers["trace"] != "abc" {
		t.Fatalf("cloned headers=%v", cloned.Headers)
	}
}

type scopedDocument struct{ scope DatabaseScope }

func (value scopedDocument) DbScope() DatabaseScope { return value.scope }

func TestResolveDatabaseScopeDefaultsAndUsesTypedScope(t *testing.T) {
	if got := ResolveDatabaseScope(nil); got != DatabaseGlobal {
		t.Fatalf("default scope=%d", got)
	}
	if got := ResolveDatabaseScope(scopedDocument{scope: DatabaseServer}); got != DatabaseServer {
		t.Fatalf("typed scope=%d", got)
	}
}

func TestMapPatchPathBuildsOnlySafeNestedPaths(t *testing.T) {
	if path, ok := MapPatchPath("items", int64(100)); !ok || path != "items.100" {
		t.Fatalf("path=%q ok=%v", path, ok)
	}
	for _, key := range []string{"bad.key", "$bad", "", "bad\x00key"} {
		if path, ok := MapPatchPath("items", key); ok {
			t.Fatalf("unsafe key %q produced %q", key, path)
		}
	}
}

type projectionTestTicket struct {
	done chan struct{}
	err  error
}

func (ticket projectionTestTicket) Done() <-chan struct{} { return ticket.done }
func (ticket projectionTestTicket) Err() error            { return ticket.err }

func TestWaitProjectionHonorsTicketAndContext(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if err := WaitProjection(context.Background(), projectionTestTicket{done: done}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("projection failed")
	if err := WaitProjection(context.Background(), projectionTestTicket{done: done, err: want}); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitProjection(ctx, projectionTestTicket{done: make(chan struct{})}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestNamedSyncViewsSelectFieldsAndRejectUnknownViews(t *testing.T) {
	fields := []SyncFieldMeta{{Name: "Position", WireName: "pos", Bit: 1}, {Name: "Gold", WireName: "gold", Bit: 8}}
	public := entity.SyncProfile{Key: "public", LOD: 1}
	views, err := NewSyncViewSet(fields,
		entity.NamedSyncView{Profile: entity.SyncProfile{}, Fields: []string{"*"}, Priority: -10},
		entity.NamedSyncView{Profile: public, Fields: []string{"pos"}, Priority: 5},
	)
	if err != nil {
		t.Fatal(err)
	}
	var masks []uint64
	packer, err := entity.NewMaskedSyncPacker(views, 1, func(mask uint64) ([]byte, error) { masks = append(masks, mask); return []byte{byte(mask)}, nil })
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := packer.PackSubjectSnapshot(public)
	if err != nil || !reflect.DeepEqual(snapshot.BytesCopy(), []byte{1}) {
		t.Fatalf("snapshot: %v %v", snapshot, err)
	}
	delta, err := packer.PackSubjectDelta(public, 9)
	if err != nil || !snapshot.Equal(delta) {
		t.Fatalf("delta whitelist: %v %v", delta, err)
	}
	empty, err := packer.PackSubjectDelta(public, 8)
	if err != nil || !empty.Empty() || len(masks) != 2 {
		t.Fatalf("private-only dirty must not marshal: %v %v %v", empty, err, masks)
	}
	if _, err := packer.PackSubjectSnapshot(entity.SyncProfile{Key: "unknown"}); !errors.Is(err, entity.ErrSyncViewUnknown) {
		t.Fatal(err)
	}
	if _, err := NewSyncViewSet(fields, entity.NamedSyncView{Fields: []string{"typo"}}); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := entity.NewSyncViewSet(entity.SyncView{}, entity.SyncView{Profile: entity.SyncProfile{Key: "default"}}); err == nil {
		t.Fatal("duplicate normalized view accepted")
	}
	priorities := views.Priorities()
	priorities[public] = -99
	view, _ := views.Lookup(public)
	if view.Priority != 5 {
		t.Fatal("caller changed immutable view")
	}
}
