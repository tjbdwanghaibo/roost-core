package driver

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	// defaultTransactionTimeout 是 Config.TransactionTimeout 未设置（<=0）时的窗口，
	// 与驱动便捷 API 的 withTransactionTimeout 相同。
	defaultTransactionTimeout = 120 * time.Second
	// transactionAbortTimeout 限住回调失败后、以及 EndSession 里的尽力 abort。abort 不影响正确性（服务端到
	// transactionLifetimeLimitSeconds 也会自己中止），不能让它在网络黑洞时无限阻塞。
	transactionAbortTimeout = 5 * time.Second
	// 回调整体重跑（TransientTransactionError）之间的退避，取值同驱动。
	transactionBackoffInitial = 5 * time.Millisecond
	transactionBackoffMax     = 500 * time.Millisecond

	labelTransient     = "TransientTransactionError"
	labelUnknownCommit = "UnknownTransactionCommitResult"
)

// session implements fmongo.ISession.
type session struct {
	sess    *mongo.Session
	timeout time.Duration
	options *options.TransactionOptionsBuilder
}

// WithTransaction 按驱动便捷 API 的重试规则执行事务，但整个过程——包括提交——都受
// TransactionTimeout 约束（RR-20261005-NC-101）。
//
// 驱动的 mongo.Session.WithTransaction 用 background ctx 调 CommitTransaction，提交重试只看
// 它自己的 120s 计时器、且只在两次尝试之间检查：网络在回调之后黑洞时，调用方的截止时间
// 不起作用，事务一直阻塞到网络恢复（实测 transaction_timeout=3s 时阻塞 40s / 150s）。
// 这里保留同样的规则——TransientTransactionError 重跑整个回调；提交返回
// UnknownTransactionCommitResult（非 MaxTimeMSExpired）时只重试提交；提交返回
// TransientTransactionError 时重跑回调——只是把同一个带截止的 ctx 交给提交。
//
// 提交发出之后失败时返回 fmongo.ErrCommitResultUnknown 包着的驱动错误（错误链与标签保留，
// 截止导致的仍满足 errors.Is(err, context.DeadlineExceeded)）：服务端可能已经提交，
// 调用方必须按结果未知处理，不能当成未提交。不带这个哨兵的错误都是确定未提交（回调失败、
// 提交前截止到期、提交被判为暂时性错误后窗口到期）。这里提交失败后不 abort（驱动同样不这么做：
// 失败的提交可能已解除 session 的服务器绑定，abort 可能与提交并发执行）。但提交因截止失败时
// 驱动不更新事务状态（CommitTransaction 的 IsTimeout 分支），之后的 EndSession 仍会对它补发
// abortTransaction；副本集上同一 txnNumber 的提交与 abort 由服务端串行裁决，结果仍是二者之一，
// 所以未知结果的含义不变。EndSession 的上限见下。
func (s *session) WithTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultTransactionTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	backoff := time.Duration(0)
	// lastErr 是触发这次重跑的 TransientTransactionError（回调或提交返回的），一定没有提交。
	var lastErr error
	for {
		if backoff > 0 {
			wait := time.NewTimer(time.Duration(rand.Int64N(int64(backoff))) + time.Nanosecond)
			select {
			case <-ctx.Done():
				wait.Stop()
				// 与驱动的 timeoutError{Wrapped: err} 相同，保留最后一次错误的链与标签：调用方按它
				// 分流（撞键换会话重试、版本冲突回读回执），只给 ctx.Err() 会让这些分支在窗口到期时落空。
				return errors.Join(ctx.Err(), lastErr)
			case <-wait.C:
			}
			backoff = min(backoff+backoff/2, transactionBackoffMax)
		} else {
			backoff = transactionBackoffInitial
		}

		if err := s.sess.StartTransaction(s.options); err != nil {
			return err
		}
		if err := fn(mongo.NewSessionContext(ctx, s.sess)); err != nil {
			s.abort(ctx)
			if hasErrorLabel(err, labelTransient) && ctx.Err() == nil {
				lastErr = err
				continue
			}
			return err
		}
		// 截止已到就不发提交：此时 abort 得到的是确定的“未提交”。
		if err := ctx.Err(); err != nil {
			s.abort(ctx)
			return err
		}
		retryCallback, err := s.commit(ctx)
		if retryCallback {
			lastErr = err
			continue
		}
		if err != nil {
			// 提交命令已经发出：服务端可能已经提交。包上专用哨兵，调用方不必再靠标签或
			// DeadlineExceeded 猜（标签可能被驱动的内部重试覆盖，DeadlineExceeded 也匹配提交前到期，
			// RR-20261005-NC-101 复审观察）；原错误链保留。
			return fmt.Errorf("%w: %w", fmongo.ErrCommitResultUnknown, err)
		}
		return nil
	}
}

// commit 提交当前事务。retryCallback 表示服务端要求整个事务重跑（提交被判为暂时性失败，
// 一定没有提交），此时 err 是那次提交错误，只供窗口到期时保留。
func (s *session) commit(ctx context.Context) (retryCallback bool, err error) {
	for {
		err = s.sess.CommitTransaction(ctx)
		if err == nil {
			return false, nil
		}
		if ctx.Err() != nil {
			return false, err
		}
		var command mongo.CommandError
		if errors.As(err, &command) {
			if command.HasErrorLabel(labelUnknownCommit) && !command.IsMaxTimeMSExpiredError() {
				continue
			}
			if command.HasErrorLabel(labelTransient) {
				return true, err
			}
		}
		return false, err
	}
}

// abort 尽力中止当前事务：不受已到期的调用方 ctx 取消，但有自己的上限。
func (s *session) abort(ctx context.Context) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transactionAbortTimeout)
	defer cancel()
	_ = s.sess.AbortTransaction(abortCtx)
}

func hasErrorLabel(err error, label string) bool {
	var labeled mongo.LabeledError
	return errors.As(err, &labeled) && labeled.HasErrorLabel(label)
}

// EndSession 结束会话。事务仍在进行时驱动会在这里先 abort——典型是提交因截止失败之后
// （见 WithTransaction）。调用方传入的通常是自己的长期 ctx（投影器、committer、saga、
// effect inbox 都是 defer EndSession(ctx)，没有截止），网络黑洞时这次 abort 会一直阻塞到网络
// 恢复，让 transaction_timeout 对调用方失效（RR-20261005-NC-101 复审）。所以与回调失败后的
// abort 相同：不受调用方取消影响，但有 transactionAbortTimeout 上限。没有进行中的事务时不发命令。
// 上限内没送达的 abort 留下的服务端事务由服务端在 transactionLifetimeLimitSeconds（缺省 60s）后
// 中止；在此之前写同一文档的事务得到 WriteConflict，按 TransientTransactionError 重跑。
func (s *session) EndSession(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transactionAbortTimeout)
	defer cancel()
	s.sess.EndSession(endCtx)
}
