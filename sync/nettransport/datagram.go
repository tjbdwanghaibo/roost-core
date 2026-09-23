package nettransport

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Datagram lane wire format: a 42-byte header in front of one fragment of a
// frame. The header carries just enough for a latest-only queue to keep the
// fragments of one frame together and to tell a newer frame from an older
// one; what the frame contains is not the transport's business.
const (
	datagramMagic           uint32 = 0x43524431 // CRD1
	datagramProtocolVersion uint16 = 1
	DatagramHeaderSize             = 42
	// DefaultMaxDatagram is the payload bound a conservative path MTU leaves
	// for one datagram; DefaultMaxFragments bounds one frame's fragment count.
	DefaultMaxDatagram  = 1200
	DefaultMaxFragments = 64
)

var (
	ErrInvalidDatagram  = errors.New("nettransport: invalid datagram")
	ErrChecksumMismatch = errors.New("nettransport: checksum mismatch")
	ErrFragmentLimit    = errors.New("nettransport: fragment limit exceeded")
)

// DatagramMeta is what the sender stamps on every fragment of one frame.
type DatagramMeta struct {
	RoomID   uint64
	Epoch    uint32
	Tick     uint32
	BaseTick uint32
	Sequence uint32
	Full     bool
}

type DatagramFlags uint16

const (
	DatagramFlagFull DatagramFlags = 1 << iota
)

type DatagramHeader struct {
	Protocol   uint16
	Flags      DatagramFlags
	RoomID     uint64
	Epoch      uint32
	Tick       uint32
	BaseTick   uint32
	Sequence   uint32
	ChunkIndex uint16
	ChunkCount uint16
	PayloadLen uint16
	Checksum   uint32
}

// FragmentDatagrams splits one encoded frame into datagrams no larger than
// maxDatagram (zero selects DefaultMaxDatagram), at most maxFragments of them
// (zero selects DefaultMaxFragments).
func FragmentDatagrams(meta DatagramMeta, encoded []byte, maxDatagram, maxFragments int) ([][]byte, error) {
	if maxDatagram <= 0 {
		maxDatagram = DefaultMaxDatagram
	}
	if maxFragments <= 0 {
		maxFragments = DefaultMaxFragments
	}
	if meta.Sequence == 0 || len(encoded) == 0 {
		return nil, ErrInvalidDatagram
	}
	maxPayload := maxDatagram - DatagramHeaderSize
	if maxPayload <= 0 || maxPayload > int(^uint16(0)) {
		return nil, ErrInvalidDatagram
	}
	count := (len(encoded) + maxPayload - 1) / maxPayload
	if count <= 0 || count > maxFragments || count > int(^uint16(0)) {
		return nil, ErrFragmentLimit
	}
	flags := DatagramFlags(0)
	if meta.Full {
		flags |= DatagramFlagFull
	}
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		start := i * maxPayload
		end := start + maxPayload
		if end > len(encoded) {
			end = len(encoded)
		}
		header := DatagramHeader{
			Protocol: datagramProtocolVersion, Flags: flags, RoomID: meta.RoomID, Epoch: meta.Epoch,
			Tick: meta.Tick, BaseTick: meta.BaseTick, Sequence: meta.Sequence,
			ChunkIndex: uint16(i), ChunkCount: uint16(count), PayloadLen: uint16(end - start),
		}
		packet, err := encodeDatagram(header, encoded[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, packet)
	}
	return out, nil
}

