package redis

import (
	"context"
	"errors"
	goredis "github.com/redis/go-redis/v9"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestCompareAndDeleteOnlyRemovesTheValueItWasShownIntegration(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	key := "roost:test:cad:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { client.Del(ctx, key) })
	runner := evalRunner{client: client}

	if err := client.Set(ctx, key, "owner-a", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	// Somebody else's value: refused, and it says who holds it now.
	result, err := CompareAndDelete(ctx, runner, key, []byte("owner-b"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Fatal("a delete against another holder's value was applied")
	}
	if string(result.Current) != "owner-a" {
		t.Fatalf("current = %q, want the value that is actually there", result.Current)
	}
	if got, err := client.Get(ctx, key).Result(); err != nil || got != "owner-a" {
		t.Fatalf("the holder's key was removed anyway: %q %v", got, err)
	}

	// Our own value: removed.
	result, err = CompareAndDelete(ctx, runner, key, []byte("owner-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied {
		t.Fatalf("the holder could not delete its own key: current=%q", result.Current)
	}
	if n, err := client.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("key still exists after its holder deleted it: %d %v", n, err)
	}

	// Already gone: not applied, and Current is nil rather than empty, so a
	// caller can tell "somebody else has it" from "there is nothing there".
	result, err = CompareAndDelete(ctx, runner, key, []byte("owner-a"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Fatal("deleting a key that is gone reported applied")
	}
	if result.Current != nil {
		t.Fatalf("current = %q for a missing key, want nil", result.Current)
	}
}

func TestCompareAndDeleteRefusesACommandItCannotHonour(t *testing.T) {
	ctx := context.Background()
	runner := evalRunner{}
	for name, call := range map[string]func() error{
		"nil client": func() error {
			_, err := CompareAndDelete(ctx, nil, "k", []byte("v"))
			return err
		},
		"blank key": func() error {
			_, err := CompareAndDelete(ctx, runner, "", []byte("v"))
			return err
		},
		// A nil Expected would mean "delete whatever is there", which is DEL
		// wearing a compare-and-delete costume: the caller would read a
		// guarantee out of the name that the call does not provide.
		"nil expected": func() error {
			_, err := CompareAndDelete(ctx, runner, "k", nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCompareAndSetRefusesAnIncompleteIndex(t *testing.T) {
	ctx := context.Background()
	runner := &recordingScriptRunner{reply: []any{int64(1), "v2"}}
	for name, index := range map[string]*CompareAndSetIndex{
		"no key":    {Member: "m"},
		"no member": {Key: "idx"},
	} {
		cmd := CompareAndSetCommand{Key: "k", Next: []byte("v"), Index: index}
		if _, err := CompareAndSet(ctx, runner, cmd); !errors.Is(err, ErrCASInvalidCommand) {
			t.Errorf("%s: error = %v, want ErrCASInvalidCommand", name, err)
		}
	}
	if runner.calls != 0 {
		t.Errorf("an incomplete index reached Redis %d times", runner.calls)
	}
}

func integrationRedis(t *testing.T) *goredis.Client {
	t.Helper()
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	client := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis at %s is not reachable: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// evalRunner adapts a go-redis client to ScriptRunner, so the test drives the
// same script the product does.
type evalRunner struct{ client *goredis.Client }

func (r evalRunner) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.client.Eval(ctx, script, keys, args...).Result()
}

func TestIndexIsMaintainedInTheSameWriteIntegration(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	unique := strconv.FormatInt(time.Now().UnixNano(), 36)
	valueKey := "roost:test:cas:" + unique + ":value"
	indexKey := "roost:test:cas:" + unique + ":index"
	t.Cleanup(func() { client.Del(ctx, valueKey, indexKey) })
	runner := evalRunner{client: client}

	// Create with an index entry: both or neither.
	result, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Next: []byte("v1"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Score: 1700},
	})
	if err != nil || !result.Applied {
		t.Fatalf("create: applied=%v err=%v", result.Applied, err)
	}
	score, err := client.ZScore(ctx, indexKey, "order-1").Result()
	if err != nil || score != 1700 {
		t.Fatalf("index score = %v (err %v), want 1700", score, err)
	}

	// A write that loses the compare must not touch the index.
	lost, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Expected: []byte("not what is stored"), Next: []byte("v2"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Remove: true},
	})
	if err != nil {
		t.Fatalf("losing write: %v", err)
	}
	if lost.Applied {
		t.Fatal("a compare against the wrong value was applied")
	}
	if exists, _ := client.ZScore(ctx, indexKey, "order-1").Result(); exists != 1700 {
		t.Fatal("a write that did not happen still changed the index")
	}

	// A successful write can move the score...
	if _, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Expected: []byte("v1"), Next: []byte("v2"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Score: 1800},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if score, _ := client.ZScore(ctx, indexKey, "order-1").Result(); score != 1800 {
		t.Fatalf("index score = %v after an update, want 1800", score)
	}

	// ...and retire the entry when the record stops being pending.
	if _, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Expected: []byte("v2"), Next: []byte("v3"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Remove: true},
	}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := client.ZScore(ctx, indexKey, "order-1").Result(); !errors.Is(err, goredis.Nil) {
		t.Fatalf("the entry was not retired: %v", err)
	}
	if stored, _ := client.Get(ctx, valueKey).Result(); stored != "v3" {
		t.Fatalf("the value is %q, want the last write", stored)
	}
}

// A score is a number on the Lua side too: one that arrives as a float with
// many digits must come back as the same number, not as a rounded string.
func TestIndexScorePrecisionSurvivesLuaIntegration(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	unique := strconv.FormatInt(time.Now().UnixNano(), 36)
	valueKey := "roost:test:cas:" + unique + ":value"
	indexKey := "roost:test:cas:" + unique + ":index"
	t.Cleanup(func() { client.Del(ctx, valueKey, indexKey) })

	const due = float64(1789781700)
	if _, err := CompareAndSet(ctx, evalRunner{client: client}, CompareAndSetCommand{
		Key: valueKey, Next: []byte("v"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order", Score: due},
	}); err != nil {
		t.Fatal(err)
	}
	score, err := client.ZScore(ctx, indexKey, "order").Result()
	if err != nil {
		t.Fatal(err)
	}
	if score != due {
		t.Fatalf("score came back as %f, want %f: a unix second must survive the script", score, due)
	}
}
