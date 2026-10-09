package lockstep

import (
	"errors"
	"reflect"
	"testing"
)

func newTestSequencer(t *testing.T) *Sequencer {
	t.Helper()
	sequencer, err := NewSequencer(SequencerConfig{Players: []PlayerID{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	return sequencer
}

func TestSequencerOptimisticLockingAndLateFolding(t *testing.T) {
	sequencer := newTestSequencer(t)
	if _, err := sequencer.SubmitInput(1, 1, []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	// Nobody waits: frame 1 cuts with whoever arrived.
	frame := sequencer.Advance()
	if frame.ID != 1 || len(frame.Inputs) != 1 || frame.Inputs[0].Player != 1 {
		t.Fatalf("frame = %+v", frame)
	}
	// A late input for the already-cut frame folds into the next uncut one.
	folded, err := sequencer.SubmitInput(2, 1, []byte{0x02})
	if err != nil || folded != 2 {
		t.Fatalf("late fold = %d err=%v, want 2", folded, err)
	}
	// An explicit input for the frame REPLACES the folded placeholder: the
	// stale packet must not shadow the player's real input.
	if _, err := sequencer.SubmitInput(2, 2, []byte{0xFF}); err != nil {
		t.Fatal(err)
	}
	// A second explicit submission keeps the first (idempotent vs datagram
	// redundancy).
	if _, err := sequencer.SubmitInput(2, 2, []byte{0xEE}); err != nil {
		t.Fatal(err)
	}
	frame = sequencer.Advance()
	if frame.ID != 2 || len(frame.Inputs) != 1 || !reflect.DeepEqual(frame.Inputs[0].Payload, []byte{0xFF}) {
		t.Fatalf("frame 2 = %+v", frame)
	}
	// Empty frame when nobody submits.
	if frame = sequencer.Advance(); frame.ID != 3 || len(frame.Inputs) != 0 {
		t.Fatalf("empty frame = %+v", frame)
	}
	// Submit window: next is 4, window 2 -> frame 6 ok, frame 7 rejected.
	if _, err := sequencer.SubmitInput(1, 6, []byte{0x06}); err != nil {
		t.Fatal(err)
	}
	if _, err := sequencer.SubmitInput(1, 7, []byte{0x07}); !errors.Is(err, ErrFrameTooEarly) {
		t.Fatalf("window bypassed: %v", err)
	}
	if _, err := sequencer.SubmitInput(9, 4, nil); !errors.Is(err, ErrPlayerUnknown) {
		t.Fatalf("unknown player accepted: %v", err)
	}
}

func TestSequencerDeterministicFrameBytes(t *testing.T) {
	run := func() [][]byte {
		sequencer := newTestSequencer(t)
		encoder := NewRedundantEncoder(3)
		var packets [][]byte
		// Same submissions in different arrival orders per run must still
		// produce identical frames (player-sorted inputs).
		submissions := [][3]any{{PlayerID(3), FrameID(1), []byte{3}}, {PlayerID(1), FrameID(1), []byte{1}}, {PlayerID(2), FrameID(2), []byte{2}}}
		for _, s := range submissions {
			if _, err := sequencer.SubmitInput(s[0].(PlayerID), s[1].(FrameID), s[2].([]byte)); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 4; i++ {
			packets = append(packets, encoder.Push(sequencer.Advance()))
		}
		return packets
	}
	if !reflect.DeepEqual(run(), run()) {
		t.Fatal("frame bytes differ across identical runs")
	}
}

func TestRedundancySurvivesPacketLoss(t *testing.T) {
	sequencer := newTestSequencer(t)
	encoder := NewRedundantEncoder(4)
	received := make(map[FrameID]Frame)
	// 30%-style deterministic loss: drop every 3rd packet. The extra tail
	// steps past 60 model the stream continuing, so redundancy on later
	// packets can heal a loss near the end of the checked range.
	for step := 1; step <= 64; step++ {
		if _, err := sequencer.SubmitInput(PlayerID(step%3+1), FrameID(step), []byte{byte(step)}); err != nil {
			t.Fatal(err)
		}
		packet := encoder.Push(sequencer.Advance())
		if step%3 == 0 {
			continue // packet lost
		}
		frames, err := DecodeBroadcast(packet)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			if _, seen := received[frame.ID]; !seen {
				received[frame.ID] = frame
			}
		}
	}
	for id := FrameID(1); id <= 60; id++ {
		if _, ok := received[id]; !ok {
			t.Fatalf("frame %d lost despite redundancy", id)
		}
	}
}

func TestBroadcastCodecRoundTripAndFailFast(t *testing.T) {
	frames := []Frame{
		{ID: 7, Inputs: []Input{{Player: 1, Payload: []byte{0xAA, 0xBB}}, {Player: 2, Payload: nil}}},
		{ID: 8},
		{ID: 9, Inputs: []Input{{Player: 3, Payload: []byte{0x01}}}},
	}
	packet := EncodeBroadcast(frames)
	decoded, err := DecodeBroadcast(packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 3 || decoded[0].ID != 7 || len(decoded[0].Inputs) != 2 || len(decoded[1].Inputs) != 0 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if !reflect.DeepEqual(decoded[0].Inputs[0].Payload, []byte{0xAA, 0xBB}) {
		t.Fatalf("payload = %+v", decoded[0].Inputs[0].Payload)
	}
	// Idle frames are tiny: an empty frame costs a few bytes.
	idle := EncodeBroadcast([]Frame{{ID: 1000}, {ID: 1001}, {ID: 1002}})
	if len(idle) > 12 {
		t.Fatalf("idle packet = %d bytes", len(idle))
	}
	for name, corrupt := range map[string][]byte{
		"bad magic":  append([]byte{0x00}, packet[1:]...),
		"truncated":  packet[:len(packet)-1],
		"trailing":   append(append([]byte(nil), packet...), 0x00),
		"empty":      {},
		"count bomb": {wireMagic, wireVersion, 0xFF, 0xFF, 0x03},
	} {
		if _, err := DecodeBroadcast(corrupt); !errors.Is(err, ErrFrameCorrupt) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func TestHistoryCatchupPagingAndSequenceGuard(t *testing.T) {
	history := NewHistory()
	for id := FrameID(1); id <= 10; id++ {
		history.Append(Frame{ID: id})
	}
	if history.Latest() != 10 || history.Len() != 10 {
		t.Fatalf("latest=%d len=%d", history.Latest(), history.Len())
	}
	page := history.ReadRange(4, 3)
	if len(page) != 3 || page[0].ID != 4 || page[2].ID != 6 {
		t.Fatalf("page = %+v", page)
	}
	if tail := history.ReadRange(9, 100); len(tail) != 2 {
		t.Fatalf("tail = %+v", tail)
	}
	if beyond := history.ReadRange(11, 5); beyond != nil {
		t.Fatalf("beyond = %+v", beyond)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("out-of-sequence append accepted")
		}
	}()
	history.Append(Frame{ID: 99})
}

func TestDesyncMajorityVerdict(t *testing.T) {
	// Quorum counts the AGREEING group: two matching + one dissenting is
	// not a verdict yet at quorum 3 — a minority reporting first can never
	// convict anyone.
	detector := NewDesyncDetector(3)
	if _, ready := detector.Report(1, 30, 0xAB); ready {
		t.Fatal("verdict before quorum")
	}
	if _, ready := detector.Report(2, 30, 0xAB); ready {
		t.Fatal("verdict before quorum")
	}
	if _, ready := detector.Report(3, 30, 0xEE); ready {
		t.Fatal("two agreeing + one dissenting must not be a verdict at quorum 3")
	}
	verdict, ready := detector.Report(4, 30, 0xAB)
	if !ready || verdict.Majority != 0xAB || !reflect.DeepEqual(verdict.Outliers, []PlayerID{3}) {
		t.Fatalf("verdict = %+v ready=%v", verdict, ready)
	}
	// A player cannot revise its story: the first report stands.
	verdict, _ = detector.Report(3, 30, 0xAB)
	if !reflect.DeepEqual(verdict.Outliers, []PlayerID{3}) {
		t.Fatalf("revised report accepted: %+v", verdict)
	}
	// Trim tombstones the frame: late reports cannot rebuild a forgeable
	// report set for it.
	detector.Trim(31)
	if _, ready := detector.Report(1, 30, 0xAB); ready {
		t.Fatal("trimmed frame re-judged from scratch")
	}
	if _, ready := detector.Report(5, 30, 0xEE); ready {
		t.Fatal("tombstoned frame accepted new reports")
	}
}

func TestDesyncMinorityFirstCannotConvictHonestMajority(t *testing.T) {
	// Regression for the weaponizable ruling: with quorum = majority of
	// seats (3 of 5), two colluders reporting a fake hash first must not
	// produce any verdict against the honest third reporter.
	detector := NewDesyncDetector(3)
	if _, ready := detector.Report(1, 10, 0xFA); ready {
		t.Fatal("premature verdict")
	}
	if _, ready := detector.Report(2, 10, 0xFA); ready {
		t.Fatal("premature verdict")
	}
	if _, ready := detector.Report(3, 10, 0x01); ready {
		t.Fatal("honest first responder convicted by colluding minority")
	}
	if _, ready := detector.Report(4, 10, 0x01); ready {
		t.Fatal("still no agreeing quorum")
	}
	verdict, ready := detector.Report(5, 10, 0x01)
	if !ready || verdict.Majority != 0x01 || !reflect.DeepEqual(verdict.Outliers, []PlayerID{1, 2}) {
		t.Fatalf("colluders not identified: %+v ready=%v", verdict, ready)
	}
}

func TestHistoryTrimBefore(t *testing.T) {
	history := NewHistory()
	for id := FrameID(1); id <= 10; id++ {
		history.Append(Frame{ID: id})
	}
	history.TrimBefore(5)
	if history.FirstID() != 5 || history.Len() != 6 || history.Latest() != 10 {
		t.Fatalf("first=%d len=%d latest=%d", history.FirstID(), history.Len(), history.Latest())
	}
	if page := history.ReadRange(1, 3); len(page) != 3 || page[0].ID != 5 {
		t.Fatalf("page after trim = %+v", page)
	}
	history.Append(Frame{ID: 11}) // sequence check still holds after trim
	if history.Latest() != 11 {
		t.Fatalf("latest = %d", history.Latest())
	}
}

func TestSequencerConfigBounds(t *testing.T) {
	if _, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}, SubmitWindow: MaxSubmitWindow + 1}); err == nil {
		t.Fatal("oversized submit window accepted")
	}
	if _, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}, SubmitWindow: ^FrameID(0)}); err == nil {
		t.Fatal("overflowing submit window accepted (would reject every input)")
	}
	if _, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: MaxInputPayloadBytes + 1}); err == nil {
		t.Fatal("oversized input cap accepted")
	}
	sequencer, err := NewSequencer(SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sequencer.SubmitInput(1, 1, make([]byte, 9)); err == nil {
		t.Fatal("payload over per-match cap accepted")
	}
}

