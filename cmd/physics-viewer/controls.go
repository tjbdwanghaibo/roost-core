package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// controls 是传输输入邮箱，不是可回滚世界状态。只接受期望加速度，物理执行器独占实际运动状态。
// 每连接绑定一个槽位，不能通过后续报文选择别人的刚体；最多保存每槽最新一条输入。
type controls struct {
	mu       sync.Mutex
	slots    [gamephysics.CrowdPlayers]*controlSlot
	accepted atomic.Uint64
	rejected atomic.Uint64
}
type controlSlot struct {
	sequence     uint32
	acceleration physx.Vec3
	received     time.Time
}

func (c *controls) claim(id uint32) (*controlSlot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == 0 || id > gamephysics.CrowdPlayers || c.slots[id-1] != nil {
		return nil, errors.New("invalid or occupied control slot")
	}
	s := new(controlSlot)
	c.slots[id-1] = s
	return s, nil
}
func (c *controls) release(id uint32, s *controlSlot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slots[id-1] == s {
		c.slots[id-1] = nil
	}
}
func (c *controls) update(id uint32, s *controlSlot, seq uint32, x, z float32, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slots[id-1] != s || seq <= s.sequence || math.IsNaN(float64(x)) || math.IsNaN(float64(z)) || math.IsInf(float64(x), 0) || math.IsInf(float64(z), 0) || x*x+z*z > 400 {
		return errors.New("invalid control input")
	}
	s.sequence = seq
	s.acceleration = physx.Vec3{x, 0, z}
	s.received = now
	c.accepted.Add(1)
	return nil
}
func (c *controls) ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.slots {
		if s == nil || s.sequence == 0 {
			return false
		}
	}
	return true
}
func (c *controls) commands(tick uint64) []physx.Command {
	out := gamephysics.CrowdCommands(tick)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.slots {
		// 输入停发后最多继续施力 250ms；断开立即释放，惯性仍由模拟器处理。
		if s != nil && s.sequence > 0 && now.Sub(s.received) <= 250*time.Millisecond {
			out = append(out, physx.Command{ID: uint64(gamephysics.CrowdCapsules + i + 1), Acceleration: s.acceleration})
		}
	}
	return out
}

// 本地验收协议：先发大端 u32 槽号（0 旁观，1..20 控制），之后每条 12 字节：seq:uint32、ax:float32、az:float32。
// 无帐号鉴权，只允许宿主原有 loopback 地址。读取有期限，所有 goroutine 随连接关闭排空。
func serveControlled(ctx context.Context, conn net.Conn, latest *atomic.Pointer[gamephysics.SpatialFrame], hz int, c *controls, radius, margin float64, ready func()) {
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return
	}
	var hello [4]byte
	if _, err := io.ReadFull(conn, hello[:]); err != nil {
		c.rejected.Add(1)
		return
	}
	id := binary.BigEndian.Uint32(hello[:])
	if id == 0 {
		serve(ctx, conn, latest, hz, 0, 0, 0, func() {})
		return
	}
	slot, err := c.claim(id)
	if err != nil {
		c.rejected.Add(1)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		defer c.release(id, slot)
		var data [12]byte
		for {
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				return
			}
			if _, err := io.ReadFull(conn, data[:]); err != nil {
				return
			}
			seq := binary.BigEndian.Uint32(data[:4])
			x := math.Float32frombits(binary.BigEndian.Uint32(data[4:8]))
			z := math.Float32frombits(binary.BigEndian.Uint32(data[8:]))
			if err := c.update(id, slot, seq, x, z, time.Now()); err != nil {
				c.rejected.Add(1)
				return
			}
			ready()
		}
	}()
	serve(ctx, conn, latest, hz, uint64(gamephysics.CrowdCapsules)+uint64(id), radius, margin, ready)
	conn.Close()
	<-done
}
