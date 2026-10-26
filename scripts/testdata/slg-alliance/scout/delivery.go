package scout

import (
	"context"
	"errors"
)

// ReportDelivery 是两笔独立事务间的业务投递协议，不宣称跨实体原子提交。
// Receive 必须在接收方持久保存正文与永久身份后返回；未知结果返回错误，下轮重投原身份。
// Read/Receive/Ack 只装配可信内部 Sender，不能接客户端自报信封。
type ReportDelivery struct {
	MapID   int64
	Read    func(context.Context, string) (Report, error)
	Receive func(context.Context, Report) error
	Ack     func(context.Context, Report) error
}

func (d ReportDelivery) Deliver(ctx context.Context, key string) error {
	if d.MapID <= 0 || d.Read == nil || d.Receive == nil || d.Ack == nil {
		return errors.New("invalid report delivery configuration")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	report, err := d.Read(ctx, key)
	if err != nil {
		return err
	}
	if !report.Valid() || report.MapID != d.MapID || report.Key != key {
		return errors.New("report identity mismatch")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = d.Receive(ctx, report); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return d.Ack(ctx, report)
}
