//go:build integration

package rank

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// These tests run the Lua scripts against a real Redis, because the unit-test
// fake reimplements their semantics in Go — so a defect in the script text
// itself is invisible to the unit suite. This is the same gap that let a
// previous implementation ship a test asserting only "a script was called".
//
//	docker run --rm -p 6379:6379 redis:7
//	REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./rank/ -run Integration
func integrationClient(t *testing.T) fredis.IRedis {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the rank Redis integration tests")
	}
	client, err := kitredis.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func integrationStore(t *testing.T) (*RedisStore, Board) {
	t.Helper()
	client := integrationClient(t)
	prefix := fmt.Sprintf("ranktest:%d", time.Now().UnixNano())
	store, err := NewRedisStore(client, RedisConfig{Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	board := Board{ID: "arena", Scope: ScopeServer, ScopeID: 1, Season: 1}
	t.Cleanup(func() { _ = store.Reset(context.Background(), board) })
	return store, board
}

// The property the fake cannot verify: the swap script really removes the old
// ordering member and inserts the new one as one atomic step, so a board never
// accumulates a second member for one owner.
func TestIntegrationSwapScriptLeavesNoOrphanMember(t *testing.T) {
	store, board := integrationStore(t)
	ctx := context.Background()

	for _, value := range []int64{10, 20, 30, 40, 50} {
		if _, err := store.Submit(ctx, board, Score{OwnerID: 1, Value: value}, UpdateSet, ""); err != nil {
			t.Fatal(err)
		}
		size, err := store.Size(ctx, board)
		if err != nil {
			t.Fatal(err)
		}
		if size != 1 {
			t.Fatalf("after submitting %d the board holds %d members: the script left an orphan", value, size)
		}
	}
	entry, found, err := store.Rank(ctx, board, 1)
	if err != nil || !found {
		t.Fatalf("rank: found=%v err=%v", found, err)
	}
	if entry.Score.Value != 50 || entry.Rank != 1 {
		t.Fatalf("entry = %+v", entry)
	}
}

// submitRetryingConflicts is the client the contract describes: Submit gives
// up after maxSubmitAttempts lost compare-and-swaps and answers ErrConflict,
// which the sentinel documents as "retry". Eight writers hammering ONE owner
// on a loaded CI runner do lose eight in a row now and then ("writer 0: rank:
// submit conflict: submit lost 8 compare-and-swaps for owner 1" on the
// v1.5.1 tag run), so the test retries the way a caller would, bounded, and
// still fails on anything that is not a conflict.
func submitRetryingConflicts(ctx context.Context, store *RedisStore, board Board, requestID string) error {
	var err error
	for round := 0; round < 20; round++ {
		if _, err = store.Submit(ctx, board, Score{OwnerID: 1, Value: 1}, UpdateAdd, requestID); !errors.Is(err, ErrConflict) {
			return err
		}
		time.Sleep(time.Duration(round+1) * time.Millisecond)
	}
	return fmt.Errorf("%d client retries exhausted: %w", 20, err)
}

// Real concurrency against real Redis: the compare-and-swap must serialize
// accumulations, and the request ring must deduplicate replays.
func TestIntegrationConcurrentAddsAgainstRealRedis(t *testing.T) {
	store, board := integrationStore(t)
	ctx := context.Background()

	const writers, per = 8, 20
	var wait sync.WaitGroup
	errs := make([]error, writers)
	for writer := 0; writer < writers; writer++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for i := 0; i < per; i++ {
				requestID := fmt.Sprintf("w%d-%d", index, i)
				// Submit each event twice: a redelivery must not double-count.
				for attempt := 0; attempt < 2; attempt++ {
					if err := submitRetryingConflicts(ctx, store, board, requestID); err != nil {
						errs[index] = err
						return
					}
				}
			}
		}(writer)
	}
	wait.Wait()
	for writer, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", writer, err)
		}
	}
	entry, found, err := store.Rank(ctx, board, 1)
	if err != nil || !found {
		t.Fatalf("rank: found=%v err=%v", found, err)
	}
	// Note the bound: the request ring holds maxAppliedRequests ids, and eight
	// writers interleave far more than that, so a redelivery whose id has
	// aged out of the ring does re-apply. The assertion is therefore a range,
	// and it is documented rather than hidden: exact-once across an unbounded
	// window needs a ledger, not a ring.
	if entry.Score.Value < writers*per {
		t.Fatalf("value = %d, want at least %d: accumulations were lost", entry.Score.Value, writers*per)
	}
	if entry.Score.Value > writers*per*2 {
		t.Fatalf("value = %d, want at most %d: deduplication did nothing", entry.Score.Value, writers*per*2)
	}
	if size, _ := store.Size(ctx, board); size != 1 {
		t.Fatalf("board holds %d members for one owner", size)
	}
}

// Ordering under a real ZREVRANGE must match the encoding's intent, including
// negative values and the tiebreak direction.
func TestIntegrationOrderingMatchesTheEncoding(t *testing.T) {
	store, board := integrationStore(t)
	ctx := context.Background()

	for _, score := range []Score{
		{OwnerID: 1, Value: 100, Tie: 50},
		{OwnerID: 2, Value: 100, Tie: 10},
		{OwnerID: 3, Value: 200, Tie: 99},
		{OwnerID: 4, Value: -5, Tie: 1},
		{OwnerID: 5, Value: 0, Tie: 1},
	} {
		if _, err := store.Submit(ctx, board, score, UpdateSet, ""); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Page(ctx, board, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{3, 2, 1, 5, 4}
	if len(page.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(page.Entries), len(want))
	}
	for index, entry := range page.Entries {
		if entry.Score.OwnerID != want[index] {
			t.Fatalf("position %d: owner %d, want %d (full: %+v)", index, entry.Score.OwnerID, want[index], page.Entries)
		}
		if entry.Rank != int64(index+1) {
			t.Fatalf("position %d: rank %d", index, entry.Rank)
		}
	}
}
