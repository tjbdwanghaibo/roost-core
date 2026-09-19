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
