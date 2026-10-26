//go:build integration && physx && cgo

package main

import (
	"context"
	"crypto/sha256"

	"encoding/hex"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"

	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	"google.golang.org/protobuf/encoding/protowire"
)

// 真实 TCP -> Gate -> NATS -> 房间 RPC -> PhysX -> Nest -> Chunk，包含内容握手和输入确认。
// 仅用于私有 NATS；与本地调试同名空间，测试时不要同时启动调试进程。
func TestRealGateUGCLevelAndInput(t *testing.T) {
	url := os.Getenv("ROOST_PHYSICS_TEST_NATS_URL")
	if url == "" {
		t.Skip("private NATS required")
	}
	level := gamephysics.ObstacleLevel()
	loadedLevel = &level
	defer func() { loadedLevel = nil }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- runPhysicsService(ctx, url) }()
	defer func() {
		cancel()
		select {
		case err := <-serviceDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("service did not drain")
		}
	}()
	caller, closeBus, err := roomBus(url, 7803)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBus()
	rooms, err := domain.NewOwnerClient(caller, roomOwnerSID, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = rooms.Describe(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	gateDone := make(chan error, 1)
	go func() { gateDone <- runPhysicsGate(ctx, addr, url) }()
	defer func() {
		cancel()
		select {
		case err := <-gateDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("gate did not drain")
		}
	}()
	var conn net.Conn
	for {
		conn, err = net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	send := func(id, seq uint32, payload []byte) {
		t.Helper()
		if err := wire.Write(conn, []*wire.Packet{{MsgID: id, Seq: seq, Payload: payload}}, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	read := func() *wire.Packet {
		t.Helper()
		p, err := wire.Read(conn, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	send(0, 1, []byte("physics-slot-1"))
	if p := read(); p.MsgID != 0 || p.Seq != 1 {
		t.Fatal("authentication", p)
	}
	send(levelMessageID, 2, nil)
	p := read()
	if p.MsgID != levelMessageID || p.Seq != 2 {
		t.Fatal("manifest", p)
	}
	data := p.Payload
	_, _, n := protowire.ConsumeTag(data)
	content, n2 := protowire.ConsumeBytes(data[n:])
	if n < 0 || n2 < 0 {
		t.Fatal("content encoding")
	}
	data = data[n+n2:]
	_, _, n = protowire.ConsumeTag(data)
	hash, n2 := protowire.ConsumeString(data[n:])
	if n < 0 || n2 < 0 {
		t.Fatal("hash encoding")
	}
	data = data[n+n2:]
	_, _, n = protowire.ConsumeTag(data)
	body, n2 := protowire.ConsumeVarint(data[n:])
	if n < 0 || n2 < 0 || body != 1 {
		t.Fatal("bound body")
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != hash {
		t.Fatal("content hash")
	}
	decoded, err := gamephysics.DecodeLevel(content)
	if err != nil || decoded.Name != level.Name {
		t.Fatal("content", err)
	}
	payload := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1)
	payload = protowire.AppendVarint(protowire.AppendTag(payload, 5, protowire.VarintType), 3)
	send(inputMessageID, 3, payload)
	var replica gamephysics.RoomReplica
	var generation uint64
	sampled := false
	moving := false
	for !sampled || !moving || generation == 0 {
		p = read()
		switch p.MsgID {
		case inputMessageID:
			_, _, n := protowire.ConsumeTag(p.Payload)
			v, m := protowire.ConsumeVarint(p.Payload[n:])
			if n < 0 || m < 0 || v == 0 {
				t.Fatal("input receipt")
			}
			generation = v
		case frameMessageID:
			if err = replica.Apply(p.Payload); err != nil {
				t.Fatal(err)
			}
			if object, ok := replica.Objects[1]; ok {
				if len(object.Data) != gamephysics.QuantizedBodyBytes {
					t.Fatal("body schema")
				}
				if replica.Ack.Sequence == 1 {
					if generation != 0 && replica.Ack.Generation != generation {
						t.Fatal("input generation mismatch")
					}
					sampled = true
				}
			}
			_, moving = replica.Objects[4]
		default:
			t.Fatal("unexpected packet", fmt.Sprint(p.MsgID))
		}
	}
	t.Logf("UGC content=%s bodies=%d tick=%d sampled input=1 generation=%d", hash, len(decoded.Bodies), replica.Tick, generation)
}
