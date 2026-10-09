package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// fakeRedisEnvelopes evaluates the operations the envelope store uses, rather
// than accepting them. A double that returned success from SetNX would make
// the insert-only test pass while proving nothing.
//
// It evaluates buffered GETs on Exec; it does not establish driver or Cluster
// routing correctness. Real-backend tests cover that separate contract.
type fakeRedisEnvelopes struct {
	mu     sync.Mutex
	values map[string][]byte
	// batchCalls counts executions, not network round trips across nodes.
	batchCalls int
	execErr    error
	commandErr error
	// lastTTL is the expiration the store asked for, so a test can assert
	// that it came from the injected clock rather than the wall clock.
	lastTTL time.Duration
}

func newFakeRedisEnvelopes() *fakeRedisEnvelopes {
	return &fakeRedisEnvelopes{values: map[string][]byte{}}
}

func (f *fakeRedisEnvelopes) SetNX(_ context.Context, key string, value any, expiration time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastTTL = expiration
	if expiration <= 0 {
		return false, fmt.Errorf("fake redis: SetNX with non-positive expiration %s", expiration)
	}
	if _, exists := f.values[key]; exists {
		return false, nil
	}
	payload, ok := value.([]byte)
	if !ok {
		return false, fmt.Errorf("fake redis: SetNX value is %T, want []byte", value)
	}
	f.values[key] = append([]byte(nil), payload...)
	return true, nil
}

func (f *fakeRedisEnvelopes) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.values[key]
	if !ok {
		return nil, fredis.ErrNil
	}
	return bytes.Clone(stored), nil
}

type fakeEnvelopePipeline struct {
	// Unused commands deliberately have no implementation: accidental use
	// fails instead of pretending that it executed successfully.
	fredis.IPipeline
	client  *fakeRedisEnvelopes
	keys    []string
	futures []*fredis.FutureBytes
}

func (f *fakeRedisEnvelopes) Pipeline() fredis.IPipeline {
	return &fakeEnvelopePipeline{client: f}
}

func (p *fakeEnvelopePipeline) Get(_ context.Context, key string) *fredis.FutureBytes {
	future := &fredis.FutureBytes{}
	p.keys = append(p.keys, key)
	p.futures = append(p.futures, future)
	return future
}

func (p *fakeEnvelopePipeline) Exec(ctx context.Context) error {
	p.client.mu.Lock()
	p.client.batchCalls++
	execErr, commandErr := p.client.execErr, p.client.commandErr
	p.client.mu.Unlock()
	for i, key := range p.keys {
		value, err := p.client.Get(ctx, key)
		if i == len(p.keys)-1 && commandErr != nil {
			err = commandErr
		}
		p.futures[i].SetResult(value, err)
	}
	return execErr
}

func (p *fakeEnvelopePipeline) Discard() {
	p.keys, p.futures = nil, nil
}

func (f *fakeRedisEnvelopes) rounds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batchCalls
}

func (f *fakeRedisEnvelopes) corrupt(prefix, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[prefix+":env:"+id] = []byte("{not json")
}

// testNow is the same fixed instant the service harness uses, so the store
// and the service agree about what time it is. Two components with two clocks
// is a mail that reads as live and has already been evicted.
var testNow = time.Unix(1_700_000_000, 0)

func newRedisEnvelopeStore(t *testing.T) (EnvelopeStore, *fakeRedisEnvelopes) {
	t.Helper()
	fake := newFakeRedisEnvelopes()
	store, err := NewRedisEnvelopes(fake, "mailtest", func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	return store, fake
}

func testEnvelope(id string, expiresIn time.Duration) Envelope {
	now := testNow
	return Envelope{
		ID: id, Audience: AudienceDirect, Recipients: []int64{1},
		Subject: "reward", SendRequestID: "send-" + id,
		CreatedAtUnix: now.Unix(), ExpiresAtUnix: now.Add(expiresIn).Unix(),
	}
}

// Create must not overwrite. That is what makes a retried send safe without a
// transaction, and it is the one property the whole contract rests on.
func TestRedisCreateRefusesAnExistingID(t *testing.T) {
	store, _ := newRedisEnvelopeStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, testEnvelope("m1", time.Hour))
	if err != nil || !created {
		t.Fatalf("first Create: created=%v err=%v", created, err)
	}
	second := testEnvelope("m1", time.Hour)
	second.Subject = "different"
	created, err = store.Create(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("Create overwrote an existing envelope")
	}
	stored, found, err := store.Get(ctx, "m1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if stored.Subject != "reward" {
		t.Fatalf("the stored envelope was replaced: subject is %q", stored.Subject)
	}
}

