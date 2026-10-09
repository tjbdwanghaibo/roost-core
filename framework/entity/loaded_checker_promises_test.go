package entity

import (
	"context"
	"testing"
)

// RR-20260926-47：ManagerAccess.IsLoaded 是 Nest 准入的冷目标判定，只读内存：
// 已加载 → true；未加载且有 loader → false；没有 loader → true（慢阶段也读不到）。
// 不调用 loader、不加入在途加载、不 Touch。
func TestManagerAccessIsLoadedNeverLoads(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	hot := newMgrTestEntity(5101, testEntityCategoryPlayer)
	manager.Add(hot)
	if !access.IsLoaded(5102) {
		t.Fatal("without a loader a missing entity has nothing to load")
	}
	loader := &countingAggregateLoader{manager: manager, block: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	if !access.IsLoaded(5101) {
		t.Fatal("loaded entity reported cold")
	}
	if access.IsLoaded(5102) {
		t.Fatal("missing entity with a loader reported loaded")
	}
	// 在途加载存在时也只回答内存状态，不等待。
	access.flights[5102] = &entityLoadFlight{done: make(chan struct{})}
	if access.IsLoaded(5102) {
		t.Fatal("in-flight load reported loaded")
	}
	delete(access.flights, 5102)
	if loader.loads.Load() != 0 {
		t.Fatalf("IsLoaded called the loader %d time(s)", loader.loads.Load())
	}
	close(loader.block)
	if _, err := access.Get(context.Background(), 5102, EntityCategoryNone); err != nil {
		t.Fatal(err)
	}
	if !access.IsLoaded(5102) {
		t.Fatal("entity published by the loader still reported cold")
	}
	var checker LoadedChecker = access
	_ = checker
}
