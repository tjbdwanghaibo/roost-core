// Package frame is the wire format of one entity-sync frame: a Frame of
// object deltas (create / update / remove, each carrying component payloads),
// its Encode / Decode, and the Limits that bound what a frame may carry. It
// knows nothing about sessions or transports; entitysync builds frames, a
// transport carries the bytes.
package frame

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// frameMagic 是帧的前两个字节（"RF"），帧协议版本紧随其后占一个字节。
const frameMagic uint16 = 0x5246

// 一帧各部分在编码后的最大字节数（计数与编号全取上限时）。组帧方按它们预留空间，保证分帧后的每帧不超过上限。
const (
	// MaxHeaderBytes：magic 2、协议 1、kind 1、StreamID 10、Epoch 5、Tick 5、Tick−BaseTick 5、SchemaVersion 3、对象数 3。
	MaxHeaderBytes = 35
	// MaxObjectHeaderBytes：操作 1、ID 3、Generation 3、Archetype 3（只有 create）、组件数 3（remove 没有）。
	MaxObjectHeaderBytes = 13
	// MaxComponentHeaderBytes：操作 1、TypeID 3、SchemaVersion 3 与载荷长度 5（只有 set）。
	MaxComponentHeaderBytes = 12
)

// Encode 校验帧后编码（协议 2，见 docs/framework/ENTITY_SYNC.md“线格式”）：
//
//	帧头   magic(2) 协议(1) kind(1) uvarint StreamID、Epoch、Tick、[delta: Tick−BaseTick]、SchemaVersion、对象数
//	对象   操作(1) uvarint ID、Generation、[create: Archetype]、[非 remove: 组件数]
//	组件   操作(1) uvarint TypeID、[set: SchemaVersion、载荷长度、载荷]
//
// 整数一律是无符号 varint：常见的小计数只占一个字节。只对某种操作有意义的字段只在那种操作里出现，
// 其余操作里它必须为零（校验器拒绝），所以解码出的 Frame 与编码前相同。
// 编码长度超过 MaxFrameBytes 时返回 ErrFrameTooLarge。
func Encode(frame Frame, limits Limits) ([]byte, error) {
	limits = normalizeLimits(limits)
	if err := validateFrame(frame, limits); err != nil {
		return nil, err
	}
	if len(frame.Objects) > int(^uint16(0)) {
		return nil, ErrObjectLimit
	}
	encodedSize := encodedHeaderBytes(frame)
	for _, obj := range frame.Objects {
		if len(obj.Components) > int(^uint16(0)) {
			return nil, ErrComponentLimit
		}
		encodedSize += encodedObjectBytes(obj)
		if encodedSize > limits.MaxFrameBytes {
			return nil, ErrFrameTooLarge
		}
	}
	buffer := make([]byte, 0, encodedSize)
	buffer = binary.BigEndian.AppendUint16(buffer, frameMagic)
	buffer = append(buffer, ProtocolVersion, uint8(frame.Kind))
	buffer = binary.AppendUvarint(buffer, frame.StreamID)
	buffer = binary.AppendUvarint(buffer, uint64(frame.Epoch))
	buffer = binary.AppendUvarint(buffer, uint64(frame.Tick))
	if frame.Kind == Delta {
		buffer = binary.AppendUvarint(buffer, uint64(frame.Tick-frame.BaseTick))
	}
	buffer = binary.AppendUvarint(buffer, uint64(frame.SchemaVersion))
	buffer = binary.AppendUvarint(buffer, uint64(len(frame.Objects)))
	for _, obj := range frame.Objects {
		buffer = append(buffer, uint8(obj.Operation))
		buffer = binary.AppendUvarint(buffer, uint64(obj.Ref.ID))
		buffer = binary.AppendUvarint(buffer, uint64(obj.Ref.Generation))
		if obj.Operation == ObjectCreate {
			buffer = binary.AppendUvarint(buffer, uint64(obj.Archetype))
		}
		if obj.Operation == ObjectRemove {
			continue
		}
		buffer = binary.AppendUvarint(buffer, uint64(len(obj.Components)))
		for _, component := range obj.Components {
			buffer = append(buffer, uint8(component.Operation))
			buffer = binary.AppendUvarint(buffer, uint64(component.TypeID))
			if component.Operation != ComponentSet {
				continue
			}
			buffer = binary.AppendUvarint(buffer, uint64(component.SchemaVersion))
			buffer = binary.AppendUvarint(buffer, uint64(len(component.Data)))
			buffer = append(buffer, component.Data...)
		}
	}
	return buffer, nil
}

