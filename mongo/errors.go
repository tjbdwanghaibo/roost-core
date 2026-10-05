package mongo

import "errors"

var (
	ErrNotFound        = errors.New("mongo: document not found")
	ErrDuplicateKey    = errors.New("mongo: duplicate key")
	ErrVersionConflict = errors.New("mongo: version conflict")
	ErrClosed          = errors.New("mongo: client closed")

	// ErrCommitResultUnknown 标记“提交已经发出、但没拿到确定结论”的事务错误：服务端可能已经提交，
	// 也可能没有。ISession.WithTransaction 只在提交命令发出之后失败时包上它（原错误链保留，
	// 截止导致的仍满足 errors.Is(err, context.DeadlineExceeded)）；回调失败、提交前截止到期、
	// 提交被判为暂时性错误后窗口到期，这些确定未提交的错误不带它。调用方见到它必须按持久身份 /
	// 回执裁决，不能当作未提交重做（A2，RR-20261005-NC-101 复审）。
	ErrCommitResultUnknown = errors.New("mongo: transaction commit result unknown")
)
