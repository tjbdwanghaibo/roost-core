package driver

import (
	"context"
	"errors"
	"fmt"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"sync"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// ElectionFactory implements fetcd.IElectionFactory.
type ElectionFactory struct {
	cli *clientv3.Client
}

func NewElectionFactory(cli *clientv3.Client) *ElectionFactory {
	return &ElectionFactory{cli: cli}
}

func (f *ElectionFactory) NewElection(prefix string) fetcd.IElection {
	e := &election{
		cli:      f.cli,
		prefix:   prefix,
		leaderCh: make(chan struct{}),
	}
	e.create = e.createWithEtcd
	return e
}

var _ fetcd.IElectionFactory = (*ElectionFactory)(nil)

// election implements fetcd.IElection using concurrency.Election.
type election struct {
	cli    *clientv3.Client
	prefix string
	create func(context.Context) (electionSession, electionBackend, error)
	// revoke overrides the lease Revoke of an abandoned campaign (tests); nil
	// uses cli.
	revoke   func(context.Context, clientv3.LeaseID) error
	session  electionSession
	elect    electionBackend
	isLeader atomic.Bool
	fence    atomic.Int64
	mu       sync.Mutex
	leaderCh chan struct{}
	closed   bool
	campaign bool
	// revoking is closed when the Revoke of the last abandoned campaign
	// session ends; nil when none is pending. See abandon.
	revoking chan struct{}
}

type electionSession interface {
	Done() <-chan struct{}
	Close() error
}

// leaseSession is the part of concurrency.Session that lets a failed campaign
// stop the keepalive and revoke the lease under a context of its own.
type leaseSession interface {
	Orphan()
	Lease() clientv3.LeaseID
}

// abandonedLeaseRevokeTimeout bounds the Revoke the election owns when a
// session ends: a campaign that failed before leadership was published, or a
// Resign. The lease still expires at its TTL if this Revoke
// does not get through.
const abandonedLeaseRevokeTimeout = 5 * time.Second

type electionBackend interface {
	Campaign(ctx context.Context, value string) error
	Resign(ctx context.Context) error
	Leader(ctx context.Context) (*clientv3.GetResponse, error)
	// Rev is the CreateRevision of this candidate's campaign key: it
	// increases monotonically across leadership changes of the prefix and
	// serves as the fencing token.
	Rev() int64
}

func (e *election) createWithEtcd(ctx context.Context) (electionSession, electionBackend, error) {
	session, err := concurrency.NewSession(e.cli, concurrency.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return session, concurrency.NewElection(session, e.prefix), nil
}

// Campaign 参选直到当选或 ctx 结束：登记本次参选 → 建立与客户端同寿命的会话（建立期间跟随调用方取消）→
// 挂上会话 → 竞选 → 解除取消桥接 → 先发布 fence 再发布 leader 标志 → 监视会话过期。
// 会话建立之后的每个失败分支都把会话交给 abandon 撤销租约。
func (e *election) Campaign(ctx context.Context, value string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.beginCampaign(ctx); err != nil {
		return err
	}

	create := e.create
	if create == nil {
		create = e.createWithEtcd
	}
	// setup follows the caller's cancellation, while a
	// successful session follows the client lifetime. Detach the cancellation
	// bridge before publishing leadership; using ctx as the lifetime directly
	// would revoke leadership when the successful caller stops waiting.
	lifetime := context.Background()
	if e.cli != nil {
		lifetime = e.cli.Ctx()
	}
	sessionCtx, cancelSession := context.WithCancel(lifetime)
	stopCancellation := context.AfterFunc(ctx, cancelSession)
	defer stopCancellation()
	session, elect, err := create(sessionCtx)
	if err != nil {
		cancelSession()
		e.finish(nil)
		if callerErr := ctx.Err(); callerErr != nil {
			return errors.Join(callerErr, err)
		}
		return err
	}
	owned := &campaignSession{electionSession: session, cancel: cancelSession}
	session = owned
	// every failure branch below hands the session to
	// abandon. Canceling sessionCtx before session.Close (as 1502f973 did)
	// made the SDK send its Revoke on that canceled context, so the lease and
	// the candidate or leader key stayed until TTL and blocked other
	// candidates.
	if !e.attachCampaignSession(session, elect) {
		e.abandon(ctx, owned, lifetime)
		return fetcd.ErrNotLeader
	}

	// Campaign blocks until elected or context cancelled
	if err := elect.Campaign(ctx, value); err != nil {
		e.abandon(ctx, owned, lifetime)
		e.finish(session)
		return err
	}
	if !stopCancellation() || ctx.Err() != nil {
		e.abandon(ctx, owned, lifetime)
		e.finish(session)
		return ctx.Err()
	}

	if published, sessionEnded := e.publishLeadership(session, elect); !published {
		e.abandon(ctx, owned, lifetime)
		if sessionEnded {
			e.finish(session)
		}
		return fetcd.ErrNotLeader
	}

	// Watch for session expiry (loss of leadership)
	go func() {
		<-session.Done()
		e.finish(session)
		owned.releaseContext()
	}()

	return nil
}

// beginCampaign 登记一次参选：先等上一次被放弃会话的 Revoke 结束，再置 campaign；已关闭的选举换一个新的
// leaderCh。已有参选在进行时拒绝。
func (e *election) beginCampaign(ctx context.Context) error {
	e.mu.Lock()
	for e.revoking != nil && !e.campaign {
		// a new session would queue its key behind the stale
		// one still being revoked; waiting also keeps at most one abandoned
		// Revoke per election.
		pending := e.revoking
		e.mu.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
			return ctx.Err()
		}
		e.mu.Lock()
	}
	if e.campaign {
		e.mu.Unlock()
		return fmt.Errorf("etcd election: campaign already active")
	}
	e.campaign = true
	if e.closed {
		e.leaderCh = make(chan struct{})
		e.closed = false
	}
	e.mu.Unlock()
	return nil
}

// attachCampaignSession 把新会话挂到选举上；会话建立期间参选已被取消（Resign / Close）时返回 false。
func (e *election) attachCampaignSession(session electionSession, elect electionBackend) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.campaign {
		return false
	}
	e.session = session
	e.elect = elect
	return true
}

