package wire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

func TestPacketKindsAndGolden(t *testing.T) {
	packets := []*Packet{{MsgID: 42, Seq: 7, Payload: []byte{0x08, 0x96, 0x01}}, {Flags: FlagPush | FlagSync, MsgID: 10103, Seq: 8, Payload: []byte("sync")}}
	encoded, err := Encode(packets, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(encoded[:19]); got != "525302000000002a0000000700000003089601" {
		t.Fatalf("wire golden = %s", got)
	}
	decoded, err := Decode(encoded, 0)
	if err != nil || len(decoded) != 2 || decoded[1].Flags != 3 || string(decoded[1].Payload) != "sync" {
		t.Fatalf("decode: %+v %v", decoded, err)
	}
	encoded[16] = 0
	if decoded[0].Payload[0] != 0x08 {
		t.Fatal("payload aliases receive buffer")
	}
	reader := bytes.NewReader(encoded)
	for _, want := range packets {
		p, err := Read(reader, 0)
		if err != nil || p.MsgID != want.MsgID || p.Flags != want.Flags {
			t.Fatalf("stream read: %+v %v", p, err)
		}
	}
	if _, err := Read(reader, 0); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF = %v", err)
	}
}

func TestPacketRefusesBadHeaderBeforeReadingPayload(t *testing.T) {
	header, _ := (Header{MsgID: 42, Seq: 1, PayloadSize: 4}).Encode(4)
	for name, change := range map[string]func([]byte){
		"old_version": func(b []byte) { b[2] = 1 }, "control_lockstep": func(b []byte) { clear(b[4:8]); b[3] = FlagLockstep },
		"reserved_kind": func(b []byte) { b[3] = 6 }, "unknown_flag": func(b []byte) { b[3] = 0x80 },
		"control_sync": func(b []byte) { clear(b[4:8]); b[3] = FlagSync },
		"oversized":    func(b []byte) { b[15] = 5 },
	} {
		t.Run(name, func(t *testing.T) {
			data := append([]byte(nil), header[:]...)
			change(data)
			_, err := Read(bytes.NewReader(data), 4)
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("header accepted or payload read: %v", err)
			}
		})
	}
	if _, err := Decode(header[:], 4); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("truncated: %v", err)
	}
	if err := Write(io.Discard, []*Packet{{Flags: 0x80}}, 0); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("encode invalid flags: %v", err)
	}
}
