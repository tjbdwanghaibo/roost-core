//go:build integration

package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
)

// These tests run bounded pipeline reads and key TTL against a real Redis.
// The unit double evaluates commands in Go and cannot prove driver semantics.
//
//	docker run --rm -p 6379:6379 redis:7
//	REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./service/mail/ -run Integration
//
// REDIS_ADDR 接受单机地址或逗号分隔的 Cluster 种子，与 redistest.Cleanup 使用同一口径。
func integrationClient(t testing.TB) fredis.IRedis {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the mail Redis integration tests")
	}
	cfg := fredis.DefaultConfig(addr)
	if strings.Contains(addr, ",") {
		cfg.ClusterAddrs = strings.Split(addr, ",")
		for i := range cfg.ClusterAddrs {
			cfg.ClusterAddrs[i] = strings.TrimSpace(cfg.ClusterAddrs[i])
		}
	}
	client, err := kitredis.NewClient(cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// integrationStores builds the real stores under a per-run prefix, so
// concurrent runs and leftovers from a failed run cannot affect each other.
func integrationStores(t *testing.T) (RedisStores, string) {
	t.Helper()
	client := integrationClient(t)
	prefix := fmt.Sprintf("mailtest:%d", time.Now().UnixNano())
	// OPEN-ITEMS B45：之前每跑一次在目标 Redis 留下约 30 个键（信封、收件箱、发送记录）。
	redistest.Cleanup(t, os.Getenv("REDIS_ADDR"), prefix)
	stores, err := NewRedisStores(client, RedisConfig{Prefix: prefix, SendTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// 存储宽限缩到最小：TestIntegrationAnEnvelopeExpiresOnItsOwnDeadline 要看到键在业务过期后立刻被
	// Redis 回收（EnvelopeStorageGrace 固定 24h，只有测试改它）。
	stores.Envelopes.(*redisEnvelopes).grace = time.Millisecond
	return stores, prefix
}

func integrationService(t *testing.T) *Service {
	t.Helper()
	stores, _ := integrationStores(t)
	service, err := New(Config{
		Envelopes: stores.Envelopes, Mailboxes: stores.Mailboxes, Sends: stores.Sends,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// The batch read against a real Redis, with a mix of present and absent keys,
// because a reply whose element count does not match the key count is how a
// page silently comes back short.
func TestIntegrationBatchReadReturnsOneValuePerKey(t *testing.T) {
	stores, _ := integrationStores(t)
	ctx := context.Background()
	now := time.Now()

	present := []string{}
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("m%d", i)
		envelope := Envelope{
			ID: id, Audience: AudienceDirect, Recipients: []int64{1},
			Subject: "reward", SendRequestID: "send-" + id,
			CreatedAtUnix: now.Unix(), ExpiresAtUnix: now.Add(time.Hour).Unix(),
		}
		created, err := stores.Envelopes.Create(ctx, envelope)
		if err != nil || !created {
			t.Fatalf("create %s: created=%v err=%v", id, created, err)
		}
		present = append(present, id)
	}
	// Interleave ids that were never written, so an off-by-one in the reply
	// mapping shows up as a value attributed to the wrong id rather than as a
	// count mismatch.
	requested := []string{}
	for i, id := range present {
		requested = append(requested, id, fmt.Sprintf("absent%d", i))
	}

	got, err := stores.Envelopes.GetMany(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(present) {
		t.Fatalf("the batch read returned %d envelopes for %d present ids", len(got), len(present))
	}
	for _, id := range present {
		envelope, ok := got[id]
		if !ok {
			t.Fatalf("the batch read lost envelope %s", id)
		}
		if envelope.ID != id {
			t.Fatalf("id %s came back holding envelope %s; the reply was mapped to the wrong key",
				id, envelope.ID)
		}
	}
	for i := range present {
		if _, ok := got[fmt.Sprintf("absent%d", i)]; ok {
			t.Fatalf("the batch read invented an envelope for absent%d", i)
		}
	}
}

// An empty envelope page returns an empty result without backend work.
func TestIntegrationAnEmptyBatchIsNotSentToRedis(t *testing.T) {
	stores, _ := integrationStores(t)
	got, err := stores.Envelopes.GetMany(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty batch produced %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty batch returned %d envelopes", len(got))
	}
}

// Create is insert-only against a real Redis, under real concurrency. This is
// the property a retried send depends on.
func TestIntegrationConcurrentCreatesOfOneIDProduceOneEnvelope(t *testing.T) {
	stores, _ := integrationStores(t)
	ctx := context.Background()
	now := time.Now()

	const racers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			envelope := Envelope{
				ID: "same-id", Audience: AudienceDirect, Recipients: []int64{1},
				Subject: fmt.Sprintf("racer %d", i), SendRequestID: "send-1",
				CreatedAtUnix: now.Unix(), ExpiresAtUnix: now.Add(time.Hour).Unix(),
			}
			ok, err := stores.Envelopes.Create(ctx, envelope)
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			if ok {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if created != 1 {
		t.Fatalf("%d of %d concurrent creates succeeded, want 1", created, racers)
	}
}

// The key ttl really expires the envelope in Redis, so an expired mail stops
// occupying storage without a sweep. The implementation this replaces stored
// timestamps as int64 milliseconds rather than dates, so it could not have a
// ttl index at all and its collection grew without bound.
func TestIntegrationAnEnvelopeExpiresOnItsOwnDeadline(t *testing.T) {
	stores, _ := integrationStores(t)
	ctx := context.Background()
	now := time.Now()

	envelope := Envelope{
		ID: "short-lived", Audience: AudienceDirect, Recipients: []int64{1},
		Subject: "reward", SendRequestID: "send-1",
		CreatedAtUnix: now.Unix(), ExpiresAtUnix: now.Add(time.Second).Unix(),
	}
	if created, err := stores.Envelopes.Create(ctx, envelope); err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if _, found, err := stores.Envelopes.Get(ctx, "short-lived"); err != nil || !found {
		t.Fatalf("the envelope is not readable straight after the write: found=%v err=%v", found, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, found, err := stores.Envelopes.Get(ctx, "short-lived")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the envelope outlived its deadline; the key ttl is not being applied")
}

// The whole service runs end to end on real Redis: send, list, claim, commit.
// The unit suite covers the logic; this covers that the wiring is real.
func TestIntegrationTheServiceRunsEndToEndOnRedis(t *testing.T) {
	service := integrationService(t)
	ctx := context.Background()

	sent, err := service.Send(ctx, SendRequest{
		Audience: AudienceDirect, Recipients: []int64{7},
		Subject: "reward", Attachment: []byte("100 gold"),
		ExpiresInSeconds: 3600, RequestID: "send-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A retried send must return the same envelope, through the real ledger.
	again, err := service.Send(ctx, SendRequest{
		Audience: AudienceDirect, Recipients: []int64{7},
		Subject: "reward", Attachment: []byte("100 gold"),
		ExpiresInSeconds: 3600, RequestID: "send-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != sent.ID {
		t.Fatalf("a retried send produced envelope %s, first was %s", again.ID, sent.ID)
	}

	page, err := service.List(ctx, 7, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the page holds %d items, want 1", len(page.Items))
	}
	if page.Unread != 1 {
		t.Fatalf("the unread count is %d, want 1", page.Unread)
	}

	claim, err := service.ReserveClaim(ctx, 7, sent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(claim.Attachment) != "100 gold" {
		t.Fatalf("the claim carried %q", claim.Attachment)
	}
	// The token is constant across reservations, which is the property the
	// whole claim design rests on — asserted here against real storage.
	if _, err := service.CancelClaim(ctx, 7, sent.ID, claim.Token, claim.Attempts); err != nil {
		t.Fatal(err)
	}
	second, err := service.ReserveClaim(ctx, 7, sent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Token != claim.Token {
		t.Fatalf("a re-reservation got token %q, the first had %q", second.Token, claim.Token)
	}
	if _, err := service.CommitClaim(ctx, 7, sent.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	if summary, err := service.Summary(ctx, 7); err != nil {
		t.Fatal(err)
	} else if summary.Unread != 0 {
		t.Fatalf("the unread count is %d after the claim, want 0", summary.Unread)
	}
}

// D17：LazyBroadcast 的 Redis 账本。两个 Service 共用同一前缀，模拟两个 mail 进程：各自有进程内账本
// 缓存，一边发布 / 撤回，另一边靠版本提示察觉变化；并发拉取同一玩家只合并一次。
func TestIntegrationLazyBroadcastOverRedis(t *testing.T) {
	client := integrationClient(t)
	prefix := fmt.Sprintf("mailtest:%d", time.Now().UnixNano())
	redistest.Cleanup(t, os.Getenv("REDIS_ADDR"), prefix)
	newProcess := func() *Service {
		stores, err := NewRedisStores(client, RedisConfig{Prefix: prefix, SendTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		service, err := New(Config{
			Envelopes: stores.Envelopes, Mailboxes: stores.Mailboxes, Sends: stores.Sends,
			Broadcast: NewLazyBroadcast(LazyBroadcastConfig{Index: stores.Broadcasts}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	sender, reader := newProcess(), newProcess()
	ctx := context.Background()
	registered := time.Now().Add(-time.Hour).Unix()
	profile := BroadcastProfile{Scopes: []string{ServerScope(1)}, RegisteredAtUnix: registered}

	// 读者先拉一次空账本，让它的缓存停在“没有邮件”。
	if _, err := reader.PullBroadcasts(ctx, 1, profile); err != nil {
		t.Fatal(err)
	}
	send := func(scope, requestID string) Envelope {
		envelope, err := sender.Send(ctx, SendRequest{
			Audience: AudienceBroadcast, Scope: scope, Subject: "compensation",
			Attachment: []byte("gold:1"), ExpiresInSeconds: 3600, RequestID: requestID,
		})
		if err != nil {
			t.Fatalf("send %s: %v", requestID, err)
		}
		return envelope
	}
	// 发送时间要严格晚于注册时间（内置条件按秒比较）。
	first := send(ServerScope(1), "it-bcast-1")
	other := send(ServerScope(2), "it-bcast-2")

	page, err := reader.List(ctx, 1, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Envelope.ID != first.ID {
		t.Fatalf("the other process listed %+v after a publish, want only %s (its cache must follow the version hint)", page.Items, first.ID)
	}

	// 并发拉取：两个进程、多个 goroutine 同时给一个新玩家拉取，只合并一次。
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(service *Service) {
			defer wg.Done()
			if _, err := service.PullBroadcasts(ctx, 2, profile); err != nil {
				errs <- err
			}
		}([]*Service{sender, reader}[i%2])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent pull: %v", err)
	}
	box, _, err := reader.Mailbox(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(box.Entries) != 1 || box.Unread != 1 || box.BroadcastWatermark != 2 {
		t.Fatalf("after concurrent pulls the mailbox has %d entries, unread %d, watermark %d; want 1, 1, 2",
			len(box.Entries), box.Unread, box.BroadcastWatermark)
	}

	// 附件只领一次（领取不需要再给 scope）。
	claim, err := reader.ReserveClaim(ctx, 2, first.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.CommitClaim(ctx, 2, first.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReserveClaim(ctx, 2, first.ID, ""); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second reserve = %v, want ErrAlreadyClaimed", err)
	}

	// 一个进程撤回，另一个进程立刻按新账本判断。
	third := send(ServerScope(1), "it-bcast-3")
	if revoked, err := sender.RevokeBroadcast(ctx, third.ID); err != nil || !revoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if _, err := reader.PullBroadcasts(ctx, 3, profile); err != nil {
		t.Fatal(err)
	}
	page, err = reader.List(ctx, 3, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Envelope.ID != first.ID {
		t.Fatalf("after a revoke in another process, a new player listed %+v; want only %s", page.Items, first.ID)
	}
	_ = other

	// 键空间：账本与版本提示都在 <prefix>:bcast: 下。
	for _, key := range []string{prefix + ":bcast:ledger", prefix + ":bcast:version"} {
		if n, err := client.Exists(ctx, key); err != nil || n != 1 {
			t.Fatalf("key %s exists = %d, %v", key, n, err)
		}
	}
}

// 全服邮件账本在上界附近的成本（KNOWN-LIMITS“全服邮件账本规模没有压测”）：账本是一个 Redis 键里的整份 JSON，
// 每次发布 / 撤回整体读写。基准把账本维持在上界 DefaultMaxActiveBroadcasts 条附近：
//
//   - publish：每次发布一封、同时让最早的一封按业务时间过期被回收（Publish 的 nowUnix 逐次前进一秒），账本条数不变；
//   - revoke：撤回一封未撤回的（每封只撤一次，撤完换新一批）；
//   - snapshot-hinted：读路径的常见情况——版本键没变、进程内缓存命中，每次只读一次版本键（十万级在线登录时的读压力）；
//   - snapshot-reload：版本变了之后第一次读（每个 mail 进程各一次），整份账本读出并解码。
//
// 运行（私有 Redis，键前缀按运行区分、用后清理）：
//
//	REDIS_ADDR=127.0.0.1:<port> go test -tags integration ./service/mail -run '^$' -bench BroadcastLedgerAtCapacity -benchtime 2000x -count 3
//
// Redis Cluster：REDIS_ADDR 写种子地址列表时同样适用（账本与版本键各在一个节点上）。
func BenchmarkBroadcastLedgerAtCapacity(b *testing.B) {
	ctx := context.Background()
	client := integrationClient(b)
	newIndex := func(b *testing.B) (BroadcastIndex, int64) {
		prefix := fmt.Sprintf("mailbench:%d", time.Now().UnixNano())
		redistest.Cleanup(b, os.Getenv("REDIS_ADDR"), prefix)
		index, err := NewRedisBroadcastIndex(client, prefix, 0, 0)
		if err != nil {
			b.Fatal(err)
		}
		// 预先放满：第 k 封在 base+k+1 过期，publish 子项每前进一秒回收一封。
		base := int64(1_900_000_000)
		for k := range DefaultMaxActiveBroadcasts {
			if _, err := index.Publish(ctx, benchBroadcast(k, base, base+int64(k)+1), base, DefaultMaxActiveBroadcasts); err != nil {
				b.Fatal(err)
			}
		}
		return index, base
	}
	b.Run("publish", func(b *testing.B) {
		index, base := newIndex(b)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			now := base + int64(i) + 1 // 第 i 封此刻过期、被回收
			mail := DefaultMaxActiveBroadcasts + i
			if _, err := index.Publish(ctx, benchBroadcast(mail, now, now+int64(DefaultMaxActiveBroadcasts)+1), now, DefaultMaxActiveBroadcasts); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
	b.Run("revoke", func(b *testing.B) {
		index, base := newIndex(b)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			if i == DefaultMaxActiveBroadcasts {
				b.StopTimer()
				index, base = newIndex(b)
				i = 0
				b.StartTimer()
			}
			if _, changed, err := index.Revoke(ctx, benchBroadcastID(i), base); err != nil || !changed {
				b.Fatalf("revoke %d: changed=%v err=%v", i, changed, err)
			}
			i++
		}
	})
	b.Run("snapshot-hinted", func(b *testing.B) {
		index, _ := newIndex(b)
		if ledger, err := index.Snapshot(ctx); err != nil || len(ledger.Mails) != DefaultMaxActiveBroadcasts {
			b.Fatalf("warm snapshot: %d mails, %v", len(ledger.Mails), err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := index.Snapshot(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("snapshot-reload", func(b *testing.B) {
		index, base := newIndex(b)
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			b.StopTimer()
			if i == DefaultMaxActiveBroadcasts {
				index, base = newIndex(b)
				i = 0
			}
			// 撤回一封让账本换版本（版本键随之前进），下一次 Snapshot 的进程内缓存失效、整份读出。
			if _, changed, err := index.Revoke(ctx, benchBroadcastID(i), base); err != nil || !changed {
				b.Fatalf("revoke %d: changed=%v err=%v", i, changed, err)
			}
			i++
			b.StartTimer()
			if ledger, err := index.Snapshot(ctx); err != nil || len(ledger.Mails) != DefaultMaxActiveBroadcasts {
				b.Fatalf("snapshot after a change: %d mails, %v", len(ledger.Mails), err)
			}
		}
	})
}

func benchBroadcastID(k int) string { return fmt.Sprintf("bench-broadcast-%06d", k) }

// benchBroadcast 是一封约 150 字节账本记录的广播（账本只记 ID、范围与时间）。
func benchBroadcast(k int, createdAt, expiresAt int64) Envelope {
	return Envelope{ID: benchBroadcastID(k), Audience: AudienceBroadcast, Scope: ServerScope(int32(1 + k%4)),
		Subject: "maintenance compensation", CreatedAtUnix: createdAt, ExpiresAtUnix: expiresAt}
}