// An envelope whose expiry has already passed is refused, rather than stored
// with a non-positive key ttl. It would be a mail nobody could ever read.
func TestRedisCreateRefusesAnAlreadyExpiredEnvelope(t *testing.T) {
	store, _ := newRedisEnvelopeStore(t)
	// Valid on its own terms — created before it expires — but both are in
	// the past relative to the store's clock, which is the case a wall-clock
	// read inside Create would get wrong for anyone with an injected clock.
	envelope := testEnvelope("m1", time.Hour)
	envelope.CreatedAtUnix = testNow.Add(-3 * time.Hour).Unix()
	envelope.ExpiresAtUnix = testNow.Add(-2 * time.Hour).Unix()
	if _, err := store.Create(context.Background(), envelope); !errors.Is(err, ErrExpired) {
		t.Fatalf("storing an expired envelope produced %v, want ErrExpired", err)
	}
}

// The batch uses one Exec regardless of how many ids it is given.
// This is the assertion the contract exists for: the implementation being
// replaced issued one read per envelope in an unbounded loop.
func TestRedisGetManyIsOneBoundedBatch(t *testing.T) {
	store, fake := newRedisEnvelopeStore(t)
	ctx := context.Background()
	ids := make([]string, 0, MaxPageSize)
	for i := 0; i < MaxPageSize; i++ {
		id := fmt.Sprintf("m%d", i)
		if _, err := store.Create(ctx, testEnvelope(id, time.Hour)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	got, err := store.GetMany(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("the batch read returned %d envelopes for %d ids", len(got), len(ids))
	}
	if rounds := fake.rounds(); rounds != 1 {
		t.Fatalf("reading %d envelopes took %d executions, want 1", len(ids), rounds)
	}
}

// A missing envelope comes back absent rather than as an error, because the
// caller distinguishes and counts them.
func TestRedisGetManyReportsMissingEnvelopesAsAbsent(t *testing.T) {
	store, _ := newRedisEnvelopeStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, testEnvelope("present", time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetMany(ctx, []string{"present", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the batch read returned %d envelopes, want 1", len(got))
	}
	if _, ok := got["gone"]; ok {
		t.Fatal("the batch read invented an envelope for a missing id")
	}
}

// A batch larger than the page cap is refused. A batch read whose size is
// whatever the caller asked for is the unbounded read this contract prevents.
func TestRedisGetManyRefusesAnOversizedBatch(t *testing.T) {
	store, _ := newRedisEnvelopeStore(t)
	ids := make([]string, MaxPageSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%d", i)
	}
	if _, err := store.GetMany(context.Background(), ids); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("an oversized batch produced %v, want ErrRangeInvalid", err)
	}
}

func TestRedisGetManyPreservesBatchAndCommandErrors(t *testing.T) {
	backendErr := errors.New("backend unavailable")
	for _, batchError := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch_error_%v", batchError), func(t *testing.T) {
			store, fake := newRedisEnvelopeStore(t)
			if _, err := store.Create(context.Background(), testEnvelope("present", time.Hour)); err != nil {
				t.Fatal(err)
			}
			if batchError {
				fake.execErr = backendErr
			} else {
				fake.commandErr = backendErr
			}
			got, err := store.GetMany(context.Background(), []string{"missing", "present"})
			if !errors.Is(err, backendErr) || got != nil {
				t.Fatalf("partial success or lost error: %v %v", got, err)
			}
		})
	}
}

