package match

import (
	"context"
	"errors"
	"testing"
)

// U-0221 · C2 · RR-20260917-01：两个都通过 Validate 的 Queue 不得映射到同一个存储键。
// 旧 Key() 用冒号拼 Mode:GroupSize:Partition，而 Mode / Partition 不限制冒号，
// {a:2, 3, x} 与 {a, 2, 3:x} 都是 "a:2:3:x"——两个池共用一个 queueState，Candidates 跨池读票，
// Commit 用二人队列把三人队列的票提交成功。
func TestDistinctQueuesNeverShareAKey(t *testing.T) {
	a := Queue{Mode: "a:2", GroupSize: 3, Partition: "x"}
	b := Queue{Mode: "a", GroupSize: 2, Partition: "3:x"}
	for _, q := range []Queue{a, b} {
		if err := q.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if a.Key() == b.Key() {
		t.Fatalf("two valid queues share key %q", a.Key())
	}
	// Keys without the reserved characters are unchanged, so no deployed key
	// moves (the compatibility promise of this fix).
	if got := (Queue{Mode: "duel", GroupSize: 2, Partition: "eu"}).Key(); got != "duel:2:eu" {
		t.Fatalf("plain key changed to %q", got)
	}
	if got := (Queue{Mode: "duel", GroupSize: 2}).Key(); got != "duel:2:" {
		t.Fatalf("plain key with empty partition changed to %q", got)
	}
	// Injective across the whole reserved set, including the escape character.
	seen := map[string]Queue{}
	for _, q := range []Queue{
		{Mode: "a%3A2", GroupSize: 3, Partition: "x"}, {Mode: "a:2", GroupSize: 3, Partition: "x"},
		{Mode: "a", GroupSize: 3, Partition: "%3Ax"}, {Mode: "a", GroupSize: 3, Partition: ":x"},
		{Mode: "a%", GroupSize: 3, Partition: "x"}, {Mode: "a%25", GroupSize: 3, Partition: "x"},
	} {
		if prev, dup := seen[q.Key()]; dup {
			t.Fatalf("%+v and %+v share key %q", prev, q, q.Key())
		}
		seen[q.Key()] = q
	}
}

func TestACommitCannotTakeTicketsFromAnotherQueue(t *testing.T) {
	a := Queue{Mode: "a:2", GroupSize: 3, Partition: "x"}
	b := Queue{Mode: "a", GroupSize: 2, Partition: "3:x"}
	store, err := NewMemoryStore(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	x, err := store.Enqueue(ctx, a, Subject{Kind: "player", ID: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	y, err := store.Enqueue(ctx, b, Subject{Kind: "player", ID: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := store.Candidates(ctx, b, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != y.ID {
		t.Fatalf("Candidates of queue b returned %d tickets (%v); a ticket from queue a leaked in", len(candidates), candidates)
	}
	if _, err := store.Commit(ctx, b, []string{x.ID, y.ID}); err == nil || !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("Commit on queue b accepted a ticket from queue a: err=%v", err)
	}
}
