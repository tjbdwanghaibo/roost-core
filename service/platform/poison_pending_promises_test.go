package platform

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RR-20260920-05：一条读不回来的订单不能永久占住重试页。
//
// `PendingOrders` 每次返回索引里**最老的**一批 due id。对不可解码的记录，
// `AttemptDelivery` 返回解码错误；旧代码只记一条日志，条目的 score 原封不动，
// 于是下一 tick 枚举到的还是同一批。构造 batch 个更老的坏成员 + 1 个健康成员，
// 健康的那个永远进不了 `AttemptDelivery`。
//
// 承诺：坏记录被**推后**（不是退休——它是一条需要人看的真记录），于是页面往前走。

// scoredPending is an index with real scores, so "oldest first" and "deferred
// entries move" both mean something here.
type scoredPending struct {
	mu     sync.Mutex
	scores map[string]int64
	now    int64
}

func (p *scoredPending) PendingOrders(_ context.Context, limit int) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	type entry struct {
		id    string
		score int64
	}
	due := make([]entry, 0, len(p.scores))
	for id, score := range p.scores {
		if score <= p.now {
			due = append(due, entry{id: id, score: score})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].score != due[j].score {
			return due[i].score < due[j].score
		}
		return due[i].id < due[j].id
	})
	out := make([]string, 0, limit)
	for _, item := range due {
		if len(out) == limit {
			break
		}
		out = append(out, item.id)
	}
	return out, nil
}

func (p *scoredPending) DeferPending(_ context.Context, orderID string, notBefore time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.scores[orderID]; !ok {
		return nil
	}
	p.scores[orderID] = notBefore.Unix()
	return nil
}

func TestAnUndecodableOrderStopsBlockingThePage(t *testing.T) {
	const poisoned = RetryBatch
	index := &scoredPending{scores: map[string]int64{}, now: 1700000000}
	orders := versionstore.NewMemoryStore[string, Order]()
	// The poisoned members are older than the healthy one, so they sort first.
	for i := 0; i < poisoned; i++ {
		index.scores[poisonID(i)] = 1600000000 + int64(i)
	}
	index.scores["healthy"] = 1650000000

	service, err := New(Config{
		Orders:        &undecodableOrders{store: orders},
		Deliver:       newDeliverer(),
		Verifier:      acceptingVerifier(),
		Players:       resolver(),
		SessionSecret: "session", PaymentSecret: "payment",
		Pending: index,
		Now:     func() time.Time { return time.Unix(index.now, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}

	// One pass: every id on the page is unreadable.
	service.retryPendingOnce(context.Background())

	page, err := index.PendingOrders(context.Background(), RetryBatch)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range page {
		if id == "healthy" {
			return // the healthy order is now reachable, which is the promise
		}
	}
	t.Fatalf("after a pass over %d unreadable orders the page is still %v; the healthy order is never read", poisoned, page[:min(3, len(page))])
}

func poisonID(i int) string { return "poison-" + time.Unix(int64(i), 0).UTC().Format("150405.000") }

// undecodableOrders answers every Get with a malformed-record error, the way a
// store does when the bytes behind a key no longer decode.
type undecodableOrders struct {
	store versionstore.Store[string, Order]
}

func (o *undecodableOrders) Get(ctx context.Context, key string) (versionstore.Versioned[Order], bool, error) {
	if key == "healthy" {
		return o.store.Get(ctx, key)
	}
	return versionstore.Versioned[Order]{}, false, versionstore.ErrMalformedRecord
}

func (o *undecodableOrders) Update(ctx context.Context, key string, mutate versionstore.Mutate[Order]) (versionstore.Versioned[Order], bool, error) {
	if key == "healthy" {
		return o.store.Update(ctx, key, mutate)
	}
	return versionstore.Versioned[Order]{}, false, versionstore.ErrMalformedRecord
}

func (o *undecodableOrders) Create(ctx context.Context, key string, value Order) (versionstore.Versioned[Order], bool, error) {
	return o.store.Create(ctx, key, value)
}

func (o *undecodableOrders) Delete(ctx context.Context, key string, expect versionstore.Versioned[Order]) error {
	return o.store.Delete(ctx, key, expect)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
