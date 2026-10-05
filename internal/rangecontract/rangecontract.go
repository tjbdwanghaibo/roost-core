// Package rangecontract 是遍历回调契约的共用测试辅助（维护者决定 C7，2026-10-05）。
//
// 仓库级契约：凡框架交给业务的 Range（容器、map、实体管理器、生成 DAO 的 RangeX），回调里可以
// 读写同一个容器，返回 false 立即停止。展开为可检查的几条：
//
//  1. 回调里对同一容器调用 Get / Set / Delete / Clear / 嵌套 Range 不死锁、不 panic；
//  2. 回调返回 false 后 Range 立即返回，不再调用回调（跨桶 / 跨分片也一样）；
//  3. 不交出不存在的条目：交出的键都是遍历期间真实存在过的（不交出零值键、已清零的实体），
//     同一个键至多交出一次；
//  4. 遍历开始时就在、遍历期间没被删除的键恰好交出一次，交出的值是它当时的值；
//  5. 遍历期间被删除的、还没走到的键：快照实现可能仍交出（带删除前的值），活遍历实现不交出；
//     回调里新增的键是否交出不承诺（与 Go map 相同）。
//
// NC-180 / 181 / 182 / 185 是同一契约在 BucketHolder、FastMap、KeyMap 上各自被打破；Check 用同一组
// 用例覆盖每种容器，新增容器按这一条审。
//
// 本包只给测试用（导入 testing），放在 internal 下不成为公开 API。生成 DAO（golden 在另一个一次性
// 模块里编译）的 RangeX 用例把本文件按源码复制进那个模块再运行（codegen/scripts/dao-golden-runtime.sh）。
package rangecontract

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// Ops 是被测容器的操作，键与值都换算成 int64。键 1..Keys 是初始内容，值为 Value(k)；
// 回调里新增的键从 addedBase 开始。容器没有某个操作时对应字段为 nil，相关用例跳过。
type Ops struct {
	// Set 写入一个键：已有键改值，没有就新增。KeysOnly 时值被忽略（例如实体：Set 只新增实体）。
	Set func(key, value int64)
	// Get 读一个键。
	Get func(key int64) (value int64, ok bool)
	// Delete 删除一个键（实体：Destroy）。
	Delete func(key int64)
	// Clear 清空整个容器；nil 表示没有。
	Clear func()
	// Range 是被测的遍历入口。
	Range func(f func(key, value int64) bool)
}

// Subject 描述一种被测容器。
type Subject struct {
	// New 新建一个空容器，返回它的操作。
	New func(t testing.TB) Ops
	// Keys 是初始键数，默认 64（足以跨桶 / 跨分片、让 FastMap 在回调新增时扩容）。
	Keys int
	// KeysOnly 表示值由键决定、Set 不能改已有键的值（实体管理器）。
	KeysOnly bool
	// Live 表示活遍历：回调里删掉的、还没走到的键不交出。false 是快照语义，允许交出。
	Live bool
	// Patience 是一次 Range 返回的期限，默认 5s；超过判定回调在等 Range 自己持有的锁。
	Patience time.Duration
	// Tx 可选：把一段写操作包进容器要求的执行环境（生成 DAO 的 SetX 要在 Nest 事务里标脏）。
	// 初始填充和每次 Range（连同回调里的写）各包一次，在同一个 goroutine 里执行。
	Tx func(body func())
}

const addedBase = 1_000_000

// Value 是初始键 k 的值。
func Value(k int64) int64 { return k*10 + 7 }

