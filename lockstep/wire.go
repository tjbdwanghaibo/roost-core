package lockstep

import (
	"encoding/binary"
	"fmt"
)

// Wire format for lockstep broadcast packets. One datagram carries the most
// recent N frames (redundancy: a lost packet is healed by the frames riding
// on its successors — no retransmission, no added latency, the classic
// mobile-MOBA loss strategy). Empty frames cost only their varint frame id,
// so idle stretches compress to a few bytes per frame.
//
//	packet  = magic(1) version(1) frameCount(uvarint) frame...
//	frame   = frameID(uvarint) inputCount(uvarint) input...
//	input   = player(uvarint) payloadLen(uvarint) payload
const (
	wireMagic   = 0xC7
	wireVersion = 1
	// MaxBroadcastFrames bounds frames per packet on decode (a corrupt or
	// hostile packet cannot allocate unbounded frames).
	MaxBroadcastFrames = 64
)

// RedundantEncoder keeps the most recent frames and encodes each broadcast
// packet with all of them.
type RedundantEncoder struct {
	depth int
	ring  []Frame
}

// NewRedundantEncoder builds an encoder carrying depth frames per packet
// (depth <= 0 selects 3).
func NewRedundantEncoder(depth int) *RedundantEncoder {
	if depth <= 0 {
		depth = 3
	}
	return &RedundantEncoder{depth: depth}
}

// Push appends a cut frame and returns the encoded broadcast packet carrying
// it plus up to depth-1 predecessors.
func (e *RedundantEncoder) Push(frame Frame) []byte {
	e.ring = append(e.ring, frame)
	if len(e.ring) > e.depth {
		e.ring = e.ring[len(e.ring)-e.depth:]
	}
	return EncodeBroadcast(e.ring)
}

// EncodeBroadcast encodes frames (oldest first) into one packet.
func EncodeBroadcast(frames []Frame) []byte {
	size := 2 + binary.MaxVarintLen32
	for _, frame := range frames {
		size += 2*binary.MaxVarintLen32 + 1
		for _, input := range frame.Inputs {
			size += 2*binary.MaxVarintLen32 + len(input.Payload)
		}
	}
	packet := make([]byte, 0, size)
	packet = append(packet, wireMagic, wireVersion)
	packet = binary.AppendUvarint(packet, uint64(len(frames)))
	for _, frame := range frames {
		packet = binary.AppendUvarint(packet, uint64(frame.ID))
		packet = binary.AppendUvarint(packet, uint64(len(frame.Inputs)))
		for _, input := range frame.Inputs {
			packet = binary.AppendUvarint(packet, uint64(uint32(input.Player)))
			packet = binary.AppendUvarint(packet, uint64(len(input.Payload)))
			packet = append(packet, input.Payload...)
		}
	}
	return packet
}

// DecodeBroadcast parses one broadcast packet. Decoding is strict: bad
// magic/version, truncated data, oversized counts and trailing bytes are all
// rejected — a corrupt packet never becomes a silently wrong frame.
func DecodeBroadcast(packet []byte) ([]Frame, error) {
	if len(packet) < 3 || packet[0] != wireMagic || packet[1] != wireVersion {
		return nil, fmt.Errorf("%w: bad header", ErrFrameCorrupt)
	}
	rest := packet[2:]
	frameCount, rest, err := readUvarint(rest)
	if err != nil {
		return nil, err
	}
	if frameCount > MaxBroadcastFrames {
		return nil, fmt.Errorf("%w: %d frames exceeds %d", ErrFrameCorrupt, frameCount, MaxBroadcastFrames)
	}
	frames := make([]Frame, 0, frameCount)
	for index := uint64(0); index < frameCount; index++ {
		var frame Frame
		var id, inputCount uint64
		if id, rest, err = readUvarint(rest); err != nil {
			return nil, err
		}
		if id == 0 || id > uint64(^FrameID(0)) {
			return nil, fmt.Errorf("%w: frame id %d", ErrFrameCorrupt, id)
		}
		frame.ID = FrameID(id)
		if inputCount, rest, err = readUvarint(rest); err != nil {
			return nil, err
		}
		if inputCount > uint64(len(rest)) {
			return nil, fmt.Errorf("%w: input count %d", ErrFrameCorrupt, inputCount)
		}
		for input := uint64(0); input < inputCount; input++ {
			var player, payloadLen uint64
			if player, rest, err = readUvarint(rest); err != nil {
				return nil, err
			}
			if player > uint64(^uint32(0)) {
				return nil, fmt.Errorf("%w: player id %d", ErrFrameCorrupt, player)
			}
			if payloadLen, rest, err = readUvarint(rest); err != nil {
				return nil, err
			}
			if payloadLen > MaxInputPayloadBytes || payloadLen > uint64(len(rest)) {
				return nil, fmt.Errorf("%w: payload length %d", ErrFrameCorrupt, payloadLen)
			}
			payload := append([]byte(nil), rest[:payloadLen]...)
			rest = rest[payloadLen:]
			frame.Inputs = append(frame.Inputs, Input{Player: PlayerID(uint32(player)), Payload: payload})
		}
		frames = append(frames, frame)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrFrameCorrupt, len(rest))
	}
	return frames, nil
}

func readUvarint(data []byte) (uint64, []byte, error) {
	value, read := binary.Uvarint(data)
	if read <= 0 {
		return 0, nil, fmt.Errorf("%w: truncated varint", ErrFrameCorrupt)
	}
	return value, data[read:], nil
}