func uvarintBytes(value uint64) int {
	n := 1
	for value >= 0x80 {
		value >>= 7
		n++
	}
	return n
}

func encodedHeaderBytes(frame Frame) int {
	n := 4 + uvarintBytes(frame.StreamID) + uvarintBytes(uint64(frame.Epoch)) + uvarintBytes(uint64(frame.Tick)) +
		uvarintBytes(uint64(frame.SchemaVersion)) + uvarintBytes(uint64(len(frame.Objects)))
	if frame.Kind == Delta {
		n += uvarintBytes(uint64(frame.Tick - frame.BaseTick))
	}
	return n
}

func encodedObjectBytes(obj ObjectDelta) int {
	n := 1 + uvarintBytes(uint64(obj.Ref.ID)) + uvarintBytes(uint64(obj.Ref.Generation))
	if obj.Operation == ObjectCreate {
		n += uvarintBytes(uint64(obj.Archetype))
	}
	if obj.Operation == ObjectRemove {
		return n
	}
	n += uvarintBytes(uint64(len(obj.Components)))
	for _, component := range obj.Components {
		n += 1 + uvarintBytes(uint64(component.TypeID))
		if component.Operation == ComponentSet {
			n += uvarintBytes(uint64(component.SchemaVersion)) + uvarintBytes(uint64(len(component.Data))) + len(component.Data)
		}
	}
	return n
}

// frameReader 按协议 2 读字节；任何截断都是 ErrInvalidFrame。
type frameReader struct {
	data []byte
	at   int
}

func (r *frameReader) byte() (uint8, error) {
	if r.at >= len(r.data) {
		return 0, fmt.Errorf("%w: truncated frame", ErrInvalidFrame)
	}
	r.at++
	return r.data[r.at-1], nil
}

// uvarint 读一个不超过 limit 的无符号 varint；超长、非最短编码或越过 limit 都拒绝，
// 保证同一帧只有一种编码。
func (r *frameReader) uvarint(limit uint64) (uint64, error) {
	value, n := binary.Uvarint(r.data[r.at:])
	if n == 0 {
		return 0, fmt.Errorf("%w: truncated frame", ErrInvalidFrame)
	}
	if n < 0 || value > limit || n != uvarintBytes(value) {
		return 0, fmt.Errorf("%w: malformed varint", ErrInvalidFrame)
	}
	r.at += n
	return value, nil
}

