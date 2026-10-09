package frame

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func validDelta() Frame {
	return Frame{
		SnapshotMeta: SnapshotMeta{RoomID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: Delta, BaseTick: 4,
		Objects: []ObjectDelta{
			{Operation: ObjectCreate, Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 10, Components: []ComponentDelta{{Operation: ComponentSet, TypeID: 1, SchemaVersion: 1, Data: []byte("pos")}}},
			{Operation: ObjectUpdate, Ref: ObjectRef{ID: 2, Generation: 1}, Components: []ComponentDelta{{Operation: ComponentRemove, TypeID: 3}}},
			{Operation: ObjectRemove, Ref: ObjectRef{ID: 3, Generation: 2}},
		},
	}
}

func expectFrameErr(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// The delta frame is the wire format between the room and every replica.
// validateFrame runs on both encode and decode; every structural rule is
// pinned here by sentinel and message. Only round trips had tests.
func TestEncodeFrameRefusesEachMalformedDelta(t *testing.T) {
	limits := DefaultLimits()
	if _, err := Encode(validDelta(), limits); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		mutate   func(*Frame)
		sentinel error
		text     string
	}{
		{"zero room", func(f *Frame) { f.RoomID = 0 }, ErrInvalidFrame, "invalid snapshot metadata"},
		{"zero schema", func(f *Frame) { f.SchemaVersion = 0 }, ErrInvalidFrame, "invalid snapshot metadata"},
		{"unknown kind", func(f *Frame) { f.Kind = Kind(9) }, ErrInvalidFrame, "unknown frame kind 9"},
		{"full frame with baseline", func(f *Frame) { f.Kind = Full; f.Objects = f.Objects[:1] }, ErrInvalidFrame, "full frame has baseline"},
		{"delta without baseline", func(f *Frame) { f.BaseTick = 0 }, ErrBaselineMismatch, ""},
		{"delta baseline not before tick", func(f *Frame) { f.BaseTick = f.Tick }, ErrBaselineMismatch, ""},
		{"invalid object ref", func(f *Frame) { f.Objects[0].Ref.Generation = 0 }, ErrInvalidObjectRef, ""},
		{"invalid object operation", func(f *Frame) { f.Objects[0].Operation = ObjectOperation(9) }, ErrInvalidFrame, "invalid object operation"},
		{"duplicate object", func(f *Frame) { f.Objects[1].Ref = f.Objects[0].Ref }, ErrInvalidFrame, "duplicate object delta"},
		{"full frame with an update", func(f *Frame) { f.Kind, f.BaseTick = Full, 0 }, ErrInvalidFrame, "full frame contains non-create operation"},
		{"create without archetype", func(f *Frame) { f.Objects[0].Archetype = 0 }, ErrInvalidObjectRef, ""},
		{"remove carrying components", func(f *Frame) {
			f.Objects[2].Components = []ComponentDelta{{Operation: ComponentRemove, TypeID: 1}}
		}, ErrInvalidFrame, "remove object has components"},
		{"component without type", func(f *Frame) { f.Objects[0].Components[0].TypeID = 0 }, ErrInvalidFrame, "invalid component delta"},
		{"component with unknown operation", func(f *Frame) { f.Objects[0].Components[0].Operation = ComponentOperation(9) }, ErrInvalidFrame, "invalid component delta"},
		{"duplicate component", func(f *Frame) {
			f.Objects[0].Components = append(f.Objects[0].Components, f.Objects[0].Components[0])
		}, ErrInvalidFrame, "duplicate component delta"},
		{"create removing a component", func(f *Frame) {
			f.Objects[0].Components[0] = ComponentDelta{Operation: ComponentRemove, TypeID: 1}
		}, ErrInvalidFrame, "create object contains component remove"},
		{"set without schema", func(f *Frame) { f.Objects[0].Components[0].SchemaVersion = 0 }, ErrInvalidFrame, "component schema version is zero"},
		{"remove carrying data", func(f *Frame) { f.Objects[1].Components[0].Data = []byte("x") }, ErrInvalidFrame, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := validDelta()
			tc.mutate(&f)
			_, err := Encode(f, limits)
			expectFrameErr(t, err, tc.sentinel, tc.text)
		})
	}
}

// Size limits are enforced on both sides, and the decoder refuses every way a
// frame can be truncated, padded, or lie about its header.
func TestFrameCodecEnforcesLimitsAndRefusesCorruptBytes(t *testing.T) {
	limits := DefaultLimits()
	encoded, err := Encode(validDelta(), limits)
	if err != nil {
		t.Fatal(err)
	}
	tight := limits
	tight.MaxFrameBytes = len(encoded) - 1
	if _, err := Encode(validDelta(), tight); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("encode over MaxFrameBytes = %v", err)
	}
	if _, err := Decode(encoded, tight); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("decode over MaxFrameBytes = %v", err)
	}
	if _, err := Decode(nil, limits); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("decode of nothing = %v", err)
	}
	small := limits
	small.MaxComponentBytes = 2
	if _, err := Decode(encoded, small); !errors.Is(err, ErrComponentTooLarge) {
		t.Fatalf("component over MaxComponentBytes = %v", err)
	}
	if _, err := Decode(encoded[:len(encoded)-2], limits); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("truncated frame = %v", err)
	}
	if _, err := Decode(append(append([]byte(nil), encoded...), 0), limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "trailing bytes") {
		t.Fatalf("trailing bytes = %v", err)
	}
	foreign := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint32(foreign[0:4], 0xdeadbeef)
	if _, err := Decode(foreign, limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "unsupported header") {
		t.Fatalf("foreign magic = %v", err)
	}
	newer := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint16(newer[4:6], ProtocolVersion+1)
	if _, err := Decode(newer, limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "unsupported header") {
		t.Fatalf("newer protocol = %v", err)
	}
	reserved := append([]byte(nil), encoded...)
	reserved[7] = 1
	if _, err := Decode(reserved, limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "unsupported header") {
		t.Fatalf("reserved byte set = %v", err)
	}
	decoded, err := Decode(encoded, limits)
	if err != nil || len(decoded.Objects) != 3 || decoded.BaseTick != 4 {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
}
