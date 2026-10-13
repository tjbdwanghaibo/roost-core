package safemap

import (
	"fmt"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSmallSafeMapContract(t *testing.T) {
	exerciseMap(t, NewSmallSafeMap[int64, string](2))
}

// 手工调用这两个方法本身不够：签名与驱动接口不匹配时它照样通过，因为直接调用不在乎 bson 会不会分派到它。经 bson.Marshal
// 的往返见 TestSmallSafeMapSurvivesABSONRoundTrip；这一条守类型码。
func TestSmallSafeMapBSONV2RoundTrip(t *testing.T) {
	source := NewSmallSafeMap[string, int](2)
	source.Set("alpha", 1)
	source.Set("beta", 2)
	typeCode, data, err := source.MarshalBSONValue()
	if err != nil {
		t.Fatal(err)
	}
	if bson.Type(typeCode) != bson.TypeEmbeddedDocument {
		t.Fatalf("BSON type = %v, want embedded document", bson.Type(typeCode))
	}
	restored := NewSmallSafeMap[string, int](0)
	if err := restored.UnmarshalBSONValue(typeCode, data); err != nil {
		t.Fatal(err)
	}
	if got, ok := restored.Get("alpha"); !ok || got != 1 {
		t.Fatalf("round-trip alpha = %d, %v", got, ok)
	}
}

func TestShardedSafeMapContract(t *testing.T) {
	exerciseMap(t, NewInt64ShardedSafeMap[string](4))
}

func TestFastMapContract(t *testing.T) {
	exerciseMap(t, NewInt64FastMap[string](2))
}

func exerciseMap(t *testing.T, m IMap[int64, string]) {
	t.Helper()

	if m.Len() != 0 {
		t.Fatalf("new map len = %d", m.Len())
	}
	if _, ok := m.Get(1); ok {
		t.Fatal("empty map should not contain key")
	}
	if m.Delete(1) {
		t.Fatal("delete missing key should return false")
	}

	m.Set(1, "one")
	m.Set(2, "two")
	m.Set(1, "uno")

	if m.Len() != 2 {
		t.Fatalf("len after set/update = %d", m.Len())
	}
	if got, ok := m.Get(1); !ok || got != "uno" {
		t.Fatalf("get updated key: got %q ok=%v", got, ok)
	}
	if got, ok := m.Get(2); !ok || got != "two" {
		t.Fatalf("get second key: got %q ok=%v", got, ok)
	}

	visited := make(map[int64]string)
	m.Range(func(key int64, value string) bool {
		visited[key] = value
		return true
	})
	if len(visited) != 2 || visited[1] != "uno" || visited[2] != "two" {
		t.Fatalf("range visited: %+v", visited)
	}

	stopCount := 0
	m.Range(func(int64, string) bool {
		stopCount++
		return false
	})
	if stopCount != 1 {
		t.Fatalf("range stop count = %d", stopCount)
	}

	if !m.Delete(1) {
		t.Fatal("delete existing key should return true")
	}
	if m.Delete(1) {
		t.Fatal("delete same key twice should return false")
	}
	if m.Len() != 1 {
		t.Fatalf("len after delete = %d", m.Len())
	}
	if _, ok := m.Get(1); ok {
		t.Fatal("deleted key should not exist")
	}

	m.Clear()
	if m.Len() != 0 {
		t.Fatalf("len after clear = %d", m.Len())
	}
	if _, ok := m.Get(2); ok {
		t.Fatal("cleared key should not exist")
	}

	m.Set(3, "three")
	if got, ok := m.Get(3); !ok || got != "three" {
		t.Fatalf("set after clear: got %q ok=%v", got, ok)
	}
}

func TestFastMapGrowthAndTombstoneReuse(t *testing.T) {
	m := NewInt64FastMap[int64](1)
	initialCap := m.Cap()
	for i := int64(0); i < 128; i++ {
		m.Set(i, i*10)
	}
	if m.Len() != 128 {
		t.Fatalf("len after growth = %d", m.Len())
	}
	if m.Cap() <= initialCap {
		t.Fatalf("capacity should grow, initial=%d current=%d", initialCap, m.Cap())
	}
	for i := int64(0); i < 128; i += 2 {
		if !m.Delete(i) {
			t.Fatalf("delete %d failed", i)
		}
	}
	for i := int64(1); i < 128; i += 2 {
		if got, ok := m.Get(i); !ok || got != i*10 {
			t.Fatalf("odd key %d lost: got=%d ok=%v", i, got, ok)
		}
	}
	for i := int64(200); i < 260; i++ {
		m.Set(i, i*10)
	}
	for i := int64(200); i < 260; i++ {
		if got, ok := m.Get(i); !ok || got != i*10 {
			t.Fatalf("new key %d lost: got=%d ok=%v", i, got, ok)
		}
	}
}

func TestFastMapStringKeys(t *testing.T) {
	m := NewStringFastMap[int](2)
	m.Set("alpha", 1)
	m.Set("beta", 2)
	m.Set("alpha", 3)

	if got, ok := m.Get("alpha"); !ok || got != 3 {
		t.Fatalf("alpha: got=%d ok=%v", got, ok)
	}
	if got, ok := m.Get("beta"); !ok || got != 2 {
		t.Fatalf("beta: got=%d ok=%v", got, ok)
	}
	if m.Delete("alpha") != true {
		t.Fatal("delete alpha failed")
	}
	if _, ok := m.Get("alpha"); ok {
		t.Fatal("alpha should be deleted")
	}
}

func TestShardedSafeMapConcurrentAccess(t *testing.T) {
	const (
		workers   = 8
		perWorker = 500
	)
	m := NewInt64ShardedSafeMap[string](8)

	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := int64(worker * perWorker)
			for i := 0; i < perWorker; i++ {
				key := base + int64(i)
				value := fmt.Sprintf("%d", key)
				m.Set(key, value)
				if got, ok := m.Get(key); !ok || got != value {
					t.Errorf("get during set key=%d got=%q ok=%v", key, got, ok)
				}
			}
		}()
	}
	wg.Wait()

	wantLen := workers * perWorker
	if got := m.Len(); got != wantLen {
		t.Fatalf("len after concurrent set = %d, want %d", got, wantLen)
	}

	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := int64(worker * perWorker)
			for i := 0; i < perWorker; i += 2 {
				m.Delete(base + int64(i))
			}
		}()
	}
	wg.Wait()

	if got := m.Len(); got != wantLen/2 {
		t.Fatalf("len after concurrent delete = %d, want %d", got, wantLen/2)
	}
}

