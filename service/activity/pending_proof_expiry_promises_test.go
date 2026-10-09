package activity

// RR-20261001-05：ledger 条目被 TTL 删除后，参与者 PendingRequestIDs 里的孤儿证明
// 必须能回收；满了的时候要报“背压”而不是“冲突”；owner 进程要有对账入口。
//
// 旧行为（RR-20260929-02 修法引入）：applyProgress 的确认扫描只认
// `found && Applied`，`!found` 的 pending 永远留着；32 个“CAS 成功、mark 失败、
// 客户端没在 ReservationTTL 内重放”的孤儿之后，该参与者每个新请求都被
// `versionstore.ErrConflict: pending progress confirmations are full` 拒绝，
// ledger 恢复也无用，且没有管理入口。
//
// 后端由 ROOST_REVIEW4_BACKEND 选：默认 Memory（ledger 无 TTL，用版本化 Delete
// 代替到期）；"redis" 时用 ROOST_REVIEW_REDIS 指向的隔离 Redis，ledger 配 3s TTL，
// 真等它过期。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type rr05Fixture struct {
	s *Service
	// ledger is the healthy ledger; the fixture starts the service on a
	// wrapper whose mark always fails, and a test swaps this back in once the
	// orphans exist.
	ledger versionstore.Store[RequestKey, ProgressReservation]
	redis  bool
	// expire makes the ledger forget keys the way the backend does.
	expire func(t *testing.T, keys []RequestKey)
}

