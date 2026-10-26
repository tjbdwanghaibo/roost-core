package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	roomwiring "github.com/tjbdwanghaibo/roost-core/wiring/physicsroom"
)

// runRoom 只提供 loopback 验收入口，房间执行、Entity、输入和 Chunk 全部复用正式 service。
// 生产认证和开房权限由业务工程处理；这里的 slot 只用于本地选择控制球。
func runRoom(ctx context.Context, addr string) error {
	if err := entity.RegisterEntityKindCategory(240, 1); err != nil {
		return err
	}
	host, err := roomwiring.NewEntityHost(240, 1, 1, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 128}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}))
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	cfg := domain.DefaultConfig(host.Commit)
	cfg.MaxRooms = 1
	cfg.Closed = host.Forget
	rooms, err := domain.New(cfg)
	if err != nil {
		return err
	}
	defer rooms.Shutdown(context.Background())
	instance, err := rooms.Describe(ctx)
	if err != nil {
		return err
	}
	ref, err := rooms.Create(ctx, domain.Spec{Instance: instance, ID: "unity-room", Bodies: gamephysics.CrowdScene(), LifetimeSeconds: 3600})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	local, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-local.Done(); listener.Close() }()
	var clients sync.WaitGroup
	slots := make(chan struct{}, 20)
	log.Printf("physics room %s; players=20 physics=50Hz sync=20Hz; EntityHost -> Nest -> StateDAO -> Chunk", addr)
	var acceptErr error
	for {
		conn, err := listener.Accept()
		if err != nil {
			if local.Err() == nil {
				acceptErr = err
			}
			break
		}
		select {
		case slots <- struct{}{}:
			clients.Add(1)
			go func() { defer clients.Done(); defer func() { <-slots }(); serveRoom(local, conn, rooms, ref) }()
		default:
			conn.Close()
		}
	}
	cancel()
	clients.Wait()
	frame, err := host.Snapshot(context.Background(), ref)
	if err == nil {
		log.Printf("room entity committed tick=%d bodies=%d", frame.Tick, len(frame.States))
	}
	return acceptErr
}

func roomToken() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

// PR01 + slot:u32 握手；输入是 seq:u64、ax:f32、az:f32、jump:u8（均大端，17 字节）。
func serveRoom(parent context.Context, conn net.Conn, rooms *domain.Service, ref domain.Ref) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hello [8]byte
	if _, err := io.ReadFull(conn, hello[:]); err != nil || string(hello[:4]) != "PR01" {
		return
	}
	slot := binary.BigEndian.Uint32(hello[4:])
	if slot < 1 || slot > 20 {
		return
	}
	binding := gateway.Binding{PlayerID: int64(slot), SessionID: fmt.Sprintf("local-room-%d", slot), ConnectionNonce: roomToken(), BindID: roomToken(), Gate: gateway.ProcessIdentity{ServerID: 1, Incarnation: "local-viewer-gate"}, Game: gateway.ProcessIdentity{ServerID: 2, Incarnation: "local-viewer-game"}}
	member, err := rooms.Join(parent, ref, binding, uint64(gamephysics.CrowdCapsules)+uint64(slot))
	if err != nil {
		return
	}
	defer rooms.Leave(context.Background(), member)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		var data [17]byte
		for {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.ReadFull(conn, data[:]); err != nil {
				return
			}
			if data[16] > 1 {
				return
			}
			in := domain.Input{Sequence: binary.BigEndian.Uint64(data[:8]), Acceleration: physx.Vec3{math.Float32frombits(binary.BigEndian.Uint32(data[8:12])), 0, math.Float32frombits(binary.BigEndian.Uint32(data[12:16]))}, Jump: data[16] == 1}
			if err := rooms.Submit(ctx, member, in); err != nil {
				return
			}
		}
	}()
	// 取消立即中断读取，退出前等待输入 goroutine，避免断连后继续写旧成员。
	closeDone := make(chan struct{})
	go func() { defer close(closeDone); <-ctx.Done(); conn.Close() }()
	defer func() { cancel(); conn.Close(); <-done; <-closeDone }()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	var ack uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		frame, err := rooms.Poll(ctx, member, ack)
		// 从 Poll 完成时安排下一轮，避免 RPC 抖动把两次服务端采样挤到 50ms 内。
		timer.Reset(50 * time.Millisecond)
		if err != nil {
			return
		}
		if len(frame.Payload) == 0 {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		if err := wire.Write(conn, []*wire.Packet{{Flags: wire.FlagSync | wire.FlagPush, MsgID: frameMessageID, Payload: frame.Payload}}, 1<<20); err != nil {
			return
		}
		ack = frame.Sequence
	}
}
