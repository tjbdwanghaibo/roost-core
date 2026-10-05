package driver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RR-20261005-NC-101 复审：回调一直返回 TransientTransactionError、窗口在两次重跑之间的退避里
// 关闭时，WithTransaction 返回的错误要保留最后一次回调错误的链与标签——与驱动便捷 API 的
// timeoutError{Wrapped: err} 相同。调用方靠它分流：saga step inbox 对 errors.Is(err,
// fmongo.ErrDuplicateKey) 换新会话再试一次，Remote committer 对撞键 / 版本冲突回读持久回执。
// 修前退避分支只返回 ctx.Err()，这些分支在窗口到期时一律落空。
//
// 不需要服务端：StartTransaction 不发命令，回调不做 I/O，Starting 状态的 abort 也不发命令；
// 回调与 abort 只占微秒，窗口几乎总在退避里关闭（修前那条分支只返回 ctx.Err()）。
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
