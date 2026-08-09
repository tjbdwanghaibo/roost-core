package etcd

import (
	"context"
	"fmt"
	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
	"sync"
	"sync/atomic"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// electionFactory implements fetcd.IElectionFactory.
type electionFactory struct {
	cli *clientv3.Client
}

func newElectionFactory(cli *clientv3.Client) *electionFactory {
	return &electionFactory{cli: cli}
}

func (f *electionFactory) NewElection(prefix string) fetcd.IElection {
	e := &election{
		cli:      f.cli,
		prefix:   prefix,
		leaderCh: make(chan struct{}),
	}
	e.create = e.createWithEtcd
	return e
}

var _ fetcd.IElectionFactory = (*electionFactory)(nil)

// election implements fetcd.IElection using concurrency.Election.
type election struct {
	cli      *clientv3.Client
	prefix   string
	create   func() (electionSession, electionBackend, error)
	session  electionSession
	elect    electionBackend
	isLeader atomic.Bool
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
}

func (e *election) createWithEtcd() (electionSession, electionBackend, error) {
	session, err := concurrency.NewSession(e.cli)
	if err != nil {
		return nil, nil, err
	}
	return session, concurrency.NewElection(session, e.prefix), nil
}

func (e *election) Campaign(ctx context.Context, value string) error {
	e.mu.Lock()
	if e.campaign {
		e.mu.Unlock()
		return fmt.Errorf("etcd election: campaign already active")
	}
	e.campaign = true
	e.leaderCh = make(chan struct{})
	e.closed = false
	e.mu.Unlock()

	create := e.create
	if create == nil {
		create = e.createWithEtcd
	}
	session, elect, err := create()
	if err != nil {
		e.finish(nil)
		return err
	}
	e.mu.Lock()
	if !e.campaign {
		e.mu.Unlock()
		_ = session.Close()
		return fetcd.ErrNotLeader
	}
	e.session = session
	e.elect = elect
	e.mu.Unlock()

	// Campaign blocks until elected or context cancelled
	if err := elect.Campaign(ctx, value); err != nil {
		_ = session.Close()
		e.finish(session)
		return err
	}

	e.mu.Lock()
	if e.session != session || !e.campaign {
		e.mu.Unlock()
		_ = session.Close()
		return fetcd.ErrNotLeader
	}
	e.isLeader.Store(true)
	e.mu.Unlock()

	// Watch for session expiry (loss of leadership)
	go func() {
		<-session.Done()
		e.finish(session)
	}()

	return nil
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
		return "", fetcd.ErrElectionNoLeader
	}
	if len(resp.Kvs) == 0 {
		return "", fetcd.ErrElectionNoLeader
	}
	return string(resp.Kvs[0].Value), nil
}

func (e *election) IsLeader() bool {
	return e.isLeader.Load()
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
