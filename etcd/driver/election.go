package driver

import (
	"context"
	"errors"
	"fmt"
	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
	"sync"
	"sync/atomic"

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
	cli      *clientv3.Client
	prefix   string
	create   func(context.Context) (electionSession, electionBackend, error)
	session  electionSession
	elect    electionBackend
	isLeader atomic.Bool
	fence    atomic.Int64
	mu       sync.Mutex
	leaderCh chan struct{}
	closed   bool
	campaign bool
}

type electionSession interface {
	Done() <-chan struct{}
	Close() error
}

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

func (e *election) Campaign(ctx context.Context, value string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
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

	create := e.create
	if create == nil {
		create = e.createWithEtcd
	}
	// RR-20261004-NC-11: setup follows the caller's cancellation, while a
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
	e.mu.Lock()
	if !e.campaign {
		e.mu.Unlock()
		cancelSession()
		_ = session.Close()
		return fetcd.ErrNotLeader
	}
	e.session = session
	e.elect = elect
	e.mu.Unlock()

	// Campaign blocks until elected or context cancelled
	if err := elect.Campaign(ctx, value); err != nil {
		cancelSession()
		_ = session.Close()
		e.finish(session)
		return err
	}
	if !stopCancellation() || ctx.Err() != nil {
		cancelSession()
		_ = session.Close()
		e.finish(session)
		return ctx.Err()
	}

	e.mu.Lock()
	if e.session != session || !e.campaign {
		e.mu.Unlock()
		cancelSession()
		_ = session.Close()
		return fetcd.ErrNotLeader
	}
	select {
	case <-session.Done():
		e.mu.Unlock()
		cancelSession()
		_ = session.Close()
		e.finish(session)
		return fetcd.ErrNotLeader
	default:
	}
	// Publish the fencing token before the leadership flag: a caller that
	// observes IsLeader must be able to read the token of that term.
	e.fence.Store(elect.Rev())
	e.isLeader.Store(true)
	e.mu.Unlock()

	// Watch for session expiry (loss of leadership)
	go func() {
		<-session.Done()
		e.finish(session)
		owned.releaseContext()
	}()

	return nil
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

func (e *election) Resign(ctx context.Context) error {
	e.mu.Lock()
	elect := e.elect
	session := e.session
	active := e.campaign
	e.mu.Unlock()
	if !active || elect == nil || session == nil {
		return fetcd.ErrNotLeader
	}
	err := elect.Resign(ctx)
	closeErr := session.Close()
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