func TestRedisGetManyValidatesBeforeExecuting(t *testing.T) {
	store, fake := newRedisEnvelopeStore(t)
	if got, err := store.GetMany(context.Background(), nil); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	if _, err := store.GetMany(context.Background(), []string{"valid", " "}); !errors.Is(err, ErrMailInvalid) {
		t.Fatal(err)
	}
	if _, err := store.GetMany(context.Background(), make([]string, MaxPageSize+1)); !errors.Is(err, ErrRangeInvalid) {
		t.Fatal(err)
	}
	if fake.rounds() != 0 {
		t.Fatal("invalid or empty batch reached backend")
	}
}

// A malformed stored value is reported, not skipped. A decode failure that
// reads as "absent" is how a page comes back short while a count says
// otherwise.
func TestRedisGetManyReportsAMalformedValue(t *testing.T) {
	store, fake := newRedisEnvelopeStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, testEnvelope("m1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	fake.corrupt("mailtest", "m1")
	if _, err := store.GetMany(ctx, []string{"m1"}); err == nil {
		t.Fatal("a malformed stored envelope was skipped instead of reported")
	}
}

// The whole service runs on the Redis-backed stores, so the wiring is exercised
// rather than only the pieces.
func TestTheServiceRunsOnTheRedisEnvelopeStore(t *testing.T) {
	fake := newFakeRedisEnvelopes()
	h := newHarness(t, func(cfg *Config) {
		envelopes, err := NewRedisEnvelopes(fake, "mailtest", cfg.Now)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Envelopes = envelopes
	})
	ctx := context.Background()

	sent := mustSend(t, h, withAttachment(directTo(1), "100 gold"))
	page, err := h.service.List(ctx, 1, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the page holds %d items, want 1", len(page.Items))
	}
	claim, err := h.service.ReserveClaim(ctx, 1, sent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(claim.Attachment) != "100 gold" {
		t.Fatalf("the claim carried %q", claim.Attachment)
	}
	if _, err := h.service.CommitClaim(ctx, 1, sent.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
}

// A stored envelope round-trips through JSON unchanged, attachment included.
// The attachment is the thing this package hands out exactly once; a codec
// that dropped it would be a silent loss.
func TestAStoredEnvelopeRoundTripsIntact(t *testing.T) {
	store, _ := newRedisEnvelopeStore(t)
	ctx := context.Background()
	original := testEnvelope("m1", time.Hour)
	original.Attachment = []byte{0x00, 0x01, 0xff, 0x7f}
	original.Body = "well played"
	original.Recipients = []int64{1, 2, 3}
	if _, err := store.Create(ctx, original); err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.Get(ctx, "m1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	wantJSON, _ := json.Marshal(original)
	gotJSON, _ := json.Marshal(stored)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("the envelope changed in storage:\n want %s\n  got %s", wantJSON, gotJSON)
	}
}

// The key ttl comes from the INJECTED clock, not the wall clock.
//
// This is a regression test for a defect in this file: Create computed its ttl
// with time.Until, so the store and the Service disagreed about the current
// time. With a Service clock set to a fixed past instant — which every test
// here uses, and which a replay or backfill job would also use — every
// envelope came out already expired. Two components with two clocks is a mail
// that reads as live and has already been evicted.
func TestTheKeyTTLComesFromTheInjectedClock(t *testing.T) {
	store, fake := newRedisEnvelopeStore(t)
	if _, err := store.Create(context.Background(), testEnvelope("m1", 2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// D-L3：键 TTL = 业务剩余时长 + 存储宽限，只兜底回收空间，比业务过期长。
	if fake.lastTTL != 2*time.Hour+EnvelopeStorageGrace {
		t.Fatalf("the key ttl is %s, want 2h plus the storage grace; a ttl derived from the wall clock while the "+
			"service runs on an injected one makes every envelope already expired",
			fake.lastTTL)
	}
}
