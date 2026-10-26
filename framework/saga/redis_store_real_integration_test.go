//go:build integration

package saga

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
)

// TestRedisKill9KeepsAcknowledgedApplies：并发 Apply 进行中对私有 Redis 发 SIGKILL，重启（重放 AOF）后：
//   - 每条回复成功（脚本回复 + WAITAOF local=1）的 Apply 都在：记录版本不低于它确认过的最高版本；
//   - 没有半个脚本：记录的 v 与 doc 一致、ls 指向的日志条目带着同一个 v 与 doc、领取索引的 score 与记录一致；
//   - 重启后脚本缓存清空，同一个 RedisStore 的下一次写经 NOSCRIPT 重发后照常成功。
//
// 只在 scripts 起的私有 redis-server 上跑（StartAOF），不碰共享实例。
func TestRedisKill9KeepsAcknowledgedApplies(t *testing.T) {
	ctx := context.Background()
	server := redistest.StartAOF(t)
	env := newRedisTestEnvAt(t, server.Addr, "kill9", mongotest.NewClient())
	store := env.open(t)
	const records, writers = 64, 16
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := range records {
		id := fmt.Sprintf("k9-%03d", i)
		if err := store.Create(ctx, Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	acknowledged := make(map[string]uint64, records)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var applied, failed int
	for w := range writers {
		wg.Go(func() {
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("k9-%03d", (w*7+n)%records)
				current, err := store.Get(ctx, id)
				if err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					time.Sleep(5 * time.Millisecond)
					continue
				}
				after := current.Clone()
				after.Version++
				after.LastError = "writer " + strconv.Itoa(w) + " step " + strconv.Itoa(n)
				after.UpdatedAt = time.Now().UTC()
				outcome, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: current.Version, After: after})
				mu.Lock()
				if err == nil && outcome == ApplyApplied {
					applied++
					if after.Version > acknowledged[id] {
						acknowledged[id] = after.Version
					}
				} else if err != nil && !errors.Is(err, ErrConflict) {
					failed++
				}
				mu.Unlock()
			}
		})
	}
	time.Sleep(400 * time.Millisecond)
	server.Kill9()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	server.Restart()
	// 连接池在连续拨号失败后由后台探测恢复（驱动 README §2），等它重新连上。
	for deadline := time.Now().Add(10 * time.Second); store.redis.Ping(ctx) != nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the store's connection pool did not reconnect after the restart")
		}
	}
	t.Logf("acknowledged applies before the kill: %d, failed or unknown calls: %d", applied, failed)
	if applied == 0 {
		t.Fatal("no apply was acknowledged before the kill; the load did not run")
	}
	raw := env.raw
	for i := range records {
		id := fmt.Sprintf("k9-%03d", i)
		fields, err := raw.HGetAll(ctx, store.keys.record(id)).Result()
		if err != nil {
			t.Fatal(err)
		}
		record, found, err := store.resident(ctx, id)
		if err != nil || !found {
			t.Fatalf("%s after restart: found=%v err=%v", id, found, err)
		}
		if record.Version < acknowledged[id] {
			t.Fatalf("%s lost an acknowledged apply: version %d after restart, %d was acknowledged", id, record.Version, acknowledged[id])
		}
		entries, err := raw.XRange(ctx, store.keys.log(store.keys.partition(id)), fields["ls"], fields["ls"]).Result()
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s: the log entry %s named by ls is missing (%v): a script was half applied", id, fields["ls"], err)
		}
		if entries[0].Values["v"] != fields["v"] || entries[0].Values["doc"] != fields["doc"] || entries[0].Values["id"] != id {
			t.Fatalf("%s: record v=%s and its last log entry v=%v differ: a script was half applied", id, fields["v"], entries[0].Values["v"])
		}
		score, err := raw.ZScore(ctx, store.keys.due(), id).Result()
		if err != nil || int64(score) != due(record) {
			t.Fatalf("%s: due score %v (%v), want %d from the record: a script was half applied", id, score, err, due(record))
		}
	}
	// 重启后脚本缓存是空的：同一个存储对象的下一次写走 NOSCRIPT → EVAL 重发。
	current, err := store.Get(ctx, "k9-000")
	if err != nil {
		t.Fatal(err)
	}
	after := current.Clone()
	after.Version++
	if outcome, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: current.Version, After: after}); err != nil || outcome != ApplyApplied {
		t.Fatalf("apply after the restart (script cache empty) = %v, %v", outcome, err)
	}
}

// due 是记录在领取索引里应有的 score：max(next_run, lease_until)。
func due(r Record) int64 {
	return max(millis(r.NextRunAt), millis(r.Lease.Until))
}
