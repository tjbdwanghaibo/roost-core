package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Moved from roost-kit/service/mail/wiring_promises_test.go with the stores it
// asserts about (M-07): the Redis constructors refuse a nil client and a blank
// prefix, and the envelope store refuses empty ids on both read paths and a
// stored envelope that carries no id.
func TestRedisStoresRefuseInvalidConfigAndEmptyIDs(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRedisStores(nil, RedisConfig{Prefix: "mail", SendTTL: time.Hour}); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisStores(nil) = %v", err)
	}
	if _, err := NewRedisEnvelopes(nil, "mail", nil); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisEnvelopes(nil) = %v", err)
	}
	if _, err := NewRedisEnvelopes(newFakeRedisEnvelopes(), "  ", nil); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisEnvelopes with a blank prefix = %v", err)
	}

	store, fake := newRedisEnvelopeStore(t)
	if _, _, err := store.Get(ctx, " "); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "id is empty") {
		t.Fatalf("Get with an empty id = %v", err)
	}
	if _, err := store.GetMany(ctx, []string{"m-1", ""}); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "batch contains an empty id") {
		t.Fatalf("GetMany with an empty id = %v", err)
	}
	fake.mu.Lock()
	fake.values["mailtest:env:blank"] = []byte(`{"subject":"orphan"}`)
	fake.mu.Unlock()
	if _, _, err := store.Get(ctx, "blank"); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "stored envelope has no id") {
		t.Fatalf("Get of a stored envelope without an id = %v", err)
	}
}
