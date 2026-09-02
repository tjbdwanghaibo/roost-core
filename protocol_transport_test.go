package nettransport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math"
	"math/big"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	core "github.com/tjbdwanghaibo/cube-core/statesync"
)

func TestAEADSessionProtectorRejectsReplayAndTamper(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	server, err := NewAESGCMProtector(key, [4]byte{1}, [4]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewAESGCMProtector(key, [4]byte{2}, [4]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := server.Seal(1, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.Seal(1, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if payload, err := client.Open(1, second); err != nil || string(payload) != "second" {
		t.Fatalf("out-of-order newest packet failed: payload=%q err=%v", payload, err)
	}
	if payload, err := client.Open(1, first); err != nil || string(payload) != "first" {
		t.Fatalf("valid out-of-order packet failed: payload=%q err=%v", payload, err)
	}
	if _, err := client.Open(1, first); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("replay was accepted: %v", err)
	}
	tampered := append([]byte(nil), second...)
	tampered[len(tampered)-1] ^= 0xff
	otherClient, _ := NewAESGCMProtector(key, [4]byte{2}, [4]byte{1})
	if _, err := otherClient.Open(1, tampered); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered packet was accepted: %v", err)
	}
}

func TestAEADSessionProtectorNeverReusesNonceAfterSequenceExhaustion(t *testing.T) {
	protector, err := NewAESGCMProtector(bytes.Repeat([]byte{5}, 32), [4]byte{1}, [4]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	protector.sendSeq.Store(math.MaxUint64)
	if _, err := protector.Seal(1, []byte("must-fail")); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("sequence exhaustion should remain terminal: %v", err)
	}
	if _, err := protector.Seal(1, []byte("must-still-fail")); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("sequence wrapped and reused a nonce: %v", err)
	}
}

func TestUDPTransportEncryptedLoopback(t *testing.T) {
	serverConn := mustUDPConn(t)
	clientConn := mustUDPConn(t)
	key := bytes.Repeat([]byte{9}, 32)
	serverProtector, _ := NewAESGCMProtector(key, [4]byte{1}, [4]byte{2})
	clientProtector, _ := NewAESGCMProtector(key, [4]byte{2}, [4]byte{1})
	server, err := NewUDPTransport(UDPTransportConfig{PacketConn: serverConn, OwnPacketConn: true, ReadPollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewUDPTransport(UDPTransportConfig{PacketConn: clientConn, OwnPacketConn: true, ReadPollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer client.Close()
	const session core.SessionID = 71
	if err := server.BindSession(session, clientConn.LocalAddr(), serverProtector); err != nil {
		t.Fatal(err)
	}
	if err := client.BindSession(session, serverConn.LocalAddr(), clientProtector); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterSession(core.SessionInfo{ID: session}); err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterSession(core.SessionInfo{ID: session}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverReceived := make(chan []byte, 1)
	clientReceived := make(chan []byte, 1)
	go func() {
		_ = server.Serve(ctx, func(_ context.Context, _ core.SessionID, payload []byte, _ net.Addr) error {
			serverReceived <- append([]byte(nil), payload...)
			return nil
		})
	}()
	go func() {
		_ = client.Serve(ctx, func(_ context.Context, _ core.SessionID, payload []byte, _ net.Addr) error {
			clientReceived <- append([]byte(nil), payload...)
			return nil
		})
	}()
	if err := client.SendDatagram(context.Background(), session, bytes.Repeat([]byte{1}, core.DefaultMaxDatagram)); err != nil {
		t.Fatal(err)
	}
	if payload := receiveBytes(t, serverReceived); len(payload) != core.DefaultMaxDatagram {
		t.Fatalf("UDP payload length=%d", len(payload))
	}
	if err := server.SendDatagram(context.Background(), session, []byte("server-state")); err != nil {
		t.Fatal(err)
	}
	if payload := receiveBytes(t, clientReceived); string(payload) != "server-state" {
		t.Fatalf("UDP payload=%q", payload)
	}
	if err := server.SendReliable(context.Background(), session, []byte("no")); !errors.Is(err, ErrProtocolConfig) {
		t.Fatalf("plain UDP reliable lane should be rejected: %v", err)
	}
}

func TestQUICTransportDatagramAndReliableLoopback(t *testing.T) {
	serverTLS, clientTLS := testQUICCertificates(t)
	listener, err := ListenQUIC("127.0.0.1:0", serverTLS, &quic.Config{MaxIdleTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan *quic.Conn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept(ctx)
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- connection
	}()
	clientConnection, err := DialQUIC(ctx, listener.Addr().String(), clientTLS, &quic.Config{MaxIdleTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var serverConnection *quic.Conn
	select {
	case serverConnection = <-accepted:
	case err := <-acceptErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	server := NewQUICTransport(QUICTransportConfig{})
	client := NewQUICTransport(QUICTransportConfig{})
	defer server.Close()
	defer client.Close()
	const session core.SessionID = 72
	if err := server.BindSession(session, serverConnection); err != nil {
		t.Fatal(err)
	}
	if err := client.BindSession(session, clientConnection); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterSession(core.SessionInfo{ID: session}); err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterSession(core.SessionInfo{ID: session}); err != nil {
		t.Fatal(err)
	}
	if err := server.SendDatagram(ctx, session, []byte("quic-state")); err != nil {
		t.Fatal(err)
	}
	if payload, err := client.ReceiveDatagram(ctx, session); err != nil || string(payload) != "quic-state" {
		t.Fatalf("QUIC datagram payload=%q err=%v", payload, err)
	}
	if err := server.SendReliable(ctx, session, []byte("quic-reliable")); err != nil {
		t.Fatal(err)
	}
	if payload, err := client.ReceiveReliable(ctx, session); err != nil || string(payload) != "quic-reliable" {
		t.Fatalf("QUIC reliable payload=%q err=%v", payload, err)
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := server.SendReliable(deadlineCtx, session, []byte("deadline-scoped")); err != nil {
		deadlineCancel()
		t.Fatal(err)
	}
	if payload, err := client.ReceiveReliable(deadlineCtx, session); err != nil || string(payload) != "deadline-scoped" {
		deadlineCancel()
		t.Fatalf("QUIC deadline payload=%q err=%v", payload, err)
	}
	<-deadlineCtx.Done()
	deadlineCancel()
	backgroundCtx, backgroundCancel := context.WithTimeout(context.Background(), time.Second)
	defer backgroundCancel()
	if err := server.SendReliable(backgroundCtx, session, []byte("after-deadline")); err != nil {
		t.Fatalf("previous write deadline leaked into reused stream: %v", err)
	}
	if payload, err := client.ReceiveReliable(backgroundCtx, session); err != nil || string(payload) != "after-deadline" {
		t.Fatalf("previous read deadline leaked into reused stream: payload=%q err=%v", payload, err)
	}
}

func TestKCPTransportOOBAndReliableLoopback(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	serverCrypt, err := NewKCPAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	clientCrypt, err := NewKCPAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ListenKCP("127.0.0.1:0", serverCrypt, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	clientSession, err := DialKCP(listener.Addr().String(), clientCrypt, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	clientSession.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := clientSession.Write([]byte("bootstrap")); err != nil {
		t.Fatal(err)
	}
	serverSession, err := listener.AcceptKCP()
	if err != nil {
		t.Fatal(err)
	}
	serverSession.SetReadDeadline(time.Now().Add(3 * time.Second))
	bootstrap := make([]byte, 32)
	if _, err := serverSession.Read(bootstrap); err != nil {
		t.Fatal(err)
	}
	server, err := NewKCPTransport(KCPTransportConfig{})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewKCPTransport(KCPTransportConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer client.Close()
	const session core.SessionID = 73
	if err := server.BindSession(session, serverSession); err != nil {
		t.Fatal(err)
	}
	if err := client.BindSession(session, clientSession); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterSession(core.SessionInfo{ID: session}); err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterSession(core.SessionInfo{ID: session}); err != nil {
		t.Fatal(err)
	}
	clientDatagrams := make(chan []byte, 1)
	if err := client.BindDatagramHandler(session, func(_ core.SessionID, payload []byte) { clientDatagrams <- payload }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.SendDatagram(ctx, session, []byte("kcp-oob-state")); err != nil {
		t.Fatal(err)
	}
	if payload := receiveBytes(t, clientDatagrams); string(payload) != "kcp-oob-state" {
		t.Fatalf("KCP OOB payload=%q", payload)
	}
	if err := server.SendReliable(ctx, session, []byte("kcp-reliable")); err != nil {
		t.Fatal(err)
	}
	if payload, err := client.ReceiveReliable(ctx, session); err != nil || string(payload) != "kcp-reliable" {
		t.Fatalf("KCP reliable payload=%q err=%v", payload, err)
	}
	largePayload := bytes.Repeat([]byte("large-kcp-message-"), 4096)
	if err := server.SendReliable(ctx, session, largePayload); err != nil {
		t.Fatal(err)
	}
	if payload, err := client.ReceiveReliable(ctx, session); err != nil || !bytes.Equal(payload, largePayload) {
		t.Fatalf("large KCP reliable payload length=%d want=%d err=%v", len(payload), len(largePayload), err)
	}
}

func mustUDPConn(t *testing.T) *net.UDPConn {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func receiveBytes(t *testing.T, channel <-chan []byte) []byte {
	t.Helper()
	select {
	case payload := <-channel:
		return payload
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for protocol payload")
		return nil
	}
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
	server := &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{DefaultQUICALPN}, MinVersion: tls.VersionTLS13}
	client := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{DefaultQUICALPN}, MinVersion: tls.VersionTLS13} // test certificate only
	return server, client
}
