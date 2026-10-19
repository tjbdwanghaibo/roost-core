package match

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	businessclock "github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// queueState is one queue's entire mutable state, held as a single versioned
// value.
//
// Keeping it in one entry is the central decision. The implementation this
// replaces spread a queue across a waiting list, a ticket record per subject
// and a match record, and mutated them in sequence: pop the queue, write the
// match, then flip each ticket. Any failure after the pop lost those players.
// With one entry, a commit is one compare-and-set: either the queue, the
// tickets and the match all move, or none of them do. Two committers racing on
// the same head cannot both win, because the second one's version is stale.
//
// The cost is that a queue's throughput is bounded by contention on one key,
// which is why callers route by Queue.Key() — see servicerpc.KeyAffinityPicker.
//
// “一个队列”指一个分区（Queue.Partition）：大队列按业务维度拆成多个分区，各分区各自一个值，单分区操作
// 互不争用。跨分区成局（CommitAcross）是唯一同时读写几个分区的操作，用多键比较并设置保持“要么都改、要么都不改”。
type queueState struct {
	// Waiting is the ticket ids in queue order, oldest first.
	Waiting []string `json:"waiting"`
	// Tickets holds the live tickets and the resolved ones still inside
	// Config.Retention; see pruneResolvedLocked.
	Tickets map[string]Ticket `json:"tickets"`
	// Matches holds committed matches still inside Config.Retention.
	Matches map[string]Match `json:"matches"`
	// SubjectTickets maps a subject key to its live ticket id. This is what
	// makes "one live ticket per subject" a lookup rather than a scan, and
	// what stops the same player being committed into two matches.
	SubjectTickets map[string]string `json:"subject_tickets"`
	// Requests maps an enqueue idempotency key to the ticket it produced. An
	// entry lives exactly as long as its ticket.
	Requests map[string]string `json:"requests"`
}

// clone copies the state before a mutation touches it.
//
// versionstore documents that a Mutate may run more than once and must be a
// pure function of its arguments, and its memory implementation hands out the
// stored value itself. The callbacks below resolve expired tickets by shifting
// Waiting in place before they decide whether to save; done on the stored
// slice, a mutation that then declined to save left the stored header over a
// shifted array with its tail duplicated, and Candidates handed out one ticket
// twice. Copying first is what makes "an aborted mutation changes nothing"
// hold for every implementation of the contract, not only the Redis one that
// happens to decode a fresh value per call.
func (s queueState) clone() queueState {
	next := queueState{
		Waiting:        append([]string(nil), s.Waiting...),
		Tickets:        make(map[string]Ticket, len(s.Tickets)+1),
		Matches:        make(map[string]Match, len(s.Matches)+1),
		SubjectTickets: make(map[string]string, len(s.SubjectTickets)+1),
		Requests:       make(map[string]string, len(s.Requests)+1),
	}
	for id, ticket := range s.Tickets {
		next.Tickets[id] = ticket
	}
	for id, match := range s.Matches {
		next.Matches[id] = match
	}
	for key, id := range s.SubjectTickets {
		next.SubjectTickets[key] = id
	}
	for key, id := range s.Requests {
		next.Requests[key] = id
	}
	return next
}

func (s *queueState) init() {
	if s.Tickets == nil {
		s.Tickets = map[string]Ticket{}
	}
	if s.Matches == nil {
		s.Matches = map[string]Match{}
	}
	if s.SubjectTickets == nil {
		s.SubjectTickets = map[string]string{}
	}
	if s.Requests == nil {
		s.Requests = map[string]string{}
	}
}

