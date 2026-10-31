package transport

import (
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"net"
	"testing"
)

func TestPlayerTransportSeparatesPushAndRetiresTimedOutRequest(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	conn := &PlayerConn{conn: local, maxPayload: 4096, sequences: make(map[uint32]uint32)}
	written := make(chan error, 1)
	go func() { written <- conn.WritePackets([]*Packet{AuthPacket("ticket"), {MsgID: 7, Seq: 11}}) }()
	auth, err := wire.Read(peer, 4096)
	if err != nil {
		t.Fatal(err)
	}
	request, err := wire.Read(peer, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if auth.Seq != 1 || request.Seq != 2 {
		t.Fatalf("nonmonotonic wire seq: %d %d", auth.Seq, request.Seq)
	}
	go func() {
		written <- wire.Write(peer, []*Packet{{Flags: wire.FlagPush, MsgID: 7, Seq: request.Seq}, {MsgID: 7, Seq: request.Seq}}, 4096)
	}()
	push, err := conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	if push.Seq != 0 {
		t.Fatal("push matched a request")
	}
	response, err := conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	if response.Seq != 11 {
		t.Fatalf("response lost correlation: %+v", response)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	conn.mu.Lock()
	conn.sequences[3] = 12
	conn.mu.Unlock()
	conn.ForgetRequest(12)
	go func() {
		written <- wire.Write(peer, []*Packet{{MsgID: 7, Seq: 3}, {Flags: wire.FlagPush, MsgID: 8, Seq: 3}}, 4096)
	}()
	push, err = conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	if push.MsgID != 8 || push.Seq != 0 {
		t.Fatalf("late response became a push: %+v", push)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.sequences) != 0 {
		t.Fatal("pending wire mappings leaked")
	}
}
