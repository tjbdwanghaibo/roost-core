package robot_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	mathrand "math/rand"
	"net"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/cube-core/lockstep"
	"github.com/tjbdwanghaibo/cube-core/robot/transport"

	"github.com/tjbdwanghaibo/cube-kit/replication"
	kitrobot "github.com/tjbdwanghaibo/cube-kit/robot"
)

func echoPackets(conn transport.Conn) {
	for {
		packet, err := conn.ReadPacket()
		if err != nil {
			return
		}
		if err := conn.WritePackets([]*transport.Packet{packet}); err != nil {
			return
		}
	}
}

func roundtrip(t *testing.T, transportType, endpoint string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, transport.Config{Type: transportType, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	want := &transport.Packet{MsgID: 42, Seq: 7, Payload: []byte("ping over " + transportType)}
	if err := conn.WritePackets([]*transport.Packet{want}); err != nil {
		t.Fatal(err)
	}
	got, err := conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	if got.MsgID != want.MsgID || got.Seq != want.Seq || string(got.Payload) != string(want.Payload) {
		t.Fatalf("echo mismatch: %+v", got)
	}
}

func TestKCPDialerEndToEnd(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	block, err := replication.NewKCPAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := replication.ListenKCP("127.0.0.1:0", block, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			session, err := listener.AcceptKCP()
			if err != nil {
				return
			}
			go echoPackets(transport.NewTCPConn(session, 0))
		}
	}()
	if err := kitrobot.RegisterKCPDialer("kcp-test", kitrobot.KCPDialerConfig{Key: key, DataShards: 10, ParityShards: 3}); err != nil {
		t.Fatal(err)
	}
	roundtrip(t, "kcp-test", listener.Addr().String())
}

func testQUICCertificates(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{certificateDER}, PrivateKey: privateKey}
	server := &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{replication.DefaultQUICALPN}, MinVersion: tls.VersionTLS13}
	client := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{replication.DefaultQUICALPN}, MinVersion: tls.VersionTLS13} // test certificate only
	return server, client
}

func TestQUICDialerEndToEnd(t *testing.T) {
	serverTLS, clientTLS := testQUICCertificates(t)
	listener, err := replication.ListenQUIC("127.0.0.1:0", serverTLS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				stream, err := conn.AcceptStream(context.Background())
				if err != nil {
					return
				}
				for {
					packet, err := transport.ReadPacketFrom(stream, 0)
					if err != nil {
						return
					}
					if err := transport.WritePacketsTo(stream, []*transport.Packet{packet}); err != nil {
						return
					}
				}
			}()
		}
	}()
	if err := kitrobot.RegisterQUICDialer("quic-test", kitrobot.QUICDialerConfig{TLS: clientTLS}); err != nil {
		t.Fatal(err)
	}
	roundtrip(t, "quic-test", listener.Addr().String())
}

func TestQUICDialerRequiresALPN(t *testing.T) {
	if err := kitrobot.RegisterQUICDialer("quic-bad", kitrobot.QUICDialerConfig{TLS: &tls.Config{}}); err == nil {
		t.Fatal("TLS config without ALPN accepted")
	}
}

// botSink wires one bot to the in-process authoritative loop.
type botSink struct {
	player    lockstep.PlayerID
	sequencer *lockstep.Sequencer
	history   *[]lockstep.Frame
	bot       *kitrobot.LockstepBot // set after NewLockstepBot
	detector  *lockstep.DesyncDetector
	verdicts  *[]lockstep.DesyncVerdict
}

func (s *botSink) SubmitInput(frame lockstep.FrameID, payload []byte) error {
	if _, err := s.sequencer.SubmitInput(s.player, frame, payload); err != nil {
		// A bot racing ahead after a catch-up may submit beyond the window;
		// the authoritative side rejecting it is normal lockstep.
		if errors.Is(err, lockstep.ErrFrameTooEarly) {
			return nil
		}
		return err
	}
	return nil
}

func (s *botSink) ReportHash(frame lockstep.FrameID, hash uint64) error {
	if verdict, ok := s.detector.Report(s.player, frame, hash); ok {
		*s.verdicts = append(*s.verdicts, verdict)
	}
	return nil
}