// publishLeadership 在当选后确认会话仍是本次参选且未过期，然后先发布 fence 再发布 leader 标志。
// published 为 false 时调用方放弃会话；sessionEnded 表示会话已过期，调用方还要 finish 它。
func (e *election) publishLeadership(session electionSession, elect electionBackend) (published, sessionEnded bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != session || !e.campaign {
		return false, false
	}
	select {
	case <-session.Done():
		return false, true
	default:
	}
	// Publish the fencing token before the leadership flag: a caller that
	// observes IsLeader must be able to read the token of that term.
	e.fence.Store(elect.Rev())
	e.isLeader.Store(true)
	return true, false
}

// campaignSession releases the detached lifetime on session loss or Close.
// Session.Close closes Done before its lease Revoke RPC. Serialize cancellation
// with Close so the expiry observer cannot cancel that RPC during normal Resign.
type campaignSession struct {
	electionSession
	cancel    context.CancelFunc
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func (s *campaignSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeOnce.Do(func() {
		s.closeErr = s.electionSession.Close()
		s.cancel()
	})
	return s.closeErr
}

func (s *campaignSession) releaseContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel()
}

// closeError reports the result of a Close that already ran.
func (s *campaignSession) closeError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

// orphan ends a session whose campaign failed: it stops the keepalive and
// releases the session context, and returns the lease still to revoke (ok
// false when the session was already closed or has no lease handle, in which
// case it is closed here). It shares closeOnce with Close.
func (s *campaignSession) orphan() (lease clientv3.LeaseID, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeOnce.Do(func() {
		leased, isLeased := s.electionSession.(leaseSession)
		if !isLeased {
			s.closeErr = s.electionSession.Close()
			s.cancel()
			return
		}
		leased.Orphan()
		s.cancel()
		lease, ok = leased.Lease(), true
	})
	return lease, ok
}

