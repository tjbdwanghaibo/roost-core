package dataengine

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"reflect"
	"testing"
)

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
