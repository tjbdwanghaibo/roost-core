package scout

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

// ReportReceiver 固定本原服、收件人和可信地图路由；FromSid 必须来自受保护的服务总线。
// NATS 的报文头不是密码学身份：生产必须以凭据/subject ACL 保护服务总线。
// 固定权威不提供 owner 交接；不能把此校验当成 Remote lease fence。
type ReportReceiver struct {
	HomeSID, MapSID  int32
	MapID, Recipient int64
	Receive          func(context.Context, Report) error
}

func (r ReportReceiver) Handle(c *bus.RpcContext) (any, error) {
	if c == nil || r.HomeSID <= 0 || r.MapSID <= 0 || r.MapID <= 0 || r.Recipient <= 0 || r.Receive == nil || c.FromSid != r.MapSID {
		return nil, errors.New("untrusted report route")
	}
	var report Report
	if err := c.Decode(&report); err != nil {
		return nil, err
	}
	if !report.Valid() || report.MapID != r.MapID || report.HomeSID != r.HomeSID || report.Recipient != r.Recipient {
		return nil, errors.New("report destination mismatch")
	}
	if err := c.Context().Err(); err != nil {
		return nil, err
	}
	if err := r.Receive(c.Context(), report); err != nil {
		return nil, err
	}
	return true, nil
}
