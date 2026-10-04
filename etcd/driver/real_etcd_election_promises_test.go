//go:build integration

package driver

// RR-20261004-06：竞选在 session 建好之后失败或被取消，必须即时撤销这次 session 的 lease，
// 候选键 / 领导键随之删除，其他候选能马上当选。旧行为（NC-11 修复 1502f973 引入）：清理分支在
// session.Close 之前先 cancelSession()，而 SDK Session.Close 用同一个 ctx 发 Revoke，于是 Revoke
// 立即以 Canceled 失败，lease 和挂在上面的键留到 TTL（缺省 60s）——残留的若是领导键，TTL 内
// 谁都选不上。三个场景都走真实单节点 etcd（PATH 上的 etcd，没有则跳过）。
import (
	"context"
	"errors"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// failAfterCampaign runs the real campaign (the Txn writes the candidate key)
// and then reports err, or calls after first, so the cleanup branches that run
// on a live etcd key can be reached deterministically.
type failAfterCampaign struct {
	electionBackend
	after func()
	err   error
}

func (b *failAfterCampaign) Campaign(ctx context.Context, value string) error {
	if err := b.electionBackend.Campaign(ctx, value); err != nil {
		return err
	}
	if b.after != nil {
		b.after()
	}
	return b.err
}

func realElectionClient(t *testing.T, endpoint string) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func realElectionKeys(t *testing.T, cli *clientv3.Client, prefix string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		t.Fatal(err)
	}
	return resp.Count
}

func realLeaseTTL(t *testing.T, cli *clientv3.Client, lease clientv3.LeaseID) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.TimeToLive(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	return resp.TTL
}

// recordingElection wraps the real session factory so the test learns the
// lease of the session the campaign created.
func recordingElection(cli *clientv3.Client, prefix string, backend func(electionBackend) electionBackend) (*election, *clientv3.LeaseID) {
	e := &election{cli: cli, prefix: prefix, leaderCh: make(chan struct{})}
	lease := new(clientv3.LeaseID)
	e.create = func(ctx context.Context) (electionSession, electionBackend, error) {
		s, b, err := e.createWithEtcd(ctx)
		if err != nil {
			return nil, nil, err
		}
		*lease = s.(*concurrency.Session).Lease()
		return s, backend(b), nil
	}
	return e, lease
}

func campaignWithin(t *testing.T, endpoint, prefix, value string, budget time.Duration) error {
	t.Helper()
	other := NewElectionFactory(realElectionClient(t, endpoint)).NewElection(prefix)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	err := other.Campaign(ctx, value)
	if err == nil {
		_ = other.Resign(context.Background())
	}
	return err
}

// (a) elect.Campaign reports a non-context error after its Txn wrote the
// candidate key (e.g. SDK waitDeletes "lost watcher"); the caller is still
// waiting, so the Revoke finishes before Campaign returns.
func TestRealEtcdFailedCampaignRevokesItsLease(t *testing.T) {
	endpoint := startEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261004-06/failed/"
	e, lease := recordingElection(cli, prefix, func(b electionBackend) electionBackend {
		return &failAfterCampaign{electionBackend: b, err: errors.New("lost watcher waiting for delete")}
	})
	err := e.Campaign(context.Background(), "a")
	ttl, keys := realLeaseTTL(t, cli, *lease), realElectionKeys(t, cli, prefix)
	t.Logf("campaign_err=%v lease_ttl_after_failed_campaign=%d candidate_keys=%d", err, ttl, keys)
	if err == nil || e.IsLeader() {
		t.Fatal("failed campaign reported success")
	}
	if ttl != -1 || keys != 0 {
		t.Fatal("failed campaign left its lease and candidate key until TTL")
	}
	if oerr := campaignWithin(t, endpoint, prefix, "b", 2*time.Second); oerr != nil {
		t.Fatalf("another candidate could not be elected after the failure: %v", oerr)
	}
}

// (b) The real campaign wins, then the caller cancels before leadership is
// published: Campaign reports cancellation, so the leader key it wrote must
// not keep blocking everyone else.
func TestRealEtcdCanceledWinningCampaignLeavesNoPhantomLeader(t *testing.T) {
	endpoint := startEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261004-06/race/"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, lease := recordingElection(cli, prefix, func(b electionBackend) electionBackend {
		return &failAfterCampaign{electionBackend: b, after: cancel}
	})
	err := e.Campaign(ctx, "a")
	if !errors.Is(err, context.Canceled) || e.IsLeader() {
		t.Fatalf("canceled winning campaign: err=%v is_leader=%v", err, e.IsLeader())
	}
	oerr := campaignWithin(t, endpoint, prefix, "b", 3*time.Second)
	ttl := realLeaseTTL(t, cli, *lease)
	t.Logf("first_campaign_err=%v other_campaign_err=%v first_lease_ttl=%d", err, oerr, ttl)
	if oerr != nil {
		t.Fatal("first candidate reported failure but its leader key still blocks every other candidate")
	}
	if ttl != -1 {
		t.Fatal("canceled winning campaign left its lease until TTL")
	}
}

// (d) The caller cancels while waiting behind another leader. The SDK deletes
// the candidate key itself, but the lease must be revoked too; otherwise every
// timed-out retry of a standby leaves one more orphan lease for a full TTL.
func TestRealEtcdCanceledWaitingCampaignRevokesItsLease(t *testing.T) {
	endpoint := startEtcd(t)
	cli := realElectionClient(t, endpoint)
	prefix := "/rr-20261004-06/waiting/"
	leader := NewElectionFactory(realElectionClient(t, endpoint)).NewElection(prefix)
	if err := leader.Campaign(context.Background(), "leader"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = leader.Resign(context.Background()) }()
	e, lease := recordingElection(cli, prefix, func(b electionBackend) electionBackend { return b })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := e.Campaign(ctx, "standby")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting campaign: %v", err)
	}
	// The caller is gone, so the Revoke runs after Campaign returned; give it
	// a bounded window well below the 60s TTL.
	deadline := time.Now().Add(2 * time.Second)
	ttl := realLeaseTTL(t, cli, *lease)
	for ttl != -1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		ttl = realLeaseTTL(t, cli, *lease)
	}
	keys := realElectionKeys(t, cli, prefix)
	t.Logf("campaign_err=%v standby_lease_ttl=%d keys=%d", err, ttl, keys)
	if ttl != -1 || keys != 1 {
		t.Fatal("canceled waiting campaign left an orphan lease until TTL")
	}
}
