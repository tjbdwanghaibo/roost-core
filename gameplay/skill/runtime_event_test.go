package skill

import (
	"testing"
)

func TestDerivedEventPreservesRootAndSetsParent(t *testing.T) {
	root := newRootEvent(1)
	child := deriveEvent(root, 2)
	grandchild := deriveEvent(child, 3)
	if grandchild.RootEventID != root.EventID || grandchild.ParentEventID != child.EventID {
		t.Fatalf("broken causal identity: %#v", grandchild)
	}
}

func TestEventContextCopiesSortsAndDeduplicatesTags(t *testing.T) {
	tags := []GameplayTagHandle{3, 1, 3, 2}
	event := newRootEvent(1).WithGameplayTags(tags)
	tags[0] = 99
	want := []GameplayTagHandle{1, 2, 3}
	got := event.GameplayTags()
	if len(got) != len(want) {
		t.Fatalf("tags = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("tags = %v", got)
		}
	}
	got[0] = 99
	if event.GameplayTags()[0] != 1 {
		t.Fatal("event exposed mutable tag storage")
	}
}

// TestCollectHostEventsSkipsEventWhenEveryRootIsPinned 是 TestCollectHostEventsDoesNotAdvanceOrCompactFailedEvent 按
// 新语义的改写（RR-20261006-55 后续，维护者选 A）。原用例约定：根事件表满且每个根都被引用时，collectHostEvents 报
// ErrRuntimeCapacityExceeded、cursor 不动、事件不被压缩，等引用结束后原地重试——Runtime 停在这个事件上。现在配置保证
// 施法 / 衍生物钉住的根放得下（rootEventReferenceBound）；排程任务（这里是 QueueExternalEvent 排在之后 tick 的外部事件）
// 钉住的根没有配置上界，表仍可能被占满，这时跳过这个事件的被动路由、计 skill.root_event.capacity_dropped.total、写一条
// Error 日志，事件照常前进、被压缩，后面的事件照常派发。
//
// RR-20261006-55 后续二起排队任务有上限（MaxQueuedTasks，计入 rootEventReferenceBound），QueueExternalEvent 满了在入口
// 拒绝，合法配置下走不到兜底分支（TestLegalConfigurationNeverReachesTheRootTableFallback）。兜底分支仍保留作防御：这里
// 绕过入口的准入直接往排程里塞外部事件任务，模拟“排队任务超出上限”这种不该发生的状态。
func TestCollectHostEventsSkipsEventWhenEveryRootIsPinned(t *testing.T) {
	host := NewMemoryHostWithOptions(AuthorityIdentity{}, MemoryHostOptions{CompactEvents: true})
	// 合法配置：RootEventLimit 5 > MaxActiveCasts 1 + MaxOwnedSpawns 1 + MaxStopPendingSpawns 1 + MaxQueuedTasks 1。
	runtime := NewRuntime(host, RuntimeOptions{RootEventLimit: 5, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1, MaxQueuedTasks: 1})
	for root := EventID(1); root <= 5; root++ {
		// 之后 tick 的外部事件钉住根 1～5（queuedTaskRoot），宿主事件把它们记进根事件表。绕过 QueueExternalEvent 的准入。
		if err := runtime.scheduleSystem(50, &externalEventTask{Event: EventContext{EventID: 100 + root, RootEventID: root, Tick: 50}}); err != nil {
			t.Fatal(err)
		}
	}
	host.mutex.Lock()
	for root := EventID(1); root <= 5; root++ {
		host.appendContextEventLocked("pinned_root", 0, 0, EventContext{EventID: root, RootEventID: root})
	}
	host.appendContextEventLocked("capacity_probe", 0, 0, EventContext{EventID: 6, RootEventID: 6})
	host.appendContextEventLocked("after_probe", 0, 0, EventContext{EventID: 7, RootEventID: 1})
	host.mutex.Unlock()
	dropped := counterValue(MetricRootEventCapacityDropped)

	runtime.collectHostEvents()
	if runtime.eventCursor != 7 {
		t.Fatalf("event cursor = %d, want 7 (past the skipped event and the one after it); a full root table must not stall the event stream", runtime.eventCursor)
	}
	if events := host.Events(0); len(events) != 0 {
		t.Fatalf("dispatched events were not compacted: %+v", events)
	}
	if got := counterValue(MetricRootEventCapacityDropped) - dropped; got != 1 {
		t.Fatalf("%s grew by %d, want 1", MetricRootEventCapacityDropped, got)
	}
	if _, tracked := runtime.rootEventCounts[6]; tracked {
		t.Fatal("the skipped event's root was tracked")
	}
	if got := runtime.rootEventCounts[1]; got != 2 {
		t.Fatalf("root 1 count = %d, want 2 (the event after the skipped one was dispatched)", got)
	}
}
