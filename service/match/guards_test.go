// 匹配入参与读写守卫：提交与取消的拒绝、读取未命中与错误上抛、重放归属、分组先校验、返回值不共享切片、缺省时钟。

package match

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	businessclock "github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func expectErr(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// Commit is one compare-and-set over the whole queue: every refusal must fire
// before anything is mutated, so the queue is exactly as it was afterwards.
// Each rule is pinned by sentinel and message.
func TestCommitRefusesEachInvalidTicketSet(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	q := ranked()
	a := enqueue(t, store, q, player(1, 10), "r-1")
	b := enqueue(t, store, q, player(2, 12), "r-2")

	cases := []struct {
		name     string
		tickets  []string
		sentinel error
		text     string
	}{
		{"wrong count", []string{a.ID}, ErrTicketInvalid, "1 tickets for a group of 2"},
		{"empty id", []string{a.ID, ""}, ErrTicketInvalid, "empty ticket id"},
		// "ticket X appears twice" is the id-level rule; the subject-level rule
		// inside the CAS says "subject X appears twice" — pin the id one.
		{"same ticket twice", []string{a.ID, a.ID}, ErrTicketInvalid, "ticket " + a.ID + " appears twice"},
		{"unknown ticket", []string{a.ID, "ghost"}, ErrTicketMissing, "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Commit(ctx, q, tc.tickets)
			expectErr(t, err, tc.sentinel, tc.text)
		})
	}
	if _, err := store.Commit(ctx, Queue{Mode: "ranked", GroupSize: 2, Partition: "empty"}, []string{a.ID, b.ID}); !errors.Is(err, ErrTicketMissing) || !strings.Contains(err.Error(), "queue is empty") {
		t.Fatalf("commit on a queue nobody entered = %v", err)
	}
	// Both tickets still waiting: nothing above touched the queue.
	for _, held := range []struct {
		ticket Ticket
		owner  Subject
	}{{a, player(1, 10)}, {b, player(2, 12)}} {
		got, found, err := store.Ticket(ctx, q, held.ticket.ID, held.owner)
		if err != nil || !found || got.State != TicketWaiting {
			t.Fatalf("ticket %s after refused commits = %+v found=%v err=%v", held.ticket.ID, got, found, err)
		}
	}
	match, err := store.Commit(ctx, q, []string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("legal commit: %v", err)
	}
	if replay, err := store.Commit(ctx, q, []string{a.ID, b.ID}); err != nil || replay.ID != match.ID {
		t.Fatalf("same tickets must replay the original match: %+v %v", replay, err)
	}
	if len(match.Members) != 2 {
		t.Fatalf("match = %+v", match)
	}
}

// The "subject appears twice" rule inside Commit is unreachable through the
// public API: Enqueue already refuses a second live ticket for the same
// subject. It stays as defence in depth and is not pinned here.

// 读一个还没有状态的队列是干净的未命中，状态存储出错原样上抛；Cancel 对没有状态的队列、空票 id 与不存在的票逐一拒绝；Commit 对
// “同一主体持有两张等待票”的损坏状态拒绝，拒绝后不产生比赛；Redis 存储缺键前缀不能构造。

// failingState answers every Get with an error; the store must report it, not
// treat the queue as empty.
type failingState struct {
	versionstore.Store[string, queueState]
	err error
}

func (s failingState) Get(context.Context, string) (versionstore.Versioned[queueState], bool, error) {
	return versionstore.Versioned[queueState]{}, false, s.err
}

