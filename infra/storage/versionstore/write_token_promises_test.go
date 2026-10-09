package versionstore

// A2 ③：写命令的回复丢失时（脚本已在服务端执行、调用方只拿到传输错误），调用方的重试不能把
// 自己那次已生效的写再生效一次，也不能把它判成冲突。
//
// 修前：store 把传输错误原样返回。调用方重试 Update，mutate 叠在自己已生效的写上（Total 2 / v3）；
// 重试 Create，键已被自己建好，返回 created=false。
//
// 承诺：每次写带一个一次性令牌，与值在同一条 SET 里写进信封；结果未知时 store 先核对令牌，
// 已生效就返回那一次的结果，确定没执行才原样重发，无法证明时返回 ErrOutcomeUnknown。
//
// 替身：fakeRedis 的 compare-and-set 是逐字节比较，与 Lua 一致；loseReplies / dropWrites /
// failGets 注入“执行后丢回复”“没执行就断开”“核对时读不到”。

import (
	"context"
	"errors"
	"testing"
)

func newTokenTestStore(t *testing.T, fake *fakeRedis) *RedisStore[string, counter] {
	t.Helper()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		Prefix: "tok:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		RetryBackoff: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// loseReplyUnchecked makes the next write lose its reply (dropped: without
// running) and the read the store checks it with fail, so the outcome stays
// open and the caller gets ErrOutcomeUnknown.
func loseReplyUnchecked(fake *fakeRedis, dropped bool) {
	if dropped {
		fake.dropWrites = 1
	} else {
		fake.loseReplies = 1
	}
	fake.afterLostReply = func() {
		fake.afterLostReply = nil
		fake.failGets = 1
	}
}

func increment(current counter, _ bool) (counter, bool, error) {
	current.Total++
	return current, true, nil
}

// 调用方的写法就是最朴素的“出错就再调一次”。
func TestAnUpdateWhoseReplyIsLostIsNotAppliedTwiceWhenTheCallerRetries(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	fake.loseReplies = 1
	got, applied, err := store.Update(ctx, "a", increment)
	if err != nil {
		got, applied, err = store.Update(ctx, "a", increment)
	}
	stored, _, readErr := store.Get(ctx, "a")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil || !applied || got.Version != 2 || got.Value.Total != 1 || stored.Value.Total != 1 || stored.Version != 2 {
		t.Fatalf("one increment whose reply was lost: update returned %+v applied=%v err=%v; store holds %+v (want total 1 at v2)",
			got, applied, err, stored)
	}
}

func TestACreateWhoseReplyIsLostIsNotReportedAsTakenWhenTheCallerRetries(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()
	fake.loseReplies = 1
	got, created, err := store.Create(ctx, "a", counter{Name: "a", Total: 7})
	if err != nil {
		got, created, err = store.Create(ctx, "a", counter{Name: "a", Total: 7})
	}
	if err != nil || !created || got.Version != 1 || got.Value.Total != 7 {
		t.Fatalf("a create whose reply was lost was reported as created=%v %+v err=%v; it is this caller's own write", created, got, err)
	}
}

// 回复丢失之后、调用方再看之前，别人又写了一次：我的写仍然已生效，不能再叠一次。
func TestAnUpdateWhoseReplyIsLostStaysAppliedOnceAfterSomeoneElseWritesOnTop(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	other := newTokenTestStore(t, fake)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	fake.loseReplies = 1
	fake.afterLostReply = func() {
		fake.afterLostReply = nil
		if _, _, err := other.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			c.Total += 100
			return c, true, nil
		}); err != nil {
			t.Errorf("other writer: %v", err)
		}
	}
	got, applied, err := store.Update(ctx, "a", increment)
	if err != nil {
		got, applied, err = store.Update(ctx, "a", increment)
	}
	stored, _, _ := store.Get(ctx, "a")
	if err != nil || !applied || got.Version != 2 || got.Value.Total != 1 || stored.Value.Total != 101 || stored.Version != 3 {
		t.Fatalf("update returned %+v applied=%v err=%v; store holds %+v (want this write at v2 total 1, store total 101 at v3)",
			got, applied, err, stored)
	}
}

// 命令根本没执行（发出前断开）：store 核对后原样重发，只生效一次。
func TestAnUpdateThatNeverRanIsResentOnce(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	fake.dropWrites = 1
	got, applied, err := store.Update(ctx, "a", increment)
	if err != nil {
		got, applied, err = store.Update(ctx, "a", increment)
	}
	stored, _, _ := store.Get(ctx, "a")
	if err != nil || !applied || got.Version != 2 || stored.Value.Total != 1 || stored.Version != 2 {
		t.Fatalf("update returned %+v applied=%v err=%v; store holds %+v (want total 1 at v2)", got, applied, err, stored)
	}
}

