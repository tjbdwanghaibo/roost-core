package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// U-0129 · C2（空洞测试）· nightly gap map core `etcd/driver` 12/20。
//
// 选举：没参选 / 会话已丢时 Resign 是 ErrNotLeader 而不是对空后端调用；Leader 在
// 没有选举对象或对方没有 leader 键时报 ErrElectionNoLeader。本地镜像：nil 客户端
// 与不支持带修订号前缀快照的客户端在构造时拒绝；负 lease id、负期望修订号、nil
// 发布上下文在触达 etcd 之前拒绝（不留下 Put / Txn）；事务返回 nil 响应要报错而
// 不是当失败；watch 通道关闭要以"watch closed"记入 LastError（与 apply 的 nil 事件
// 错误只差文本，用长重试间隔把它冻结住再读）。`client.go:46`（Get 无键 →
// ErrKeyNotFound）包着 *clientv3.Client，无法替身，留给真机集成。

type leaderValueBackend struct {
	fakeElectionBackend
	value string
}

func (b *leaderValueBackend) Leader(context.Context) (*clientv3.GetResponse, error) {
	return &clientv3.GetResponse{Kvs: []*mvccpb.KeyValue{{Value: []byte(b.value)}}}, nil
}

func TestElectionResignAndLeaderRefuseWithoutALeadership(t *testing.T) {
	e, sessions := newTestElection()
	ctx := context.Background()
	if err := e.Resign(ctx); !errors.Is(err, fetcd.ErrNotLeader) {
		t.Fatalf("Resign before any campaign = %v", err)
	}
	if _, err := e.Leader(ctx); !errors.Is(err, fetcd.ErrElectionNoLeader) {
		t.Fatalf("Leader before any campaign = %v", err)
	}
	if err := e.Campaign(ctx, "server-1"); err != nil {
		t.Fatal(err)
	}
	// 后端没有 leader 键：同样是 ErrElectionNoLeader，而不是空字符串。
	if leader, err := e.Leader(ctx); !errors.Is(err, fetcd.ErrElectionNoLeader) || leader != "" {
		t.Fatalf("Leader with an empty backend answer = (%q, %v)", leader, err)
	}
	// 会话丢失后再 Resign：领导权已经没了。
	_ = (*sessions)[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for e.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.Resign(ctx); !errors.Is(err, fetcd.ErrNotLeader) {
		t.Fatalf("Resign after the session ended = %v", err)
	}

	// 对照：真有 leader 键时返回它的值，主动 Resign 成功并放弃领导权。
	held := &election{leaderCh: make(chan struct{})}
	held.create = func() (electionSession, electionBackend, error) {
		return newFakeElectionSession(), &leaderValueBackend{value: "server-9"}, nil
	}
	if err := held.Campaign(ctx, "server-9"); err != nil {
		t.Fatal(err)
	}
	if leader, err := held.Leader(ctx); err != nil || leader != "server-9" {
		t.Fatalf("Leader = (%q, %v), want server-9", leader, err)
	}
	if err := held.Resign(ctx); err != nil {
		t.Fatalf("Resign while leading = %v", err)
	}
	if held.IsLeader() {
		t.Fatal("still leader after Resign")
	}
}

// snapshotlessEtcd is an IEtcd without IPrefixSnapshotReader.
type snapshotlessEtcd struct{ fetcd.IEtcd }

// nilTxnClient answers every transaction with no response and no error.
type nilTxnClient struct{ *mirrorTestClient }

func (nilTxnClient) Txn(context.Context, fetcd.Cmp, []fetcd.Op, []fetcd.Op) (*fetcd.TxnResponse, error) {
	return nil, nil
}

func TestNewLocalMirrorRefusesClientsWithoutRevisionedSnapshots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := NewLocalMirror[string](ctx, nil, mirrorConfig()); err == nil || !strings.Contains(err.Error(), "client is nil") {
		t.Fatalf("NewLocalMirror(nil) = %v", err)
	}
	if _, err := NewLocalMirror[string](ctx, snapshotlessEtcd{}, mirrorConfig()); err == nil || !strings.Contains(err.Error(), "does not support revisioned prefix snapshots") {
		t.Fatalf("NewLocalMirror without IPrefixSnapshotReader = %v", err)
	}
}

func TestLocalMirrorWritesRefuseInvalidArgumentsBeforeReachingEtcd(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 3})
	mirror, err := newLocalMirror(context.Background(), client, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	record := mirrorTestRecord{Count: 1}
	var nilCtx context.Context
	if err := mirror.Publish(nilCtx, "/sync/a", record); err == nil || !strings.Contains(err.Error(), "publish context is nil") {
		t.Fatalf("Publish with a nil context = %v", err)
	}
	ctx := context.Background()
	if err := mirror.PublishWithOptions(ctx, "/sync/a", record, fetcd.LocalMirrorPublishOptions{LeaseID: -1}); !errors.Is(err, fetcd.ErrMirrorInvalidConfig) || !strings.Contains(err.Error(), "lease id is negative") {
		t.Fatalf("PublishWithOptions with a negative lease = %v", err)
	}
	if _, err := mirror.PublishIfRevisionWithOptions(ctx, "/sync/a", 3, record, fetcd.LocalMirrorPublishOptions{LeaseID: -1}); !errors.Is(err, fetcd.ErrMirrorInvalidConfig) || !strings.Contains(err.Error(), "lease id is negative") {
		t.Fatalf("PublishIfRevisionWithOptions with a negative lease = %v", err)
	}
	if _, err := mirror.PublishIfRevision(ctx, "/sync/a", -1, record); err == nil || !strings.Contains(err.Error(), "expected revision is negative") {
		t.Fatalf("PublishIfRevision with a negative revision = %v", err)
	}
	if _, err := mirror.DeleteIfRevision(ctx, "/sync/a", -1); err == nil || !strings.Contains(err.Error(), "expected revision is negative") {
		t.Fatalf("DeleteIfRevision with a negative revision = %v", err)
	}
	client.mu.Lock()
	puts, txns := len(client.puts), len(client.txns)
	client.mu.Unlock()
	if puts != 0 || txns != 0 {
		t.Fatalf("refused writes reached the client: puts=%d txns=%d", puts, txns)
	}

	silent, err := newLocalMirror(context.Background(), nilTxnClient{newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 3})}, mirrorTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	if applied, err := silent.PublishIfRevision(ctx, "/sync/a", 3, record); err == nil || applied || !strings.Contains(err.Error(), "nil transaction response") {
		t.Fatalf("compare-and-apply with a nil transaction response = (%v, %v)", applied, err)
	}
}

func TestLocalMirrorRecordsAClosedWatchAsItsLastError(t *testing.T) {
	client := newMirrorTestClient(&fetcd.PrefixSnapshot{Revision: 3})
	cfg := mirrorTestConfig()
	// 重试间隔拉到一小时：watch 关闭后镜像停在等待重试，LastError 不会被重连覆盖。
	cfg.RetryMinInterval, cfg.RetryMaxInterval = time.Hour, time.Hour
	mirror, err := newLocalMirror(context.Background(), client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	<-client.watchRevisions
	close(client.watcher(0).events)
	deadline := time.Now().Add(2 * time.Second)
	for {
		last := mirror.Status().LastError
		if last != nil && strings.Contains(last.Error(), "watch closed") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("LastError after the watch closed = %v, want \"watch closed\"", last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