func TestReadsOfAnUnknownQueueMissCleanlyAndStoreErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	q := ranked()
	if _, found, err := store.Ticket(ctx, q, "t-1", player(1, 10)); err != nil || found {
		t.Fatalf("Ticket on an unknown queue = (found=%v, %v), want a clean miss", found, err)
	}
	if tickets, err := store.Candidates(ctx, q, 4); err != nil || len(tickets) != 0 {
		t.Fatalf("Candidates on an unknown queue = (%d, %v)", len(tickets), err)
	}
	if _, found, err := store.Match(ctx, q, "m-1"); err != nil || found {
		t.Fatalf("Match on an unknown queue = (found=%v, %v)", found, err)
	}
	if length, err := store.QueueLength(ctx, q); err != nil || length != 0 {
		t.Fatalf("QueueLength on an unknown queue = (%d, %v)", length, err)
	}

	boom := errors.New("redis unreachable")
	broken, err := NewStore(failingState{Store: versionstore.NewMemoryStore[string, queueState](), err: boom}, Config{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := broken.Ticket(ctx, q, "t-1", player(1, 10)); !errors.Is(err, boom) || found {
		t.Fatalf("Ticket with a failing store = (found=%v, %v), want the store error", found, err)
	}
	if tickets, err := broken.Candidates(ctx, q, 4); !errors.Is(err, boom) || len(tickets) != 0 {
		t.Fatalf("Candidates with a failing store = (%d, %v)", len(tickets), err)
	}
	if _, found, err := broken.Match(ctx, q, "m-1"); !errors.Is(err, boom) || found {
		t.Fatalf("Match with a failing store = (found=%v, %v)", found, err)
	}
	if length, err := broken.QueueLength(ctx, q); !errors.Is(err, boom) || length != 0 {
		t.Fatalf("QueueLength with a failing store = (%d, %v)", length, err)
	}
}

func TestCancelRefusesUnknownQueuesAndTickets(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	q := ranked()
	_, err := store.Cancel(ctx, q, "", player(1, 10))
	expectErr(t, err, ErrTicketInvalid, "id is empty")
	if _, err := store.Cancel(ctx, q, "ghost", player(1, 10)); !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("Cancel on a queue without state = %v", err)
	}
	ticket := enqueue(t, store, q, player(1, 10), "r-1")
	if _, err := store.Cancel(ctx, q, "ghost", player(1, 10)); !errors.Is(err, ErrTicketMissing) || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("Cancel of an unknown ticket = %v", err)
	}
	if got, found, err := store.Ticket(ctx, q, ticket.ID, player(1, 10)); err != nil || !found || got.State != TicketWaiting {
		t.Fatalf("a refused cancel touched the live ticket: %+v found=%v err=%v", got, found, err)
	}
	if _, err := store.Cancel(ctx, q, ticket.ID, player(1, 10)); err != nil {
		t.Fatalf("Cancel of the live ticket = %v", err)
	}
}

func TestCommitRefusesACorruptedQueueWhereOneSubjectHoldsTwoTickets(t *testing.T) {
	ctx := context.Background()
	q := ranked()
	now := time.Unix(1_700_000_000, 0)
	subject := player(1, 10)
	state := versionstore.NewMemoryStore[string, queueState]()
	corrupted := queueState{Waiting: []string{"t-1", "t-2"}, Tickets: map[string]Ticket{}}
	for _, id := range corrupted.Waiting {
		corrupted.Tickets[id] = Ticket{ID: id, Queue: q, Subject: subject, State: TicketWaiting, CreatedAtUnix: now.Unix(), ExpiresAtUnix: now.Add(time.Hour).Unix()}
	}
	if _, created, err := state.Create(ctx, q.Key(), corrupted); err != nil || !created {
		t.Fatalf("seed corrupted state: created=%v err=%v", created, err)
	}
	store, err := NewStore(state, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, q, []string{"t-1", "t-2"}); !errors.Is(err, ErrTicketInvalid) || !strings.Contains(err.Error(), "appears twice") {
		t.Fatalf("Commit over two tickets of one subject = %v, want ErrTicketInvalid \"subject ... appears twice\"", err)
	}
	after, found, err := state.Get(ctx, q.Key())
	if err != nil || !found || len(after.Value.Matches) != 0 || after.Value.Tickets["t-1"].State != TicketWaiting {
		t.Fatalf("a refused commit changed the queue: %+v found=%v err=%v", after.Value, found, err)
	}
}

func TestNewRedisStoreRequiresAKeyPrefix(t *testing.T) {
	if _, err := NewRedisStore(nil, "  ", Config{}); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStore with a blank prefix = %v", err)
	}
}

func TestTicketTTLRejectsSubsecond(t *testing.T) {
	if _, err := NewStore(versionstore.NewMemoryStore[string, queueState](), Config{TicketTTL: time.Millisecond}); err == nil {
		t.Fatal("subsecond ticket TTL accepted despite Unix-second deadline")
	}
}

