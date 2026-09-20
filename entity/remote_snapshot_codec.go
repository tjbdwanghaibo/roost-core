package entity

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc64"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var (
	ErrRemoteSnapshotDecoderMissing   = errors.New("remote snapshot: decoder missing")
	ErrRemoteSnapshotDecoderDuplicate = errors.New("remote snapshot: decoder already registered")
)

type RemoteSnapshotDecodeFunc func([]byte) (any, error)
type RemoteSnapshotDeltaFunc func(base []byte, delta []byte) ([]byte, error)

var remoteSnapshotDecoders struct {
	sync.RWMutex
	values map[uint32]RemoteSnapshotDecodeFunc
	deltas map[uint32]RemoteSnapshotDeltaFunc
}

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

// RemoteChecksum is a snapshot's CRC64. It is a named type because BSON has
// no unsigned 64-bit integer and this value uses the WHOLE range: the top bit
// is set for about half of all payloads, and writing it as a BSON number
// fails with "overflows int64" for exactly those (RR-20260920-01).
//
// Nothing compares or ranges over a checksum, so the storage form is fixed 8
// bytes, big-endian. Reading also accepts the numeric forms written before
// this existed, so the snapshots already in a database stay readable.
type RemoteChecksum uint64

// The signature takes a plain byte, not bson.Type: the driver's ValueMarshaler
// is declared as (byte, []byte, error), and bson.Type is a DEFINED type over
// byte rather than an alias — a method returning bson.Type compiles, does not
// implement the interface, and is silently never called.
func (c RemoteChecksum) MarshalBSONValue() (byte, []byte, error) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], uint64(c))
	typ, data, err := bson.MarshalValue(bson.Binary{Subtype: bson.TypeBinaryGeneric, Data: raw[:]})
	return byte(typ), data, err
}

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
	case bson.TypeInt64, bson.TypeInt32, bson.TypeDouble:
		// Written before checksums became bytes; only values below MaxInt64
		// could have been stored, so this is exact.
		var numeric int64
		if err := bson.UnmarshalValue(t, data, &numeric); err != nil {
			return err
		}
		*c = RemoteChecksum(numeric)
		return nil
	case bson.TypeNull, bson.TypeUndefined:
		*c = 0
		return nil
	default:
		return fmt.Errorf("entity: remote checksum has unexpected BSON type %s", t)
	}
}

func RemoteSnapshotChecksum(data []byte) RemoteChecksum {
	return RemoteChecksum(crc64.Checksum(data, remoteSnapshotChecksumTable))
}

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

func MustRegisterRemoteSnapshotDecoder(schema uint32, decode RemoteSnapshotDecodeFunc) {
	if err := RegisterRemoteSnapshotDecoder(schema, decode); err != nil {
		panic(err)
	}
}

func DecodeRemoteSnapshot(snapshot RemoteSnapshotEnvelope) (any, error) {
	remoteSnapshotDecoders.RLock()
	decode := remoteSnapshotDecoders.values[snapshot.Schema]
	remoteSnapshotDecoders.RUnlock()
	if decode == nil {
		return nil, fmt.Errorf("%w: schema=%d", ErrRemoteSnapshotDecoderMissing, snapshot.Schema)
	}
	return decode(snapshot.Payload.BytesCopy())
}
