package mongo

import "context"

// IMongo is the top-level MongoDB client abstraction.
type IMongo interface {
	// Database returns a database by name.
	Database(name string) IDatabase

	// DatabaseForSid returns a database named "{prefix}_{sid}".
	DatabaseForSid(prefix string, sid int32) IDatabase

	// StartSession starts a session for multi-document transactions.
	StartSession(ctx context.Context) (ISession, error)

	// Ping verifies connectivity.
	Ping(ctx context.Context) error

	// GetParameter 读一个服务端参数（admin 库的只读命令 getParameter），例如
	// transactionLifetimeLimitSeconds。返回服务端给出的值（数字类型随服务端，int32 / int64 / float64）；参数不存在、
	// 账号没有权限或部署不支持时返回错误。只读，不改任何状态：启动探针用它核对部署要求（saga 的 Mongo 步骤）。
	GetParameter(ctx context.Context, name string) (any, error)

	// Close disconnects the client.
	Close(ctx context.Context) error
}
