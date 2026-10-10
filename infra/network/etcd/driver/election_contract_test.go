package driver

import (
	"context"
	"errors"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"net"
	"sync"
	"testing"
	"time"
)

func TestEtcdLifetimeSuccessfulCampaignDetachesCallerAndReleasesSessionContext(t *testing.T) {
	e := &election{leaderCh: make(chan struct{})}
	var sessionCtx context.Context
	session := newFakeElectionSession()
	e.create = func(ctx context.Context) (electionSession, electionBackend, error) {
		sessionCtx = ctx
		return session, &fakeElectionBackend{rev: 42}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.Campaign(ctx, "candidate"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if sessionCtx.Err() != nil || !e.IsLeader() {
		t.Fatal("successful session still follows caller cancellation")
	}
	if token, ok := e.Fence(); !ok || token != 42 {
		t.Fatalf("fence=(%d,%v)", token, ok)
	}
	if err := e.Resign(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sessionCtx.Err() == nil || e.IsLeader() {
		t.Fatal("Resign did not release the detached context and leadership")
	}
	if err := e.Campaign(context.Background(), "next-term"); !errors.Is(err, fetcd.ErrNotLeader) {
		t.Fatalf("an already-dead session was promoted: %v", err)
	}
}

type lifetimeCancelBackend struct {
	fakeElectionBackend
	cancel context.CancelFunc
}

func (b *lifetimeCancelBackend) Campaign(context.Context, string) error {
	b.cancel()
	return nil // Simulate success racing with caller cancellation.
}

func TestEtcdLifetimeCancellationBeforeLeadershipPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &election{leaderCh: make(chan struct{})}
	session := newFakeElectionSession()
	var sessionCtx context.Context
	e.create = func(lifetime context.Context) (electionSession, electionBackend, error) {
		sessionCtx = lifetime
		return session, &lifetimeCancelBackend{cancel: cancel}, nil
	}
	if err := e.Campaign(ctx, "candidate"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled setup reported success: %v", err)
	}
	if e.IsLeader() || sessionCtx.Err() == nil {
		t.Fatal("canceled setup retained leadership or context")
	}
	select {
	case <-session.Done():
	default:
		t.Fatal("canceled session was not closed")
	}
	if _, ok := e.Fence(); ok {
		t.Fatal("canceled setup published a fence")
	}
}

func TestEtcdLifetimeCanceledCampaignNeverStartsSetup(t *testing.T) {
	e := &election{leaderCh: make(chan struct{})}
	e.create = func(context.Context) (electionSession, electionBackend, error) {
		t.Fatal("already canceled request started session creation")
		return nil, nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Campaign(ctx, "candidate"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type lifetimeClosingSession struct {
	done    chan struct{}
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	ctx     context.Context
}

func (s *lifetimeClosingSession) Done() <-chan struct{} { return s.done }
func (s *lifetimeClosingSession) Close() error {
	s.once.Do(func() { close(s.done); close(s.entered) })
	<-s.release // SDK Close signals Done before its Revoke RPC completes.
	return s.ctx.Err()
}

func TestEtcdLifetimeNormalResignDoesNotCancelRevoke(t *testing.T) {
	e := &election{leaderCh: make(chan struct{})}
	session := &lifetimeClosingSession{done: make(chan struct{}), entered: make(chan struct{}), release: make(chan struct{})}
	e.create = func(ctx context.Context) (electionSession, electionBackend, error) {
		session.ctx = ctx
		return session, &fakeElectionBackend{}, nil
	}
	if err := e.Campaign(context.Background(), "candidate"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- e.Resign(context.Background()) }()
	select {
	case <-session.entered:
	case <-time.After(time.Second):
		t.Fatal("Revoke phase not entered")
	}
	select {
	case <-e.LeaderChan(): // Expiry observer processed Done.
	case <-time.After(time.Second):
		t.Fatal("loss observer did not run")
	}
	if session.ctx.Err() != nil {
		t.Error("expiry observer canceled normal Close's Revoke")
	}
	close(session.release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Resign did not complete")
	}
	if session.ctx.Err() == nil {
		t.Fatal("Close left its context attached")
	}
}

type lifetimeLeaseServer struct {
	pb.UnimplementedLeaseServer
	entered chan struct{}
}

func (s *lifetimeLeaseServer) LeaseGrant(ctx context.Context, _ *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestEtcdLifetimeCampaignCancellationReachesSessionGrant(t *testing.T) {
	testLifetimeSessionGrant(t, false)
}

func TestEtcdLifetimeCampaignDeadlinePreservesCause(t *testing.T) {
	testLifetimeSessionGrant(t, true)
}

func testLifetimeSessionGrant(t *testing.T, deadline bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	lease := &lifetimeLeaseServer{entered: make(chan struct{}, 1)}
	pb.RegisterLeaseServer(server, lease)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{listener.Addr().String()}, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	e := NewElectionFactory(cli).NewElection("/lifetime/election")
	ctx, cancel := context.WithCancel(context.Background())
	if deadline {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
	}
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Campaign(ctx, "candidate") }()
	select {
	case <-lease.entered:
	case <-time.After(2 * time.Second):
		_ = cli.Close()
		t.Fatal("LeaseGrant never entered; not a behavior counterexample")
	}
	wantErr := error(context.Canceled)
	if deadline {
		<-ctx.Done()
		wantErr = context.DeadlineExceeded
	} else {
		cancel()
	}
	returned := false
	var campaignErr error
	select {
	case campaignErr = <-done:
		returned = true
	case <-time.After(100 * time.Millisecond):
	}
	if !returned {
		_ = cli.Close()
		select {
		case campaignErr = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("campaign did not exit after client close")
		}
	}
	t.Logf("returned_after_campaign_cancel=%v err=%v", returned, campaignErr)
	if !returned || !errors.Is(campaignErr, wantErr) {
		t.Fatalf("session Grant did not return within the canceled budget with cause %v: returned=%v err=%v", wantErr, returned, campaignErr)
	}
}

func TestEtcdLifetimeWatcherReadinessAndTerminalCauses(t *testing.T) {
	for _, mode := range []string{"created", "compacted", "closed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			raw := make(chan clientv3.WatchResponse, 1)
			w := newWatcher(ctx, raw, cancel)
			defer w.Close()
			select {
			case <-w.Ready():
				t.Fatal("premature readiness")
			default:
			}
			switch mode {
			case "created":
				raw <- clientv3.WatchResponse{Created: true}
			case "compacted":
				raw <- clientv3.WatchResponse{Canceled: true, CompactRevision: 9}
			case "closed":
				close(raw)
			}
			select {
			case <-w.Ready():
			case <-time.After(time.Second):
				t.Fatal("ready blocked")
			}
			if mode == "compacted" && !errors.Is(w.WatchError(), fetcd.ErrWatchCompacted) {
				t.Fatal(w.WatchError())
			}
			if mode == "closed" && !errors.Is(w.WatchError(), fetcd.ErrWatchClosed) {
				t.Fatal(w.WatchError())
			}
		})
	}
}

func TestEtcdLifetimeWatcherCloseUnblocksFullDeliveryQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	raw := make(chan clientv3.WatchResponse, 1)
	w := newWatcher(ctx, raw, cancel)
	events := make([]*clientv3.Event, 128)
	for i := range events {
		events[i] = &clientv3.Event{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/lifetime/a"), ModRevision: int64(i + 1)}}
	}
	raw <- clientv3.WatchResponse{Events: events}
	select {
	case <-w.Ready():
	case <-time.After(time.Second):
		t.Fatal("watch response not processed")
	}
	done := make(chan error, 1)
	go func() { done <- w.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close blocked on delivery queue")
	}
}

func TestEtcdLifetimeMirrorReadCopyCASAndStaleRevision(t *testing.T) {
	c := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 5, KVs: []*fetcd.KV{{Key: "/sync/a", Value: `{"count":1,"labels":{"owner":"one"}}`, ModRevision: 5}}})
	m, err := newLocalMirror(context.Background(), c, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.WaitForSync(ctx); err != nil {
		t.Fatal(err)
	}
	value, _, err := m.Get("/sync/a")
	if err != nil {
		t.Fatal(err)
	}
	value.Labels["owner"] = "caller"
	current, _, _ := m.Get("/sync/a")
	if current.Labels["owner"] != "one" {
		t.Fatal("read leaked stored map")
	}
	if ok, err := m.PublishIfRevision(ctx, "/sync/a", 5, mirrorTestRecord{Count: 7}); !ok || err != nil {
		t.Fatalf("CAS=%v %v", ok, err)
	}
	current, _, _ = m.Get("/sync/a")
	if current.Count != 1 {
		t.Fatal("write bypassed authoritative watch")
	}
	if err := m.apply(&fetcd.WatchEvent{Type: fetcd.EventPut, KV: &fetcd.KV{Key: "/sync/a", Value: `{"count":9}`, ModRevision: 4}}); err != nil {
		t.Fatal(err)
	}
	current, _, _ = m.Get("/sync/a")
	if current.Count != 1 || m.Revision() != 5 {
		t.Fatal("older revision overwrote current state")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.txns) != 1 || c.txns[0].cmp.Target != fetcd.CmpModRevision || c.txns[0].cmp.Value != int64(5) {
		t.Fatal("CAS did not carry expected revision")
	}
}

func resignBudgetElection(rec *revokeRecorder) *election {
	e := &election{leaderCh: make(chan struct{}), revoke: rec.revoke}
	n := 0
	e.create = func(ctx context.Context) (electionSession, electionBackend, error) {
		n++
		return &sdkLikeSession{ctx: ctx, lease: clientv3.LeaseID(n), rec: rec, done: make(chan struct{})}, &fakeElectionBackend{rev: int64(n)}, nil
	}
	return e
}

func TestElectionResignHonoursTheCallerBudget(t *testing.T) {
	rec := newRevokeRecorder(true) // Revoke hangs like an etcd that stopped answering
	e := resignBudgetElection(rec)
	if err := e.Campaign(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	lost := e.LeaderChan()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	go func() { result <- e.Resign(ctx) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(2 * time.Second):
		close(rec.release)
		t.Fatalf("Resign with a 50ms budget still blocked after 2s: the lease Revoke ignores the caller's ctx")
	}
	t.Logf("Resign(50ms) against a hanging Revoke: err=%v elapsed=%s", err, time.Since(started).Round(time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Resign past its budget = %v, want context.DeadlineExceeded", err)
	}
	if e.IsLeader() {
		t.Error("leadership still reported after Resign returned")
	}
	select {
	case <-lost:
	default:
		t.Error("LeaderChan not closed after Resign returned")
	}
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].ctxErr != nil || !calls[0].hasDeadline || calls[0].budget > abandonedLeaseRevokeTimeout {
		t.Fatalf("revoke calls = %+v, want one live Revoke bounded by %v", calls, abandonedLeaseRevokeTimeout)
	}

	// The next Campaign waits for the Revoke the election still owns.
	campaign := make(chan error, 1)
	go func() { campaign <- e.Campaign(context.Background(), "a-again") }()
	select {
	case err := <-campaign:
		t.Fatalf("Campaign started while the previous lease Revoke was still pending: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(rec.release)
	select {
	case err := <-campaign:
		if err != nil {
			t.Fatalf("Campaign after the Revoke finished: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Campaign never started after the Revoke finished")
	}
}

func TestElectionResignWaitsForTheRevokeWhenEtcdAnswers(t *testing.T) {
	rec := newRevokeRecorder(false)
	e := resignBudgetElection(rec)
	if err := e.Campaign(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resign(context.Background()); err != nil {
		t.Fatalf("Resign = %v", err)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0].ctxErr != nil {
		t.Fatalf("revoke calls = %+v, want exactly one on a live context", calls)
	}
	if e.IsLeader() {
		t.Fatal("leadership still reported after Resign")
	}
}

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