func TestShardedSafeMapClearConcurrentReaders(t *testing.T) {
	m := NewInt64ShardedSafeMap[int](16)
	for i := int64(0); i < 1000; i++ {
		m.Set(i, int(i))
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := int64(0); j < 1000; j++ {
				_, _ = m.Get(j)
			}
		}()
	}
	m.Clear()
	wg.Wait()

	if got := m.Len(); got != 0 {
		t.Fatalf("len after clear = %d", got)
	}
}

func TestShardedSafeMapComputeLoadOrStoreAndSnapshot(t *testing.T) {
	m := NewInt64ShardedSafeMap[string](8)
	if got := m.ShardCount(); got != 8 {
		t.Fatalf("shard count = %d, want 8", got)
	}
	if idx := m.ShardIndex(42); idx < 0 || idx >= m.ShardCount() {
		t.Fatalf("shard index out of range: %d", idx)
	}

	value, loaded := m.LoadOrStore(1, "one")
	if loaded || value != "one" {
		t.Fatalf("first LoadOrStore value=%q loaded=%v", value, loaded)
	}
	value, loaded = m.LoadOrStore(1, "uno")
	if !loaded || value != "one" {
		t.Fatalf("second LoadOrStore value=%q loaded=%v", value, loaded)
	}

	got, ok := m.Compute(1, func(old string, exists bool) (string, bool) {
		if !exists || old != "one" {
			t.Fatalf("compute old=%q exists=%v", old, exists)
		}
		return "uno", true
	})
	if !ok || got != "uno" {
		t.Fatalf("compute update got=%q ok=%v", got, ok)
	}
	got, ok = m.Compute(1, func(old string, exists bool) (string, bool) {
		return "", false
	})
	if ok || got != "" {
		t.Fatalf("compute delete got=%q ok=%v", got, ok)
	}
	if m.Len() != 0 {
		t.Fatalf("len after compute delete = %d", m.Len())
	}

	m.Set(2, "two")
	m.Set(3, "three")
	readCalled := false
	readOK := m.Read(2, func(value string, exists bool) {
		readCalled = true
		if !exists || value != "two" {
			t.Fatalf("read value=%q exists=%v", value, exists)
		}
	})
	if !readOK || !readCalled {
		t.Fatalf("read should report existing key, ok=%v called=%v", readOK, readCalled)
	}
	missingOK := m.Read(4, func(value string, exists bool) {
		if exists || value != "" {
			t.Fatalf("missing read value=%q exists=%v", value, exists)
		}
	})
	if missingOK {
		t.Fatal("read should report missing key")
	}
	entries := m.Snapshot()
	if len(entries) != 2 {
		t.Fatalf("snapshot len = %d", len(entries))
	}
	entries[0].Value = "changed"
	if got, _ := m.Get(entries[0].Key); got == "changed" {
		t.Fatal("snapshot should not expose mutable entry storage")
	}
}