func TestCommitReplayReturnsOriginalMatchForSameTicketSet(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a, b := enqueue(t, s, ranked(), player(1, 100), "a"), enqueue(t, s, ranked(), player(2, 100), "b")
	first, err := s.Commit(ctx, ranked(), []string{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Commit(ctx, ranked(), []string{b.ID, a.ID})
	if err != nil || again.ID != first.ID {
		t.Fatalf("same ticket set replay = %+v %v, want %s", again, err, first.ID)
	}
	c := enqueue(t, s, ranked(), player(3, 100), "c")
	if _, err := s.Commit(ctx, ranked(), []string{a.ID, c.ID}); err == nil {
		t.Fatal("different ticket set replayed a match")
	}
}

// Enqueue 的幂等重放先校验归属：同一队列里另一个主体用同一个 requestID 时拒绝（客户端缺陷要暴露出来），不交出别人的票。
// 历史：RR-20260909-05。
func TestEnqueueReplayRefusesARequestIDFromAnotherSubject(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	queue := ranked()

	first := enqueue(t, store, queue, player(1, 100), "shared-request")

	// The owner's own retry still replays: same ticket, no second queue entry.
	replay, err := store.Enqueue(ctx, queue, player(1, 100), "shared-request")
	if err != nil {
		t.Fatalf("the owner's retry must replay, got %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("owner retry produced ticket %s, want the original %s", replay.ID, first.ID)
	}

	// Another subject using the same request id is refused, and told nothing
	// about who owns it.
	stolen, err := store.Enqueue(ctx, queue, player(2, 500), "shared-request")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("cross-subject replay = (%+v, %v), want ErrNotPermitted", stolen, err)
	}
	if stolen.ID != "" {
		t.Fatalf("a refused enqueue returned ticket %s", stolen.ID)
	}

	// The refusal changed nothing: the owner still holds the only ticket, the
	// request still maps to it, and player 2 has no ticket at all.
	if current, ok, err := store.Ticket(ctx, queue, first.ID, player(1, 100)); err != nil || !ok || current.ID != first.ID {
		t.Fatalf("owner's ticket after the refusal = (%s, %v, %v)", current.ID, ok, err)
	}
	candidates, err := store.Candidates(ctx, queue, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != first.ID {
		t.Fatalf("candidates = %+v, want only the owner's ticket %s", candidates, first.ID)
	}

	// Player 2 can still enqueue under its own request id.
	own, err := store.Enqueue(ctx, queue, player(2, 500), "player-2-request")
	if err != nil {
		t.Fatalf("player 2 must still be able to enqueue: %v", err)
	}
	if own.Subject.ID != 2 {
		t.Fatalf("player 2 received a ticket for subject %d", own.Subject.ID)
	}
}

func TestGroupingValidatesBeforeReadingCandidates(t *testing.T) {
	policies := map[string]Grouping{"fifo": FirstComeGrouping{}, "score": ScoreWindowGrouping{NowUnix: func() int64 { return 1700000000 }}}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			for _, size := range []int{-1, 0, 1, MaxGroupSize + 1} {
				for _, count := range []int{0, 2} {
					t.Run(fmt.Sprintf("size_%d_candidates_%d", size, count), func(t *testing.T) {
						q := ranked()
						q.GroupSize = size
						group, formed, err := policy.Group(q, make([]Ticket, count))
						if !errors.Is(err, ErrQueueInvalid) || formed || len(group) != 0 {
							t.Fatal(group, formed, err)
						}
					})
				}
			}
			q := ranked()
			q.Mode = ""
			if _, _, err := policy.Group(q, nil); !errors.Is(err, ErrQueueInvalid) {
				t.Fatal(err)
			}
			for _, size := range []int{2, MaxGroupSize} {
				q := ranked()
				q.GroupSize = size
				candidates := make([]Ticket, size)
				for i := range candidates {
					candidates[i] = Ticket{ID: fmt.Sprint(i), Subject: player(int64(i+1), 100)}
				}
				group, ok, err := policy.Group(q, candidates)
				if err != nil || !ok || len(group) != size {
					t.Fatal(group, ok, err)
				}
				if _, ok, err := policy.Group(q, candidates[:size-1]); err != nil || ok {
					t.Fatal(ok, err)
				}
			}
		})
	}
}