// abandon releases a session: one whose campaign failed before leadership was
// published, and also the session a Resign gives up. The keepalive stops at once; the lease Revoke runs under its
// own deadline derived from lifetime (the client context, so closing the
// client cancels it), never from sessionCtx, which the caller's cancellation
// may already have canceled, and never from the SDK's Session.Close, whose
// Revoke waits up to the session TTL whatever the caller's budget is. While the
// caller still waits it waits for the Revoke and gets its result, so the call
// returns with the keys gone; a caller whose ctx ends first gets ctx.Err() at
// once and the election owns the single Revoke, which the next Campaign waits
// for. The result is nil or the Revoke error when the Revoke finished first.
func (e *election) abandon(ctx context.Context, s *campaignSession, lifetime context.Context) error {
	lease, ok := s.orphan()
	if !ok {
		return s.closeError()
	}
	done := make(chan struct{})
	var revokeErr error
	e.mu.Lock()
	e.revoking = done
	e.mu.Unlock()
	go func() {
		revokeCtx, cancel := context.WithTimeout(lifetime, abandonedLeaseRevokeTimeout)
		revokeErr = e.revokeLease(revokeCtx, lease) // On failure the lease expires at its TTL.
		cancel()
		e.mu.Lock()
		if e.revoking == done {
			e.revoking = nil
		}
		e.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return revokeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *election) revokeLease(ctx context.Context, lease clientv3.LeaseID) error {
	if e.revoke != nil {
		return e.revoke(ctx, lease)
	}
	if e.cli == nil {
		return nil
	}
	_, err := e.cli.Revoke(ctx, lease)
	return err
}

func (e *election) Resign(ctx context.Context) error {
	e.mu.Lock()
	elect := e.elect
	session := e.session
	active := e.campaign
	e.mu.Unlock()
	if !active || elect == nil || session == nil {
		return fetcd.ErrNotLeader
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := elect.Resign(ctx)
	// SDK Session.Close 用 session 自己的 context 加 TTL 秒发 Revoke，不看调用方
	// ctx，etcd 无响应时 Resign 会阻塞到 TTL（默认 60s）。改走 abandon：调用方在等就等 Revoke 的
	// 结果，预算先到就返回 ctx 错误，Revoke 由 election 持有并自带截止，下一次 Campaign 等它。
	var closeErr error
	if owned, ok := session.(*campaignSession); ok {
		lifetime := context.Background()
		if e.cli != nil {
			lifetime = e.cli.Ctx()
		}
		closeErr = e.abandon(ctx, owned, lifetime)
	} else {
		closeErr = session.Close()
	}
	e.finish(session)
	if err == nil {
		err = closeErr
	}
	return err
}

func (e *election) Leader(ctx context.Context) (string, error) {
	e.mu.Lock()
	elect := e.elect
	e.mu.Unlock()
	if elect == nil {
		return "", fetcd.ErrElectionNoLeader
	}
	resp, err := elect.Leader(ctx)
	if err != nil {
		return "", errors.Join(fetcd.ErrElectionNoLeader, err)
	}
	if len(resp.Kvs) == 0 {
		return "", fetcd.ErrElectionNoLeader
	}
	return string(resp.Kvs[0].Value), nil
}

func (e *election) IsLeader() bool {
	return e.isLeader.Load()
}

// Fence implements IFencedElection: the campaign key's CreateRevision,
// monotonic across leadership changes of the prefix. Leadership-sensitive
// writes should carry it and reject older tokens, which closes the inherent
// IsLeader stale window (lease expired server-side, client not yet aware).
func (e *election) Fence() (int64, bool) {
	if !e.isLeader.Load() {
		return 0, false
	}
	return e.fence.Load(), true
}

func (e *election) LeaderChan() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leaderCh
}

func (e *election) finish(session electionSession) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if session != nil && e.session != session {
		return
	}
	e.isLeader.Store(false)
	e.campaign = false
	e.session = nil
	e.elect = nil
	if !e.closed {
		close(e.leaderCh)
		e.closed = true
	}
}

var _ fetcd.IElection = (*election)(nil)
var _ fetcd.IFencedElection = (*election)(nil)
