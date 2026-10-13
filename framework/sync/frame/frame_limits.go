package frame

import (
	"errors"
	"fmt"
)

// ProtocolVersion 是帧格式版本，写在帧头；Decode 只接受当前版本。
const ProtocolVersion uint16 = 1

var (
	ErrInvalidObjectRef  = errors.New("frame: invalid object ref")
	ErrObjectLimit       = errors.New("frame: object limit exceeded")
	ErrComponentLimit    = errors.New("frame: component limit exceeded")
	ErrComponentTooLarge = errors.New("frame: component payload too large")
	ErrFrameTooLarge     = errors.New("frame: frame payload too large")
	ErrInvalidFrame      = errors.New("frame: invalid frame")
	ErrBaselineMismatch  = errors.New("frame: baseline mismatch")
)

// Limits 约束一帧可携带的对象数、每对象组件数、组件 payload 与整帧字节数；
// 非正值取 DefaultLimits 中的对应项。
type Limits struct {
	MaxObjects             int
	MaxComponentsPerObject int
	MaxComponentBytes      int
	MaxFrameBytes          int
}

func DefaultLimits() Limits {
	return Limits{
		MaxObjects:             100,
		MaxComponentsPerObject: 64,
		MaxComponentBytes:      64 << 10,
		MaxFrameBytes:          4 << 20,
	}
}

func normalizeLimits(limits Limits) Limits {
	defaults := DefaultLimits()
	if limits.MaxObjects <= 0 {
		limits.MaxObjects = defaults.MaxObjects
	}
	if limits.MaxComponentsPerObject <= 0 {
		limits.MaxComponentsPerObject = defaults.MaxComponentsPerObject
	}
	if limits.MaxComponentBytes <= 0 {
		limits.MaxComponentBytes = defaults.MaxComponentBytes
	}
	if limits.MaxFrameBytes <= 0 {
		limits.MaxFrameBytes = defaults.MaxFrameBytes
	}
	return limits
}

// ObjectRef 是流内对象引用：ID 可复用，Generation 区分复用前后的对象；二者都非零才有效。
type ObjectRef struct {
	ID         uint16
	Generation uint16
}

func (r ObjectRef) Valid() bool { return r.ID != 0 && r.Generation != 0 }

func (r ObjectRef) Less(other ObjectRef) bool {
	if r.ID != other.ID {
		return r.ID < other.ID
	}
	return r.Generation < other.Generation
}

// SnapshotMeta 是一帧所属的流、纪元与 tick；四个字段都必须非零。
type SnapshotMeta struct {
	// StreamID 标识当前连接/同步上下文内的流，不是房间、玩家或全局唯一 ID。
	StreamID      uint64
	Epoch         uint32
	Tick          uint32
	SchemaVersion uint16
}

func (m SnapshotMeta) validate() error {
	if m.StreamID == 0 || m.Epoch == 0 || m.Tick == 0 || m.SchemaVersion == 0 {
		return fmt.Errorf("%w: invalid snapshot metadata", ErrInvalidFrame)
	}
	return nil
}

// Kind 区分全量帧（Full，BaseTick 为 0，只含 create）与增量帧（Delta，基于 BaseTick）。
type Kind uint8

const (
	Full Kind = iota + 1
	Delta
)

type ObjectOperation uint8

const (
	ObjectCreate ObjectOperation = iota + 1
	ObjectUpdate
	ObjectRemove
)

type ComponentOperation uint8

const (
	ComponentSet ComponentOperation = iota + 1
	ComponentRemove
)

type ComponentDelta struct {
	Operation     ComponentOperation
	TypeID        uint16
	SchemaVersion uint16
	Data          []byte
}

type ObjectDelta struct {
	Operation  ObjectOperation
	Ref        ObjectRef
	Archetype  uint16
	Components []ComponentDelta
}

// Frame 是一条 entity-sync 帧：帧元数据加若干对象增量。
type Frame struct {
	SnapshotMeta
	Kind     Kind
	BaseTick uint32
	Objects  []ObjectDelta
}