// Check 按契约逐条检查 subject，失败通过 t 报告。
func Check(t *testing.T, subject Subject) {
	t.Helper()
	if subject.New == nil {
		t.Fatal("rangecontract: Subject.New is required")
	}
	if subject.Keys <= 0 {
		subject.Keys = 64
	}
	if subject.Patience <= 0 {
		subject.Patience = 5 * time.Second
	}
	t.Run("read inside the callback", func(t *testing.T) { checkRead(t, subject) })
	t.Run("set inside the callback", func(t *testing.T) { checkSet(t, subject) })
	t.Run("delete inside the callback", func(t *testing.T) { checkDelete(t, subject) })
	t.Run("clear inside the callback", func(t *testing.T) { checkClear(t, subject) })
	t.Run("false stops immediately", func(t *testing.T) { checkStop(t, subject) })
}

func fill(t *testing.T, subject Subject) Ops {
	t.Helper()
	ops := subject.New(t)
	if ops.Set == nil || ops.Get == nil || ops.Delete == nil || ops.Range == nil {
		t.Fatal("rangecontract: Set, Get, Delete and Range are required")
	}
	inTx(subject, func() {
		for k := int64(1); k <= int64(subject.Keys); k++ {
			ops.Set(k, Value(k))
		}
	})
	return ops
}

func inTx(subject Subject, body func()) {
	if subject.Tx == nil {
		body()
		return
	}
	subject.Tx(body)
}

// visit 有界地跑一次 Range，记录交出的键值（按顺序）。
func visit(t *testing.T, subject Subject, ops Ops, f func(key, value int64) bool) []entry {
	t.Helper()
	var seen []entry
	done := make(chan struct{})
	go func() {
		defer close(done)
		inTx(subject, func() {
			ops.Range(func(key, value int64) bool {
				seen = append(seen, entry{key, value})
				return f(key, value)
			})
		})
	}()
	select {
	case <-done:
	case <-time.After(subject.Patience):
		t.Fatalf("Range did not return within %v: the callback is waiting for a lock its own Range holds", subject.Patience)
	}
	return seen
}

type entry struct{ key, value int64 }

// checkDelivered 断言 3：没有不存在的键、没有重复，初始键交出的值正确。返回交出的初始键集合。
func checkDelivered(t *testing.T, subject Subject, seen []entry, allowAdded bool) map[int64]bool {
	t.Helper()
	got := make(map[int64]bool, len(seen))
	for _, e := range seen {
		original := e.key >= 1 && e.key <= int64(subject.Keys)
		added := allowAdded && e.key >= addedBase && e.key < addedBase+int64(subject.Keys)+1
		if !original && !added {
			t.Fatalf("Range handed out key %d (value %d) that never existed", e.key, e.value)
		}
		if got[e.key] {
			t.Fatalf("Range handed out key %d twice", e.key)
		}
		got[e.key] = true
		if original && !subject.KeysOnly && e.value != Value(e.key) {
			t.Fatalf("Range handed out key %d with value %d, want %d", e.key, e.value, Value(e.key))
		}
	}
	return got
}

func requireAll(t *testing.T, subject Subject, got map[int64]bool, why string) {
	t.Helper()
	var missing []int64
	for k := int64(1); k <= int64(subject.Keys); k++ {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("Range skipped %d original key(s) %v: %s", len(missing), head(missing), why)
	}
}

func checkRead(t *testing.T, subject Subject) {
	ops := fill(t, subject)
	seen := visit(t, subject, ops, func(key, value int64) bool {
		if v, ok := ops.Get(key); !ok || (!subject.KeysOnly && v != value) {
			t.Errorf("Get(%d) inside the callback = (%d, %v), want (%d, true)", key, v, ok, value)
		}
		ops.Get(-key) // 未命中的读（BucketHolder 曾在此升级写锁自锁，NC-180）
		inner := 0
		ops.Range(func(int64, int64) bool { inner++; return inner < 3 })
		return true
	})
	requireAll(t, subject, checkDelivered(t, subject, seen, false), "nothing was deleted")
}

