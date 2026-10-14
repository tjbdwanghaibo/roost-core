// 活动进度：预留证明不被回收后再应用、未确认进度不被淘汰、正数不回绕、孤儿证明回收与背压、对账、集群部分完成恢复。

package activity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type failedProgressMark struct {
	versionstore.Store[RequestKey, ProgressReservation]
}

type pausedParticipantWrite struct {
	versionstore.Store[ParticipantKey, Participant]
	once             atomic.Bool
	entered, release chan struct{}
}

func (s *pausedParticipantWrite) Update(ctx context.Context, k ParticipantKey, m versionstore.Mutate[Participant]) (versionstore.Versioned[Participant], bool, error) {
	if s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.Store.Update(ctx, k, m)
}
func TestStaleReservedReaderCannotApplyAfterProofWasReclaimed(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.Ledger = &lostReplyReservations{c.Ledger, true} })
	ctx := context.Background()
	key := activityKey("stale-reserved")
	openActivity(t, s, key, 1)
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("expected mark failure")
	}
	gate := &pausedParticipantWrite{Store: s.cfg.Participants, entered: make(chan struct{}), release: make(chan struct{})}
	s.cfg.Participants = gate
	done := make(chan error, 1)
	go func() { _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); done <- err }()
	<-gate.entered
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err != nil {
		close(gate.release)
		<-done
		t.Fatal(err)
	}
	for i := 0; i < MaxProgressWindow; i++ {
		if _, err := s.ApplyProgress(ctx, key, "p", fmt.Sprintf("new-%d", i), ProgressDelta{Score: 1}); err != nil {
			close(gate.release)
			<-done
			t.Fatal(err)
		}
	}
	before, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || before.Applied("lost") {
		close(gate.release)
		<-done
		t.Fatal("proof was not reclaimed", before, err)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || after.Score != before.Score || after.Applies != before.Applies {
		t.Fatalf("stale reader reapplied: before=%+v after=%+v err=%v", before, after, err)
	}
}

func (s failedProgressMark) Update(context.Context, RequestKey, versionstore.Mutate[ProgressReservation]) (versionstore.Versioned[ProgressReservation], bool, error) {
	return versionstore.Versioned[ProgressReservation]{}, false, errors.New("mark unavailable")
}

func TestUnconfirmedProgressBackpressureNeverEvictsProof(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.Ledger = failedProgressMark{c.Ledger} })
	ctx := context.Background()
	key := activityKey("full-proof")
	openActivity(t, s, key, 1)
	for i := 0; i < MaxProgressWindow; i++ {
		if _, err := s.ApplyProgress(ctx, key, "p", fmt.Sprint(i), ProgressDelta{Score: 1}); err == nil {
			t.Fatal("missing mark failure")
		}
	}
	before, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || before.Score != MaxProgressWindow || len(before.PendingRequestIDs) != MaxProgressWindow {
		t.Fatal(before, err)
	}
	// Full inside the TTL is backpressure (ErrProgressBacklog since
	// RR-20261001-05; it was versionstore.ErrConflict before), never eviction.
	if _, err = s.ApplyProgress(ctx, key, "p", "extra", ProgressDelta{Score: 1}); !errors.Is(err, ErrProgressBacklog) {
		t.Fatalf("full proof ignored: %v", err)
	}
	after, _, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || after.Score != before.Score || !after.Applied("0") {
		t.Fatal(after, err)
	}
	if _, err = s.ApplyProgress(ctx, key, "p", "0", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("mark failure hidden")
	}
	after, _, _ = s.LookupParticipant(ctx, key, "p")
	if after.Score != before.Score {
		t.Fatalf("old pending proof applied twice: %+v", after)
	}
}

// ledger 条目被 TTL 删除后参与者 PendingRequestIDs 里的孤儿证明能回收；满了时报“背压”而不是“冲突”；owner 进程有对账入口。
// 后端由 ROOST_REVIEW4_BACKEND 选：默认 Memory（ledger 无 TTL，用版本化 Delete 代替到期）；"redis" 时用 ROOST_REVIEW_REDIS 指向的隔离
// Redis，ledger 配 3s TTL，真等它过期。
// 历史：RR-20261001-05。

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
			stores, err := NewRedisStores(client, redistest.Prefix(t, addr, "rr05:activity"), redisTTL)
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

// pending 窗口满是背压，不是 CAS 冲突：调用方拿到 ErrProgressBacklog / CodeProgressBacklog，能和 CodeConflict（立刻重试）区分开；拒绝不改参与者。
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

// owner 进程可以用 Admin.ReconcileProgress 对账：把每个 pending 证明缺的那一步 ledger mark 补上并释放证明；补过的 requestID 重放仍是 no-op；
// ledger 还写不了时失败且不动参与者；需要操作员备注。
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

type lostReplyReservations struct {
	versionstore.Store[RequestKey, ProgressReservation]
	fail bool
}

func (s *lostReplyReservations) Update(ctx context.Context, k RequestKey, m versionstore.Mutate[ProgressReservation]) (versionstore.Versioned[ProgressReservation], bool, error) {
	if k.RequestID == "lost" && s.fail {
		s.fail = false
		return versionstore.Versioned[ProgressReservation]{}, false, fmt.Errorf("injected mark failure")
	}
	return s.Store.Update(ctx, k, m)
}