func (s *botSink) RequestCatchup(from lockstep.FrameID) error {
	// Replay the authoritative history from the requested frame.
	for _, frame := range *s.history {
		if frame.ID >= from {
			if err := s.bot.HandleFrames([]lockstep.Frame{frame}); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestLockstepBotsSurviveThirtyPercentLoss is the desync regression the
// framework was built for: three bots behind independent 30% packet loss
// must apply every frame, and their keyframe hashes must never produce a
// desync outlier.
func TestLockstepBotsSurviveThirtyPercentLoss(t *testing.T) {
	const players, steps = 3, 600
	seats := []lockstep.PlayerID{1, 2, 3}
	sequencer, err := lockstep.NewSequencer(lockstep.SequencerConfig{Players: seats, MaxInputBytes: 16, SubmitWindow: 8})
	if err != nil {
		t.Fatal(err)
	}
	encoder := lockstep.NewRedundantEncoder(4)
	detector := lockstep.NewDesyncDetector(players)
	var history []lockstep.Frame
	var verdicts []lockstep.DesyncVerdict

	bots := make([]*kitrobot.LockstepBot, players)
	losses := make([]*mathrand.Rand, players)
	for i, seat := range seats {
		sink := &botSink{player: seat, sequencer: sequencer, history: &history, detector: detector, verdicts: &verdicts}
		bot, err := kitrobot.NewLockstepBot(kitrobot.LockstepBotConfig{
			Player:           seat,
			Sink:             sink,
			Input:            func(frame lockstep.FrameID) []byte { return []byte{byte(frame), byte(seat)} },
			KeyframeInterval: 20,
			MaxBuffer:        16, // small on purpose: force the catch-up path
		})
		if err != nil {
			t.Fatal(err)
		}
		sink.bot = bot
		bots[i] = bot
		losses[i] = mathrand.New(mathrand.NewSource(int64(1000 + i)))
	}

	for step := 1; step <= steps; step++ {
		frame := sequencer.Advance()
		history = append(history, frame)
		packet := encoder.Push(frame)
		for i, bot := range bots {
			if losses[i].Float64() < 0.30 && step < steps-8 {
				continue // this bot lost the datagram
			}
			if err := bot.HandleBroadcast(packet); err != nil {
				t.Fatalf("bot %d at step %d: %v", i, step, err)
			}
		}
	}
	// Drain: replay the tail so every bot finishes the full stream.
	for _, bot := range bots {
		if bot.Next() <= steps {
			if err := bot.HandleFrames(history[bot.Next()-1:]); err != nil {
				t.Fatal(err)
			}
		}
	}

	catchups := uint64(0)
	for i, bot := range bots {
		stats := bot.Stats()
		if stats.FramesApplied != steps {
			t.Fatalf("bot %d applied %d/%d frames: %+v", i, stats.FramesApplied, steps, stats)
		}
		if stats.HashesReported != steps/20 {
			t.Fatalf("bot %d reported %d hashes: %+v", i, stats.HashesReported, stats)
		}
		if stats.DuplicatesDropped == 0 || stats.InputsSubmitted == 0 {
			t.Fatalf("bot %d stats implausible: %+v", i, stats)
		}
		catchups += stats.CatchupsRequested
	}
	if len(verdicts) == 0 {
		t.Fatal("detector produced no verdicts — keyframe reporting broken")
	}
	for _, verdict := range verdicts {
		if len(verdict.Outliers) != 0 {
			t.Fatalf("false desync at frame %d: %+v", verdict.Frame, verdict)
		}
	}
	t.Logf("verdicts=%d catchups=%d", len(verdicts), catchups)
}

func TestLockstepBotRequestsCatchupOncePerGap(t *testing.T) {
	requested := []lockstep.FrameID{}
	sink := &recordingSink{onCatchup: func(from lockstep.FrameID) { requested = append(requested, from) }}
	bot, err := kitrobot.NewLockstepBot(kitrobot.LockstepBotConfig{Sink: sink, MaxBuffer: 4, KeyframeInterval: 100})
	if err != nil {
		t.Fatal(err)
	}
	// Frames 10..20 with 1..9 missing: the bound trips, one catch-up fires,
	// repeats stay quiet.
	for id := lockstep.FrameID(10); id <= 20; id++ {
		if err := bot.HandleFrames([]lockstep.Frame{{ID: id}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(requested) != 1 || requested[0] != 1 {
		t.Fatalf("catch-up requests = %v", requested)
	}
	// The catch-up closes the gap; the buffered prefix drains with it.
	var missing []lockstep.Frame
	for id := lockstep.FrameID(1); id <= 9; id++ {
		missing = append(missing, lockstep.Frame{ID: id})
	}
	if err := bot.HandleFrames(missing); err != nil {
		t.Fatal(err)
	}
	if bot.Stats().FramesApplied != 13 {
		t.Fatalf("applied = %+v", bot.Stats())
	}
}

type recordingSink struct {
	onCatchup func(lockstep.FrameID)
}

func (s *recordingSink) SubmitInput(lockstep.FrameID, []byte) error { return nil }
func (s *recordingSink) ReportHash(lockstep.FrameID, uint64) error  { return nil }
func (s *recordingSink) RequestCatchup(from lockstep.FrameID) error {
	if s.onCatchup != nil {
		s.onCatchup(from)
	}
	return nil
}

func TestFrameHasherDivergesOnDifferentInputs(t *testing.T) {
	a, b := kitrobot.NewFrameHasher(), kitrobot.NewFrameHasher()
	frame := lockstep.Frame{ID: 1, Inputs: []lockstep.Input{{Player: 1, Payload: []byte{1}}}}
	a.Fold(frame)
	frame.Inputs[0].Payload = []byte{2}
	b.Fold(frame)
	if a.Sum() == b.Sum() {
		t.Fatal("different inputs hashed equal")
	}
}
