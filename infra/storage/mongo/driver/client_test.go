// Mongo 客户端：Close 幂等与并发等待、索引创建穿过选举有界重试、事务重试保留最后的错误链。

package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 驱动 Close 的统一口径：重复 Close 幂等返回 nil，第一次的错误只报一次；并发 Close 的后到者等第一个做完再返回；Close 之后的其他调用返回
// mongo.ErrClientDisconnected。用 disconnect 替身把第一次断开卡住，观察后到者是否在它完成之前返回。
// 历史：RR-20261006-10。
func TestClientConcurrentCloseWaitsForTheFirstDisconnect(t *testing.T) {
	cfg := fmongo.DefaultConfig("mongodb://127.0.0.1:1")
	cfg.ConnectTimeout = 100 * time.Millisecond
	c, err := NewClient(cfg, IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	c.disconnect = func(ctx context.Context) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if !first {
			// 与 mongo-driver 相同：拓扑已经在关闭，第二次断开立刻返回 ErrClientDisconnected。
			return mongo.ErrClientDisconnected
		}
		close(entered)
		<-release
		return c.cli.Disconnect(ctx)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- c.Close(context.Background()) }()
	<-entered

	secondDone := make(chan error, 1)
	go func() { secondDone <- c.Close(context.Background()) }()
	select {
	case err := <-secondDone:
		close(release)
		<-firstDone
		t.Fatalf("concurrent Close returned %v while the first Close was still disconnecting; want it to wait", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Close = %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("concurrent Close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close still blocked after the first finished")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close after close = %v, want nil", err)
	}
}

// 第一次断开失败时拓扑没断开，再调 Close 要重做断开，而不是按“已关闭”吞掉；关完之后的命令返回 ErrClientDisconnected。
func TestClientCloseRetriesAfterAFailedDisconnect(t *testing.T) {
	cfg := fmongo.DefaultConfig("mongodb://127.0.0.1:1")
	cfg.ConnectTimeout = 100 * time.Millisecond
	c, err := NewClient(cfg, IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("disconnect failed")
	calls := 0
	c.disconnect = func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return boom
		}
		return c.cli.Disconnect(ctx)
	}
	if err := c.Close(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("first Close = %v, want the disconnect error", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("retry Close = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("disconnect calls = %d, want 2: the retry must disconnect again", calls)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close after a successful retry = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx); !errors.Is(err, mongo.ErrClientDisconnected) {
		t.Fatalf("Ping after Close = %v, want mongo.ErrClientDisconnected", err)
	}
}

// O-M6-5（维护者第十二轮决定）：启动期建索引遇到副本集选举，做有界重试。次数与间隔都有上限，
// 用完失败并点名是选举没有结束；不是选举的错误不重试。
func TestIndexCreationRetriesThroughAnElectionWithinBounds(t *testing.T) {
	stepDown := mongo.CommandError{Code: 11602, Name: "InterruptedDueToReplStateChange", Message: "operation was interrupted"}
	notPrimary := mongo.CommandError{Code: 10107, Name: "NotWritablePrimary", Message: "not primary"}
	policy := electionRetry{attempts: 4, interval: time.Millisecond}

	old := metrics.DefaultRegistry()
	registry := metrics.NewRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })

	t.Run("an election that settles", func(t *testing.T) {
		calls := 0
		err := retryDuringElection(context.Background(), policy, func() error {
			calls++
			switch calls {
			case 1:
				return stepDown
			case 2:
				return notPrimary
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("err=%v after %d calls, want success on the 3rd", err, calls)
		}
		if got := registry.Snapshot(); len(got) != 1 || got[0].Name != electionRetryMetric || got[0].Value != 2 {
			t.Fatalf("election retries counted %+v, want %s = 2", got, electionRetryMetric)
		}
	})

	t.Run("an election that does not settle", func(t *testing.T) {
		calls := 0
		err := retryDuringElection(context.Background(), policy, func() error { calls++; return stepDown })
		if calls != policy.attempts {
			t.Fatalf("made %d attempts, want exactly the bound %d", calls, policy.attempts)
		}
		if err == nil || !strings.Contains(err.Error(), "election did not settle after 4 attempts") || !errors.As(err, new(mongo.CommandError)) {
			t.Fatalf("err = %v, want the bound named and the server error kept in the chain", err)
		}
	})

	t.Run("the deadline comes first", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := retryDuringElection(ctx, electionRetry{attempts: 100, interval: time.Hour}, func() error {
			calls++
			cancel()
			return stepDown
		})
		if calls != 1 || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "election did not settle before the deadline") {
			t.Fatalf("calls=%d err=%v, want one attempt and the deadline named", calls, err)
		}
	})

	t.Run("other errors are not retried", func(t *testing.T) {
		duplicate := mongo.CommandError{Code: 85, Name: "IndexOptionsConflict"}
		calls := 0
		err := retryDuringElection(context.Background(), policy, func() error { calls++; return duplicate })
		if calls != 1 || !errors.As(err, new(mongo.CommandError)) || strings.Contains(err.Error(), "election") {
			t.Fatalf("calls=%d err=%v, want the error returned as is after one attempt", calls, err)
		}
	})
}