// 核对时读不到（后端不答）：store 不猜，返回 ErrOutcomeUnknown；调用方用 Resume 再调一次，
// 拿到上一次那次写的结果，不再写第二次。
func TestResumeReturnsTheEarlierWriteInsteadOfWritingAgain(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	loseReplyUnchecked(fake, false)
	_, applied, err := store.Update(ctx, "a", increment)
	var unknown *UnknownOutcomeError
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, errLostReply) || !errors.As(err, &unknown) || applied || unknown.Token == "" {
		t.Fatalf("an unsettled lost reply returned applied=%v err=%v; want *UnknownOutcomeError wrapping the transport error", applied, err)
	}
	got, applied, err := store.Update(Resume(ctx, err), "a", increment)
	stored, _, _ := store.Get(ctx, "a")
	if err != nil || !applied || got.Version != 2 || got.Value.Total != 1 || stored.Value.Total != 1 || stored.Version != 2 {
		t.Fatalf("resumed update returned %+v applied=%v err=%v; store holds %+v (want the earlier write: total 1 at v2)", got, applied, err, stored)
	}

	loseReplyUnchecked(fake, false)
	_, _, err = store.Create(ctx, "b", counter{Name: "b", Total: 3})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("create: %v", err)
	}
	created, ok, err := store.Create(Resume(ctx, err), "b", counter{Name: "b", Total: 3})
	if err != nil || !ok || created.Version != 1 || created.Value.Total != 3 {
		t.Fatalf("resumed create returned %+v created=%v err=%v; the earlier create landed", created, ok, err)
	}
}

// 上一次确定没生效（别人先写了那个版本）：Resume 照常执行这次写，只生效一次。
func TestResumeAfterTheEarlierWriteProvablyLostPerformsTheWrite(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	other := newTokenTestStore(t, fake)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	loseReplyUnchecked(fake, true)
	_, _, err := store.Update(ctx, "a", increment)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("update: %v", err)
	}
	if _, _, err := other.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
		c.Total += 100
		return c, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, applied, err := store.Update(Resume(ctx, err), "a", increment)
	if err != nil || !applied || got.Version != 3 || got.Value.Total != 101 {
		t.Fatalf("resumed update after a provable loss returned %+v applied=%v err=%v; want total 101 at v3", got, applied, err)
	}
}

// 同一个令牌、不同的值：Resume 报 ErrWriteTokenMismatch，什么都不写。
func TestResumeRefusesTheSameTokenForADifferentWrite(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()

	loseReplyUnchecked(fake, false)
	_, _, createErr := store.Create(ctx, "c", counter{Name: "c", Total: 1})
	if !errors.Is(createErr, ErrOutcomeUnknown) {
		t.Fatalf("create: %v", createErr)
	}
	if _, _, err := store.Create(Resume(ctx, createErr), "c", counter{Name: "c", Total: 2}); !errors.Is(err, ErrWriteTokenMismatch) {
		t.Fatalf("create resumed with a different value returned %v, want ErrWriteTokenMismatch", err)
	}
	if _, _, err := store.Update(Resume(ctx, createErr), "c", increment); !errors.Is(err, ErrWriteTokenMismatch) {
		t.Fatalf("an Update resuming a Create returned %v, want ErrWriteTokenMismatch", err)
	}

	loseReplyUnchecked(fake, false)
	_, _, updateErr := store.Update(ctx, "c", increment)
	if !errors.Is(updateErr, ErrOutcomeUnknown) {
		t.Fatalf("update: %v", updateErr)
	}
	addTwo := func(c counter, _ bool) (counter, bool, error) {
		c.Total += 2
		return c, true, nil
	}
	if _, _, err := store.Update(Resume(ctx, updateErr), "c", addTwo); !errors.Is(err, ErrWriteTokenMismatch) {
		t.Fatalf("update resumed with a different mutate returned %v, want ErrWriteTokenMismatch", err)
	}
	stored, _, _ := store.Get(ctx, "c")
	if stored.Version != 2 || stored.Value.Total != 2 {
		t.Fatalf("refused resumes changed the store: %+v (want the create and the one increment: total 2 at v2)", stored)
	}

	loseReplyUnchecked(fake, false)
	deleteErr := store.Delete(ctx, "c", stored)
	if !errors.Is(deleteErr, ErrOutcomeUnknown) {
		t.Fatalf("delete: %v", deleteErr)
	}
	if err := store.Delete(Resume(ctx, deleteErr), "c", Versioned[counter]{Version: 1}); !errors.Is(err, ErrWriteTokenMismatch) {
		t.Fatalf("delete resumed holding another version returned %v, want ErrWriteTokenMismatch", err)
	}

	// Writes to other keys ignore the resumed state.
	if _, ok, err := store.Create(Resume(ctx, createErr), "other", counter{Name: "other"}); err != nil || !ok {
		t.Fatalf("a create on another key under a resumed context: created=%v err=%v", ok, err)
	}
}