// 内存 Store 的输入与返回值不与存储共享可变切片（Subject.Payload、Match.Members、Match.TicketIDs）。历史：RR-20260917-03。
func TestReturnedAndInputSlicesDoNotAliasTheStore(t *testing.T) {
	ctx := context.Background()
	queue := Queue{Mode: "ranked", GroupSize: 2}
	fresh := func(t *testing.T) (Store, Ticket, Ticket) {
		t.Helper()
		s, err := NewMemoryStore(Config{})
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.Enqueue(ctx, queue, Subject{Kind: "player", ID: 1, Payload: []byte("old")}, "")
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Enqueue(ctx, queue, Subject{Kind: "player", ID: 2, Payload: []byte("old")}, "")
		if err != nil {
			t.Fatal(err)
		}
		return s, a, b
	}
	payloadOf := func(t *testing.T, s Store, id string) string {
		t.Helper()
		got, found, err := s.Ticket(ctx, queue, id, Subject{Kind: "player", ID: 1})
		if err != nil || !found {
			t.Fatalf("ticket %s: found=%v err=%v", id, found, err)
		}
		return string(got.Subject.Payload)
	}

	t.Run("enqueue input payload", func(t *testing.T) {
		s, err := NewMemoryStore(Config{})
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("old")
		a, err := s.Enqueue(ctx, queue, Subject{Kind: "player", ID: 1, Payload: payload}, "")
		if err != nil {
			t.Fatal(err)
		}
		payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("the caller's input slice rewrote the stored payload: %q", got)
		}
	})
	t.Run("enqueue result payload", func(t *testing.T) {
		s, a, _ := fresh(t)
		a.Subject.Payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("Enqueue's returned ticket aliases the store: %q", got)
		}
	})
	t.Run("ticket query payload", func(t *testing.T) {
		s, a, _ := fresh(t)
		got, _, _ := s.Ticket(ctx, queue, a.ID, Subject{Kind: "player", ID: 1})
		got.Subject.Payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("Ticket's result aliases the store: %q", got)
		}
	})
	t.Run("candidates payload", func(t *testing.T) {
		s, a, _ := fresh(t)
		candidates, err := s.Candidates(ctx, queue, 10)
		if err != nil {
			t.Fatal(err)
		}
		candidates[0].Subject.Payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("Candidates' result aliases the store: %q", got)
		}
	})
	t.Run("commit result members and ticket ids", func(t *testing.T) {
		s, a, b := fresh(t)
		m, err := s.Commit(ctx, queue, []string{a.ID, b.ID})
		if err != nil {
			t.Fatal(err)
		}
		m.Members[0].ID = 999
		m.Members[0].Payload[0] = 'X'
		m.TicketIDs[0] = "forged"
		again, found, err := s.Match(ctx, queue, m.ID)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		if again.Members[0].ID != 1 || string(again.Members[0].Payload) != "old" || again.TicketIDs[0] != a.ID {
			t.Fatalf("Commit's returned match aliases the store: %+v", again)
		}
	})
	t.Run("match query members and ticket ids", func(t *testing.T) {
		s, a, b := fresh(t)
		m, err := s.Commit(ctx, queue, []string{a.ID, b.ID})
		if err != nil {
			t.Fatal(err)
		}
		first, _, _ := s.Match(ctx, queue, m.ID)
		first.Members[0].ID = 999
		first.TicketIDs[0] = "forged"
		again, _, _ := s.Match(ctx, queue, m.ID)
		if again.Members[0].ID != 1 || again.TicketIDs[0] != a.ID {
			t.Fatalf("Match's result aliases the store: %+v", again)
		}
	})
}

func TestMissingBusinessClockUsesProcessOffset(t *testing.T) {
	old := businessclock.Offset()
	businessclock.SetOffset(24 * time.Hour)
	t.Cleanup(func() { businessclock.SetOffset(old) })
	s, _ := newStore(t, func(c *Config) { c.Now = nil })
	if delta := s.(*queueStore).cfg.Now().Sub(time.Now()); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("default business clock ignores process offset: %s", delta)
	}
}
