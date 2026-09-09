package statesync

import (
	"encoding/binary"
	"errors"
	"testing"
)

// U-0139 · C2（空洞测试）· nightly gap map core `statesync` 7/20。
//
// 线格式的计数字段是 uint16：对象数超过 65535 必须拒绝而不是回绕成 0 编出一个
// "空帧"；帧大小上限在对象头与组件两级都生效；校验器的对象数上限在编码
// 侧也要拒绝（解码侧另有头部预检）。解码侧对头部声称的对象数 / 组件数 / 载荷
// 长度要在分配与读取之前按上限拒绝，回答的是"超限"而不是"截断"。
// `codec.go:44`（单对象 65536 个组件）：组件 TypeID 非零且唯一，最多 65535 个，
// 校验器先以重复拒绝，编码路径到不了，记不可达保留。

func TestEncodeFrameRefusesCountsAndSizesTheWireFormatCannotCarry(t *testing.T) {
	// 65536 个对象：uint16 计数会回绕成 0。放宽 MaxObjects 让校验器放行。
	wide := DefaultLimits()
	wide.MaxObjects = 40000
	frame := DeltaFrame{SnapshotMeta: SnapshotMeta{RoomID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: FrameDelta, BaseTick: 4}
	frame.Objects = make([]ObjectDelta, 0, 65536)
	for id := 1; id <= 65535; id++ {
		frame.Objects = append(frame.Objects, ObjectDelta{Operation: ObjectRemove, Ref: ObjectRef{ID: uint16(id), Generation: 1}})
	}
	frame.Objects = append(frame.Objects, ObjectDelta{Operation: ObjectRemove, Ref: ObjectRef{ID: 1, Generation: 2}})
	if _, err := EncodeFrame(frame, wide); !errors.Is(err, ErrObjectLimit) {
		t.Fatalf("EncodeFrame with 65536 objects = %v, want ErrObjectLimit", err)
	}

	// 只有对象头、没有组件：帧大小上限也要在对象头处生效。
	headersOnly := DeltaFrame{SnapshotMeta: SnapshotMeta{RoomID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: FrameDelta, BaseTick: 4,
		Objects: []ObjectDelta{{Operation: ObjectRemove, Ref: ObjectRef{ID: 1, Generation: 1}}, {Operation: ObjectRemove, Ref: ObjectRef{ID: 2, Generation: 1}}}}
	tiny := DefaultLimits()
	tiny.MaxFrameBytes = 40 // 32 字节头 + 一个 9 字节对象头就越界
	if encoded, err := EncodeFrame(headersOnly, tiny); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("EncodeFrame over MaxFrameBytes with component-less objects = (%d bytes, %v), want ErrFrameTooLarge", len(encoded), err)
	}

	// 对象头放得下、加上组件就越界：组件级的大小检查。
	oneComponent := DeltaFrame{SnapshotMeta: SnapshotMeta{RoomID: 7, Epoch: 1, Tick: 5, SchemaVersion: 1}, Kind: FrameDelta, BaseTick: 4,
		Objects: []ObjectDelta{{Operation: ObjectCreate, Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 10,
			Components: []ComponentDelta{{Operation: ComponentSet, TypeID: 1, SchemaVersion: 1, Data: []byte("pos")}}}}}
	snug := DefaultLimits()
	snug.MaxFrameBytes = 45 // 32 + 9 放得下对象头，再加 9 + 3 的组件就越界
	if encoded, err := EncodeFrame(oneComponent, snug); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("EncodeFrame whose component pushes past MaxFrameBytes = (%d bytes, %v), want ErrFrameTooLarge", len(encoded), err)
	}

	// 编码侧的对象数上限（MaxObjects*2）。
	few := DefaultLimits()
	few.MaxObjects = 1
	if _, err := EncodeFrame(validDelta(), few); !errors.Is(err, ErrObjectLimit) {
		t.Fatalf("EncodeFrame with 3 objects under MaxObjects=1 = %v, want ErrObjectLimit", err)
	}
}

func TestDecodeFrameRefusesHeaderCountsBeforeReadingOrAllocating(t *testing.T) {
	limits := DefaultLimits()
	encoded, err := EncodeFrame(validDelta(), limits)
	if err != nil {
		t.Fatal(err)
	}
	// 头部声称 65535 个对象，后面什么都没有：答案是"超限"，不是"截断"。
	header := append([]byte(nil), encoded[:32]...)
	binary.BigEndian.PutUint16(header[30:32], 0xFFFF)
	if _, err := DecodeFrame(header, limits); !errors.Is(err, ErrObjectLimit) {
		t.Fatalf("DecodeFrame with an oversized object count = %v, want ErrObjectLimit", err)
	}
	// 第一个对象声称 65535 个组件。
	object := append([]byte(nil), encoded[:41]...)
	binary.BigEndian.PutUint16(object[30:32], 1)
	binary.BigEndian.PutUint16(object[39:41], 0xFFFF)
	if _, err := DecodeFrame(object, limits); !errors.Is(err, ErrComponentLimit) {
		t.Fatalf("DecodeFrame with an oversized component count = %v, want ErrComponentLimit", err)
	}
	// 第一个组件声称的载荷长度超过 MaxComponentBytes（也超过剩余字节）。
	component := append([]byte(nil), encoded[:50]...)
	binary.BigEndian.PutUint16(component[30:32], 1)
	binary.BigEndian.PutUint16(component[39:41], 1)
	binary.BigEndian.PutUint32(component[46:50], uint32(limits.MaxComponentBytes+1))
	if _, err := DecodeFrame(component, limits); !errors.Is(err, ErrComponentTooLarge) {
		t.Fatalf("DecodeFrame with an oversized payload length = %v, want ErrComponentTooLarge", err)
	}
	// 载荷长度在上限内但超过剩余字节：同样是超限而不是分配后再截断。
	binary.BigEndian.PutUint32(component[46:50], 64)
	if _, err := DecodeFrame(component, limits); !errors.Is(err, ErrComponentTooLarge) {
		t.Fatalf("DecodeFrame with a payload length past the end = %v, want ErrComponentTooLarge", err)
	}
}