func TestDecodeRejectsNonIncreasingFrameIDsAndInputBombs(t *testing.T) {
	packet := EncodeBroadcast([]Frame{{ID: 5}, {ID: 5}})
	if _, err := DecodeBroadcast(packet); err == nil {
		t.Fatal("duplicate frame ids in one packet accepted")
	}
	packet = EncodeBroadcast([]Frame{{ID: 6}, {ID: 5}})
	if _, err := DecodeBroadcast(packet); err == nil {
		t.Fatal("decreasing frame ids accepted")
	}
	// Encoder depth is clamped to what decoders accept.
	encoder := NewRedundantEncoder(MaxBroadcastFrames * 2)
	var packetOut []byte
	for i := 1; i <= MaxBroadcastFrames+8; i++ {
		packetOut = encoder.Push(Frame{ID: FrameID(i)})
	}
	frames, err := DecodeBroadcast(packetOut)
	if err != nil {
		t.Fatalf("clamped-depth packet rejected: %v", err)
	}
	if len(frames) != MaxBroadcastFrames {
		t.Fatalf("frames = %d, want clamp at %d", len(frames), MaxBroadcastFrames)
	}
}

func BenchmarkRedundantEncodePush(b *testing.B) {
	sequencer, err := NewSequencer(SequencerConfig{Players: []PlayerID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, SubmitWindow: 4})
	if err != nil {
		b.Fatal(err)
	}
	encoder := NewRedundantEncoder(3)
	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame := sequencer.NextFrame()
		for player := PlayerID(1); player <= 10; player++ {
			if _, err := sequencer.SubmitInput(player, frame, payload); err != nil {
				b.Fatal(err)
			}
		}
		if packet := encoder.Push(sequencer.Advance()); len(packet) == 0 {
			b.Fatal("empty packet")
		}
	}
}