// 回复丢失之后，别人写到把我的令牌挤出了保留范围：无法证明，返回 ErrOutcomeUnknown，不猜。
func TestATokenPushedOutOfTheHistoryIsNotGuessed(t *testing.T) {
	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		Prefix: "tok:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		RetryBackoff: -1, WriteTokenHistory: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	fake.loseReplies = 1
	fake.afterLostReply = func() {
		fake.afterLostReply = nil
		for i := 0; i < 2; i++ {
			if _, _, err := store.Update(ctx, "a", increment); err != nil {
				t.Errorf("other writer: %v", err)
			}
		}
	}
	_, applied, err := store.Update(ctx, "a", increment)
	if !errors.Is(err, ErrOutcomeUnknown) || applied {
		t.Fatalf("a token pushed out of the history returned applied=%v err=%v, want ErrOutcomeUnknown", applied, err)
	}
	stored, _, _ := store.Get(ctx, "a")
	if stored.Value.Total != 3 || stored.Version != 4 {
		t.Fatalf("store holds %+v, want total 3 at v4 (this write once, the others twice)", stored)
	}
}

// 键原本不存在、核对时仍不存在：分不清“没执行”与“执行后被别人删掉”，不重发、不猜。
// Delete 不留令牌，删掉之后同样无从核对。键还在原值时 Delete 原样重发，键被别人改过时是确定的冲突。
func TestAbsenceProvesNothing(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()

	fake.dropWrites = 1
	if _, _, err := store.Create(ctx, "never", counter{}); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a create that never ran on an absent key returned %v, want ErrOutcomeUnknown", err)
	}
	if _, found, _ := store.Get(ctx, "never"); found {
		t.Fatal("an unproven create was resent")
	}

	held, _, err := store.Create(ctx, "d", counter{Name: "d"})
	if err != nil {
		t.Fatal(err)
	}
	fake.loseReplies = 1
	if err := store.Delete(ctx, "d", held); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a delete whose reply was lost returned %v, want ErrOutcomeUnknown", err)
	}

	held, _, _ = store.Create(ctx, "d", counter{Name: "d"})
	fake.dropWrites = 1
	if err := store.Delete(ctx, "d", held); err != nil {
		t.Fatalf("a delete that never ran was not resent: %v", err)
	}
	if _, found, _ := store.Get(ctx, "d"); found {
		t.Fatal("the resent delete did not delete")
	}

	held, _, _ = store.Create(ctx, "d", counter{Name: "d"})
	fake.dropWrites = 1
	fake.afterLostReply = func() {
		fake.afterLostReply = nil
		if _, _, err := store.Update(ctx, "d", increment); err != nil {
			t.Errorf("other writer: %v", err)
		}
	}
	if err := store.Delete(ctx, "d", held); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("a delete that never ran, after someone else wrote, returned %v, want ErrVersionMismatch", err)
	}
}

// 令牌与值在同一条 SET 里：信封头带最近 WriteTokenHistory 个令牌，新的在前。
func TestTheEnvelopeKeepsABoundedTokenHistory(t *testing.T) {
	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		Prefix: "tok:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{}, WriteTokenHistory: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{}); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for i := 0; i < 5; i++ {
		header, err := parseEnvelope(fake.values["tok:a"])
		if err != nil {
			t.Fatal(err)
		}
		seen = append([]string{header.tokens[0]}, seen...)
		if want := min(len(seen), 3); len(header.tokens) != want || header.version != uint64(i+1) {
			t.Fatalf("v%d keeps %d tokens %v, want %d", header.version, len(header.tokens), header.tokens, want)
		}
		for j, token := range header.tokens {
			if token != seen[j] || len(token) != 11 {
				t.Fatalf("v%d tokens %v, want newest first %v", header.version, header.tokens, seen[:len(header.tokens)])
			}
		}
		if _, _, err := store.Update(ctx, "a", increment); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{}, WriteTokenHistory: -1,
	}); err == nil {
		t.Fatal("a negative token history was accepted")
	}
}
