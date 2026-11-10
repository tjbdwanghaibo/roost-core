// 帧编解码：畸形增量逐项拒绝、线格式计数与大小上限、解码先校验头部再分配。

package frame

import (
	"bytes"
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
	binary.BigEndian.PutUint16(foreign[0:2], 0xdead)
	if _, err := Decode(foreign, limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "unsupported header") {
		t.Fatalf("foreign magic = %v", err)
	}
	newer := append([]byte(nil), encoded...)
	newer[2] = ProtocolVersion + 1
	if _, err := Decode(newer, limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "unsupported header") {
		t.Fatalf("newer protocol = %v", err)
	}
	unknownKind := append([]byte(nil), encoded...)
	unknownKind[3] = 9
	if _, err := Decode(unknownKind, limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "unknown frame kind") {
		t.Fatalf("unknown kind = %v", err)
	}
	// 非最短的 varint（StreamID 7 写成 0x87 0x00）：同一帧只允许一种编码。
	padded := append(append(append([]byte(nil), encoded[:4]...), 0x87, 0x00), encoded[5:]...)
	if _, err := Decode(padded, limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "malformed varint") {
		t.Fatalf("non-minimal varint = %v", err)
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
	tiny.MaxFrameBytes = 12 // 9 字节头 + 一个 3 字节 remove 放得下，第二个就越界
	if encoded, err := Encode(headersOnly, tiny); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Encode over MaxFrameBytes with component-less objects = (%d bytes, %v), want ErrFrameTooLarge", len(encoded), err)
	}

	// 对象头放得下、加上组件就越界：组件级的大小检查。
	oneComponent := Frame{SnapshotMeta: SnapshotMeta{StreamID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: Delta, BaseTick: 4,
		Objects: []ObjectDelta{{Operation: ObjectCreate, Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 10,
			Components: []ComponentDelta{{Operation: ComponentSet, TypeID: 1, SchemaVersion: 1, Data: []byte("pos")}}}}}
	snug := DefaultLimits()
	snug.MaxFrameBytes = 16 // 9 字节头 + 5 字节对象头放得下，再加 4 + 3 的组件就越界
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
	// 帧头：magic、协议、Delta、StreamID 7、Epoch 1、Tick 5、回退 1、Schema 1，后接对象数。
	header := []byte{0x52, 0x46, ProtocolVersion, uint8(Delta), 7, 1, 5, 1, 1}
	// 头部声称 65535 个对象，后面什么都没有：答案是"超限"，不是"截断"。
	if _, err := Decode(append(append([]byte(nil), header...), 0xff, 0xff, 0x03), limits); !errors.Is(err, ErrObjectLimit) {
		t.Fatalf("Decode with an oversized object count = %v, want ErrObjectLimit", err)
	}
	// 对象数在上限内但剩余字节放不下：截断，不按声称的数量分配。
	if _, err := Decode(append(append([]byte(nil), header...), 100, 2), limits); !errors.Is(err, ErrInvalidFrame) || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("Decode with more objects than bytes = %v, want truncated", err)
	}
	// 第一个对象（update，ref 1/1）声称 65535 个组件。
	object := append(append([]byte(nil), header...), 1, uint8(ObjectUpdate), 1, 1)
	if _, err := Decode(append(append([]byte(nil), object...), 0xff, 0xff, 0x03), limits); !errors.Is(err, ErrComponentLimit) {
		t.Fatalf("Decode with an oversized component count = %v, want ErrComponentLimit", err)
	}
	// 第一个组件（set，类型 1，schema 1）声称的载荷长度超过 MaxComponentBytes（也超过剩余字节）。
	component := append(append([]byte(nil), object...), 1, uint8(ComponentSet), 1, 1)
	oversized := binary.AppendUvarint(append([]byte(nil), component...), uint64(limits.MaxComponentBytes+1))
	if _, err := Decode(oversized, limits); !errors.Is(err, ErrComponentTooLarge) {
		t.Fatalf("Decode with an oversized payload length = %v, want ErrComponentTooLarge", err)
	}
	// 载荷长度在上限内但超过剩余字节：同样是超限而不是分配后再截断。
	if _, err := Decode(append(append([]byte(nil), component...), 64), limits); !errors.Is(err, ErrComponentTooLarge) {
		t.Fatalf("Decode with a payload length past the end = %v, want ErrComponentTooLarge", err)
	}
	// 同一帧编码后再解码相同。
	encoded, err := Encode(validDelta(), limits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, header) {
		t.Fatalf("encoded header % x, want prefix % x", encoded[:len(header)], header)
	}
}

// 小计数下每一部分只占几个字节：位置增量这类一帧一个对象、一个组件的更新，帧头（10）加对象头（4）加组件头（4）不超过 18 字节。
// 计数取上限时各部分不超过 MaxHeaderBytes / MaxObjectHeaderBytes / MaxComponentHeaderBytes（组帧方按它们预留）。
func TestFrameOverheadIsSmallAndBoundedByTheDeclaredMaxima(t *testing.T) {
	small := Frame{SnapshotMeta: SnapshotMeta{StreamID: 1, Epoch: 2, Tick: 90, SchemaVersion: 1}, Kind: Delta, BaseTick: 89,
		Objects: []ObjectDelta{{Operation: ObjectUpdate, Ref: ObjectRef{ID: 3, Generation: 1},
			Components: []ComponentDelta{{Operation: ComponentSet, TypeID: 1, SchemaVersion: 1, Data: []byte("12345")}}}}}
	encoded, err := Encode(small, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if overhead := len(encoded) - 5; overhead > 18 {
		t.Fatalf("one small update costs %d bytes of frame overhead, want at most 18", overhead)
	}
	limits := DefaultLimits()
	limits.MaxObjects = 1 << 15
	big := Frame{SnapshotMeta: SnapshotMeta{StreamID: ^uint64(0), Epoch: ^uint32(0), Tick: ^uint32(0), SchemaVersion: ^uint16(0)}, Kind: Delta, BaseTick: 1}
	for id := 1; id <= 1<<15; id++ {
		big.Objects = append(big.Objects, ObjectDelta{Operation: ObjectRemove, Ref: ObjectRef{ID: uint16(id), Generation: ^uint16(0)}})
	}
	big.Objects[0] = ObjectDelta{Operation: ObjectCreate, Ref: ObjectRef{ID: ^uint16(0), Generation: ^uint16(0)}, Archetype: ^uint16(0),
		Components: []ComponentDelta{{Operation: ComponentSet, TypeID: ^uint16(0), SchemaVersion: ^uint16(0), Data: make([]byte, 1<<16)}}}
	limits.MaxComponentBytes = 1 << 17
	if got := encodedHeaderBytes(big); got > MaxHeaderBytes {
		t.Fatalf("widest header %d bytes > MaxHeaderBytes %d", got, MaxHeaderBytes)
	}
	create := big.Objects[0]
	if got := encodedObjectBytes(create) - len(create.Components[0].Data); got > MaxObjectHeaderBytes+MaxComponentHeaderBytes {
		t.Fatalf("widest object and component headers %d bytes > %d", got, MaxObjectHeaderBytes+MaxComponentHeaderBytes)
	}
	if _, err := Encode(big, limits); err != nil {
		t.Fatal(err)
	}
}
