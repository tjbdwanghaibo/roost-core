//go:build integration && physx && cgo

package gate_test

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/client/wire"

	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	roomwiring "github.com/tjbdwanghaibo/roost-core/wiring/physicsroom"
	"testing"
	"time"
)

// 复用真实 Gate/NATS/TCP 验证夹具，确认 Chunk 数据走认证绑定而非本地验收槽位。
func TestRealGatePhysicsRoomRelay(t *testing.T) {
	h := realGateHarness(t, nil)
	conn := h.connect(t)
	// 完成一次真实请求，确保认证绑定已进入 active 状态。
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 901, Seq: 2, Payload: []byte("ready")}}, 8192); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.Read(conn, 8192); err != nil {
		t.Fatal(err)
	}
	binding, _, ok := h.game.SessionReceiver("session")
	if !ok {
		t.Fatal("binding missing")
	}
	cfg := domain.DefaultConfig(func(context.Context, domain.Ref, gamephysics.Frame) error { return nil })
	s, err := domain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	instance, err := s.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Create(context.Background(), domain.Spec{Instance: instance, ID: "gate-room", LifetimeSeconds: 10, Bodies: []physx.Body{{ID: 1, Shape: physx.Sphere, Position: physx.Vec3{0, 5, 0}, Dimensions: physx.Vec3{.5}, Mass: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.Join(context.Background(), ref, binding, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	poller := &jitteredRoomPoller{Rooms: s}
	go func() { done <- roomwiring.Relay(ctx, poller, h.game, binding, member, 902) }()
	var replica gamephysics.RoomReplica
	for range 3 {
		packet, err := wire.Read(conn, 8192)
		if err != nil {
			t.Fatal(err)
		}
		if packet.MsgID != 902 || packet.Flags != wire.FlagSync|wire.FlagPush {
			t.Fatalf("packet=%+v", packet)
		}
		if err = replica.Apply(packet.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if replica.Epoch != ref.Epoch || replica.Sequence != 3 || len(replica.Objects) != 1 {
		t.Fatalf("replica=%+v", replica)
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if poller.empty != 0 {
		t.Fatalf("fixed ticker polled before room send interval: %d empty responses", poller.empty)
	}
	// Relay 结束释放成员，原 token 不能再操作；不同连接不能借同 player ID 重用 token。
	if err = s.Submit(context.Background(), member, domain.Input{Sequence: 1}); !errors.Is(err, domain.ErrMember) {
		t.Fatal(err)
	}
	stale := binding
	stale.BindID = "2223456789abcdef0123456789abcdef"
	if err = h.game.PushBound(context.Background(), stale, 902, wire.PayloadSync, []byte{1}); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("stale binding: %v", err)
	}
}

// 交替 RPC 准入延迟使固定 ticker 的两次服务端 Poll 间隔小于 50ms。
// 接收响应后再安排下一次轮询，不应触发 Room 的过早请求限流。
type jitteredRoomPoller struct {
	domain.Rooms
	calls, empty int
}

func (p *jitteredRoomPoller) Poll(ctx context.Context, m domain.Member, ack uint64) (domain.Delivery, error) {
	p.calls++
	if p.calls%2 == 1 {
		select {
		case <-ctx.Done():
			return domain.Delivery{}, ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
	}
	d, err := p.Rooms.Poll(ctx, m, ack)
	if err == nil && len(d.Payload) == 0 {
		p.empty++
	}
	return d, err
}