func TestLastDispatchRetryKeepsAckWindow(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.DispatchMaxAttempts = 1 })
	ctx := context.Background()
	key := activityKey("last-attempt")
	openActivity(t, s, key, 1)
	notify(t, s, key, 1)
	first, err := s.AttemptDispatch(ctx, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, retryErr := s.AttemptDispatch(ctx, key, 1)
	_, ackErr := s.AckDispatch(ctx, key, 1, first.Token)
	if !errors.Is(retryErr, ErrDispatchNotDue) || ackErr != nil {
		t.Fatalf("unelapsed final ACK window was closed: retry=%v ack=%v", retryErr, ackErr)
	}
}
func TestUnconfirmedProgressSurvivesRingEviction(t *testing.T) {
	s, _ := newActivityService(t, func(c *Config) { c.Ledger = &lostReplyReservations{c.Ledger, true} })
	ctx := context.Background()
	key := activityKey("ring")
	openActivity(t, s, key, 1)
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err == nil {
		t.Fatal("expected mark failure")
	}
	for i := 0; i < MaxProgressWindow; i++ {
		if _, err := s.ApplyProgress(ctx, key, "p", fmt.Sprintf("later-%d", i), ProgressDelta{Score: 1}); err != nil {
			t.Fatal(err)
		}
	}
	before, _, _ := s.LookupParticipant(ctx, key, "p")
	if _, err := s.ApplyProgress(ctx, key, "p", "lost", ProgressDelta{Score: 1}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := s.LookupParticipant(ctx, key, "p")
	if after.Score != before.Score {
		t.Fatalf("unconfirmed request reapplied inside TTL: %d -> %d", before.Score, after.Score)
	}
}

func TestPositiveProgressCannotWrapNegative(t *testing.T) {
	s, _ := newActivityService(t)
	ctx := context.Background()
	key := activityKey("overflow")
	openActivity(t, s, key, 1)
	if _, err := s.ApplyProgress(ctx, key, "p", "first", ProgressDelta{Score: math.MaxInt64, Progress: math.MaxInt64}); err != nil {
		t.Fatal(err)
	}
	_, applyErr := s.ApplyProgress(ctx, key, "p", "second", ProgressDelta{Score: 1, Progress: 1})
	p, found, err := s.LookupParticipant(ctx, key, "p")
	if err != nil || !found {
		t.Fatal(err)
	}
	if !errors.Is(applyErr, ErrRequestInvalid) || p.Score != math.MaxInt64 || p.Progress != math.MaxInt64 || p.Applies != 1 {
		t.Fatalf("positive delta persisted negative standing: score=%d progress=%d", p.Score, p.Progress)
	}
}

func TestActivityClusterRecoversPartialCompletion(t *testing.T) {
	m, err := clusterBackedService(t, fmt.Sprintf("bugfix7:{activity-%d}", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k := activityKey("cluster-review")
	base := m.cfg
	m.cfg.Dispatches = failingSecondGameDispatches{base.Dispatches}
	if _, err := m.OpenActivity(ctx, k, []int32{1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NotifyPhase(ctx, k, 1); err != nil {
		t.Fatal(err)
	}
	complete, err := m.NotifyPhase(ctx, k, 2)
	if !errors.Is(err, context.DeadlineExceeded) || complete.Status != StatusComplete {
		t.Fatalf("partial completion: %+v %v", complete, err)
	}
	first, found, err := m.LookupDispatch(ctx, k, 1)
	if err != nil || !found {
		t.Fatalf("first durable dispatch: %+v %v", first, err)
	}
	// A new service object over the same Redis repairs the complete aggregate.
	restarted, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AdvanceExpired(ctx, k.GroupID, 10); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []int32{1, 2} {
		d, found, err := restarted.LookupDispatch(ctx, k, sid)
		if err != nil || !found {
			t.Fatalf("restored %d: %+v %v", sid, d, err)
		}
		if sid == 1 && d.Token != first.Token {
			t.Fatal("recovery replaced the durable token")
		}
		if owed, err := restarted.OwedDispatches(ctx, k.GroupID, sid, 10); err != nil || len(owed) != 1 {
			t.Fatalf("owed %d: %v %v", sid, owed, err)
		}
		if _, err := restarted.AckDispatch(ctx, k, sid, d.Token); err != nil {
			t.Fatal(err)
		}
	}
}

type failingSecondGameDispatches struct {
	versionstore.Store[DispatchKey, Dispatch]
}

func (s failingSecondGameDispatches) Create(ctx context.Context, key DispatchKey, value Dispatch) (versionstore.Versioned[Dispatch], bool, error) {
	if key.GameSID == 2 {
		return versionstore.Versioned[Dispatch]{}, false, context.DeadlineExceeded
	}
	return s.Store.Create(ctx, key, value)
}

// 存储恢复故障留在领域包；Wiring 的真实集群生命周期由 TaggedClusterLifecycle 单独验证。
func clusterBackedService(t *testing.T, prefix string) (*Service, error) {
	t.Helper()
	addr := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addr == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := redis.DefaultConfig("")
	cfg.ClusterAddrs = strings.Split(addr, ",")
	client, err := driver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	stores, err := NewRedisStores(client, prefix, time.Hour)
	if err != nil {
		return nil, err
	}
	key := activityKey("cluster-review")
	t.Cleanup(func() {
		_, _ = client.Del(context.Background(), prefix+":win:"+key.GroupID, prefix+":act:"+key.String(), prefix+":disp:"+(DispatchKey{Activity: key, GameSID: 1}).String(), prefix+":disp:"+(DispatchKey{Activity: key, GameSID: 2}).String(), OwedDispatchKey(prefix, key.GroupID, 1), OwedDispatchKey(prefix, key.GroupID, 2))
	})
	return New(Config{Groups: testGroups(t), Activities: stores.Activities, Participants: stores.Participants, Ledger: stores.Ledger, Audits: stores.Audits, Dispatches: stores.Dispatches, Windows: stores.Windows, ReservationTTL: time.Hour})
}
