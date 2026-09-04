package redis

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// MGet's contract has three parts a unit test cannot establish, because they
// are properties of Redis and of the driver rather than of this code: that the
// reply is one element per key, that an absent key comes back as a nil element
// rather than being dropped, and that zero keys must not reach the server at
// all.
//
//	docker run --rm -p 6379:6379 redis:7
//	ROOST_REDIS_TEST_ADDR=127.0.0.1:6379 go test ./redis/ -run MGet
func mgetClient(t *testing.T) (fredis.IRedis, string) {
	t.Helper()
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	client, err := NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	prefix := "roost:test:mget:" + rand.Text() + ":"
	t.Cleanup(func() { _ = client.Close() })
	return client, prefix
}

// The result is positional: one element per key, in order, with nil for the
// keys that are absent. A caller comparing lengths can then detect a truncated
// read; one holding a map with the misses dropped cannot.
func TestMGetReturnsOneElementPerKeyIncludingMisses(t *testing.T) {
	client, prefix := mgetClient(t)
	ctx := context.Background()

	present := []string{prefix + "a", prefix + "c", prefix + "e"}
	for index, key := range present {
		if err := client.Set(ctx, key, []byte{byte('A' + index)}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = client.Del(context.Background(), present...) })

	// Interleave keys that were never written, so an implementation that
	// dropped misses would return a result whose positions no longer line up
	// with the keys — visible here as the wrong value at the wrong index, not
	// merely as a shorter slice.
	requested := []string{
		prefix + "a", prefix + "b", prefix + "c", prefix + "d", prefix + "e",
	}
	values, err := client.MGet(ctx, requested...)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != len(requested) {
		t.Fatalf("MGet returned %d values for %d keys; a short result is indistinguishable "+
			"from a truncated read", len(values), len(requested))
	}
	want := []string{"A", "", "B", "", "C"}
	for index, expected := range want {
		if expected == "" {
			if values[index] != nil {
				t.Fatalf("key %q is absent but came back as %q", requested[index], values[index])
			}
			continue
		}
		if string(values[index]) != expected {
			t.Fatalf("key %q came back as %q, want %q; the reply was mapped to the wrong key",
				requested[index], values[index], expected)
		}
	}
}

// Zero keys must not reach the server: `MGET` with no arguments is an error in
// Redis, so an implementation that passed the empty case through would fail on
// an ordinary empty page.
func TestMGetWithNoKeysMakesNoRoundTrip(t *testing.T) {
	client, _ := mgetClient(t)
	values, err := client.MGet(context.Background())
	if err != nil {
		t.Fatalf("MGet with no keys produced %v; an empty page is not an error", err)
	}
	if len(values) != 0 {
		t.Fatalf("MGet with no keys returned %d values", len(values))
	}
}

// A value written as bytes comes back as the same bytes, including ones that
// are not valid UTF-8. The driver hands bulk replies back as strings, so a
// conversion that assumed text would corrupt a binary payload — and an
// envelope's attachment is binary.
func TestMGetPreservesBinaryValues(t *testing.T) {
	client, prefix := mgetClient(t)
	ctx := context.Background()
	key := prefix + "binary"
	payload := []byte{0x00, 0x01, 0xff, 0x7f, 0xfe}
	if err := client.Set(ctx, key, payload, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = client.Del(context.Background(), key) })

	values, err := client.MGet(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("MGet returned %d values for one key", len(values))
	}
	if string(values[0]) != string(payload) {
		t.Fatalf("a binary value came back as % x, want % x", values[0], payload)
	}
}
