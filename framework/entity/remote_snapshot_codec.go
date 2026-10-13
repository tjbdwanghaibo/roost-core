package entity

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc64"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// 快照解码器登记的错误：schema 没有登记解码器 / delta 函数，或重复登记。
var (
	ErrRemoteSnapshotDecoderMissing   = errors.New("remote snapshot: decoder missing")
	ErrRemoteSnapshotDecoderDuplicate = errors.New("remote snapshot: decoder already registered")
)

// RemoteSnapshotDecodeFunc 把某 schema 的快照载荷解码为业务视图。
type RemoteSnapshotDecodeFunc func([]byte) (any, error)

// RemoteSnapshotDeltaFunc 把 delta 应用到 base 载荷上，返回新的完整载荷。
type RemoteSnapshotDeltaFunc func(base []byte, delta []byte) ([]byte, error)

// remoteSnapshotDecoders 是按 schema 登记的解码器与 delta 函数（启动时登记，运行时只读）。
var remoteSnapshotDecoders struct {
	sync.RWMutex
	values map[uint32]RemoteSnapshotDecodeFunc
	deltas map[uint32]RemoteSnapshotDeltaFunc
}

// RegisterRemoteSnapshotDelta 为 schema 登记 delta 应用函数；重复登记返回 ErrRemoteSnapshotDecoderDuplicate。
func RegisterRemoteSnapshotDelta(schema uint32, apply RemoteSnapshotDeltaFunc) error {
	if schema == 0 || apply == nil {
		return fmt.Errorf("remote snapshot: invalid delta registration")
	}
	remoteSnapshotDecoders.Lock()
	defer remoteSnapshotDecoders.Unlock()
	if remoteSnapshotDecoders.deltas == nil {
		remoteSnapshotDecoders.deltas = make(map[uint32]RemoteSnapshotDeltaFunc)
	}
	if _, exists := remoteSnapshotDecoders.deltas[schema]; exists {
		return fmt.Errorf("%w: delta schema=%d", ErrRemoteSnapshotDecoderDuplicate, schema)
	}
	remoteSnapshotDecoders.deltas[schema] = apply
	return nil
}

// applyRemoteSnapshotDelta 用 schema 登记的函数应用 delta（传入副本，函数可以修改它们）。
func applyRemoteSnapshotDelta(schema uint32, base, delta []byte) ([]byte, error) {
	remoteSnapshotDecoders.RLock()
	apply := remoteSnapshotDecoders.deltas[schema]
	remoteSnapshotDecoders.RUnlock()
	if apply == nil {
		return nil, fmt.Errorf("%w: delta schema=%d", ErrRemoteSnapshotDecoderMissing, schema)
	}
	return apply(append([]byte(nil), base...), append([]byte(nil), delta...))
}

var remoteSnapshotChecksumTable = crc64.MakeTable(crc64.ECMA)

// RemoteChecksum 是快照的 CRC64。单独定义类型是因为 BSON 没有无符号 64 位整数，而这个值用满整个范围：约一半载荷的
// 最高位为 1，按 BSON 数值写入时恰好这些会报 “overflows int64”。
//
// 没有任何地方对校验和做比较或区间运算，所以存储形式固定为 8 字节大端二进制。数值形式不被接受：按意外的 BSON 类型
// 拒绝，null 与 undefined 同样拒绝——所有写者都经 MarshalBSONValue（没有 omitempty），这两者都不是当前形式。缺失
// 字段解码为 0，随后被 RemoteSnapshotEnvelope.Valid 按校验和不符拒绝。
type RemoteChecksum uint64

// MarshalBSONValue 把校验和写成 8 字节大端二进制。签名用 byte 而不是 bson.Type：驱动的 ValueMarshaler 声明为
// (byte, []byte, error)，而 bson.Type 是基于 byte 的定义类型而非别名——返回 bson.Type 的方法能编译，却不实现该接口，
// 会被静默地永不调用。
func (c RemoteChecksum) MarshalBSONValue() (byte, []byte, error) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], uint64(c))
	typ, data, err := bson.MarshalValue(bson.Binary{Subtype: bson.TypeBinaryGeneric, Data: raw[:]})
	return byte(typ), data, err
}

// UnmarshalBSONValue 只接受 8 字节二进制形式，其余 BSON 类型都拒绝。
func (c *RemoteChecksum) UnmarshalBSONValue(typ byte, data []byte) error {
	t := bson.Type(typ)
	switch t {
	case bson.TypeBinary:
		var binaryValue bson.Binary
		if err := bson.UnmarshalValue(t, data, &binaryValue); err != nil {
			return err
		}
		if len(binaryValue.Data) != 8 {
			return fmt.Errorf("entity: remote checksum is %d bytes, want 8", len(binaryValue.Data))
		}
		*c = RemoteChecksum(binary.BigEndian.Uint64(binaryValue.Data))
		return nil
	default:
		return fmt.Errorf("entity: remote checksum has unexpected BSON type %s", t)
	}
}

// RemoteSnapshotChecksum 计算载荷的 CRC64（ECMA）。
func RemoteSnapshotChecksum(data []byte) RemoteChecksum {
	return RemoteChecksum(crc64.Checksum(data, remoteSnapshotChecksumTable))
}

// RegisterRemoteSnapshotDecoder 为 schema 登记解码器；重复登记返回 ErrRemoteSnapshotDecoderDuplicate。
func RegisterRemoteSnapshotDecoder(schema uint32, decode RemoteSnapshotDecodeFunc) error {
	if schema == 0 || decode == nil {
		return fmt.Errorf("remote snapshot: invalid decoder registration")
	}
	remoteSnapshotDecoders.Lock()
	defer remoteSnapshotDecoders.Unlock()
	if remoteSnapshotDecoders.values == nil {
		remoteSnapshotDecoders.values = make(map[uint32]RemoteSnapshotDecodeFunc)
	}
	if _, exists := remoteSnapshotDecoders.values[schema]; exists {
		return fmt.Errorf("%w: schema=%d", ErrRemoteSnapshotDecoderDuplicate, schema)
	}
	remoteSnapshotDecoders.values[schema] = decode
	return nil
}

// MustRegisterRemoteSnapshotDecoder 是 RegisterRemoteSnapshotDecoder 的 panic 版本。
func MustRegisterRemoteSnapshotDecoder(schema uint32, decode RemoteSnapshotDecodeFunc) {
	if err := RegisterRemoteSnapshotDecoder(schema, decode); err != nil {
		panic(err)
	}
}

// DecodeRemoteSnapshot 用快照 schema 登记的解码器解码载荷副本。
func DecodeRemoteSnapshot(snapshot RemoteSnapshotEnvelope) (any, error) {
	remoteSnapshotDecoders.RLock()
	decode := remoteSnapshotDecoders.values[snapshot.Schema]
	remoteSnapshotDecoders.RUnlock()
	if decode == nil {
		return nil, fmt.Errorf("%w: schema=%d", ErrRemoteSnapshotDecoderMissing, snapshot.Schema)
	}
	return decode(snapshot.Payload.BytesCopy())
}