// SmallSafeMap 写进 BSON 再读回来，还是它自己：MarshalBSONValue 的签名必须是驱动 ValueMarshaler 的
// `(byte, []byte, error)`——`bson.Type` 是定义类型而不是别名，签名不匹配时接口静默未实现，类型退回默认结构体编码器，
// 未导出的字段全部丢失、得到空文档。
func TestSmallSafeMapSurvivesABSONRoundTrip(t *testing.T) {
	for name, src := range map[string]map[string]int64{
		"empty":   {},
		"one":     {"a": 1},
		"several": {"a": 1, "b": 2, "c": 3},
	} {
		t.Run(name, func(t *testing.T) {
			value := &SmallSafeMap[string, int64]{}
			value.SetRawMap(src)

			raw, err := bson.Marshal(bson.M{"m": value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back struct {
				M *SmallSafeMap[string, int64] `bson:"m"`
			}
			if err := bson.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.M == nil {
				t.Fatal("the map came back nil")
			}
			got := back.M.RawMap()
			if len(got) != len(src) {
				t.Fatalf("round trip produced %d entries, want %d (%v)", len(got), len(src), got)
			}
			for key, want := range src {
				if got[key] != want {
					t.Fatalf("round trip lost %q: got %d want %d", key, got[key], want)
				}
			}
		})
	}
}

// 写出去的必须是一个**文档**，不是默认结构体编码器产出的空文档。这条和上面那条分开，
// 是因为一个同样空的 map 会让上面那条在损坏的实现下也通过。
func TestSmallSafeMapEncodesItsContentsNotItsFields(t *testing.T) {
	value := &SmallSafeMap[string, int64]{}
	value.SetRawMap(map[string]int64{"a": 7})
	raw, err := bson.Marshal(bson.M{"m": value})
	if err != nil {
		t.Fatal(err)
	}
	var back bson.M
	if err := bson.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	document, ok := back["m"].(bson.D)
	if !ok {
		t.Fatalf("encoded as %T, want a document", back["m"])
	}
	if len(document) != 1 || document[0].Key != "a" {
		t.Fatalf("encoded as %v, want the map's own entries", document)
	}
}

// 让下一次写错签名在**编译期**就红，而不是等到某个文档变成 {}。
var (
	_ bson.ValueMarshaler   = (*SmallSafeMap[string, int64])(nil)
	_ bson.ValueUnmarshaler = (*SmallSafeMap[string, int64])(nil)
)

// FastMap.Range 的回调可以改这个 map，遍历仍然只交出真实存在的键，且每个未被删除的原有键恰好一次；回调里改已有键不触发扩容重排，
// Clear 不越界。生成 DAO 的 RangeX 对 map=fast 字段直接转发 FastMap.Range，`RangeItems(func(k, v) { SetItems(k, v*10) })`
// 这类写法就是这里的场景。
func TestFastMapRangeToleratesWritesFromTheCallback(t *testing.T) {
	fill := func() *FastMap[int64, int64] {
		m := NewInt64FastMap[int64](0)
		for i := int64(1); i <= 6; i++ { // 6 of 8 slots: the next growth check trips
			m.Set(i, i)
		}
		return m
	}
	exactlyOnce := func(t *testing.T, seen map[int64]int, keys ...int64) {
		t.Helper()
		if len(seen) != len(keys) {
			t.Fatalf("Range delivered %v, want each of %v exactly once", seen, keys)
		}
		for _, k := range keys {
			if seen[k] != 1 {
				t.Fatalf("Range delivered %v, want each of %v exactly once", seen, keys)
			}
		}
	}

	t.Run("update every existing key", func(t *testing.T) {
		m := fill()
		seen := map[int64]int{}
		m.Range(func(k, v int64) bool {
			seen[k]++
			m.Set(k, v*10)
			return true
		})
		exactlyOnce(t, seen, 1, 2, 3, 4, 5, 6)
		if m.Len() != 6 {
			t.Fatalf("Len = %d after updating six existing keys, want 6", m.Len())
		}
		for i := int64(1); i <= 6; i++ {
			if v, ok := m.Get(i); !ok || v != i*10 {
				t.Fatalf("key %d = %d,%v after the update pass, want %d", i, v, ok, i*10)
			}
		}
		if _, ok := m.Get(0); ok {
			t.Fatal("the update pass created key 0, which was never in the map")
		}
	})

	t.Run("insert keys until the table grows", func(t *testing.T) {
		m := fill()
		seen := map[int64]int{}
		m.Range(func(k, v int64) bool {
			if k <= 6 {
				seen[k]++
			}
			if k == 1 {
				for n := int64(100); n < 120; n++ {
					m.Set(n, n)
				}
			}
			if k == 0 {
				t.Fatal("Range delivered key 0, which was never in the map")
			}
			return true
		})
		exactlyOnce(t, seen, 1, 2, 3, 4, 5, 6)
		if m.Len() != 26 {
			t.Fatalf("Len = %d, want 26", m.Len())
		}
	})

	t.Run("delete a key not yet reached after growth", func(t *testing.T) {
		m := fill()
		var first int64
		deleted := map[int64]bool{}
		m.Range(func(k, v int64) bool {
			if deleted[k] {
				t.Fatalf("Range delivered key %d after the callback deleted it", k)
			}
			if first == 0 {
				first = k
				for n := int64(100); n < 120; n++ {
					m.Set(n, n)
				}
				for i := int64(1); i <= 6; i++ {
					if i != k {
						m.Delete(i)
						deleted[i] = true
					}
				}
			}
			return true
		})
	})

	t.Run("clear from the callback", func(t *testing.T) {
		m := fill()
		calls := 0
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Clear inside Range panicked: %v", r)
				}
			}()
			m.Range(func(int64, int64) bool {
				calls++
				m.Clear()
				return true
			})
		}()
		if calls != 1 {
			t.Fatalf("Range delivered %d entries, want 1: the map was cleared after the first", calls)
		}
		if m.Len() != 0 {
			t.Fatalf("Len = %d after Clear, want 0", m.Len())
		}
	})
}
