package failurelog

// RR-20261005-NC-160：脚本结果未知时不能再走非原子降级。
//
// AppendRaw / DeleteRaw / Purge 先发一条 Lua 脚本，脚本返回错误就退回 RPUSH / LREM / LLEN+DEL
// 这组“非原子降级”。降级本来只为“适配器没有 Lua”准备，但旧代码把任何 Eval 错误都当成“脚本没执行”：
// 回复丢失（连接断开、读超时、ctx 到期）时脚本可能已经在服务端执行过，再降级就是第二次执行——
// 追加两次、多删一条同值记录、把脚本清空之后新到的死信也一起删掉。结果未知只能交给调用方，
// 与 cache RefHMap（RR-20261004-NC-21）和驱动脚本不重放（RR-20261005-NC-100）同一契约。

import (
	"context"
	"errors"
	"testing"
)

var errReplyLost = errors.New("read tcp: i/o timeout (reply lost)")

// lostReplyRedis 在 fakeRedis 上模拟“脚本已在服务端执行、回复丢了”：Eval 先按脚本语义改动列表，
// 再返回传输错误。afterScript 用来插入“脚本执行后、调用方收到错误前”另一写者的动作。
type lostReplyRedis struct {
	*fakeRedis
	evals       int
	afterScript func()
}

func (r *lostReplyRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	r.evals++
	key := keys[0]
	switch script {
	case appendTrimScript:
		_, _ = r.fakeRedis.RPush(ctx, key, args[0])
	case deleteRawScript:
		remove := map[string]int{}
		for _, arg := range args {
			remove[arg.(string)]++
		}
		kept := r.lists[key][:0]
		for _, item := range r.lists[key] {
			if remove[item] > 0 {
				remove[item]--
				continue
			}
			kept = append(kept, item)
		}
		r.lists[key] = kept
	case purgeRawScript:
		delete(r.lists, key)
	}
	if r.afterScript != nil {
		r.afterScript()
	}
	return nil, errReplyLost
}

func TestUnknownScriptResultIsReturnedWithoutReplayingTheWrite(t *testing.T) {
	const key = "failure:{x}"
	ctx := context.Background()

	t.Run("append", func(t *testing.T) {
		redis := &lostReplyRedis{fakeRedis: newFakeRedis()}
		list := NewRedisList(redis, Config{Namespace: "dlq", MaxEntries: 10})
		err := list.AppendRaw(ctx, key, []byte("dead-letter"))
		if got := redis.lists[key]; len(got) != 1 {
			t.Fatalf("one AppendRaw whose script ran but whose reply was lost stored %d entries %v (err=%v); want exactly the script's one", len(got), got, err)
		}
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("AppendRaw err = %v, want the unknown-result error kept for the caller", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		redis := &lostReplyRedis{fakeRedis: newFakeRedis()}
		redis.lists[key] = []string{"same", "same"}
		list := NewRedisList(redis, Config{Namespace: "dlq", MaxEntries: 10})
		_, err := list.DeleteRaw(ctx, key, [][]byte{[]byte("same")})
		if got := redis.lists[key]; len(got) != 1 {
			t.Fatalf("deleting one copy whose script ran but whose reply was lost left %v (err=%v); want one copy kept", got, err)
		}
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("DeleteRaw err = %v, want the unknown-result error", err)
		}
	})

	t.Run("purge", func(t *testing.T) {
		redis := &lostReplyRedis{fakeRedis: newFakeRedis()}
		redis.lists[key] = []string{"old-1", "old-2"}
		// 脚本清空之后、调用方收到错误之前，另一个消费者写进一条新死信。
		redis.afterScript = func() { redis.lists[key] = []string{"arrived-after-purge"} }
		list := NewRedisList(redis, Config{Namespace: "dlq", MaxEntries: 10})
		_, err := list.Purge(ctx, key)
		if got := redis.lists[key]; len(got) != 1 || got[0] != "arrived-after-purge" {
			t.Fatalf("Purge whose script ran but whose reply was lost left %v (err=%v); the dead letter written after the purge must survive", got, err)
		}
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("Purge err = %v, want the unknown-result error", err)
		}
	})
}

// 降级路径仍然留给真的没有 Lua 的适配器（Eval 返回 nil, nil 的替身）：行为不变。
func TestAdapterWithoutLuaStillUsesTheFallback(t *testing.T) {
	redis := newFakeRedis()
	list := NewRedisList(redis, Config{MaxEntries: 10})
	if err := list.AppendRaw(context.Background(), "failure:{x}", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if got := redis.lists["failure:{x}"]; len(got) != 1 {
		t.Fatalf("fallback append stored %v", got)
	}
}
