package entity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerAccessPreservesOrderAndMissingEntries(t *testing.T) {
	manager := NewEntityManager()
	first := newMgrTestEntity(1001, testEntityCategoryPlayer)
	second := newMgrTestEntity(1002, testEntityCategoryPlayer)
	manager.Add(first)
	manager.Add(second)
	access := NewManagerAccess(manager)
	values, err := access.GetMany(context.Background(), []int64{1002, 9999, 1001}, []EntityCategory{
		testEntityCategoryPlayer, testEntityCategoryPlayer, testEntityCategoryPlayer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || values[0] != second || values[1] != nil || values[2] != first {
		t.Fatalf("unexpected ordered lookup: %+v", values)
	}
}

type concurrentAggregateLoader struct {
	manager *EntityManager
	active  atomic.Int32
	max     atomic.Int32
}

func (l *concurrentAggregateLoader) LoadEntity(_ context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	active := l.active.Add(1)
	for current := l.max.Load(); active > current && !l.max.CompareAndSwap(current, active); current = l.max.Load() {
	}
	time.Sleep(10 * time.Millisecond)
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	if err := l.manager.TryAdd(value); err != nil {
		l.active.Add(-1)
		return nil, err
	}
	l.active.Add(-1)
	return value, nil
}

func TestManagerAccessGetManyLoadsColdEntitiesConcurrently(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	access.ConfigureLoadConcurrency(4)
	loader := &concurrentAggregateLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	ids := []int64{2001, 2002, 2003, 2004}
	values, err := access.GetMany(context.Background(), ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != len(ids) || loader.max.Load() < 2 {
		t.Fatalf("cold load did not run concurrently: values=%d max=%d", len(values), loader.max.Load())
	}
}

type countingAggregateLoader struct {
	manager *EntityManager
	loads   atomic.Int32
	fail    atomic.Bool
	block   chan struct{}
}

func (l *countingAggregateLoader) LoadEntity(_ context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	l.loads.Add(1)
	if l.block != nil {
		<-l.block
	}
	if l.fail.Load() {
		return nil, ErrEntityNotManaged
	}
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	if err := l.manager.TryAdd(value); err != nil {
		return nil, err
	}
	return value, nil
}

// Regression: a cold-cache Get used to issue one LoadEntity per concurrent
// caller — a hot entity's cache miss stampeded the database. Concurrent
// loads of one id must collapse into a single flight.
func TestManagerAccessColdLoadIsSingleFlight(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := &countingAggregateLoader{manager: manager, block: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const callers = 16
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			value, err := access.Get(context.Background(), 4242, testEntityCategoryPlayer)
			if err == nil && value == nil {
				err = ErrEntityNotManaged
			}
			results <- err
		}()
	}
	for loader.loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let every waiter join the flight
	close(loader.block)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := loader.loads.Load(); got != 1 {
		t.Fatalf("concurrent cold loads issued %d LoadEntity calls, want 1", got)
	}
	// A failed flight shares the error but is removed, so a retry loads fresh.
	failing := &countingAggregateLoader{manager: manager}
	failing.fail.Store(true)
	if _, err := access.ConfigureLoader(failing); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Get(context.Background(), 5555, testEntityCategoryPlayer); err == nil {
		t.Fatal("expected load failure")
	}
	failing.fail.Store(false)
	if value, err := access.Get(context.Background(), 5555, testEntityCategoryPlayer); err != nil || value == nil {
		t.Fatalf("retry after failed flight: value=%v err=%v", value, err)
	}
	if got := failing.loads.Load(); got != 2 {
		t.Fatalf("retry did not start a fresh load: loads=%d", got)
	}
}

// A waiter whose own context is cancelled stops waiting without killing the
// in-flight load for everyone else.
func TestManagerAccessFlightWaiterHonorsOwnContext(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := &countingAggregateLoader{manager: manager, block: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		_, err := access.Get(context.Background(), 7777, testEntityCategoryPlayer)
		started <- err
	}()
	for loader.loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := access.Get(cancelled, 7777, testEntityCategoryPlayer)
		waiterDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-waiterDone; err != context.Canceled {
		t.Fatalf("cancelled waiter returned %v", err)
	}
	close(loader.block)
	if err := <-started; err != nil {
		t.Fatalf("original load was affected by waiter cancellation: %v", err)
	}
	if got := loader.loads.Load(); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}
}
