// 帧编解码：畸形增量逐项拒绝、线格式计数与大小上限、解码先校验头部再分配。

package frame

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func validDelta() Frame {
	return Frame{
		SnapshotMeta: SnapshotMeta{StreamID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: Delta, BaseTick: 4,
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

// The delta frame is the wire format between the stream and every replica.
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
		{"zero stream", func(f *Frame) { f.StreamID = 0 }, ErrInvalidFrame, "invalid snapshot metadata"},
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

// 线格式的计数字段是 uint16：对象数超过 65535 必须拒绝而不是回绕成 0 编出一个“空帧”；帧大小上限在对象头与组件两级都生效；
// 校验器的对象数上限在编码侧也要拒绝。解码侧对头部声称的对象数 / 组件数 / 载荷长度在分配与读取之前按上限拒绝，回答“超限”
// 而不是“截断”。单对象 65536 个组件的分支被校验器先以重复拒绝，编码路径到不了，保留为不可达防御。

func TestEncodeFrameRefusesCountsAndSizesTheWireFormatCannotCarry(t *testing.T) {
	// 65536 个对象：uint16 计数会回绕成 0。放宽 MaxObjects 让校验器放行。
	wide := DefaultLimits()
	wide.MaxObjects = 40000
	frame := Frame{SnapshotMeta: SnapshotMeta{StreamID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: Delta, BaseTick: 4}
	frame.Objects = make([]ObjectDelta, 0, 65536)
	for id := 1; id <= 65535; id++ {
		frame.Objects = append(frame.Objects, ObjectDelta{Operation: ObjectRemove, Ref: ObjectRef{ID: uint16(id), Generation: 1}})
	}
	frame.Objects = append(frame.Objects, ObjectDelta{Operation: ObjectRemove, Ref: ObjectRef{ID: 1, Generation: 2}})
	if _, err := Encode(frame, wide); !errors.Is(err, ErrObjectLimit) {
		t.Fatalf("Encode with 65536 objects = %v, want ErrObjectLimit", err)
	}

	// 只有对象头、没有组件：帧大小上限也要在对象头处生效。
	headersOnly := Frame{SnapshotMeta: SnapshotMeta{StreamID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: Delta, BaseTick: 4,
		Objects: []ObjectDelta{{Operation: ObjectRemove, Ref: ObjectRef{ID: 1, Generation: 1}}, {Operation: ObjectRemove, Ref: ObjectRef{ID: 2, Generation: 1}}}}
	tiny := DefaultLimits()
	tiny.MaxFrameBytes = 40 // 32 字节头 + 一个 9 字节对象头就越界
	if encoded, err := Encode(headersOnly, tiny); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Encode over MaxFrameBytes with component-less objects = (%d bytes, %v), want ErrFrameTooLarge", len(encoded), err)
	}

	// 对象头放得下、加上组件就越界：组件级的大小检查。
	oneComponent := Frame{SnapshotMeta: SnapshotMeta{StreamID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: Delta, BaseTick: 4,
		Objects: []ObjectDelta{{Operation: ObjectCreate, Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 10,
			Components: []ComponentDelta{{Operation: ComponentSet, TypeID: 1, SchemaVersion: 1, Data: []byte("pos")}}}}}
	snug := DefaultLimits()
	snug.MaxFrameBytes = 45 // 32 + 9 放得下对象头，再加 9 + 3 的组件就越界
	if encoded, err := Encode(oneComponent, snug); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Encode whose component pushes past MaxFrameBytes = (%d bytes, %v), want ErrFrameTooLarge", len(encoded), err)
	}

	// 编码侧的对象数上限（MaxObjects*2）。
	few := DefaultLimits()
	few.MaxObjects = 1
	if _, err := Encode(validDelta(), few); !errors.Is(err, ErrObjectLimit) {
		t.Fatalf("Encode with 3 objects under MaxObjects=1 = %v, want ErrObjectLimit", err)
	}
}

func TestDecodeFrameRefusesHeaderCountsBeforeReadingOrAllocating(t *testing.T) {
	limits := DefaultLimits()
	encoded, err := Encode(validDelta(), limits)
	if err != nil {
		t.Fatal(err)
	}
	// 头部声称 65535 个对象，后面什么都没有：答案是"超限"，不是"截断"。
	header := append([]byte(nil), encoded[:32]...)
	binary.BigEndian.PutUint16(header[30:32], 0xFFFF)
	if _, err := Decode(header, limits); !errors.Is(err, ErrObjectLimit) {
		t.Fatalf("Decode with an oversized object count = %v, want ErrObjectLimit", err)
	}
	// 第一个对象声称 65535 个组件。
	object := append([]byte(nil), encoded[:41]...)
	binary.BigEndian.PutUint16(object[30:32], 1)
	binary.BigEndian.PutUint16(object[39:41], 0xFFFF)
	if _, err := Decode(object, limits); !errors.Is(err, ErrComponentLimit) {
		t.Fatalf("Decode with an oversized component count = %v, want ErrComponentLimit", err)
	}
	// 第一个组件声称的载荷长度超过 MaxComponentBytes（也超过剩余字节）。
	component := append([]byte(nil), encoded[:50]...)
	binary.BigEndian.PutUint16(component[30:32], 1)
	binary.BigEndian.PutUint16(component[39:41], 1)
	binary.BigEndian.PutUint32(component[46:50], uint32(limits.MaxComponentBytes+1))
	if _, err := Decode(component, limits); !errors.Is(err, ErrComponentTooLarge) {
		t.Fatalf("Decode with an oversized payload length = %v, want ErrComponentTooLarge", err)
	}
	// 载荷长度在上限内但超过剩余字节：同样是超限而不是分配后再截断。
	binary.BigEndian.PutUint32(component[46:50], 64)
	if _, err := Decode(component, limits); !errors.Is(err, ErrComponentTooLarge) {
		t.Fatalf("Decode with a payload length past the end = %v, want ErrComponentTooLarge", err)
	}
}
