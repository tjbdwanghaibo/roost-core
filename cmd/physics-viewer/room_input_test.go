package main

import (
	"google.golang.org/protobuf/encoding/protowire"
	"math"
	"testing"
)

func TestRoomInputProtobuf(t *testing.T) {
	payload := protowire.AppendTag(nil, 1, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 300)
	payload = protowire.AppendTag(payload, 2, protowire.Fixed32Type)
	payload = protowire.AppendFixed32(payload, math.Float32bits(-10))
	payload = protowire.AppendTag(payload, 4, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)
	got, err := decodeRoomInput(payload)
	if err != nil || got.Sequence != 300 || got.Acceleration[0] != -10 || !got.Jump {
		t.Fatalf("input=%+v err=%v", got, err)
	}
	for _, bad := range [][]byte{nil, {8}, {8, 0}, {8, 1, 8, 2}, {8, 1, 32, 2}, {8, 1, 21, 0}, {8, 1, 18, 0}} {
		if _, err := decodeRoomInput(bad); err == nil {
			t.Fatalf("invalid protobuf accepted: %x", bad)
		}
	}
}
