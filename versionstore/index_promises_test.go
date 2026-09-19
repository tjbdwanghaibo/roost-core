package versionstore

import (
	"context"
	"testing"
)

// RR-20260919-04：一条记录和"它在不在待办集合里"必须是同一次写。
//
// 部署侧在记录写完之后补写索引，中间崩一次就留下一笔谁也枚举不到的记录——
// 对 platform 来说那是一笔已付款、后台循环永远找不到的订单。所以索引不该是
// 使用方在存储外面加的一层，而是存储自己的一部分：值怎么写，索引就怎么动，
// 值没写成功索引就不动。

type indexedRecord struct {
	Name    string `json:"name"`
	Pending bool   `json:"pending"`
	Due     int64  `json:"due"`
}

func newIndexedStore(t *testing.T, client RedisClient) *RedisStore[string, indexedRecord] {
	t.Helper()
	store, err := NewRedisStore(client, RedisConfig[string, indexedRecord]{
		Prefix: "test:rec:",
		KeyOf:  func(key string) string { return key },
		Codec:  JSONCodec[indexedRecord]{},
		Index: &RedisIndex[indexedRecord]{
			Key: "test:pending",
			Entry: func(value indexedRecord) (float64, bool) {
				return float64(value.Due), value.Pending
			},
		},
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

func TestAnIndexedCreateEntersTheIndex(t *testing.T) {
	client := newFakeRedis()
	store := newIndexedStore(t, client)
	ctx := context.Background()

	if _, created, err := store.Create(ctx, "a", indexedRecord{Name: "a", Pending: true, Due: 100}); err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	due, err := store.IndexDue(ctx, 1000, 10)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if len(due) != 1 || due[0] != "a" {
		t.Fatalf("index = %v, want the created record", due)
	}

	// A record that is not pending is not indexed at all: the index is the
	// set of things the reader has work for, not a copy of the keyspace.
	if _, created, err := store.Create(ctx, "b", indexedRecord{Name: "b", Due: 50}); err != nil || !created {
		t.Fatalf("create b: %v", err)
	}
	if due, _ := store.IndexDue(ctx, 1000, 10); len(due) != 1 {
		t.Fatalf("index = %v, want only the pending record", due)
	}
}

// The update that ends the work retires the entry, in the same write.
func TestAnIndexedUpdateRetiresTheEntry(t *testing.T) {
	client := newFakeRedis()
	store := newIndexedStore(t, client)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", indexedRecord{Name: "a", Pending: true, Due: 100}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, "a", func(current indexedRecord, _ bool) (indexedRecord, bool, error) {
		current.Pending = false
		return current, true, nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if due, _ := store.IndexDue(ctx, 1000, 10); len(due) != 0 {
		t.Fatalf("index = %v, want the finished record retired", due)
	}
}

// Due is a cursor, not a filter applied afterwards: an entry whose score is in
// the future is in the index and is not returned.
func TestTheIndexIsReadByScoreAndBounded(t *testing.T) {
	client := newFakeRedis()
	store := newIndexedStore(t, client)
	ctx := context.Background()
	for _, record := range []indexedRecord{
		{Name: "soon", Pending: true, Due: 100},
		{Name: "later", Pending: true, Due: 500},
		{Name: "never", Pending: true, Due: 900},
	} {
		if _, _, err := store.Create(ctx, record.Name, record); err != nil {
			t.Fatal(err)
		}
	}
	due, err := store.IndexDue(ctx, 600, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0] != "soon" || due[1] != "later" {
		t.Fatalf("due = %v, want the two due entries oldest first", due)
	}
	if due, _ := store.IndexDue(ctx, 50, 10); len(due) != 0 {
		t.Fatalf("due = %v, want nothing before the first score", due)
	}
}

// A store without an index keeps working, and asking one for its index says
// so rather than answering an empty list.
func TestAStoreWithoutAnIndexSaysSo(t *testing.T) {
	client := newFakeRedis()
	store, err := NewRedisStore(client, RedisConfig[string, indexedRecord]{
		Prefix: "test:rec:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[indexedRecord]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(context.Background(), "a", indexedRecord{Name: "a"}); err != nil {
		t.Fatalf("an unindexed store stopped working: %v", err)
	}
	if _, err := store.IndexDue(context.Background(), 100, 10); err == nil {
		t.Fatal("a store with no index answered an index read with an empty list")
	}
}

// RR-20260919-10：一个存储常常需要**每个所有者一份**待办清单，而不是一份全局的。
//
// activity 的 dispatch 就是这个形状：一个游戏服要问的是"欠我的有哪些"，
// 而不是"欠所有人的有哪些里哪些是我的"——后者要么是全局扫描，要么是按活动 id
// 去猜。索引键因此要能由值算出来。
//
// 约束是明确的：键只能取值里**不会变**的部分，因为一次写只会碰它当时算出来的
// 那个键；键变了，旧键上的条目就成了孤儿。

type ownedRecord struct {
	Owner   string `json:"owner"`
	Pending bool   `json:"pending"`
	Due     int64  `json:"due"`
}

func TestAPerOwnerIndexKeepsOwnersApart(t *testing.T) {
	client := newFakeRedis()
	store, err := NewRedisStore(client, RedisConfig[string, ownedRecord]{
		Prefix: "test:owned:",
		KeyOf:  func(key string) string { return key },
		Codec:  JSONCodec[ownedRecord]{},
		Index: &RedisIndex[ownedRecord]{
			KeyOf: func(value ownedRecord) string { return "test:owed:" + value.Owner },
			Entry: func(value ownedRecord) (float64, bool) { return float64(value.Due), value.Pending },
		},
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	for key, record := range map[string]ownedRecord{
		"a-1": {Owner: "a", Pending: true, Due: 100},
		"a-2": {Owner: "a", Pending: true, Due: 200},
		"b-1": {Owner: "b", Pending: true, Due: 150},
	} {
		if _, _, err := store.Create(ctx, key, record); err != nil {
			t.Fatal(err)
		}
	}

	owedA, err := store.IndexDueIn(ctx, "test:owed:a", 1000, 10)
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	if len(owedA) != 2 || owedA[0] != "a-1" || owedA[1] != "a-2" {
		t.Fatalf("owner a is owed %v, want its own two, oldest first", owedA)
	}
	owedB, _ := store.IndexDueIn(ctx, "test:owed:b", 1000, 10)
	if len(owedB) != 1 || owedB[0] != "b-1" {
		t.Fatalf("owner b is owed %v, want only its own", owedB)
	}

	// Finishing one owner's record leaves the other's alone.
	if _, _, err := store.Update(ctx, "a-1", func(current ownedRecord, _ bool) (ownedRecord, bool, error) {
		current.Pending = false
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if owed, _ := store.IndexDueIn(ctx, "test:owed:a", 1000, 10); len(owed) != 1 || owed[0] != "a-2" {
		t.Fatalf("owner a is owed %v after one was finished, want the other one", owed)
	}
	if owed, _ := store.IndexDueIn(ctx, "test:owed:b", 1000, 10); len(owed) != 1 {
		t.Fatalf("owner b's index was disturbed: %v", owed)
	}
}

// Exactly one of Key / KeyOf: a store that says both, or neither, is a
// configuration nobody can read.
func TestAnIndexNeedsExactlyOneKeyForm(t *testing.T) {
	client := newFakeRedis()
	for name, index := range map[string]*RedisIndex[ownedRecord]{
		"neither": {Entry: func(ownedRecord) (float64, bool) { return 0, true }},
		"both": {
			Key:   "test:owed",
			KeyOf: func(ownedRecord) string { return "test:owed:x" },
			Entry: func(ownedRecord) (float64, bool) { return 0, true },
		},
	} {
		_, err := NewRedisStore(client, RedisConfig[string, ownedRecord]{
			Prefix: "test:owned:", KeyOf: func(key string) string { return key },
			Codec: JSONCodec[ownedRecord]{}, Index: index,
		})
		if err == nil {
			t.Errorf("%s: an index with an unclear key form was accepted", name)
		}
	}
}
