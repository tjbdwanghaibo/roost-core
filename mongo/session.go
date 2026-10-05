package mongo

import "context"

// ISession provides multi-document transaction support.
type ISession interface {
	// WithTransaction executes fn within a transaction with automatic retry.
	// If fn returns nil, the transaction commits; otherwise it aborts.
	//
	// 提交命令发出之后的失败（网络中断、截止落在提交中途、MaxTimeMS 到期）包着
	// ErrCommitResultUnknown 返回：服务端可能已经提交。其余错误都是确定未提交。
	WithTransaction(ctx context.Context, fn func(ctx context.Context) error) error

	// EndSession releases the session resources.
	EndSession(ctx context.Context)
}
