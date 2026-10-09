package driver

// RR-20261004-NC-11：竞选取消必须到达真实 session Grant，成功领导权不绑定等待 context。
import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
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
