package skill

import (
	"errors"
	"testing"
)

func TestMemoryHostRevisionAndEventCursorContract(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{}, Resources: map[string]int64{"mana": 5}})
	current := host.CurrentRevision()
	if _, err := host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: current + 1}, Payload: ResourceRead{Entity: 1, Resource: "mana"}}); !errors.Is(err, ErrRevisionUnavailable) {
		t.Fatalf("future read = %v", err)
	}
	if _, err := host.Select(SelectRequest{Meta: QueryMeta{RequiredRevision: current + 1}, Shape: CircleSelectShape{Radius: 10}, Limit: 1}); !errors.Is(err, ErrRevisionUnavailable) {
		t.Fatalf("future select = %v", err)
	}

	failedRevision := host.CurrentRevision()
	failed, err := host.Apply(EffectCommand{Payload: TeleportCommand{Target: 999, Destination: Position{X: 1}}})
	payload, ok := failed.Payload.(TeleportEffectResult)
	if err != nil || !ok || payload.Succeeded || payload.FailureReason != ExpectedFailureInvalidTarget {
		t.Fatalf("missing target = %#v, %v", failed, err)
	}
	if host.CurrentRevision() != failedRevision {
		t.Fatal("expected failure advanced revision")
	}

	result, err := host.Apply(EffectCommand{Payload: TeleportCommand{Target: 1, Destination: Position{X: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	events := host.Events(0)
	last := events[len(events)-1]
	if result.Commit.Revision != last.Revision || result.Commit.Revision != host.CurrentRevision() {
		t.Fatalf("commit/event/current revisions differ: %#v %#v %d", result.Commit, last, host.CurrentRevision())
	}
	read, err := host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: result.Commit.Revision}, Payload: PositionRead{Entity: 1}})
	if err != nil || read.Meta.Revision != result.Commit.Revision {
		t.Fatalf("read did not use required committed snapshot: %#v %v", read, err)
	}
	if got := host.Events(last.Cursor); len(got) != 0 {
		t.Fatalf("Events(after) returned stale events: %#v", got)
	}
	if len(events) > 1 {
		after := host.Events(events[len(events)-2].Cursor)
		if len(after) != 1 || after[0].Cursor != last.Cursor {
			t.Fatalf("event cursor is not strict: %#v", after)
		}
	}
}
