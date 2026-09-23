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
	// MaxFrameInputs bounds inputs per frame on decode: a frame can never
	// carry more inputs than seats, and no match has this many seats. Caps
	// the decode-side memory amplification of a hostile reliable-lane page.
	MaxFrameInputs = 256
)

// RedundantEncoder keeps the most recent frames in a fixed ring and encodes
// each broadcast packet with all of them.
type RedundantEncoder struct {
	ring    []Frame // fixed length = depth
	count   int
	next    int
	ordered []Frame // reused oldest-first view for encoding
}

// NormalizeRedundancyDepth maps a configured depth to the effective one:
// depth <= 0 selects 3, and the decoder-side MaxBroadcastFrames is the hard
// ceiling — a deeper encoder would emit packets every receiver rejects.
func NormalizeRedundancyDepth(depth int) int {
	if depth <= 0 {
		return 3
	}
	if depth > MaxBroadcastFrames {
		return MaxBroadcastFrames
	}
	return depth
}

// NewRedundantEncoder builds an encoder carrying depth frames per packet
// (normalized via NormalizeRedundancyDepth).
func NewRedundantEncoder(depth int) *RedundantEncoder {
	depth = NormalizeRedundancyDepth(depth)
	return &RedundantEncoder{ring: make([]Frame, depth), ordered: make([]Frame, 0, depth)}
}

// Push appends a cut frame and returns the encoded broadcast packet carrying
// it plus up to depth-1 predecessors. The ring is fixed-size: no sliding
// reallocation, and evicted frames drop their payload references.
func (e *RedundantEncoder) Push(frame Frame) []byte {
	e.ring[e.next] = frame
	e.next = (e.next + 1) % len(e.ring)
	if e.count < len(e.ring) {
		e.count++
	}
	e.ordered = e.ordered[:0]
	start := (e.next - e.count + len(e.ring)) % len(e.ring)
	for i := 0; i < e.count; i++ {
		e.ordered = append(e.ordered, e.ring[(start+i)%len(e.ring)])
	}
	return EncodeBroadcast(e.ordered)
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
	lastID := uint64(0)
	for index := uint64(0); index < frameCount; index++ {
		var frame Frame
		var id, inputCount uint64
		if id, rest, err = readUvarint(rest); err != nil {
			return nil, err
		}
		if id == 0 || id > uint64(^FrameID(0)) {
			return nil, fmt.Errorf("%w: frame id %d", ErrFrameCorrupt, id)
		}
		// Frames in one packet are strictly increasing (the encoder emits
		// oldest-first): duplicates or reordering mark a forged packet.
		if id <= lastID {
			return nil, fmt.Errorf("%w: frame id %d not increasing", ErrFrameCorrupt, id)
		}
		lastID = id
		frame.ID = FrameID(id)
		if inputCount, rest, err = readUvarint(rest); err != nil {
			return nil, err
		}
		if inputCount > MaxFrameInputs || inputCount > uint64(len(rest)) {
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
