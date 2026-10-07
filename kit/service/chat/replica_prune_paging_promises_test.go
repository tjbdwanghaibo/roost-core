package chat

// N06 S2 复核（2026-10-05）：两个时钟相差两小时的副本并发写同一频道、一个副本同时按
// 年龄 Prune，之后客户端按 AfterSeq 翻页。验证单键 CAS 下序号唯一且单调、保留 /
// 淘汰计数守恒、年龄清理留下的页内洞与尾部洞都报 Gap、连续的页不报 Gap，返回的
// 消息体与存储隔离。RR-20260929-27 / RR-20261001-08 的 Gap 语义在真实交错下的组合控制。
//
// 默认 Memory；与同包 RR-27/RR-08 用例同一个门（ROOST_BUGFIX5_BACKEND=redis 且
// ROOST_REVIEW_REDIS 指向隔离 Redis）时状态在真实 Redis 上，
// 键前缀 revn06:chat:<纳秒>（测试结束删除）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

func revn06ChatState(t *testing.T) StateStore {
	t.Helper()
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if os.Getenv("ROOST_BUGFIX5_BACKEND") != "redis" || addr == "" {
		return versionstore.NewMemoryStore[string, channelState]()
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("revn06:chat:%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = client.Close()
		c := goredis.NewClient(&goredis.Options{Addr: addr})
		defer c.Close()
		ctx := context.Background()
		keys, err := c.Keys(ctx, prefix+"*").Result()
		if err != nil {
			t.Errorf("cleanup scan: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := c.Del(ctx, keys...).Err(); err != nil {
				t.Errorf("cleanup delete: %v", err)
			}
		}
	})
	state, err := NewRedisStateStore(client, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSkewedReplicasPruneAndPageWithHonestGaps(t *testing.T) {
	const (
		perWriter = 10
		writers   = 3 // per replica
		total     = 2 * writers * perWriter
	)
	state := revn06ChatState(t)
	registry := testRegistry(t)
	rule := ChannelRule{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: total}
	fresh := newClock()
	skewed := &clock{now: fresh.Now().Add(-2 * time.Hour)}
	newReplica := func(c *clock) Store {
		store, err := NewStore(state, Config{Policy: allowAllPolicy{}, Bodies: registry, Rules: []ChannelRule{rule},
			RetentionAge: time.Hour, Now: c.Now, SystemNow: c.Now})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	freshStore, skewedStore := newReplica(fresh), newReplica(skewed)
	ctx := context.Background()
	ref, err := freshStore.Resolve(world(), 0)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, total+1)
	stop := make(chan struct{})
	for w := 0; w < writers; w++ {
		for replica, store := range map[int64]Store{1: freshStore, 2: skewedStore} {
			wg.Add(1)
			go func(roleID int64, store Store) {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					req := text(fmt.Sprintf("r%d-%d", roleID, i), fmt.Sprintf("k-%d-%d", roleID, i), world())
					if _, err := store.Append(ctx, role(roleID), req); err != nil {
						errs <- fmt.Errorf("append %s: %w", req.RequestID, err)
						return
					}
				}
			}(replica*100+int64(w), store)
		}
	}
	pruned := 0
	var pruneWG sync.WaitGroup
	pruneWG.Add(1)
	go func() {
		defer pruneWG.Done()
		for {
			n, err := freshStore.Prune(ctx, ref, MaxRetain)
			if err != nil && !isRetryableConflict(err) {
				errs <- fmt.Errorf("prune: %w", err)
				return
			}
			pruned += n
			// A retention loop runs on a period; a tight loop here would only
			// measure this test's own contention.
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	wg.Wait()
	close(stop)
	pruneWG.Wait()
	n, err := freshStore.Prune(ctx, ref, MaxRetain)
	if err != nil {
		t.Fatal(err)
	}
	pruned += n
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	stats, err := freshStore.Stats(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LatestSeq != total || stats.Retained != total/2 || stats.Evicted != total/2 || pruned != total/2 {
		t.Fatalf("conservation broken: %+v pruned=%d, want latest %d retained/evicted/pruned %d", stats, pruned, total, total/2)
	}

	// Walk forward from before the first sequence, seven at a time.
	seen := map[uint64]bool{}
	cursor := uint64(0)
	first := true
	for {
		query := HistoryQuery{Channel: world(), AfterSeq: cursor, Limit: 7}
		if first {
			// AfterSeq 0 means "latest page"; start the walk with a cursor
			// below every issued sequence by scrolling back to the oldest.
			query = HistoryQuery{Channel: world(), BeforeSeq: total + 1, Limit: MaxPageSize}
		}
		page, err := freshStore.History(ctx, role(9), query)
		if err != nil {
			t.Fatal(err)
		}
		if first {
			first = false
			if len(page.Messages) != total/2 {
				t.Fatalf("full scrollback returned %d messages, want %d", len(page.Messages), total/2)
			}
			cursor = 1 // seq 1 may or may not be retained; it is a valid cursor either way
			if page.Messages[0].Seq == 1 {
				seen[1] = true
			}
			continue
		}
		expectGap := false
		prev := cursor
		for _, m := range page.Messages {
			if m.From.RoleID < 100 || m.From.RoleID >= 200 {
				t.Fatalf("a skewed replica's expired message %d (role %d) survived the prune", m.Seq, m.From.RoleID)
			}
			if m.Seq <= prev || seen[m.Seq] {
				t.Fatalf("sequence %d out of order or repeated after %d", m.Seq, prev)
			}
			if m.Seq-prev > 1 {
				expectGap = true
			}
			seen[m.Seq] = true
			prev = m.Seq
		}
		if !page.HasMore && prev < page.LatestSeq {
			expectGap = true
		}
		if page.Gap != expectGap {
			t.Fatalf("after %d: Gap=%v, want %v (page %v, has_more %v, latest %d)", cursor, page.Gap, expectGap, seqs(page), page.HasMore, page.LatestSeq)
		}
		// Isolation: editing a returned body must not edit the stored one.
		if len(page.Messages) > 0 {
			page.Messages[0].Body[0] ^= 0xff
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != total/2 {
		t.Fatalf("the forward walk saw %d messages, want %d", len(seen), total/2)
	}
	again, err := freshStore.History(ctx, role(9), HistoryQuery{Channel: world(), BeforeSeq: total + 1, Limit: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range again.Messages {
		if want := fmt.Sprintf("r%d-", m.From.RoleID); len(m.Body) < len(want) || string(m.Body[:len(want)]) != want {
			t.Fatalf("a caller's edit reached stored message %d: %q", m.Seq, m.Body)
		}
	}
	if !sort.SliceIsSorted(again.Messages, func(i, j int) bool { return again.Messages[i].Seq < again.Messages[j].Seq }) {
		t.Fatalf("scrollback is not in sequence order: %v", seqs(again))
	}
}

func isRetryableConflict(err error) bool {
	return err != nil && (errors.Is(err, versionstore.ErrConflict) || errors.Is(err, versionstore.ErrVersionMismatch))
}

func seqs(page Page) []uint64 {
	out := make([]uint64, 0, len(page.Messages))
	for _, m := range page.Messages {
		out = append(out, m.Seq)
	}
	return out
}