func TestFrameAssemblerHealsThirtyPercentLoss(t *testing.T) {
	sequencer, err := NewSequencer(SequencerConfig{Players: []PlayerID{1, 2, 3}, MaxInputBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	encoder := NewRedundantEncoder(4)
	assembler := NewFrameAssembler(0)
	received := 0
	for step := 1; step <= 400; step++ {
		if _, err := sequencer.SubmitInput(PlayerID(step%3+1), FrameID(step), []byte{byte(step)}); err != nil {
			t.Fatal(err)
		}
		packet := encoder.Push(sequencer.Advance())
		// Deterministic ~30% loss with the tail kept so redundancy can heal
		// the checked range.
		if step%10 < 3 && step <= 396 {
			continue
		}
		frames, err := assembler.Ingest(packet)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			received++
			if frame.ID != FrameID(received) {
				t.Fatalf("out-of-order release: got %d want %d", frame.ID, received)
			}
		}
	}
	if received != 400 {
		t.Fatalf("frames released = %d, want 400 (zero loss after redundancy)", received)
	}
	if assembler.Duplicates() == 0 {
		t.Fatal("redundancy produced no duplicates? loss simulation broken")
	}
	if assembler.Gap() {
		t.Fatal("gap left open at stream end")
	}
}

func TestFrameAssemblerReportsUnhealableGap(t *testing.T) {
	assembler := NewFrameAssembler(4)
	// Frames 10..14 arrive but 1..9 never do: the buffer bound trips and the
	// client is told to catch up instead of buffering forever.
	var err error
	for id := FrameID(10); id <= 14 && err == nil; id++ {
		_, err = assembler.IngestFrames([]Frame{{ID: id}})
	}
	if err == nil || !assembler.Gap() {
		t.Fatalf("unhealable gap not reported: %v", err)
	}
	// Catch-up delivers the missing prefix; everything releases in order.
	var missing []Frame
	for id := FrameID(1); id <= 9; id++ {
		missing = append(missing, Frame{ID: id})
	}
	released, err := assembler.IngestFrames(missing)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 13 || released[0].ID != 1 || released[12].ID != 13 {
		t.Fatalf("released = %d frames, first=%v", len(released), released[0].ID)
	}
}