func checkSet(t *testing.T, subject Subject) {
	ops := fill(t, subject)
	seen := visit(t, subject, ops, func(key, _ int64) bool {
		if key >= addedBase {
			return true
		}
		ops.Set(key, Value(key)+1) // 改刚交出的键
		ops.Set(addedBase+key, 1)  // 新增键（FastMap 在这里扩容重排，NC-182）
		return true
	})
	requireAll(t, subject, checkDelivered(t, subject, seen, true), "keys were only overwritten or added")
	for k := int64(1); k <= int64(subject.Keys); k++ {
		want := Value(k) + 1
		if subject.KeysOnly {
			want = k
		}
		if v, ok := ops.Get(k); !ok || v != want {
			t.Fatalf("after Range, Get(%d) = (%d, %v), want (%d, true): a write inside the callback was lost", k, v, ok, want)
		}
		if _, ok := ops.Get(addedBase + k); !ok {
			t.Fatalf("after Range, added key %d is missing", addedBase+k)
		}
	}
}

// partner 把初始键两两配对：回调删掉刚交出的键和它的伙伴，伙伴可能已走过、也可能还没走到。
func partner(k int64) int64 {
	if k%2 == 1 {
		return k + 1
	}
	return k - 1
}

func checkDelete(t *testing.T, subject Subject) {
	if subject.Keys%2 != 0 {
		t.Fatal("rangecontract: Keys must be even for the delete case")
	}
	ops := fill(t, subject)
	deletedBeforeReached := map[int64]bool{}
	got := map[int64]bool{}
	seen := visit(t, subject, ops, func(key, _ int64) bool {
		got[key] = true
		ops.Delete(key)
		if p := partner(key); !got[p] {
			deletedBeforeReached[p] = true
			ops.Delete(p)
		}
		return true
	})
	delivered := checkDelivered(t, subject, seen, false)
	for k := int64(1); k <= int64(subject.Keys); k++ {
		switch {
		case delivered[k]:
			if subject.Live && deletedBeforeReached[k] {
				t.Fatalf("live Range handed out key %d after the callback deleted it", k)
			}
		case !deletedBeforeReached[k]:
			t.Fatalf("Range skipped key %d, which was never deleted before being reached", k)
		}
	}
	remaining := 0
	ops.Range(func(int64, int64) bool { remaining++; return true })
	if remaining != 0 {
		t.Fatalf("%d key(s) left after the callback deleted every key", remaining)
	}
}

func checkClear(t *testing.T, subject Subject) {
	ops := fill(t, subject)
	if ops.Clear == nil {
		t.Skip("container has no Clear")
	}
	cleared := false
	seen := visit(t, subject, ops, func(int64, int64) bool {
		if !cleared {
			cleared = true
			ops.Clear()
		}
		return true
	})
	checkDelivered(t, subject, seen, false)
	if subject.Live && len(seen) != 1 {
		t.Fatalf("live Range handed out %d key(s) after the callback cleared the container, want only the first", len(seen))
	}
	remaining := 0
	ops.Range(func(int64, int64) bool { remaining++; return true })
	if remaining != 0 {
		t.Fatalf("%d key(s) left after Clear inside the callback", remaining)
	}
	ops.Set(7, Value(7))
	if _, ok := ops.Get(7); !ok {
		t.Fatal("container unusable after Clear inside the callback: Set then Get misses")
	}
}

func checkStop(t *testing.T, subject Subject) {
	for _, stopAt := range []int{1, 3} {
		t.Run(fmt.Sprintf("at call %d", stopAt), func(t *testing.T) {
			ops := fill(t, subject)
			calls := 0
			visit(t, subject, ops, func(key, _ int64) bool {
				calls++
				if calls == stopAt {
					ops.Set(addedBase+key, 1) // 停止前改容器，停止照样立即生效
					ops.Delete(key)
					return false
				}
				return true
			})
			if calls != stopAt {
				t.Fatalf("callback ran %d time(s), want %d: Range kept going after false", calls, stopAt)
			}
		})
	}
}

func head(keys []int64) []int64 {
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	if len(keys) > 8 {
		return keys[:8]
	}
	return keys
}
