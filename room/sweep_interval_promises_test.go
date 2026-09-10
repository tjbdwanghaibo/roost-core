package room

import (
	"context"
	"testing"
	"time"
)

// U-0163 · C2 · RR-20260910-01：派生出来的默认扫描周期必须严格为正。
//
// SweepInterval 留零时按 min(IdleTTL/2, 30s) 推导。IdleTTL 是整数除法,任何小于 2ns 的
// 有效正值都会推出 0,而构造器接受了这个 IdleTTL。Start 起的 run 随后 time.NewTicker(0),
// 那会 panic 在后台 goroutine 里 —— 一个构造成功、启动也成功、然后把进程带走的配置。
//
// 极端但有效的正值不该被静默推导成非法值:要么构造阶段拒绝,要么推导保证为正。这里选后者,
// 因为 IdleTTL 本身合法,只是"一半"在这个量级上无法表示。
func TestDerivedSweepIntervalIsAlwaysPositive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		idleTTL time.Duration
	}{
		{"one nanosecond", time.Nanosecond},
		{"one nanosecond over the divisor", 2 * time.Nanosecond},
		{"three nanoseconds", 3 * time.Nanosecond},
		{"a microsecond", time.Microsecond},
		{"a normal minute", 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, err := NewRoomManager(RoomManagerConfig{
				Downstream: ReliableRoomFrameSinkFunc(func(context.Context, []RoomFrame) error { return nil }),
				IdleTTL:    tc.idleTTL,
			})
			if err != nil {
				t.Fatalf("IdleTTL=%v is a valid positive duration but was refused: %v", tc.idleTTL, err)
			}
			if manager.config.SweepInterval <= 0 {
				t.Fatalf("accepted positive IdleTTL %v but derived SweepInterval=%v; the sweep ticker would panic",
					tc.idleTTL, manager.config.SweepInterval)
			}
			if manager.config.SweepInterval > 30*time.Second {
				t.Fatalf("derived SweepInterval=%v exceeds the 30s cap", manager.config.SweepInterval)
			}
			// Starting is what builds the ticker; a zero interval panics there.
			if err := manager.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}

	// An explicit positive SweepInterval is still honoured verbatim, and an
	// explicit one larger than the cap is the caller's choice.
	manager, err := NewRoomManager(RoomManagerConfig{
		Downstream:    ReliableRoomFrameSinkFunc(func(context.Context, []RoomFrame) error { return nil }),
		IdleTTL:       time.Nanosecond,
		SweepInterval: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manager.config.SweepInterval != 2*time.Minute {
		t.Fatalf("explicit SweepInterval was rewritten to %v", manager.config.SweepInterval)
	}
}