// Config configures a queue store.
type Config struct {
	// TicketTTL is how long a ticket waits before expiring; zero selects
	// DefaultTicketTTL. It must be positive: a ticket with no deadline is a
	// queue entry that leaks when its client disconnects, which is what
	// happened when the deadline was written but never read.
	TicketTTL time.Duration
	// Retention 是已结束记录的保留期：已成局 / 取消 / 过期的票、已提交的比赛与排队请求记录，
	// 从结束（ResolvedAtUnix / Match.CreatedAtUnix）起至少保留这么久，之后在下一次写入或 Sweep 时移除；
	// 零取 DefaultRetention，不得小于 1s（时间按 Unix 秒记）。
	//
	// 为什么必须有：整个队列状态是一个版本化的值，Redis 存储每次 Enqueue / Commit / Sweep 都整体读写它，
	// 只增不删时状态随历史无界增长，每次操作越来越慢（维护者 2026-10-19 选方案 A：固定时长保留）。
	// 保留期内的读者照旧：读票、读比赛、同一 requestID 的 Enqueue 重放、同一票集合的 Commit 重放都返回原结果；
	// 超期后它们看到的是“不存在”，见 Store.Commit 与 pruneResolvedLocked。
	Retention time.Duration
	// Now is the clock tickets are dated on (CreatedAtUnix, ExpiresAtUnix,
	// ResolvedAtUnix, Match.CreatedAtUnix) and expiry is judged by; nil means
	// time.Now. The match Mod injects the business clock: matchmaking
	// is business logic, and ScoreWindowGrouping.NowUnix must read the same
	// clock, since the widening window is measured from CreatedAtUnix.
	Now func() time.Time
	// SweepQueues is the queue set this process's server sweeps for expired
	// tickets, once per tick. A deployment knows its queues — from the modes
	// it runs — and this package cannot enumerate them without an unbounded
	// scan. Empty means the server sweeps nothing, which it logs at start.
	// Expiry is still enforced inline on every read and mutation, so tickets
	// do not wait forever without it; what the sweep adds is resolving
	// expired tickets nobody touches again.
	SweepQueues []Queue
	// NewID mints ticket and match ids; nil means a 128-bit random id.
	//
	// Ids must be globally unique and unguessable. The implementation this
	// replaces used "ticket-1" from a process-local counter, so two replicas
	// minted identical ids — and an "idempotent" enqueue could hand back
	// another player's ticket.
	NewID func() (string, error)
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation. Queue depth and expiry counts are the two signals
	// the implementation this replaces had no way to emit, which is why its
	// ticket loss and its unenforced deadline both went unnoticed.
	Metrics servicemetrics.Reporter
}

// queueStore implements Store over versioned per-queue state.
type queueStore struct {
	state  versionstore.Store[string, queueState]
	cfg    Config
	report servicemetrics.Sink
}

// SweepQueues is the configured queue set the server's expiry sweep covers.
func (s *queueStore) SweepQueues() []Queue { return append([]Queue(nil), s.cfg.SweepQueues...) }

