package room

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
)

func TestRoomManagerEnforcesGlobalBudgetsAndReleasesThem(t *testing.T) {
	manager, err := NewRoomManager(RoomManagerConfig{
		Downstream: ReliableRoomFrameSinkFunc(func(context.Context, []RoomFrame) error { return nil }),
		MaxRooms:   2, MaxTotalSubjects: 1, MaxTotalSubscribers: 1,
		MaxSubjectsPerRoom: 2, MaxSubscribersPerRoom: 2,
		IdleTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(context.Background()) }()

	first, err := manager.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(3); !errors.Is(err, ErrRoomLimit) {
		t.Fatalf("room limit error = %v", err)
	}
	firstState := testRoomState(101, nil)
	secondState := testRoomState(202, nil)
	if err := first.RegisterSubject(firstState); err != nil {
		t.Fatal(err)
	}
	if err := second.RegisterSubject(secondState); !errors.Is(err, ErrRoomGlobalSubjectLimit) {
		t.Fatalf("global subject limit error = %v", err)
	}
	subscriber := coreentitysync.SubscriberRef{ID: 1}
	if _, err := first.Subscribe(context.Background(), subscriber, 101, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := first.Unsubscribe(context.Background(), subscriber, 101); err != nil {
		t.Fatal(err)
	}
	if err := first.UnregisterSubject(101); err != nil {
		t.Fatal(err)
	}
	if err := second.RegisterSubject(secondState); err != nil {
		t.Fatalf("released subject budget was not reusable: %v", err)
	}
	stats := manager.Stats()
	if stats.ActiveRooms != 2 || stats.ActiveSubjects != 1 || stats.ActiveSubscribers != 0 {
		t.Fatalf("manager stats = %+v", stats)
	}
}

func TestRoomManagerExpiresOnlyEmptyIdleRooms(t *testing.T) {
	manager, err := NewRoomManager(RoomManagerConfig{
		Downstream: ReliableRoomFrameSinkFunc(func(context.Context, []RoomFrame) error { return nil }),
		MaxRooms:   2, IdleTTL: 20 * time.Millisecond, SweepInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(context.Background()) }()
	if _, err := manager.Create(10); err != nil {
		t.Fatal(err)
	}
	// Get counts as room activity and would keep the room alive, so expiry is
	// observed through Stats only.
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats := manager.Stats()
		if stats.ActiveRooms == 0 && stats.IdleExpired == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle room was not expired: stats=%+v", stats)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := manager.Get(10); ok {
		t.Fatal("expired room must not be retrievable")
	}
}

func TestRoomManagerLifecycleIsFailClosed(t *testing.T) {
	manager, err := NewRoomManager(RoomManagerConfig{Downstream: ReliableRoomFrameSinkFunc(func(context.Context, []RoomFrame) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(1); !errors.Is(err, ErrRoomManagerNotRunning) {
		t.Fatalf("create before start error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(2); !errors.Is(err, ErrRoomManagerStopped) {
		t.Fatalf("create after close error = %v", err)
	}
}

func TestRoomManagerCloseTimeoutRetainsRoomsForRetry(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	manager, err := NewRoomManager(RoomManagerConfig{
		Downstream: ReliableRoomFrameSinkFunc(func(context.Context, []RoomFrame) error {
			if calls.Add(1) > 1 {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
			}
			return nil
		}),
		ReplicationInterval: time.Millisecond,
		IdleTTL:             time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	room, err := manager.Create(99)
	if err != nil {
		t.Fatal(err)
	}
	state := testRoomState(909, nil)
	if err := room.RegisterSubject(state); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Subscribe(context.Background(), coreentitysync.SubscriberRef{ID: 9}, 909, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(1)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("room flush did not enter blocking downstream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = manager.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error=%v", err)
	}
	if stats := manager.Stats(); stats.ActiveRooms != 1 {
		t.Fatalf("timed-out Close lost live room reference: %+v", stats)
	}
	if _, ok := manager.Get(99); ok {
		t.Fatal("stopped manager exposed room after shutdown began")
	}
	close(release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("retry Close error=%v", err)
	}
	if stats := manager.Stats(); stats.ActiveRooms != 0 || stats.ActiveSubjects != 0 || stats.ActiveSubscribers != 0 {
		t.Fatalf("retry Close leaked resources: %+v", stats)
	}
}
