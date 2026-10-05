package driver

// RR-20261005-NC-93：正常 Resign 的 lease 撤销不受调用方预算约束。旧行为：Resign 在 elect.Resign(ctx)
// 之后调用 SDK Session.Close，后者用 session 自己的 context 加 TTL 秒（默认 60s）发 Revoke，不看调用方
// ctx；etcd 无响应时 Resign(500ms) 阻塞 20s 以上。承诺：Resign 在调用方 ctx 到期后返回 ctx 错误，本地
// 领导权已结束；Revoke 由 election 持有、带自己的截止（与 RR-20261004-06 的放弃路径同一机制），下一次
// Campaign 等它结束；健康时 Resign 等到 Revoke 完成、返回 nil。真实 etcd 上的同一承诺见
// real_etcd_election_promises_test.go（integration tag）。

import (
	"context"
	"errors"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

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
