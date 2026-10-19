// CAS 契约：比较后写、比较后删、索引同次维护与分数精度、多键全有或全无、不完整命令与畸形回复拒绝。

package redis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func TestCompareAndSetAppliesWhenExpectedValueMatches(t *testing.T) {
	client := &casFakeRedis{values: map[string][]byte{"role:1": []byte(`{"version":1}`)}}

	result, err := CompareAndSet(context.Background(), client, CompareAndSetCommand{
		Key:      "role:1",
		Expected: []byte(`{"version":1}`),
		Next:     []byte(`{"version":2}`),
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("CompareAndSet: %v", err)
	}
	if !result.Applied {
		t.Fatalf("applied = false current=%s", result.Current)
	}
	if got := string(client.values["role:1"]); got != `{"version":2}` {
		t.Fatalf("value = %s", got)
	}
	if client.ttl["role:1"] != time.Hour {
		t.Fatalf("ttl = %v, want %v", client.ttl["role:1"], time.Hour)
	}
}

func TestCompareAndSetReturnsCurrentWhenExpectedValueDiffers(t *testing.T) {
	client := &casFakeRedis{values: map[string][]byte{"role:1": []byte(`{"version":3}`)}}

	result, err := CompareAndSet(context.Background(), client, CompareAndSetCommand{
		Key:      "role:1",
		Expected: []byte(`{"version":1}`),
		Next:     []byte(`{"version":2}`),
	})
	if err != nil {
		t.Fatalf("CompareAndSet: %v", err)
	}
	if result.Applied {
		t.Fatal("applied = true, want conflict")
	}
	if string(result.Current) != `{"version":3}` {
		t.Fatalf("current = %s", result.Current)
	}
}

func TestCompareAndSetCanRequireMissingKey(t *testing.T) {
	client := &casFakeRedis{values: map[string][]byte{}}

	result, err := CompareAndSet(context.Background(), client, CompareAndSetCommand{
		Key:  "role:1",
		Next: []byte(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("CompareAndSet: %v", err)
	}
	if !result.Applied {
		t.Fatalf("applied = false current=%s", result.Current)
	}
	if string(client.values["role:1"]) != `{"version":1}` {
		t.Fatalf("stored = %s", client.values["role:1"])
	}
}

type casFakeRedis struct {
	IRedis
	values map[string][]byte
	ttl    map[string]time.Duration
}

func (r *casFakeRedis) Eval(_ context.Context, _ string, keys []string, args ...any) (any, error) {
	if len(keys) != 1 || len(args) != 4 {
		return nil, fmt.Errorf("unexpected CAS eval keys=%v args=%v", keys, args)
	}
	if r.values == nil {
		r.values = make(map[string][]byte)
	}
	if r.ttl == nil {
		r.ttl = make(map[string]time.Duration)
	}
	key := keys[0]
	expected := []byte(fmt.Sprint(args[0]))
	next := []byte(fmt.Sprint(args[1]))
	ttlMillis, ok := args[2].(int64)
	if !ok {
		return nil, fmt.Errorf("ttl arg type = %T", args[2])
	}
	expectMissing := fmt.Sprint(args[3]) == "1"
	current, found := r.values[key]
	switch {
	case expectMissing && found:
		return []any{int64(0), string(current)}, nil
	case !expectMissing && (!found || !bytes.Equal(current, expected)):
		if !found {
			return []any{int64(0), nil}, nil
		}
		return []any{int64(0), string(current)}, nil
	default:
		r.values[key] = append([]byte(nil), next...)
		if ttlMillis > 0 {
			r.ttl[key] = time.Duration(ttlMillis) * time.Millisecond
		}
		return []any{int64(1), string(next)}, nil
	}
}

// CompareAndSet 是 versionstore / service 的乐观并发原语：命令不完整时在发出脚本之前拒绝（nil 客户端、空 key、nil Next）；脚本回复不是
// {applied, current} 二元组时报错而不是越界读取。

type recordingScriptRunner struct {
	calls int
	reply any
}

func (r *recordingScriptRunner) Eval(context.Context, string, []string, ...any) (any, error) {
	r.calls++
	return r.reply, nil
}

func TestCompareAndSetRejectsIncompleteCommandsBeforeIO(t *testing.T) {
	ctx := context.Background()
	runner := &recordingScriptRunner{reply: []any{int64(1), "v2"}}
	valid := CompareAndSetCommand{Key: "k", Expected: []byte("v1"), Next: []byte("v2")}
	if _, err := CompareAndSet(ctx, runner, valid); err != nil {
		t.Fatalf("baseline CompareAndSet = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("baseline issued %d scripts, want 1", runner.calls)
	}

	cases := []struct {
		name   string
		runner ScriptRunner
		cmd    CompareAndSetCommand
	}{
		{name: "nil client", runner: nil, cmd: valid},
		{name: "empty key", runner: runner, cmd: CompareAndSetCommand{Expected: []byte("v1"), Next: []byte("v2")}},
		{name: "nil next", runner: runner, cmd: CompareAndSetCommand{Key: "k", Expected: []byte("v1")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := runner.calls
			result, err := CompareAndSet(ctx, tc.runner, tc.cmd)
			if !errors.Is(err, ErrCASInvalidCommand) {
				t.Fatalf("err=%v, want ErrCASInvalidCommand", err)
			}
			if result.Applied || result.Current != nil {
				t.Fatalf("rejected command produced a result: %+v", result)
			}
			if runner.calls != before {
				t.Fatal("rejected command still reached the script runner")
			}
		})
	}
}

func TestCompareAndSetRejectsMalformedScriptReply(t *testing.T) {
	ctx := context.Background()
	cmd := CompareAndSetCommand{Key: "k", Expected: []byte("v1"), Next: []byte("v2")}
	for _, reply := range []any{[]any{int64(1)}, []any{}, []any{int64(1), "v2", "extra"}} {
		runner := &recordingScriptRunner{reply: reply}
		result, err := CompareAndSet(ctx, runner, cmd)
		if err == nil {
			t.Fatalf("reply %v (len %d) was accepted: %+v", reply, len(reply.([]any)), result)
		}
	}
}

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

// CompareAndSetAll 的命令不完整时在发出脚本之前拒绝：没有键、空 key、nil Next、重复键、带索引。
func TestCompareAndSetAllRejectsIncompleteCommandsBeforeIO(t *testing.T) {
	ctx := context.Background()
	runner := &recordingScriptRunner{reply: int64(1)}
	valid := []CompareAndSetCommand{{Key: "a", Expected: []byte("1"), Next: []byte("2")}, {Key: "b", Next: []byte("1")}}
	if applied, err := CompareAndSetAll(ctx, runner, valid); err != nil || !applied || runner.calls != 1 {
		t.Fatalf("baseline CompareAndSetAll = applied=%v err=%v calls=%d", applied, err, runner.calls)
	}
	for name, cmds := range map[string][]CompareAndSetCommand{
		"no commands":   nil,
		"empty key":     {{Next: []byte("1")}},
		"nil next":      {{Key: "a", Expected: []byte("1")}},
		"duplicate key": {{Key: "a", Next: []byte("1")}, {Key: "a", Next: []byte("2")}},
		"index":         {{Key: "a", Next: []byte("1"), Index: &CompareAndSetIndex{Key: "z", Member: "a"}}},
	} {
		before := runner.calls
		if applied, err := CompareAndSetAll(ctx, runner, cmds); !errors.Is(err, ErrCASInvalidCommand) || applied {
			t.Fatalf("%s: CompareAndSetAll = applied=%v err=%v, want ErrCASInvalidCommand", name, applied, err)
		}
		if runner.calls != before {
			t.Fatalf("%s: a rejected command reached the script runner", name)
		}
	}
	if _, err := CompareAndSetAll(ctx, nil, valid); !errors.Is(err, ErrCASInvalidCommand) {
		t.Fatalf("nil client = %v", err)
	}
}

// 真实 Redis 上的多键脚本：一个键不是期望值时另一个键也不写；全部符合时全部写入并按各自的 TTL；
// 期望不存在的键已存在时整体不写。Go 替身只能证明我以为脚本做了什么，脚本本身在这里核对（ROOST_REDIS_TEST_ADDR）。
func TestCompareAndSetAllIsAllOrNothingIntegration(t *testing.T) {
	client := integrationRedis(t)
	runner := evalRunner{client: client}
	ctx := context.Background()
	prefix := fmt.Sprintf("cas-all:%d:{t}:", time.Now().UnixNano())
	a, b, c := prefix+"a", prefix+"b", prefix+"c"
	t.Cleanup(func() { client.Del(context.Background(), a, b, c) })
	client.Set(ctx, a, "a0", 0)
	client.Set(ctx, b, "b0", 0)

	applied, err := CompareAndSetAll(ctx, runner, []CompareAndSetCommand{
		{Key: a, Expected: []byte("a0"), Next: []byte("a1")},
		{Key: b, Expected: []byte("stale"), Next: []byte("b1")},
	})
	if err != nil || applied {
		t.Fatalf("a set with one stale key = applied=%v err=%v, want not applied", applied, err)
	}
	if got := client.Get(ctx, a).Val(); got != "a0" {
		t.Fatalf("the stale key's partner was written: a = %q", got)
	}
	applied, err = CompareAndSetAll(ctx, runner, []CompareAndSetCommand{
		{Key: a, Expected: []byte("a0"), Next: []byte("a1")},
		{Key: b, Expected: []byte("b0"), Next: []byte("b1"), TTL: time.Hour},
		{Key: c, Next: []byte("c1")},
	})
	if err != nil || !applied {
		t.Fatalf("a matching set = applied=%v err=%v", applied, err)
	}
	for key, want := range map[string]string{a: "a1", b: "b1", c: "c1"} {
		if got := client.Get(ctx, key).Val(); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if ttl := client.PTTL(ctx, b).Val(); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("b's ttl = %v, want (0, 1h]", ttl)
	}
	if ttl := client.PTTL(ctx, a).Val(); ttl != -1*time.Millisecond && ttl != -1 {
		t.Fatalf("a's ttl = %v, want none", ttl)
	}
	applied, err = CompareAndSetAll(ctx, runner, []CompareAndSetCommand{
		{Key: a, Expected: []byte("a1"), Next: []byte("a2")},
		{Key: c, Next: []byte("c2")},
	})
	if err != nil || applied || client.Get(ctx, a).Val() != "a1" {
		t.Fatalf("expecting an existing key to be absent = applied=%v err=%v a=%q", applied, err, client.Get(ctx, a).Val())
	}
}