// NewStore returns a Store over versioned state.
func NewStore(state versionstore.Store[string, queueState], cfg Config) (Store, error) {
	if state == nil {
		return nil, fmt.Errorf("match: state store is nil")
	}
	if cfg.TicketTTL < 0 {
		return nil, fmt.Errorf("match: ticket ttl must not be negative")
	}
	if cfg.TicketTTL == 0 {
		cfg.TicketTTL = DefaultTicketTTL
	}
	if cfg.TicketTTL < time.Second {
		return nil, fmt.Errorf("TicketTTL must be at least 1s: deadlines use Unix seconds")
	}
	if cfg.Retention < 0 {
		return nil, fmt.Errorf("match: retention must not be negative")
	}
	if cfg.Retention == 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.Retention < time.Second {
		return nil, fmt.Errorf("match: retention must be at least 1s: resolution times use Unix seconds")
	}
	for _, queue := range cfg.SweepQueues {
		if err := queue.Validate(); err != nil {
			return nil, fmt.Errorf("match: sweep queue %s: %w", queue.Key(), err)
		}
	}
	cfg.SweepQueues = append([]Queue(nil), cfg.SweepQueues...)
	if cfg.Now == nil {
		cfg.Now = businessclock.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	return &queueStore{state: state, cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

// randomID mints an unguessable id. Guessable ids are what let any client
// cancel another player's ticket and read its score.
func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("match: mint id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (s *queueStore) Enqueue(ctx context.Context, queue Queue, subject Subject, requestID string) (Ticket, error) {
	if err := validateEnqueue(queue, subject); err != nil {
		return Ticket{}, err
	}
	now := s.cfg.Now()
	id, err := s.cfg.NewID()
	if err != nil {
		return Ticket{}, err
	}

	var (
		result   Ticket
		replayed bool
	)
	_, _, err = s.state.Update(ctx, queue.Key(), func(current queueState, _ bool) (queueState, bool, error) {
		current = current.clone()
		replayed = false
		s.expireLocked(&current, now)

		// Idempotent per request: a retried enqueue returns the ticket it
		// already produced instead of queueing the subject a second time.
		if requestID != "" {
			if existing, ok := current.Requests[requestID]; ok {
				if ticket, ok := current.Tickets[existing]; ok {
					// Whose retry is this? The request id comes from the
					// caller, so two subjects in one queue can collide on it,
					// and replaying without checking handed the second one the
					// first one's ticket and score while dropping its own
					// enqueue entirely — no request record, no queue entry.
					// The read path has always checked this
					// (validateOwnership); the replay branch had not.
					if err := validateOwnership(ticket, subject); err != nil {
						s.report.Refused("enqueue", "request_subject_mismatch")
						return current, false, err
					}
					result = ticket
					replayed = true
					return current, false, nil
				}
			}
		}
		// One live ticket per subject. This is the check whose absence let a
		// single client queue the same player twice and have both committed.
		if live, ok := current.SubjectTickets[subject.Key()]; ok {
			if ticket, ok := current.Tickets[live]; ok && !ticket.State.terminal() {
				s.report.Refused("enqueue", "already_queued")
				return current, false, fmt.Errorf("%w: %s holds ticket %s", ErrAlreadyQueued, subject.Key(), ticket.ID)
			}
		}
		if len(current.Waiting) >= MaxQueueLength {
			s.report.Refused("enqueue", "queue_full")
			return current, false, fmt.Errorf("%w: queue holds %d tickets, limit %d", ErrQueueInvalid, len(current.Waiting), MaxQueueLength)
		}

		ticket := Ticket{
			// The caller keeps its Subject; the store keeps its own copy of the
			// payload.
			ID: id, Queue: queue, Subject: subject.clone(), State: TicketWaiting,
			RequestID: requestID, CreatedAtUnix: now.Unix(),
			ExpiresAtUnix: now.Add(s.cfg.TicketTTL).Unix(),
		}
		current.Tickets[id] = ticket
		current.Waiting = append(current.Waiting, id)
		current.SubjectTickets[subject.Key()] = id
		if requestID != "" {
			current.Requests[requestID] = id
		}
		result = ticket
		return current, true, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	if replayed {
		// A retried enqueue is not a second player joining the queue. Counting
		// it as one makes the accept rate a measure of client retries.
		s.report.Replayed("enqueue")
	} else {
		s.report.Accepted("enqueue")
	}
	// Queue depth is the signal the implementation this replaces could not
	// emit: its tickets lived in per-subject records with no aggregate, so
	// "how many players are waiting" had no answer.
	if length, err := s.QueueLength(ctx, queue); err == nil {
		// The queue is a label, not part of the name (maintainer decision): one gauge, one
		// series per configured queue.
		s.report.DepthOf("queue", queue.Key(), int64(length))
	}
	return result.clone(), nil
}

func (s *queueStore) Cancel(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, error) {
	if err := queue.Validate(); err != nil {
		return Ticket{}, err
	}
	if ticketID == "" {
		return Ticket{}, fmt.Errorf("%w: id is empty", ErrTicketInvalid)
	}
	now := s.cfg.Now()

	var result Ticket
	_, _, err := s.state.Update(ctx, queue.Key(), func(current queueState, found bool) (queueState, bool, error) {
		current = current.clone()
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrTicketMissing, ticketID)
		}
		s.expireLocked(&current, now)
		ticket, ok := current.Tickets[ticketID]
		if !ok {
			return current, false, fmt.Errorf("%w: %s", ErrTicketMissing, ticketID)
		}
		if err := validateOwnership(ticket, subject); err != nil {
			return current, false, err
		}
		if ticket.State == TicketMatched {
			// A matched ticket cannot be withdrawn. Reporting this rather
			// than answering OK is the difference from an implementation that
			// told the caller "cancelled" while the store said "matched".
			return current, false, fmt.Errorf("%w: %s is in match %s", ErrTicketMatched, ticketID, ticket.MatchID)
		}
		if ticket.State.terminal() {
			// Already cancelled or expired: idempotent.
			result = ticket
			return current, false, nil
		}
		ticket.State = TicketCancelled
		ticket.ResolvedAtUnix = now.Unix()
		current.Tickets[ticketID] = ticket
		removeWaiting(&current, ticketID)
		delete(current.SubjectTickets, ticket.Subject.Key())
		result = ticket
		return current, true, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	return result.clone(), nil
}

func (s *queueStore) Ticket(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, bool, error) {
	if err := queue.Validate(); err != nil {
		return Ticket{}, false, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return Ticket{}, false, err
	}
	state := current.Value
	state.init()
	ticket, ok := state.Tickets[ticketID]
	if !ok {
		return Ticket{}, false, nil
	}
	if err := validateOwnership(ticket, subject); err != nil {
		return Ticket{}, false, err
	}
	// Report an elapsed deadline as expired even before a sweep runs, so a
	// reader never sees a ticket that is waiting only because nothing has
	// gotten round to resolving it.
	if ticket.Expired(s.cfg.Now()) {
		ticket.State = TicketExpired
	}
	return ticket.clone(), true, nil
}

func (s *queueStore) Candidates(ctx context.Context, queue Queue, limit int) ([]Ticket, error) {
	if err := queue.Validate(); err != nil {
		return nil, err
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return nil, err
	}
	state := current.Value
	state.init()
	now := s.cfg.Now()
	out := make([]Ticket, 0, limit)
	for _, id := range state.Waiting {
		ticket, ok := state.Tickets[id]
		if !ok || ticket.State != TicketWaiting || ticket.Expired(now) {
			continue
		}
		// Defence in depth behind the injective Key: a
		// ticket that says it belongs to another queue is never a candidate
		// here, whatever key it was stored under.
		if ticket.Queue != queue {
			continue
		}
		out = append(out, ticket.clone())
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *queueStore) Commit(ctx context.Context, queue Queue, ticketIDs []string) (Match, error) {
	if err := queue.Validate(); err != nil {
		return Match{}, err
	}
	seen, err := checkTicketSet(ticketIDs, queue.GroupSize)
	if err != nil {
		return Match{}, err
	}
	now := s.cfg.Now()
	matchID, err := s.cfg.NewID()
	if err != nil {
		return Match{}, err
	}

	var result Match
	var replayed bool
	_, _, err = s.state.Update(ctx, queue.Key(), func(current queueState, found bool) (queueState, bool, error) {
		replayed = false
		current = current.clone()
		if !found {
			return current, false, fmt.Errorf("%w: queue is empty", ErrTicketMissing)
		}
		s.expireLocked(&current, now)
		// 票集合就是提交身份。整组已匹配到同一局时返回原结果，不能再生成第二局，
		// 也不能把部分重叠的集合当作重放。比赛与它的票只保留 Config.Retention：上面的 expireLocked
		// 已移除到期记录，超期的重放在下面的校验里因票不存在得到 ErrTicketMissing。
		first := current.Tickets[ticketIDs[0]]
		if first.State == TicketMatched && first.Queue == queue {
			previous, ok := current.Matches[first.MatchID]
			if ok && len(previous.TicketIDs) == len(ticketIDs) {
				same := true
				for _, id := range previous.TicketIDs {
					ticket := current.Tickets[id]
					if !seen[id] || ticket.State != TicketMatched || ticket.MatchID != previous.ID || ticket.Queue != queue {
						same = false
						break
					}
				}
				if same {
					result, replayed = previous, true
					return current, false, nil
				}
			}
		}

		// Validate every ticket before mutating anything. Because this whole
		// function is one compare-and-set, a failure here leaves the queue
		// exactly as it was — the property the three-write sequence it
		// replaces could not offer.
		members := make([]Subject, 0, len(ticketIDs))
		subjects := make(map[string]bool, len(ticketIDs))
		for _, id := range ticketIDs {
			ticket, ok := current.Tickets[id]
			if !ok {
				return current, false, fmt.Errorf("%w: %s", ErrTicketMissing, id)
			}
			// A ticket of another queue is "not in this queue", even if the
			// two ever shared storage.
			if ticket.Queue != queue {
				return current, false, fmt.Errorf("%w: %s belongs to queue %s", ErrTicketMissing, id, ticket.Queue.Key())
			}
			if ticket.State != TicketWaiting {
				return current, false, fmt.Errorf("%w: %s is %s", ErrConflict, id, ticket.State)
			}
			// The same subject must not appear twice in one match, which is
			// what a duplicate live ticket used to produce.
			if subjects[ticket.Subject.Key()] {
				return current, false, fmt.Errorf("%w: subject %s appears twice", ErrTicketInvalid, ticket.Subject.Key())
			}
			subjects[ticket.Subject.Key()] = true
			members = append(members, ticket.Subject.clone())
		}

		match := Match{
			ID: matchID, Queue: queue, Members: members,
			TicketIDs: append([]string(nil), ticketIDs...), CreatedAtUnix: now.Unix(),
		}
		for _, id := range ticketIDs {
			ticket := current.Tickets[id]
			ticket.State = TicketMatched
			ticket.MatchID = matchID
			ticket.ResolvedAtUnix = now.Unix()
			current.Tickets[id] = ticket
			removeWaiting(&current, id)
			delete(current.SubjectTickets, ticket.Subject.Key())
		}
		current.Matches[matchID] = match
		result = match
		return current, true, nil
	})
	if err != nil {
		return Match{}, err
	}
	if replayed {
		s.report.Replayed("commit")
	} else {
		s.report.Accepted("commit")
	}
	return result.clone(), nil
}

func (s *queueStore) Match(ctx context.Context, queue Queue, matchID string) (Match, bool, error) {
	if err := queue.Validate(); err != nil {
		return Match{}, false, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return Match{}, false, err
	}
	state := current.Value
	state.init()
	match, ok := state.Matches[matchID]
	return match.clone(), ok, nil
}

func (s *queueStore) Sweep(ctx context.Context, queue Queue, limit int) (int, error) {
	if err := queue.Validate(); err != nil {
		return 0, err
	}
	if err := validateLimit(limit); err != nil {
		return 0, err
	}
	now := s.cfg.Now()
	resolved := 0
	_, _, err := s.state.Update(ctx, queue.Key(), func(current queueState, found bool) (queueState, bool, error) {
		current = current.clone()
		if !found {
			return current, false, nil
		}
		resolved = s.expireLockedLimit(&current, now, limit)
		pruned := s.pruneResolvedLocked(&current, now)
		return current, resolved > 0 || pruned > 0, nil
	})
	if err != nil {
		// Counted as well as returned: the background sweep only logs this,
		// and a sweep that fails on every tick must show up somewhere other
		// than a log line.
		s.report.Dropped("sweep.failed", 1)
		return 0, err
	}
	// Expired tickets are dropped data: a player who waited and got nothing.
	// The implementation this replaces wrote a deadline nothing read, so this
	// count was structurally unobtainable.
	s.report.Dropped("ticket.expired", resolved)
	return resolved, nil
}

func (s *queueStore) QueueLength(ctx context.Context, queue Queue) (int, error) {
	if err := queue.Validate(); err != nil {
		return 0, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return 0, err
	}
	state := current.Value
	state.init()
	now := s.cfg.Now()
	live := 0
	for _, id := range state.Waiting {
		if ticket, ok := state.Tickets[id]; ok && ticket.State == TicketWaiting && !ticket.Expired(now) {
			live++
		}
	}
	return live, nil
}

// expireLocked resolves every lapsed waiting ticket. It runs at the start of
// every mutation, so an expired ticket can never be committed into a match
// even if no sweep has run — the deadline is enforced by the code that would
// otherwise use the ticket, not only by a background job.
//
// 同一处也移除超过保留期的已结束记录（pruneResolvedLocked）：状态有界不依赖是否配置了
// match.sweep_queues——任何一次写入都会顺带清掉到期的历史，Sweep 只是让无人再写的队列也回落。
func (s *queueStore) expireLocked(state *queueState, now time.Time) int {
	resolved := s.expireLockedLimit(state, now, len(state.Waiting))
	s.pruneResolvedLocked(state, now)
	return resolved
}

func (s *queueStore) expireLockedLimit(state *queueState, now time.Time, limit int) int {
	resolved := 0
	for _, id := range append([]string(nil), state.Waiting...) {
		if resolved >= limit {
			break
		}
		ticket, ok := state.Tickets[id]
		if !ok {
			removeWaiting(state, id)
			continue
		}
		if ticket.State != TicketWaiting || !ticket.Expired(now) {
			continue
		}
		ticket.State = TicketExpired
		ticket.ResolvedAtUnix = now.Unix()
		state.Tickets[id] = ticket
		removeWaiting(state, id)
		delete(state.SubjectTickets, ticket.Subject.Key())
		resolved++
	}
	return resolved
}

// pruneResolvedLocked 移除结束已满 Retention 的记录，返回移除的票数与比赛数之和。
//
// 规则（全部由状态与 now 决定，CAS 重跑得到同一结果）：
//   - 比赛在 CreatedAtUnix+Retention 到期时，与它消耗的票（State=matched、MatchID 指向它）一起移除。
//     票与比赛同进同出：不会留下指向已删比赛的 matched 票，也不会留下成员票已删的比赛。
//   - 已取消 / 已过期的票，以及找不到比赛的 matched 票，在 ResolvedAtUnix+Retention 到期时移除。
//   - 排队请求记录随它指向的票移除。
//   - 等待中的票（不论等了多久）与保留期内的记录不动：这里只看终态，等待中的票只由过期、成局或取消结束。
//
// 跨分区成局的比赛在每个涉及的分区各存一份，每个分区按上面同一条规则移除自己那份与本分区的票（别的分区的票
// 不在本状态里，循环里查不到就跳过），所以“比赛与它的票同进同出”在每个分区内成立；各分区的移除发生在各自
// 下一次写入或 Sweep 时，截止点相同（成局时刻 + Retention），保留期内所有分区都还在。
//
// 被移除的票 ID 不会“复活”：票 ID 由 Enqueue 新铸，Commit 只接受状态里存在且等待中的票，
// 所以超期后用同一票集合 Commit 得到 ErrTicketMissing，不会再成一局；同一 requestID 的 Enqueue
// 重试则不再是重放，而是一次新的排队（新票、新 ID）。
//
// 不受 Sweep 的 limit 约束：状态本来就整体读写，一次清完到期记录不增加 I/O，正是它让写回的值变小。
//
// 时间按 Unix 秒记，真实结束时刻在 [R, R+1) 内；截止点多减 1s，保证“至少保留 Retention”，代价是最多多留约 2s。
func (s *queueStore) pruneResolvedLocked(state *queueState, now time.Time) int {
	cutoff := now.Add(-s.cfg.Retention - time.Second).Unix()
	removed := 0
	for matchID, match := range state.Matches {
		if match.CreatedAtUnix > cutoff {
			continue
		}
		for _, ticketID := range match.TicketIDs {
			if ticket, ok := state.Tickets[ticketID]; ok && ticket.State == TicketMatched && ticket.MatchID == matchID {
				dropResolvedTicket(state, ticket)
				removed++
			}
		}
		delete(state.Matches, matchID)
		removed++
	}
	for _, ticket := range state.Tickets {
		if !ticket.State.terminal() || ticket.ResolvedAtUnix > cutoff {
			continue
		}
		if ticket.State == TicketMatched {
			if _, ok := state.Matches[ticket.MatchID]; ok {
				// 随它的比赛一起走，不单独移除。
				continue
			}
		}
		dropResolvedTicket(state, ticket)
		removed++
	}
	for requestID, ticketID := range state.Requests {
		if _, ok := state.Tickets[ticketID]; !ok {
			delete(state.Requests, requestID)
		}
	}
	return removed
}

// dropResolvedTicket 删除一张已结束的票。终态票在结束时已离开 Waiting 与 SubjectTickets（Cancel、
// Commit、expireLockedLimit 各自移除）；主体映射这里仍按“只删指向它自己的条目”再清一次。Waiting 不逐个扫：
// 那是 O(等待票数) 的线性删除，按不变量本来就不含终态票，万一残留的 ID 指向已删的票，
// expireLockedLimit 会在下一次写入时移除，Candidates / QueueLength 也跳过它。
func dropResolvedTicket(state *queueState, ticket Ticket) {
	delete(state.Tickets, ticket.ID)
	if state.SubjectTickets[ticket.Subject.Key()] == ticket.ID {
		delete(state.SubjectTickets, ticket.Subject.Key())
	}
}

func removeWaiting(state *queueState, ticketID string) {
	for index, id := range state.Waiting {
		if id == ticketID {
			state.Waiting = append(state.Waiting[:index], state.Waiting[index+1:]...)
			return
		}
	}
}

// checkTicketSet 校验一次成局的票 ID 集合：张数等于 groupSize，没有空 ID 与重复 ID。返回 ID 集合供重放判定用。
func checkTicketSet(ticketIDs []string, groupSize int) (map[string]bool, error) {
	if len(ticketIDs) != groupSize {
		return nil, fmt.Errorf("%w: %d tickets for a group of %d", ErrTicketInvalid, len(ticketIDs), groupSize)
	}
	seen := make(map[string]bool, len(ticketIDs))
	for _, id := range ticketIDs {
		if id == "" {
			return nil, fmt.Errorf("%w: empty ticket id", ErrTicketInvalid)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: ticket %s appears twice", ErrTicketInvalid, id)
		}
		seen[id] = true
	}
	return seen, nil
}

// CommitAcross 见 Store.CommitAcross。
//
// 主流程：校验票集合属于同一逻辑队列 → 按分区归组（只有一个分区时交给 Commit）→ 在一次多键比较并设置里
// 读全部涉及的分区，先判重放、再逐张校验，全部通过才在每个分区写同一场比赛并标记本分区的票。
// mutate 只读参数、先复制再改，所以重试时得到同一结果，失败时任何分区都不写。
func (s *queueStore) CommitAcross(ctx context.Context, tickets TicketRefs) (Match, error) {
	if len(tickets) == 0 {
		return Match{}, fmt.Errorf("%w: no tickets", ErrTicketInvalid)
	}
	logical := tickets[0].Queue
	ticketIDs := make([]string, len(tickets))
	// partitionOf[i] 是第 i 张票所在分区在 partitions / keys 里的下标；分区按首次出现的顺序排列。
	partitionOf := make([]int, len(tickets))
	var partitions []Queue
	index := map[Queue]int{}
	for i, ref := range tickets {
		if err := ref.Queue.Validate(); err != nil {
			return Match{}, err
		}
		if !ref.Queue.SameLogicalQueue(logical) {
			return Match{}, fmt.Errorf("%w: ticket %s is in queue %s, which is not a partition of %s",
				ErrQueueInvalid, ref.TicketID, ref.Queue.Key(), logical.LogicalKey())
		}
		ticketIDs[i] = ref.TicketID
		at, ok := index[ref.Queue]
		if !ok {
			at = len(partitions)
			index[ref.Queue] = at
			partitions = append(partitions, ref.Queue)
		}
		partitionOf[i] = at
	}
	if _, err := checkTicketSet(ticketIDs, logical.GroupSize); err != nil {
		return Match{}, err
	}
	if len(partitions) == 1 {
		// 全在一个分区就是普通 Commit：同一个键、同一套重放规则，结果里不带 TicketPartitions。
		return s.Commit(ctx, partitions[0], ticketIDs)
	}
	multi, ok := s.state.(versionstore.MultiUpdater[string, queueState])
	if !ok {
		return Match{}, fmt.Errorf("match: the queue state store %T cannot update several partitions atomically", s.state)
	}
	keys := make([]string, len(partitions))
	for i, queue := range partitions {
		keys[i] = queue.Key()
	}
	ticketPartitions := make([]string, len(tickets))
	for i, ref := range tickets {
		ticketPartitions[i] = ref.Queue.Partition
	}
	now := s.cfg.Now()
	matchID, err := s.cfg.NewID()
	if err != nil {
		return Match{}, err
	}

	var result Match
	var replayed bool
	_, _, err = multi.UpdateAll(ctx, keys, func(current []queueState, found []bool) ([]queueState, bool, error) {
		replayed = false
		states := make([]queueState, len(current))
		for i := range current {
			states[i] = current[i].clone()
			if !found[i] {
				return states, false, fmt.Errorf("%w: partition %s holds no tickets", ErrTicketMissing, partitions[i].Key())
			}
			s.expireLocked(&states[i], now)
		}
		ticketAt := func(i int) (Ticket, bool) {
			ticket, ok := states[partitionOf[i]].Tickets[ticketIDs[i]]
			return ticket, ok
		}

		// 重放：第一张票已匹配，且它那一局的票集合与本次完全相同、每张票都在本次所说的分区里指向那一局。
		// 超期的重放在上面的 expireLocked 里已随比赛移除，下面的校验报 ErrTicketMissing。
		if first, ok := ticketAt(0); ok && first.State == TicketMatched && first.Queue == tickets[0].Queue {
			previous, ok := states[partitionOf[0]].Matches[first.MatchID]
			if ok && len(previous.TicketIDs) == len(ticketIDs) {
				where := make(map[string]int, len(ticketIDs))
				for i, id := range ticketIDs {
					where[id] = i
				}
				same := true
				for _, id := range previous.TicketIDs {
					i, named := where[id]
					if !named {
						same = false
						break
					}
					ticket, ok := ticketAt(i)
					if !ok || ticket.State != TicketMatched || ticket.MatchID != previous.ID || ticket.Queue != tickets[i].Queue {
						same = false
						break
					}
				}
				if same {
					result, replayed = previous, true
					return states, false, nil
				}
			}
		}

		// 全部校验在任何修改之前；任何一张不通过，整次多键写不发生。
		members := make([]Subject, 0, len(ticketIDs))
		subjects := make(map[string]bool, len(ticketIDs))
		for i, id := range ticketIDs {
			ticket, ok := ticketAt(i)
			if !ok {
				return states, false, fmt.Errorf("%w: %s in partition %s", ErrTicketMissing, id, tickets[i].Queue.Key())
			}
			if ticket.Queue != tickets[i].Queue {
				return states, false, fmt.Errorf("%w: %s belongs to queue %s", ErrTicketMissing, id, ticket.Queue.Key())
			}
			if ticket.State != TicketWaiting {
				return states, false, fmt.Errorf("%w: %s is %s", ErrConflict, id, ticket.State)
			}
			// 一人一张活票只在分区内成立：同一主体可以同时在两个分区排队，所以跨分区必须再查一次。
			if subjects[ticket.Subject.Key()] {
				return states, false, fmt.Errorf("%w: subject %s appears twice", ErrTicketInvalid, ticket.Subject.Key())
			}
			subjects[ticket.Subject.Key()] = true
			members = append(members, ticket.Subject.clone())
		}

		match := Match{
			ID: matchID, Queue: tickets[0].Queue, Members: members,
			TicketIDs:        append([]string(nil), ticketIDs...),
			TicketPartitions: append([]string(nil), ticketPartitions...),
			CreatedAtUnix:    now.Unix(),
		}
		for i, id := range ticketIDs {
			state := &states[partitionOf[i]]
			ticket := state.Tickets[id]
			ticket.State = TicketMatched
			ticket.MatchID = matchID
			ticket.ResolvedAtUnix = now.Unix()
			state.Tickets[id] = ticket
			removeWaiting(state, id)
			delete(state.SubjectTickets, ticket.Subject.Key())
		}
		// 每个涉及的分区各存一份完整的比赛：读比赛不需要知道“主分区”，保留期清理也按分区各自完成，
		// 每个分区里比赛与本分区的票同进同出（pruneResolvedLocked）。
		for i := range states {
			states[i].Matches[matchID] = match.clone()
		}
		result = match
		return states, true, nil
	})
	if err != nil {
		return Match{}, err
	}
	if replayed {
		s.report.Replayed("commit_across")
	} else {
		s.report.Accepted("commit_across")
	}
	return result.clone(), nil
}

var _ Store = (*queueStore)(nil)
