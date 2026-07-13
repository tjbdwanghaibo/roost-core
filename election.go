package etcd

import (
	"context"
	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
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
	return &election{
		cli:      f.cli,
		prefix:   prefix,
		leaderCh: make(chan struct{}),
	}
}

var _ fetcd.IElectionFactory = (*electionFactory)(nil)

// election implements fetcd.IElection using concurrency.Election.
type election struct {
	cli      *clientv3.Client
	prefix   string
	session  *concurrency.Session
	elect    *concurrency.Election
	isLeader atomic.Bool
	leaderCh chan struct{}
}

func (e *election) Campaign(ctx context.Context, value string) error {
	// Create session with default TTL
	session, err := concurrency.NewSession(e.cli)
	if err != nil {
		return err
	}
	e.session = session

	elect := concurrency.NewElection(session, e.prefix)
	e.elect = elect

	// Campaign blocks until elected or context cancelled
	if err := elect.Campaign(ctx, value); err != nil {
		session.Close()
		return err
	}

	e.isLeader.Store(true)

	// Watch for session expiry (loss of leadership)
	go func() {
		select {
		case <-session.Done():
			e.isLeader.Store(false)
			close(e.leaderCh)
		case <-ctx.Done():
			e.isLeader.Store(false)
			close(e.leaderCh)
		}
	}()

	return nil
}

func (e *election) Resign(ctx context.Context) error {
	if e.elect == nil {
		return fetcd.ErrNotLeader
	}
	err := e.elect.Resign(ctx)
	e.isLeader.Store(false)
	if e.session != nil {
		e.session.Close()
	}
	return err
}

func (e *election) Leader(ctx context.Context) (string, error) {
	if e.elect == nil {
		return "", fetcd.ErrElectionNoLeader
	}
	resp, err := e.elect.Leader(ctx)
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
	return e.leaderCh
}

var _ fetcd.IElection = (*election)(nil)
