package entitysync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// Push 中同步改变控制面，固定“已捕获、尚未采纳”的交错，无需靠 sleep 碰运气。
func TestInFlightDeliveryCannotOverwriteNewIntent(t *testing.T) {
	for _, change := range []string{"hold", "reopen", "reopen_failure", "profile", "unsubscribe", "unregister"} {
		t.Run(change, func(t *testing.T) {
			records := newRecordingTransport()
			var manager *Manager
			var hook func()
			manager = newTestManager(t, TransportFunc(func(ctx context.Context, sid SessionID, data []byte) error {
				if hook != nil {
					f := hook
					hook = nil
					f()
					if change == "reopen_failure" {
						return errors.New("old connection failed")
					}
				}
				return records.Push(ctx, sid, data)
			}), ManagerConfig{})
			packs := 0
			state := testSubject(t, 4101, &packs)
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, 4101, entity.SyncProfile{})
			hook = func() {
				switch change {
				case "hold":
					if err := manager.HoldSession(1); err != nil {
						t.Fatal(err)
					}
				case "reopen", "reopen_failure":
					manager.CloseSession(1)
					open(t, manager, 1)
					mustSubscribe(t, manager, 1, 4101, entity.SyncProfile{})
				case "profile":
					mustSubscribe(t, manager, 1, 4101, entity.SyncProfile{Key: "far", LOD: 2})
				case "unsubscribe":
					if err := manager.Unsubscribe(1, 4101); err != nil {
						t.Fatal(err)
					}
				case "unregister":
					if err := manager.Unregister(4101); err != nil {
						t.Fatal(err)
					}
				}
			}
			mustFlush(t, manager)
			records.take(1)
			if change == "hold" {
				if manager.Stats().HeldSessions != 1 {
					t.Fatal("old delivery overwrote the held session")
				}
				if err := manager.ReadySession(1); err != nil {
					t.Fatal(err)
				}
			}
			mustFlush(t, manager)
			got := oneFrame(t, records, 1)
			switch change {
			case "hold", "reopen", "reopen_failure":
				if got.wire.Kind != frame.Full || got.objects[4101] != frame.ObjectCreate {
					t.Fatalf("new lifetime lost snapshot: %+v", got)
				}
				if change == "hold" && got.wire.Epoch != 2 {
					t.Fatalf("epoch = %d", got.wire.Epoch)
				}
			case "profile":
				if !got.updates[4101].Full || got.updates[4101].Profile.Key != "far" {
					t.Fatalf("profile change lost: %+v", got.updates)
				}
			default:
				if len(got.wire.Objects) != 1 || got.wire.Objects[0].Operation != frame.ObjectRemove {
					t.Fatal("accepted create was not removed")
				}
			}
		})
	}
}

func TestStopAndCloseRespectDeadlineWhileAnotherFlushIsInFlight(t *testing.T) {
	for _, operation := range []string{"stop", "close"} {
		t.Run(operation, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			manager := newTestManager(t, TransportFunc(func(context.Context, SessionID, []byte) error { close(started); <-release; return nil }), ManagerConfig{})
			packs := 0
			state := testSubject(t, 4201, &packs)
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, 4201, entity.SyncProfile{})
			flushed := make(chan error, 1)
			go func() { flushed <- manager.Flush(context.Background()) }()
			<-started
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			stopped := make(chan error, 1)
			go func() {
				if operation == "stop" {
					stopped <- manager.Stop(ctx)
				} else {
					stopped <- manager.Close(ctx)
				}
			}()
			returned := false
			select {
			case err := <-stopped:
				returned = true
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("got %v, want deadline exceeded", err)
				}
			case <-time.After(time.Second):
			}
			close(release)
			if err := <-flushed; err != nil {
				t.Fatal(err)
			}
			if !returned {
				<-stopped
				t.Fatal("deadline was ignored while waiting for Flush")
			}
		})
	}
}

func TestStopCancelsPushAndPreventsRestartUntilItReturns(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	manager := newTestManager(t, TransportFunc(func(ctx context.Context, _ SessionID, _ []byte) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	}), ManagerConfig{Interval: time.Millisecond})
	packs := 0
	state := testSubject(t, 4202, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(1)
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 4202, entity.SyncProfile{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tick did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- manager.Stop(ctx) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		cancel()
		close(release)
		t.Fatal("Stop did not cancel in-flight Push")
	}
	cancel()
	if err := <-stopped; !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v", err)
	}
	if err := manager.Start(context.Background()); !errors.Is(err, ErrManagerStopping) {
		t.Fatalf("Start during stop: %v", err)
	}
	manager.runMu.Lock()
	done := manager.stopState.done
	manager.runMu.Unlock()
	close(release)
	<-done
	if manager.Stats().Sessions != 1 || !state.PendingDirty() {
		t.Fatal("cancelling the tick blamed the session or lost dirty content")
	}
	// 清理不再次调用上面的单次屏障传输；下一次 Flush 用已取消 context 不会进入 Push。
	manager.CloseSession(1)
	if err := manager.Stop(nil); err != nil {
		t.Fatal(err)
	}
}

func TestCloseReleasesStateAfterFinalFlushError(t *testing.T) {
	manager := newTestManager(t, TransportFunc(func(context.Context, SessionID, []byte) error { return ErrRetryLater }), ManagerConfig{})
	packs := 0
	if err := manager.Register(testSubject(t, 4203, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 4203, entity.SyncProfile{})
	if err := manager.Close(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("Close: %v", err)
	}
	if !manager.isClosed() || manager.Stats().Sessions != 0 || manager.Stats().Subjects != 0 {
		t.Fatal("final flush failure prevented cleanup")
	}
}