func encodeDatagram(header DatagramHeader, payload []byte) ([]byte, error) {
	if header.Protocol != datagramProtocolVersion || header.RoomID == 0 || header.Epoch == 0 || header.Tick == 0 ||
		header.Sequence == 0 || header.ChunkCount == 0 || header.ChunkIndex >= header.ChunkCount || len(payload) != int(header.PayloadLen) {
		return nil, ErrInvalidDatagram
	}
	packet := make([]byte, DatagramHeaderSize+len(payload))
	binary.BigEndian.PutUint32(packet[0:4], datagramMagic)
	binary.BigEndian.PutUint16(packet[4:6], header.Protocol)
	binary.BigEndian.PutUint16(packet[6:8], uint16(header.Flags))
	binary.BigEndian.PutUint64(packet[8:16], header.RoomID)
	binary.BigEndian.PutUint32(packet[16:20], header.Epoch)
	binary.BigEndian.PutUint32(packet[20:24], header.Tick)
	binary.BigEndian.PutUint32(packet[24:28], header.BaseTick)
	binary.BigEndian.PutUint32(packet[28:32], header.Sequence)
	binary.BigEndian.PutUint16(packet[32:34], header.ChunkIndex)
	binary.BigEndian.PutUint16(packet[34:36], header.ChunkCount)
	binary.BigEndian.PutUint16(packet[36:38], header.PayloadLen)
	copy(packet[DatagramHeaderSize:], payload)
	checksum := checksumDatagram(packet)
	binary.BigEndian.PutUint32(packet[38:42], checksum)
	return packet, nil
}

// InspectDatagram validates the complete packet, including checksum, without
// copying its payload. Transport queues use it to validate frame batches on
// the hot path. Zero bounds select the defaults.
func InspectDatagram(packet []byte, maxDatagram, maxFragments int) (DatagramHeader, error) {
	header, _, err := decodeDatagram(packet, maxDatagram, maxFragments, false)
	return header, err
}

func decodeDatagram(packet []byte, maxDatagram, maxFragments int, copyPayload bool) (DatagramHeader, []byte, error) {
	if maxDatagram <= 0 {
		maxDatagram = DefaultMaxDatagram
	}
	if maxFragments <= 0 {
		maxFragments = DefaultMaxFragments
	}
	if len(packet) < DatagramHeaderSize || len(packet) > maxDatagram || binary.BigEndian.Uint32(packet[0:4]) != datagramMagic {
		return DatagramHeader{}, nil, ErrInvalidDatagram
	}
	header := DatagramHeader{
		Protocol:   binary.BigEndian.Uint16(packet[4:6]),
		Flags:      DatagramFlags(binary.BigEndian.Uint16(packet[6:8])),
		RoomID:     binary.BigEndian.Uint64(packet[8:16]),
		Epoch:      binary.BigEndian.Uint32(packet[16:20]),
		Tick:       binary.BigEndian.Uint32(packet[20:24]),
		BaseTick:   binary.BigEndian.Uint32(packet[24:28]),
		Sequence:   binary.BigEndian.Uint32(packet[28:32]),
		ChunkIndex: binary.BigEndian.Uint16(packet[32:34]),
		ChunkCount: binary.BigEndian.Uint16(packet[34:36]),
		PayloadLen: binary.BigEndian.Uint16(packet[36:38]),
		Checksum:   binary.BigEndian.Uint32(packet[38:42]),
	}
	if header.Protocol != datagramProtocolVersion || header.RoomID == 0 || header.Epoch == 0 || header.Tick == 0 || header.Sequence == 0 ||
		header.ChunkCount == 0 || int(header.ChunkCount) > maxFragments || header.ChunkIndex >= header.ChunkCount ||
		int(header.PayloadLen) != len(packet)-DatagramHeaderSize || header.Flags & ^DatagramFlagFull != 0 {
		return DatagramHeader{}, nil, ErrInvalidDatagram
	}
	if checksumDatagram(packet) != header.Checksum {
		return DatagramHeader{}, nil, ErrChecksumMismatch
	}
	payload := packet[DatagramHeaderSize:]
	if copyPayload {
		payload = append([]byte(nil), payload...)
	}
	return header, payload, nil
}

func checksumDatagram(packet []byte) uint32 {
	hash := crc32.NewIEEE()
	_, _ = hash.Write(packet[:38])
	_, _ = hash.Write([]byte{0, 0, 0, 0})
	if len(packet) > DatagramHeaderSize {
		_, _ = hash.Write(packet[DatagramHeaderSize:])
	}
	return hash.Sum32()
}