func newRR05Fixture(t *testing.T) *rr05Fixture {
	t.Helper()
	f := &rr05Fixture{redis: os.Getenv("ROOST_REVIEW4_BACKEND") == "redis"}
	const redisTTL = 3 * time.Second
	f.s, _ = newActivityService(t, func(c *Config) {
		if f.redis {
			addr := os.Getenv("ROOST_REVIEW_REDIS")
			if addr == "" {
				t.Fatal("isolated Redis address required (ROOST_REVIEW_REDIS)")
			}
			client, err := driver.NewClient(fredis.DefaultConfig(addr))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			stores, err := NewRedisStores(client, fmt.Sprintf("rr05:activity:%d", time.Now().UnixNano()), redisTTL)
			if err != nil {
				t.Fatal(err)
			}
			c.Activities, c.Participants, c.Ledger = stores.Activities, stores.Participants, stores.Ledger
			c.Audits, c.Dispatches, c.Windows = stores.Audits, stores.Dispatches, stores.Windows
			c.ReservationTTL = redisTTL
		}
		f.ledger = c.Ledger
		c.Ledger = failedProgressMark{c.Ledger}
	})
	f.expire = func(t *testing.T, keys []RequestKey) {
		t.Helper()
		ctx := context.Background()
		if !f.redis {
			for _, k := range keys {
				stored, found, err := f.ledger.Get(ctx, k)
				if err != nil || !found {
					t.Fatalf("ledger entry %s: found=%v err=%v", k, found, err)
				}
				if err := f.ledger.Delete(ctx, k, stored); err != nil {
					t.Fatal(err)
				}
			}
			return
		}
		// A real TTL: wait for Redis to reap every key, bounded by well over
		// the configured TTL so a slow box does not turn this into a flake.
		deadline := time.Now().Add(redisTTL * 5)
		for _, k := range keys {
			for {
				_, found, err := f.ledger.Get(ctx, k)
				if err != nil {
					t.Fatal(err)
				}
				if !found {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("ledger entry %s did not expire within %s", k, redisTTL*5)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	return f
}

// orphanPendingProofs fills the participant's pending window with requests
// whose participant CAS landed and whose ledger mark failed — the exact state
// a Redis blip between the two writes leaves behind.
func orphanPendingProofs(t *testing.T, s *Service, key Key) []RequestKey {
	t.Helper()
	ctx := context.Background()
	keys := make([]RequestKey, 0, MaxProgressWindow)
	for i := 0; i < MaxProgressWindow; i++ {
		id := fmt.Sprintf("lost-%d", i)
		if _, err := s.ApplyProgress(ctx, key, "p", id, ProgressDelta{Score: 1}); err == nil {
			t.Fatal("expected mark failure")
		}
		keys = append(keys, RequestKey{Activity: key, ParticipantID: "p", RequestID: id})
	}
	p, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || p.Score != MaxProgressWindow || len(p.PendingRequestIDs) != MaxProgressWindow {
		t.Fatalf("orphan setup: %+v err=%v", p, err)
	}
	return keys
}

// 承诺：ledger 把一条 reservation 按 TTL 删掉之后，它在参与者上的 pending 证明已经
// 越过了客户端的重试地平线（ledger 的既有契约：TTL 之后的重放等同新请求），
// 回收它；一个曾经积满 32 个孤儿的参与者在 ledger 恢复后能重新计分。
func TestExpiredLedgerEntriesFreeOrphanedPendingProofs(t *testing.T) {
	f := newRR05Fixture(t)
	ctx := context.Background()
	key := activityKey("orphan-pending")
	openActivity(t, f.s, key, 1)
	keys := orphanPendingProofs(t, f.s, key)
	f.s.cfg.Ledger = f.ledger // the ledger is healthy again

	if !f.redis {
		// Inside the TTL the proofs still guard the participant: the window
		// is full and says so. Pinned on the Memory backend only, where "inside
		// the TTL" is not a race against the clock (the Redis fixture's TTL is
		// seconds long and the entries above take real time to write).
		if _, err := f.s.ApplyProgress(ctx, key, "p", "too-early", ProgressDelta{Score: 1}); err == nil {
			t.Fatal("full pending window admitted a request while its ledger entries were alive")
		}
	}

	f.expire(t, keys)
	if _, err := f.s.ApplyProgress(ctx, key, "p", "fresh-1", ProgressDelta{Score: 1}); err != nil {
		t.Fatalf("participant permanently refused after ledger expiry: %v", err)
	}
	p, _, err := f.s.LookupParticipant(ctx, key, "p")
	if err != nil || p.Score != MaxProgressWindow+1 || len(p.PendingRequestIDs) != 0 {
		t.Fatalf("after reclaim: %+v err=%v", p, err)
	}
	// A replay of an orphan that arrives after the TTL is, by the ledger's
	// stated contract, indistinguishable from a new request and applies as
	// one. The proof it used to have stopped protecting anything the moment
	// the ledger forgot the reservation; this pins what a late replay gets.
	again, err := f.s.ApplyProgress(ctx, key, "p", "lost-0", ProgressDelta{Score: 1})
	if err != nil || again.Score != MaxProgressWindow+2 {
		t.Fatalf("post-TTL replay of an orphan is a new request by contract: %+v err=%v", again, err)
	}
}

// 承诺：pending 窗口满是背压，不是 CAS 冲突——调用方拿到 ErrProgressBacklog /
// CodeProgressBacklog，能和 CodeConflict（立刻重试）区分开；拒绝不改参与者。
// 修前这里是 versionstore.ErrConflict → CodeConflict（见上一个用例的修前失败文本）。
func TestFullPendingProofWindowReportsBacklogNotConflict(t *testing.T) {
	f := newRR05Fixture(t)
	ctx := context.Background()
	key := activityKey("backlog-code")
	openActivity(t, f.s, key, 1)
	orphanPendingProofs(t, f.s, key)
	f.s.cfg.Ledger = f.ledger
	_, err := f.s.ApplyProgress(ctx, key, "p", "one-more", ProgressDelta{Score: 1})
	if !errors.Is(err, ErrProgressBacklog) {
		t.Fatalf("full window reported %v, want ErrProgressBacklog", err)
	}
	if errors.Is(err, versionstore.ErrConflict) {
		t.Fatalf("backpressure must not look like compare-and-set contention: %v", err)
	}
	if code := Code(err); code != CodeProgressBacklog {
		t.Fatalf("full window maps to code %d, want CodeProgressBacklog %d", code, CodeProgressBacklog)
	}
	p, _, err := f.s.LookupParticipant(ctx, key, "p")
	if err != nil || p.Score != MaxProgressWindow || len(p.PendingRequestIDs) != MaxProgressWindow {
		t.Fatalf("the refusal changed the participant: %+v err=%v", p, err)
	}
}

// 承诺：owner 进程可以用 Admin.ReconcileProgress 对账——把每个 pending 证明缺的那一步
// ledger mark 补上并释放证明；补过的 requestID 重放仍是 no-op；ledger 还写不了时
// 失败且不动参与者；需要操作员备注。修前没有这个入口。
func TestReconcileProgressCompletesLostMarksAndFreesTheParticipant(t *testing.T) {
	f := newRR05Fixture(t)
	ctx := context.Background()
	key := activityKey("reconcile")
	openActivity(t, f.s, key, 1)
	keys := orphanPendingProofs(t, f.s, key)

	if _, err := f.s.ReconcileProgress(ctx, key, "p", "  "); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("reconcile without a note: %v, want ErrAdminNoteRequired", err)
	}
	// While the ledger still cannot be written the reconciliation fails
	// loudly and releases nothing.
	if _, err := f.s.ReconcileProgress(ctx, key, "p", "ledger still down"); err == nil {
		t.Fatal("reconcile succeeded without being able to mark the ledger")
	}
	p, _, err := f.s.LookupParticipant(ctx, key, "p")
	if err != nil || len(p.PendingRequestIDs) != MaxProgressWindow || p.AdminNote != "" {
		t.Fatalf("a failed reconcile touched the participant: %+v err=%v", p, err)
	}

	f.s.cfg.Ledger = f.ledger
	got, err := f.s.ReconcileProgress(ctx, key, "p", "redis blip 2026-10-01; clients idle")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PendingRequestIDs) != 0 || got.Score != MaxProgressWindow || got.Applies != MaxProgressWindow ||
		got.AdminNote != "redis blip 2026-10-01; clients idle" || got.AdminActionAtUnix == 0 {
		t.Fatalf("reconcile result: %+v", got)
	}
	for _, k := range keys {
		entry, found, err := f.ledger.Get(ctx, k)
		if err != nil || !found || entry.Value.State != ReservationApplied {
			t.Fatalf("reconcile did not complete the mark for %s: found=%v state=%q err=%v", k, found, entry.Value.State, err)
		}
	}
	// A replay of a reconciled request is answered by the ledger: no second
	// apply, Applies does not move.
	again, err := f.s.ApplyProgress(ctx, key, "p", "lost-0", ProgressDelta{Score: 1})
	if err != nil || again.Score != MaxProgressWindow || again.Applies != MaxProgressWindow {
		t.Fatalf("replay after reconcile applied again: %+v err=%v", again, err)
	}
	if _, err := f.s.ApplyProgress(ctx, key, "p", "fresh-1", ProgressDelta{Score: 1}); err != nil {
		t.Fatalf("participant still refused after reconcile: %v", err)
	}
	if _, err := f.s.ReconcileProgress(ctx, key, "p", "nothing pending"); err != nil {
		t.Fatalf("reconcile with nothing pending must be a harmless no-op: %v", err)
	}
	if _, err := f.s.ReconcileProgress(ctx, key, "nobody", "no such participant"); !errors.Is(err, ErrMissing) {
		t.Fatalf("reconcile of an unknown participant: %v, want ErrMissing", err)
	}
}
