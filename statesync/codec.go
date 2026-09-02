package statesync

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const frameMagic uint32 = 0x43525031 // CRP1

func EncodeFrame(frame DeltaFrame, limits Limits) ([]byte, error) {
	limits = normalizeLimits(limits)
	if err := validateDeltaFrame(frame, limits); err != nil {
		return nil, err
	}
	if len(frame.Objects) > int(^uint16(0)) {
		return nil, ErrObjectLimit
	}
	encodedSize := 32
	for _, obj := range frame.Objects {
		encodedSize += 9
		if encodedSize > limits.MaxFrameBytes {
			return nil, ErrFrameTooLarge
		}
		for _, component := range obj.Components {
			encodedSize += 9 + len(component.Data)
			if encodedSize > limits.MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
		}
	}
	buffer := make([]byte, 0, encodedSize)
	buffer = binary.BigEndian.AppendUint32(buffer, frameMagic)
	buffer = binary.BigEndian.AppendUint16(buffer, ProtocolVersion)
	buffer = append(buffer, uint8(frame.Kind), 0)
	buffer = binary.BigEndian.AppendUint64(buffer, frame.RoomID)
	buffer = binary.BigEndian.AppendUint32(buffer, frame.Epoch)
	buffer = binary.BigEndian.AppendUint32(buffer, frame.Tick)
	buffer = binary.BigEndian.AppendUint32(buffer, frame.BaseTick)
	buffer = binary.BigEndian.AppendUint16(buffer, frame.SchemaVersion)
	buffer = binary.BigEndian.AppendUint16(buffer, uint16(len(frame.Objects)))
	for _, obj := range frame.Objects {
		if len(obj.Components) > int(^uint16(0)) {
			return nil, ErrComponentLimit
		}
		buffer = append(buffer, uint8(obj.Operation))
		buffer = binary.BigEndian.AppendUint16(buffer, obj.Ref.ID)
		buffer = binary.BigEndian.AppendUint16(buffer, obj.Ref.Generation)
		buffer = binary.BigEndian.AppendUint16(buffer, obj.Archetype)
		buffer = binary.BigEndian.AppendUint16(buffer, uint16(len(obj.Components)))
		for _, component := range obj.Components {
			buffer = append(buffer, uint8(component.Operation))
			buffer = binary.BigEndian.AppendUint16(buffer, component.TypeID)
			buffer = binary.BigEndian.AppendUint16(buffer, component.SchemaVersion)
			buffer = binary.BigEndian.AppendUint32(buffer, uint32(len(component.Data)))
			buffer = append(buffer, component.Data...)
		}
	}
	return buffer, nil
}

func DecodeFrame(data []byte, limits Limits) (DeltaFrame, error) {
	limits = normalizeLimits(limits)
	if len(data) == 0 || len(data) > limits.MaxFrameBytes {
		return DeltaFrame{}, ErrFrameTooLarge
	}
	reader := bytes.NewReader(data)
	read := func(value any) error {
		if err := binary.Read(reader, binary.BigEndian, value); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return fmt.Errorf("%w: truncated frame", ErrInvalidFrame)
			}
			return err
		}
		return nil
	}
	var magic uint32
	var protocol uint16
	var kind, reserved uint8
	var objectCount uint16
	frame := DeltaFrame{}
	for _, value := range []any{
		&magic, &protocol, &kind, &reserved, &frame.RoomID, &frame.Epoch,
		&frame.Tick, &frame.BaseTick, &frame.SchemaVersion, &objectCount,
	} {
		if err := read(value); err != nil {
			return DeltaFrame{}, err
		}
	}
	if magic != frameMagic || protocol != ProtocolVersion || reserved != 0 {
		return DeltaFrame{}, fmt.Errorf("%w: unsupported header", ErrInvalidFrame)
	}
	if int(objectCount) > limits.MaxObjects*2 {
		return DeltaFrame{}, ErrObjectLimit
	}
	frame.Kind = FrameKind(kind)
	frame.Objects = make([]ObjectDelta, 0, int(objectCount))
	for i := 0; i < int(objectCount); i++ {
		var operation uint8
		var componentCount uint16
		obj := ObjectDelta{}
		for _, value := range []any{&operation, &obj.Ref.ID, &obj.Ref.Generation, &obj.Archetype, &componentCount} {
			if err := read(value); err != nil {
				return DeltaFrame{}, err
			}
		}
		if int(componentCount) > limits.MaxComponentsPerObject*2 {
			return DeltaFrame{}, ErrComponentLimit
		}
		obj.Operation = ObjectOperation(operation)
		obj.Components = make([]ComponentDelta, 0, int(componentCount))
		for j := 0; j < int(componentCount); j++ {
			var componentOperation uint8
			var payloadLength uint32
			component := ComponentDelta{}
			for _, value := range []any{&componentOperation, &component.TypeID, &component.SchemaVersion, &payloadLength} {
				if err := read(value); err != nil {
					return DeltaFrame{}, err
				}
			}
			if payloadLength > uint32(limits.MaxComponentBytes) || uint64(payloadLength) > uint64(reader.Len()) {
				return DeltaFrame{}, ErrComponentTooLarge
			}
			component.Operation = ComponentOperation(componentOperation)
			if payloadLength > 0 {
				component.Data = make([]byte, int(payloadLength))
				if _, err := io.ReadFull(reader, component.Data); err != nil {
					return DeltaFrame{}, fmt.Errorf("%w: truncated component", ErrInvalidFrame)
				}
			}
			obj.Components = append(obj.Components, component)
		}
		frame.Objects = append(frame.Objects, obj)
	}
	if reader.Len() != 0 {
		return DeltaFrame{}, fmt.Errorf("%w: trailing bytes", ErrInvalidFrame)
	}
	if err := validateDeltaFrame(frame, limits); err != nil {
		return DeltaFrame{}, err
	}
	return frame, nil
}

func validateDeltaFrame(frame DeltaFrame, limits Limits) error {
	if err := frame.SnapshotMeta.validate(); err != nil {
		return err
	}
	if frame.Kind != FrameFull && frame.Kind != FrameDelta {
		return fmt.Errorf("%w: unknown frame kind %d", ErrInvalidFrame, frame.Kind)
	}
	if frame.Kind == FrameFull && frame.BaseTick != 0 {
		return fmt.Errorf("%w: full frame has baseline", ErrInvalidFrame)
	}
	if frame.Kind == FrameDelta && (frame.BaseTick == 0 || frame.BaseTick >= frame.Tick) {
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
		if frame.Kind == FrameFull && obj.Operation != ObjectCreate {
			return fmt.Errorf("%w: full frame contains non-create operation", ErrInvalidFrame)
		}
		if obj.Operation == ObjectCreate && obj.Archetype == 0 {
			return ErrInvalidObjectRef
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
