package redis

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// RR-20260920-03 的另一半：给"放弃自己的那把租约"一个原子操作。
//
// 没有它的时候，调用方只能 GET 确认是自己的、再 DEL。两条命令之间租约可能过期
// 并被另一个进程取得，于是这次 DEL 删掉的是**别人的** key。compare-and-delete
// 把比较和删除放进同一个脚本，是那个窗口唯一的关门方式。
//
// 这些用例跑真 Redis（ROOST_REDIS_TEST_ADDR），因为要验证的是 Lua 本身：
// 一个用 Go 写的替身只会证明我对脚本的理解，不会证明脚本。
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
