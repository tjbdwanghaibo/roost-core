package skill

import "testing"

func TestRedactRuntimeValueRecursivelyFiltersSensitiveReferences(t *testing.T) {
	visible := func(entity EntityID) (bool, error) { return entity == 1, nil }
	list, err := RedactRuntimeValue(EntityListRuntimeValue([]EntityID{1, 2}), RuntimeValueRedactionOptions{EntityVisible: visible})
	if err != nil {
		t.Fatal(err)
	}
	entities, ok := list.Entities()
	if !ok || len(entities) != 1 || entities[0] != 1 {
		t.Fatalf("entities = %v, %v", entities, ok)
	}
	hidden, err := RedactRuntimeValue(EntityRuntimeValue(2), RuntimeValueRedactionOptions{EntityVisible: visible})
	if err != nil || hidden.Present() {
		t.Fatalf("hidden = %#v, err=%v", hidden, err)
	}
	spatial, err := RedactRuntimeValue(PositionRuntimeValue(Position{X: 10}), RuntimeValueRedactionOptions{EntityVisible: visible, RedactSpatial: true})
	if err != nil || spatial.Present() {
		t.Fatalf("spatial = %#v, err=%v", spatial, err)
	}
}
