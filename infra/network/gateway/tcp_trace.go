package gateway

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// TCPWriteEvent 记录 socket 写尝试，不把写成功当成客户端已收到或已展示。
// Duration 从进入写临界区前计时，包含 writeMu 等待；不含上游队列时间。
// Epoch/Tick 只从 Sync 的固定帧头提取用于诊断，不代替协议校验。
type TCPWriteEvent struct {
	At                  int64
	Connection          string
	PlayerID            int64
	MessageID, Sequence uint32
	Kind                wire.PayloadKind
	Epoch, Tick         uint32
	Bytes               int64
	Duration            time.Duration
	Failed              bool
}

// TCPWriteTrace 是可选有界写出记录；不保存票据、PB 或实体 payload。
type TCPWriteTrace struct {
	mu          sync.Mutex
	events      []TCPWriteEvent
	next, count int
	overwritten uint64
}

func NewTCPWriteTrace(capacity int) (*TCPWriteTrace, error) {
	if capacity < 1 || capacity > 1<<20 {
		return nil, fmt.Errorf("gateway: trace capacity must be 1..1048576")
	}
	return &TCPWriteTrace{events: make([]TCPWriteEvent, capacity)}, nil
}

func (t *TCPWriteTrace) record(event TCPWriteEvent, payload []byte) {
	if event.Kind == wire.PayloadSync && len(payload) >= 32 && binary.BigEndian.Uint32(payload[:4]) == 0x43525031 {
		event.Epoch, event.Tick = binary.BigEndian.Uint32(payload[16:20]), binary.BigEndian.Uint32(payload[20:24])
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.events) == 0 {
		return
	}
	if t.count == len(t.events) {
		t.overwritten++
	} else {
		t.count++
	}
	t.events[t.next] = event
	t.next = (t.next + 1) % len(t.events)
}

func (t *TCPWriteTrace) Drain() ([]TCPWriteEvent, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]TCPWriteEvent, t.count)
	for i := range result {
		result[i] = t.events[(t.next-t.count+i+len(t.events))%len(t.events)]
	}
	clear(t.events)
	t.count = 0
	return result, t.overwritten
}
