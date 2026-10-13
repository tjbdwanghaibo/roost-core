package dataengine

import (
	"context"
	"errors"
)

var ErrSchemaMismatch = errors.New("dataengine: persisted schema does not match runtime schema")

// Descriptor is implemented by generated DAOs to declare their current
// persisted schema.
type Descriptor interface {
	SchemaVersion() uint32
}

// ProjectionTicket 表示一笔已准入事务的投影结果：Done 关闭后 Err 给出最终结论。
type ProjectionTicket interface {
	Done() <-chan struct{}
	Err() error
}

// SystemCommitter 供框架内部（非 handler）提交系统事务并取得投影票据。
type SystemCommitter interface {
	CommitSystem(context.Context, CommitRecord) (ProjectionTicket, error)
}

// WaitProjection 等待投影完成或 ctx 结束；ctx 结束只表示调用方不再等待，
// 不代表事务未提交，投影仍在后台继续。
func WaitProjection(ctx context.Context, ticket ProjectionTicket) error {
	if ticket == nil {
		return errors.New("dataengine: projection ticket is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ticket.Done():
		return ticket.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
