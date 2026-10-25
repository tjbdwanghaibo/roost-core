package main

import (
	"context"
	"encoding/binary"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestControlOwnershipSequenceAndExpiry(t *testing.T) {
	c := new(controls)
	if _, err := c.claim(51); err == nil {
		t.Fatal("invalid slot accepted")
	}
	s, err := c.claim(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.claim(1); err == nil {
		t.Fatal("duplicate owner")
	}
	now := time.Now()
	if err = c.update(1, s, 1, 8, 0, now); err != nil {
		t.Fatal(err)
	}
	if got := c.commands(1); len(got) != 51 || got[50].ID != 51 {
		t.Fatalf("wrong binding: %v", got)
	}
	for _, v := range []float32{float32(math.NaN()), float32(math.Inf(1)), 21} {
		if err = c.update(1, s, 2, v, 0, now); err == nil {
			t.Fatal("invalid acceleration accepted")
		}
	}
	if err = c.update(1, s, 1, 8, 0, now); err == nil {
		t.Fatal("replayed input")
	}
	if err = c.update(1, s, 2, 8, 0, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(c.commands(1)) != 50 {
		t.Fatal("expired input retained")
	}
	c.release(1, s)
	next, err := c.claim(1)
	if err != nil {
		t.Fatal(err)
	}
	c.release(1, s)
	if err = c.update(1, next, 1, 8, 0, now); err != nil {
		t.Fatal("old release cleared new owner")
	}
	if err = c.update(1, s, 3, 8, 0, now); err == nil {
		t.Fatal("old owner accepted")
	}
}

// 真实双向连接的读取生命周期必须随断线排空，不能只在邮箱单测中释放槽位。
func TestControlledConnectionDisconnectReleasesSlot(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	mailbox := new(controls)
	var latest atomic.Pointer[gamephysics.SpatialFrame]
	latest.Store(&gamephysics.SpatialFrame{Snapshot: chunksync.Snapshot{Epoch: 1}})
	ready, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		serveControlled(ctx, server, &latest, 20, mailbox, 0, 0, func() { once.Do(func() { close(ready) }) })
	}()
	data := make([]byte, 16)
	binary.BigEndian.PutUint32(data[:4], 1)
	binary.BigEndian.PutUint32(data[4:8], 1)
	binary.BigEndian.PutUint32(data[8:12], math.Float32bits(8))
	if _, err := client.Write(data); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("input not received")
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection did not drain")
	}
	if _, err := mailbox.claim(1); err != nil {
		t.Fatal("disconnected slot remained owned", err)
	}
	if len(mailbox.commands(1)) != 50 {
		t.Fatal("disconnected input retained")
	}
}

// 旁观连接不占小球、不参与玩家 AOI；用于 Unity 观察完整的物理世界。
func TestCrowdSpectatorReceivesFullScene(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	frame, err := gamephysics.NewSpatialFrame(gamephysics.InitialFrame(1, gamephysics.CrowdScene()), 25, 10)
	if err != nil {
		t.Fatal(err)
	}
	var latest atomic.Pointer[gamephysics.SpatialFrame]
	latest.Store(frame)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); serveControlled(ctx, server, &latest, 50, new(controls), 12, 2, func() {}) }()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = client.Write(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	var header [16]byte
	if _, err = io.ReadFull(client, header[:]); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, binary.BigEndian.Uint32(header[12:]))
	if _, err = io.ReadFull(client, data); err != nil {
		t.Fatal(err)
	}
	var replica chunksync.Replica
	if err = replica.Apply(data); err != nil {
		t.Fatal(err)
	}
	if len(replica.Objects) != gamephysics.CrowdBodies {
		t.Fatalf("spectator only received %d bodies", len(replica.Objects))
	}
	cancel()
	client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("spectator did not drain")
	}
}
