//go:build integration

package mail

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// These tests run the batch read and the key ttl against a real Redis,
// because the unit-test double reimplements MGet's semantics in Go: it
// establishes that this package handles a positional reply correctly, not that
// Redis and the driver produce one.
//
// The gap used to be larger. The batch read was a one-line Lua script — purely
// because MGet was missing from the client interface — and a Go double cannot
// evaluate a script at all, so a defect in the script text was invisible to
// every unit test. Adding MGet to the interface removed that blind spot
// instead of testing around it.
//
//	docker run --rm -p 6379:6379 redis:7
//	REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./mail/ -run Integration
func integrationClient(t *testing.T) fredis.IRedis {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the mail Redis integration tests")
	}
	client, err := kitredis.NewClient(fredis.DefaultConfig(addr))
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
	stores, err := NewRedisStores(client, RedisConfig{
		Prefix: prefix, SendTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
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

// An empty batch must not reach Redis at all: `MGET` with no keys is an error
// there, so a store that passed an empty list through would fail on a
// perfectly ordinary empty page. MGet itself guarantees this, and it is
// asserted here too because the failure would surface as a broken mailbox
// page rather than as a client error.
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
	if _, err := service.CancelClaim(ctx, 7, sent.ID, claim.Token); err != nil {
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
