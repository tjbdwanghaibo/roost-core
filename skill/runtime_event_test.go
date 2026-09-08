package skill

import (
	"errors"
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

func TestCollectHostEventsDoesNotAdvanceOrCompactFailedEvent(t *testing.T) {
	host := NewMemoryHostWithOptions(AuthorityIdentity{}, MemoryHostOptions{CompactEvents: true})
	runtime := NewRuntime(host, RuntimeOptions{RootEventLimit: 1})
	runtime.rootEventCounts[1] = 1
	runtime.rootEventOrder = append(runtime.rootEventOrder, 1)
	runtime.casts[1] = &castInstance{id: 1, status: CastRunning, eventContext: EventContext{RootEventID: 1}}

	host.mutex.Lock()
	host.appendContextEventLocked("capacity_probe", 0, 0, EventContext{EventID: 2, RootEventID: 2})
	host.mutex.Unlock()

	if err := runtime.collectHostEvents(); !errors.Is(err, ErrRuntimeCapacityExceeded) {
		t.Fatalf("collect error = %v, want capacity error", err)
	}
	if runtime.eventCursor != 0 {
		t.Fatalf("event cursor advanced to %d after failed dispatch", runtime.eventCursor)
	}
	if events := host.Events(0); len(events) != 1 || events[0].Cursor != 1 {
		t.Fatalf("failed event was compacted: %+v", events)
	}

	delete(runtime.casts, 1)
	if err := runtime.collectHostEvents(); err != nil {
		t.Fatalf("retry collect: %v", err)
	}
	if runtime.eventCursor != 1 {
		t.Fatalf("event cursor = %d, want 1", runtime.eventCursor)
	}
	if events := host.Events(0); len(events) != 0 {
		t.Fatalf("successful event was not compacted: %+v", events)
	}
}
