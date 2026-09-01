package etcd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type fakeElectionSession struct {
	done chan struct{}
	once sync.Once
}

func TestElectionFirstCampaignKeepsPreCampaignLeaderChannel(t *testing.T) {
	e, sessions := newTestElection()
	before := e.LeaderChan()
	if err := e.Campaign(context.Background(), "server-1"); err != nil {
		t.Fatal(err)
	}
	if before != e.LeaderChan() {
		t.Fatal("first campaign replaced the channel observed by an early waiter")
	}
	_ = (*sessions)[0].Close()
	select {
	case <-before:
	case <-time.After(time.Second):
		t.Fatal("pre-campaign waiter was not notified of leadership loss")
	}
}

type failingLeaderBackend struct{ fakeElectionBackend }

func (f *failingLeaderBackend) Leader(context.Context) (*clientv3.GetResponse, error) {
	return nil, context.DeadlineExceeded
}

func TestElectionLeaderPreservesBackendError(t *testing.T) {
	e := &election{elect: &failingLeaderBackend{}}
	_, err := e.Leader(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Leader error lost backend cause: %v", err)
	}
}

func newFakeElectionSession() *fakeElectionSession {
	return &fakeElectionSession{done: make(chan struct{})}
}

func (s *fakeElectionSession) Done() <-chan struct{} { return s.done }
func (s *fakeElectionSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

type fakeElectionBackend struct {
	value string
	rev   int64
}

func (e *fakeElectionBackend) Campaign(_ context.Context, value string) error {
	e.value = value
	return nil
}

func (e *fakeElectionBackend) Rev() int64 { return e.rev }

func (e *fakeElectionBackend) Resign(context.Context) error { return nil }

func (e *fakeElectionBackend) Leader(context.Context) (*clientv3.GetResponse, error) {
	return &clientv3.GetResponse{}, nil
}

func newTestElection() (*election, *[]*fakeElectionSession) {
	sessions := make([]*fakeElectionSession, 0, 2)
	e := &election{leaderCh: make(chan struct{})}
	e.create = func() (electionSession, electionBackend, error) {
		session := newFakeElectionSession()
		sessions = append(sessions, session)
		return session, &fakeElectionBackend{rev: int64(len(sessions))}, nil
	}
	return e, &sessions
}

func TestElectionCampaignContextCancellationAfterElectionDoesNotLoseLeadership(t *testing.T) {
	e, sessions := newTestElection()
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Campaign(ctx, "server-1"); err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	leaderLost := e.LeaderChan()
	cancel()
	time.Sleep(10 * time.Millisecond)
	if !e.IsLeader() {
		t.Fatal("cancelling the campaign wait context after election must not lose the session-backed leadership")
	}
	select {
	case <-leaderLost:
		t.Fatal("leader channel closed while the election session is still active")
	default:
	}
	if err := (*sessions)[0].Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-leaderLost:
	case <-time.After(time.Second):
		t.Fatal("leader channel did not close after session loss")
	}
}

func TestElectionFenceTracksLeadershipTerm(t *testing.T) {
	e, sessions := newTestElection()
	if _, ok := e.Fence(); ok {
		t.Fatal("fence must be unavailable before leadership")
	}
	if err := e.Campaign(context.Background(), "server-1"); err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	token, ok := e.Fence()
	if !ok || token != 1 {
		t.Fatalf("fence=(%d,%v), want (1,true)", token, ok)
	}
	// Session end (lease loss) revokes the fence together with leadership.
	_ = (*sessions)[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := e.Fence(); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fence survived leadership loss")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestElectionResignClearsLifecycleBeforeReturning(t *testing.T) {
	e, _ := newTestElection()
	if err := e.Campaign(context.Background(), "server-1"); err != nil {
		t.Fatalf("first Campaign: %v", err)
	}
	firstLeaderLost := e.LeaderChan()
	if err := e.Resign(context.Background()); err != nil {
		t.Fatalf("Resign: %v", err)
	}
	if e.IsLeader() {
		t.Fatal("Resign returned while IsLeader remained true")
	}
	select {
	case <-firstLeaderLost:
	default:
		t.Fatal("Resign returned before closing LeaderChan")
	}
	if err := e.Campaign(context.Background(), "server-1-again"); err != nil {
		t.Fatalf("second Campaign immediately after Resign: %v", err)
	}
	if !e.IsLeader() {
		t.Fatal("second campaign did not become leader")
	}
}
