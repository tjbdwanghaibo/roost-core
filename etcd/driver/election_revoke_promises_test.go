package driver

// RR-20261004-06：竞选在 session 建好之后失败或被取消，lease 的 Revoke 不能用已被取消的
// context 发出——那样服务端什么都没撤，候选键 / 领导键留到 TTL。caller 还在等时，Campaign
// 返回前 Revoke 已经结束；caller 已走时 Campaign 立刻返回，election 持有唯一一个有截止的
// Revoke，下一次 Campaign 等它结束，关闭 client 会取消它。旧行为（1502f973）：清理分支先
// cancelSession() 再 session.Close()，SDK Close 用这个已取消的 context 发 Revoke。
// 真实 etcd 上的同一承诺见 real_etcd_election_promises_test.go（integration tag）。
import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type revokeCall struct {
	lease       clientv3.LeaseID
	ctxErr      error
	hasDeadline bool
	budget      time.Duration
}

// revokeRecorder stands for the lease Revoke RPC. Like gRPC it fails at once
// on an already canceled context; otherwise it waits for release (when set)
// or for its context.
type revokeRecorder struct {
	mu      sync.Mutex
	calls   []revokeCall
	entered chan struct{}
	release chan struct{}
}

func newRevokeRecorder(blocking bool) *revokeRecorder {
	r := &revokeRecorder{entered: make(chan struct{}, 4)}
	if blocking {
		r.release = make(chan struct{})
	}
	return r
}

func (r *revokeRecorder) revoke(ctx context.Context, lease clientv3.LeaseID) error {
	call := revokeCall{lease: lease, ctxErr: ctx.Err()}
	if deadline, ok := ctx.Deadline(); ok {
		call.hasDeadline, call.budget = true, time.Until(deadline)
	}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	r.entered <- struct{}{}
	if call.ctxErr != nil {
		return call.ctxErr
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *revokeRecorder) snapshot() []revokeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]revokeCall(nil), r.calls...)
}

// sdkLikeSession mirrors concurrency.Session: Close orphans the keepalive and
// then revokes the lease on the context the session was created with.
type sdkLikeSession struct {
	ctx    context.Context
	lease  clientv3.LeaseID
	rec    *revokeRecorder
	done   chan struct{}
	orphan sync.Once
}

func (s *sdkLikeSession) Done() <-chan struct{}   { return s.done }
func (s *sdkLikeSession) Lease() clientv3.LeaseID { return s.lease }
func (s *sdkLikeSession) Orphan()                 { s.orphan.Do(func() { close(s.done) }) }
func (s *sdkLikeSession) Close() error {
	s.Orphan()
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute) // SDK: ttl seconds
	defer cancel()
	return s.rec.revoke(ctx, s.lease)
}

type failingCampaignBackend struct {
	fakeElectionBackend
	run func(ctx context.Context) error
}

func (b *failingCampaignBackend) Campaign(ctx context.Context, _ string) error { return b.run(ctx) }

func revokeTestElection(rec *revokeRecorder, run func(ctx context.Context) error) (*election, *[]*sdkLikeSession) {
	sessions := []*sdkLikeSession{}
	e := &election{leaderCh: make(chan struct{}), revoke: rec.revoke}
	e.create = func(ctx context.Context) (electionSession, electionBackend, error) {
		s := &sdkLikeSession{ctx: ctx, lease: clientv3.LeaseID(len(sessions) + 1), rec: rec, done: make(chan struct{})}
		sessions = append(sessions, s)
		if len(sessions) > 1 {
			return s, &fakeElectionBackend{rev: 7}, nil
		}
		return s, &failingCampaignBackend{run: run}, nil
	}
	return e, &sessions
}

func checkAbandonRevoke(t *testing.T, calls []revokeCall) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("revoke calls=%d, want exactly 1", len(calls))
	}
	call := calls[0]
	t.Logf("revoke_ctx_err=%v has_deadline=%v budget=%v", call.ctxErr, call.hasDeadline, call.budget.Round(time.Millisecond))
	if call.ctxErr != nil {
		t.Fatal("abandoned campaign revoked its lease on an already canceled context; the lease stays until TTL")
	}
	if !call.hasDeadline || call.budget > abandonedLeaseRevokeTimeout {
		t.Fatalf("abandoned Revoke must carry its own bounded deadline (<= %v)", abandonedLeaseRevokeTimeout)
	}
}

// (a) The campaign fails with a non-context error while the caller waits: the
// Revoke is done, on a live context, before Campaign returns.
func TestElectionRevokeFailedCampaignRevokesBeforeReturning(t *testing.T) {
	rec := newRevokeRecorder(false)
	lost := errors.New("lost watcher waiting for delete")
	e, sessions := revokeTestElection(rec, func(context.Context) error { return lost })
	if err := e.Campaign(context.Background(), "a"); !errors.Is(err, lost) {
		t.Fatalf("Campaign=%v", err)
	}
	checkAbandonRevoke(t, rec.snapshot())
	select {
	case <-(*sessions)[0].Done():
	default:
		t.Fatal("failed campaign left the keepalive running")
	}
	if (*sessions)[0].ctx.Err() == nil {
		t.Fatal("failed campaign kept its session context")
	}
}

// (b)/(d) The caller cancels (here: as the real campaign wins). Campaign
// returns at once; the election owns one bounded Revoke, the next Campaign
// waits for it, and closing the client cancels it.
func TestElectionRevokeCanceledCampaignOwnsOneBoundedRevoke(t *testing.T) {
	rec := newRevokeRecorder(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, sessions := revokeTestElection(rec, func(context.Context) error { cancel(); return nil })
	returned := make(chan error, 1)
	go func() { returned <- e.Campaign(ctx, "a") }()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Campaign=%v", err)
		}
	case <-time.After(time.Second):
		close(rec.release)
		t.Fatal("canceled Campaign waited for the lease Revoke")
	}
	select {
	case <-rec.entered:
	case <-time.After(time.Second):
		t.Fatal("canceled campaign never revoked its lease")
	}
	checkAbandonRevoke(t, rec.snapshot())

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer waitCancel()
	if err := e.Campaign(waitCtx, "b"); !errors.Is(err, context.DeadlineExceeded) || len(*sessions) != 1 {
		t.Fatalf("next Campaign did not wait for the pending Revoke: err=%v sessions=%d", err, len(*sessions))
	}
	next := make(chan error, 1)
	go func() { next <- e.Campaign(context.Background(), "b") }()
	close(rec.release)
	select {
	case err := <-next:
		if err != nil || !e.IsLeader() {
			t.Fatalf("Campaign after the Revoke ended: err=%v leader=%v", err, e.IsLeader())
		}
	case <-time.After(time.Second):
		t.Fatal("next Campaign stayed blocked after the Revoke ended")
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("revoke calls=%d, want 1", got)
	}
}

func TestElectionRevokeAbandonedRevokeEndsWithClient(t *testing.T) {
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{"127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := newRevokeRecorder(true)
	defer close(rec.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, _ := revokeTestElection(rec, func(context.Context) error { cancel(); return context.Canceled })
	e.cli = cli
	if err := e.Campaign(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Campaign=%v", err)
	}
	<-rec.entered
	e.mu.Lock()
	pending := e.revoking
	e.mu.Unlock()
	if pending == nil {
		t.Fatal("no Revoke owned by the election")
	}
	_ = cli.Close()
	select {
	case <-pending:
	case <-time.After(time.Second):
		t.Fatal("closing the client did not end the abandoned Revoke")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoking != nil {
		t.Fatal("finished Revoke left the election waiting")
	}
}
