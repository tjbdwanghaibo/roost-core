package wire

import (
	"bytes"
	"errors"
	"testing"
)

// 鉴权首包载荷的两种写法与拒绝规则（客户端接入说明 client/README.md 的字节布局以此为准）。
func TestAuthPayloadEnvelope(t *testing.T) {
	plain, err := EncodeAuth("session:7:abc", 0)
	if err != nil || string(plain) != "session:7:abc" {
		t.Fatalf("plain = %q, %v", plain, err)
	}
	envelope, err := EncodeAuth("session:7:abc", 0x0102030405060708)
	want := append([]byte{0x00, 'G', 1, 2, 3, 4, 5, 6, 7, 8}, "session:7:abc"...)
	if err != nil || !bytes.Equal(envelope, want) {
		t.Fatalf("envelope = % x, %v; want % x", envelope, err, want)
	}
	for _, payload := range [][]byte{plain, envelope} {
		ticket, generation, err := DecodeAuth(payload)
		if err != nil || string(ticket) != "session:7:abc" {
			t.Fatalf("decode % x = %q, %v", payload, ticket, err)
		}
		if wantGeneration := map[bool]uint64{false: 0, true: 0x0102030405060708}[payload[0] == 0]; generation != wantGeneration {
			t.Fatalf("decode % x generation = %d, want %d", payload, generation, wantGeneration)
		}
	}
	for _, ticket := range []string{"", "\x00ticket"} {
		if _, err := EncodeAuth(ticket, 1); !errors.Is(err, ErrInvalidPacket) {
			t.Fatalf("EncodeAuth(%q) = %v, want ErrInvalidPacket", ticket, err)
		}
	}
	for _, payload := range [][]byte{
		nil,
		{0x00},
		{0x00, 'G', 0, 0, 0, 0, 0, 0, 0, 1},
		append([]byte{0x00, 'X', 0, 0, 0, 0, 0, 0, 0, 1}, 't'),
		append([]byte{0x00, 'G', 0, 0, 0, 0, 0, 0, 0, 0}, 't'),
		append([]byte{0x00, 'G', 0, 0, 0, 0, 0, 0, 0, 1}, 0x00, 't'),
	} {
		if _, _, err := DecodeAuth(payload); !errors.Is(err, ErrInvalidPacket) {
			t.Fatalf("DecodeAuth(% x) = %v, want ErrInvalidPacket", payload, err)
		}
	}
}