// 回调一直返回 TransientTransactionError、窗口在两次重跑之间的退避里关闭时，WithTransaction 返回的错误保留最后一次回调错误的链与标签
// （与驱动便捷 API 的 timeoutError{Wrapped: err} 相同），调用方据此分流撞键与版本冲突。不需要服务端：StartTransaction 不发命令，
// 回调不做 I/O。
// 历史：RR-20261005-NC-101。
func TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff(t *testing.T) {
	cli, err := drivermongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?directConnection=true").SetServerSelectionTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Disconnect(context.Background()) }()
	sess, err := cli.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	s := &session{sess: sess, timeout: 300 * time.Millisecond, options: options.Transaction()}
	defer s.EndSession(context.Background())

	duplicate := drivermongo.CommandError{
		Code: 11000, Message: "E11000 duplicate key error", Labels: []string{labelTransient},
		Wrapped: fmongo.ErrDuplicateKey,
	}
	calls := 0
	err = s.WithTransaction(context.Background(), func(context.Context) error {
		calls++
		return fmt.Errorf("reserve: %w", duplicate)
	})
	if calls < 2 {
		t.Fatalf("callback ran %d times; a TransientTransactionError must re-run it", calls)
	}
	// 窗口偶尔在回调与 ctx 检查之间关闭，那条分支直接返回回调错误；两条分支都必须保留它。
	t.Logf("callbacks=%d closed-in-backoff=%v", calls, errors.Is(err, context.DeadlineExceeded))
	if !errors.Is(err, fmongo.ErrDuplicateKey) || !hasErrorLabel(err, labelTransient) {
		t.Fatalf("err=%v after %d callbacks lost the last callback error (errors.Is ErrDuplicateKey=%v, transient label=%v)",
			err, calls, errors.Is(err, fmongo.ErrDuplicateKey), hasErrorLabel(err, labelTransient))
	}
}

// A2：确定未提交的失败不带 fmongo.ErrCommitResultUnknown——回调失败、截止在提交前到期时不发提交，
// 调用方可以把它们当作“没提交”。哨兵只属于提交发出之后的失败（真实副本集用例见
// TestRealMongoCommitIsBoundedByTransactionTimeout）。不需要服务端：回调不做 I/O，Starting 状态的
// abort 不发命令。
func TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown(t *testing.T) {
	cli, err := drivermongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?directConnection=true").SetServerSelectionTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Disconnect(context.Background()) }()
	for _, tc := range []struct {
		name string
		fn   func(ctx context.Context) error
		want error
	}{
		{"callback error", func(context.Context) error { return fmongo.ErrVersionConflict }, fmongo.ErrVersionConflict},
		{"deadline before commit", func(ctx context.Context) error { <-ctx.Done(); return nil }, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := cli.StartSession()
			if err != nil {
				t.Fatal(err)
			}
			s := &session{sess: sess, timeout: 100 * time.Millisecond, options: options.Transaction()}
			defer s.EndSession(context.Background())
			err = s.WithTransaction(context.Background(), tc.fn)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
			if errors.Is(err, fmongo.ErrCommitResultUnknown) {
				t.Fatalf("a failure before the commit was sent reads as result unknown: %v", err)
			}
		})
	}
}
