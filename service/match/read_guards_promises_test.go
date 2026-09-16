package match

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// U-0141 · C2（空洞测试）· nightly gap map kit `service/match` 8/20。
//
// 读一个还没有状态的队列是干净的未命中，状态存储出错则原样上抛（不把错误当
// 空队列）；Cancel 对没有状态的队列与不存在的票都报 ErrTicketMissing；Commit
// 对"同一主体持有两张等待票"的损坏状态拒绝（Enqueue 本不允许，这是对旧数据 /
// 外部写入的兜底），拒绝后不产生比赛；Redis 存储缺键前缀不能构造。

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
