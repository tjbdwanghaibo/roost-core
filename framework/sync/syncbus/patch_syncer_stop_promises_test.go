package syncbus

import (
	"context"
	"errors"
	"testing"
	"time"
)

// RR-20261006-36：PatchSyncer.Stop 曾只调退订函数就返回，Apply 仍可能在跑（修前红文本见 docs/bug/RR-20261006-36.md）。
// 承诺（A3 ② 之后）：Stop(ctx) 在 Apply 返回前不返回 nil，超时返回 ctx 错误、重试再等，返回 nil 之后不再 Apply；
// 停止之后可以再 Start。
func TestPatchSyncerStopWaitsForInFlightApply(t *testing.T) {
	bus := newPatchFakeBus()
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	applied := 0
	s := NewPatchSyncer[testPatch](bus, PatchSyncerConfig[testPatch]{Topic: "patch", LocalSid: 1,
		KeyOf: func(p testPatch) int64 { return p.PlayerID },
		Apply: func(_ context.Context, p testPatch) error {
			applied++
			if p.Name == "block" {
				close(entered)
				<-release
				close(returned)
			}
			return nil
		}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	sub := bus.handlers["patch"][0]
	deliver := func(name string) error {
		return sub.Deliver(context.Background(), &SyncMsg{Topic: "patch", Key: 7, FromSid: 2, Data: []byte(`{"player_id":7,"name":"` + name + `"}`)})
	}
	go func() { _ = deliver("block") }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with Apply in flight = %v, want DeadlineExceeded", err)
	}
	close(release)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop = %v, want nil", err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Stop returned nil while Apply was still running")
	}
	if err := deliver("late"); !errors.Is(err, ErrUnsubscribed) || applied != 1 {
		t.Fatalf("late delivery = %v applied=%d, want ErrUnsubscribed and 1", err, applied)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
