package frame

import (
	"fmt"
	"testing"
)

func BenchmarkFrameCodec(b *testing.B) {
	for _, objects := range []int{1, 32, 256} {
		value := Frame{SnapshotMeta: SnapshotMeta{RoomID: 1, Epoch: 1, Tick: 1, SchemaVersion: 1}, Kind: Full}
		for id := 1; id <= objects; id++ {
			value.Objects = append(value.Objects, ObjectDelta{Operation: ObjectCreate, Ref: ObjectRef{ID: uint16(id), Generation: 1}, Archetype: 1,
				Components: []ComponentDelta{{Operation: ComponentSet, TypeID: 1, SchemaVersion: 1, Data: make([]byte, 256)}},
			})
		}
		limits := DefaultLimits()
		limits.MaxObjects = max(limits.MaxObjects, objects)
		encoded, err := Encode(value, limits)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("encode/objects=%d/payload=256", objects), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(encoded)))
			for b.Loop() {
				if _, err := Encode(value, limits); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("decode/objects=%d/payload=256", objects), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(encoded)))
			for b.Loop() {
				if _, err := Decode(encoded, limits); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