// Decode 解码并完整校验一帧；截断、尾随字节、未知帧头或越过 limits 都返回错误。
// 头部声称的对象数、组件数与载荷长度在分配之前按上限检查。
func Decode(data []byte, limits Limits) (Frame, error) {
	limits = normalizeLimits(limits)
	if len(data) == 0 || len(data) > limits.MaxFrameBytes {
		return Frame{}, ErrFrameTooLarge
	}
	if len(data) < 4 || binary.BigEndian.Uint16(data[0:2]) != frameMagic || data[2] != ProtocolVersion {
		return Frame{}, fmt.Errorf("%w: unsupported header", ErrInvalidFrame)
	}
	frame := Frame{Kind: Kind(data[3])}
	if frame.Kind != Full && frame.Kind != Delta {
		return Frame{}, fmt.Errorf("%w: unknown frame kind %d", ErrInvalidFrame, frame.Kind)
	}
	r := &frameReader{data: data, at: 4}
	var err error
	if frame.StreamID, err = r.uvarint(^uint64(0)); err != nil {
		return Frame{}, err
	}
	epoch, err := r.uvarint(uint64(^uint32(0)))
	if err != nil {
		return Frame{}, err
	}
	tick, err := r.uvarint(uint64(^uint32(0)))
	if err != nil {
		return Frame{}, err
	}
	frame.Epoch, frame.Tick = uint32(epoch), uint32(tick)
	if frame.Kind == Delta {
		back, err := r.uvarint(tick)
		if err != nil {
			return Frame{}, err
		}
		frame.BaseTick = uint32(tick - back)
	}
	schema, err := r.uvarint(uint64(^uint16(0)))
	if err != nil {
		return Frame{}, err
	}
	frame.SchemaVersion = uint16(schema)
	objectCount, err := r.uvarint(uint64(^uint16(0)))
	if err != nil {
		return Frame{}, err
	}
	if int(objectCount) > limits.MaxObjects*2 {
		return Frame{}, ErrObjectLimit
	}
	// 每个对象至少三个字节：声称的对象数放不下时按截断拒绝，不按它分配。
	if int(objectCount) > (len(data)-r.at)/3 {
		return Frame{}, fmt.Errorf("%w: truncated frame", ErrInvalidFrame)
	}
	frame.Objects = make([]ObjectDelta, 0, int(objectCount))
	for i := 0; i < int(objectCount); i++ {
		obj, err := decodeObject(r, limits)
		if err != nil {
			return Frame{}, err
		}
		frame.Objects = append(frame.Objects, obj)
	}
	if r.at != len(data) {
		return Frame{}, fmt.Errorf("%w: trailing bytes", ErrInvalidFrame)
	}
	if err := validateFrame(frame, limits); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func decodeObject(r *frameReader, limits Limits) (ObjectDelta, error) {
	operation, err := r.byte()
	if err != nil {
		return ObjectDelta{}, err
	}
	obj := ObjectDelta{Operation: ObjectOperation(operation)}
	if obj.Operation < ObjectCreate || obj.Operation > ObjectRemove {
		return ObjectDelta{}, fmt.Errorf("%w: invalid object operation", ErrInvalidFrame)
	}
	id, err := r.uvarint(uint64(^uint16(0)))
	if err != nil {
		return ObjectDelta{}, err
	}
	generation, err := r.uvarint(uint64(^uint16(0)))
	if err != nil {
		return ObjectDelta{}, err
	}
	obj.Ref = ObjectRef{ID: uint16(id), Generation: uint16(generation)}
	if obj.Operation == ObjectCreate {
		archetype, err := r.uvarint(uint64(^uint16(0)))
		if err != nil {
			return ObjectDelta{}, err
		}
		obj.Archetype = uint16(archetype)
	}
	if obj.Operation == ObjectRemove {
		return obj, nil
	}
	componentCount, err := r.uvarint(uint64(^uint16(0)))
	if err != nil {
		return ObjectDelta{}, err
	}
	if int(componentCount) > limits.MaxComponentsPerObject*2 {
		return ObjectDelta{}, ErrComponentLimit
	}
	// 每个组件至少两个字节。
	if int(componentCount) > (len(r.data)-r.at)/2 {
		return ObjectDelta{}, fmt.Errorf("%w: truncated frame", ErrInvalidFrame)
	}
	obj.Components = make([]ComponentDelta, 0, int(componentCount))
	for j := 0; j < int(componentCount); j++ {
		componentOperation, err := r.byte()
		if err != nil {
			return ObjectDelta{}, err
		}
		component := ComponentDelta{Operation: ComponentOperation(componentOperation)}
		typeID, err := r.uvarint(uint64(^uint16(0)))
		if err != nil {
			return ObjectDelta{}, err
		}
		component.TypeID = uint16(typeID)
		if component.Operation == ComponentSet {
			schema, err := r.uvarint(uint64(^uint16(0)))
			if err != nil {
				return ObjectDelta{}, err
			}
			component.SchemaVersion = uint16(schema)
			payloadLength, err := r.uvarint(uint64(^uint32(0)))
			if err != nil {
				return ObjectDelta{}, err
			}
			if payloadLength > uint64(limits.MaxComponentBytes) || payloadLength > uint64(len(r.data)-r.at) {
				return ObjectDelta{}, ErrComponentTooLarge
			}
			if payloadLength > 0 {
				component.Data = bytes.Clone(r.data[r.at : r.at+int(payloadLength)])
				r.at += int(payloadLength)
			}
		}
		obj.Components = append(obj.Components, component)
	}
	return obj, nil
}

func validateFrame(frame Frame, limits Limits) error {
	if err := frame.SnapshotMeta.validate(); err != nil {
		return err
	}
	if frame.Kind != Full && frame.Kind != Delta {
		return fmt.Errorf("%w: unknown frame kind %d", ErrInvalidFrame, frame.Kind)
	}
	if frame.Kind == Full && frame.BaseTick != 0 {
		return fmt.Errorf("%w: full frame has baseline", ErrInvalidFrame)
	}
	if frame.Kind == Delta && (frame.BaseTick == 0 || frame.BaseTick >= frame.Tick) {
		return ErrBaselineMismatch
	}
	if len(frame.Objects) > limits.MaxObjects*2 {
		return ErrObjectLimit
	}
	seenObjects := make(map[ObjectRef]struct{}, len(frame.Objects))
	for _, obj := range frame.Objects {
		if !obj.Ref.Valid() {
			return ErrInvalidObjectRef
		}
		if obj.Operation < ObjectCreate || obj.Operation > ObjectRemove {
			return fmt.Errorf("%w: invalid object operation", ErrInvalidFrame)
		}
		if _, exists := seenObjects[obj.Ref]; exists {
			return fmt.Errorf("%w: duplicate object delta", ErrInvalidFrame)
		}
		seenObjects[obj.Ref] = struct{}{}
		if frame.Kind == Full && obj.Operation != ObjectCreate {
			return fmt.Errorf("%w: full frame contains non-create operation", ErrInvalidFrame)
		}
		if obj.Operation == ObjectCreate && obj.Archetype == 0 {
			return ErrInvalidObjectRef
		}
		if obj.Operation != ObjectCreate && obj.Archetype != 0 {
			return fmt.Errorf("%w: only a create carries an archetype", ErrInvalidFrame)
		}
		if obj.Operation == ObjectRemove && len(obj.Components) > 0 {
			return fmt.Errorf("%w: remove object has components", ErrInvalidFrame)
		}
		if len(obj.Components) > limits.MaxComponentsPerObject*2 {
			return ErrComponentLimit
		}
		seenComponents := make(map[uint16]struct{}, len(obj.Components))
		for _, component := range obj.Components {
			if component.TypeID == 0 || component.Operation < ComponentSet || component.Operation > ComponentRemove {
				return fmt.Errorf("%w: invalid component delta", ErrInvalidFrame)
			}
			if _, exists := seenComponents[component.TypeID]; exists {
				return fmt.Errorf("%w: duplicate component delta", ErrInvalidFrame)
			}
			seenComponents[component.TypeID] = struct{}{}
			if obj.Operation == ObjectCreate && component.Operation != ComponentSet {
				return fmt.Errorf("%w: create object contains component remove", ErrInvalidFrame)
			}
			if component.Operation == ComponentSet && component.SchemaVersion == 0 {
				return fmt.Errorf("%w: component schema version is zero", ErrInvalidFrame)
			}
			if component.Operation == ComponentRemove && (component.SchemaVersion != 0 || len(component.Data) != 0) {
				return fmt.Errorf("%w: remove component has payload", ErrInvalidFrame)
			}
			if len(component.Data) > limits.MaxComponentBytes {
				return ErrComponentTooLarge
			}
		}
	}
	return nil
}
