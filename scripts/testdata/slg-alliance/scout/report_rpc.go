package scout

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"time"
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

// OwnerRoute 是投递准入代际，不属于永久报告身份。换 owner 不得换 MapID/Key 或重写正文。
type OwnerRoute struct {
	SID           int32
	Marker, Route uint64
}
type RoutedReport struct {
	Report Report
	Owner  OwnerRoute
}

// RoutedReportReceiver 每次请求重读权威路由，查询失败即拒绝；不信任调用方自报 owner。
// 路由校验与 Player 提交不是一个分布式事务：校验通过的在途请求可跨越交接完成，
// 安全性来自已固定的报告正文与永久回执；需要撤销已准入报告的业务必须另设协议。
type RoutedReportReceiver struct {
	// 普通 Bus RPC 不传播调用方 deadline；接收端必须自己限定整个处理预算。
	// 零值采用 5 秒，负值拒绝。超时不能强杀忽略 context 的回调，停机仍需真实排空。
	Timeout          time.Duration
	HomeSID          int32
	MapID, Recipient int64
	Resolve          func(context.Context, int64) (OwnerRoute, error)
	Receive          func(context.Context, Report) error
}

func (r RoutedReportReceiver) Handle(c *bus.RpcContext) (any, error) {
	if c == nil || r.Resolve == nil || r.Receive == nil || r.HomeSID <= 0 || r.MapID <= 0 || r.Recipient <= 0 || r.Timeout < 0 {
		return nil, errors.New("invalid routed report receiver")
	}
	budget := r.Timeout
	if budget == 0 {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Context(), budget)
	defer cancel()
	var req RoutedReport
	if err := c.Decode(&req); err != nil {
		return nil, err
	}
	report := req.Report
	if !report.Valid() || report.MapID != r.MapID || report.HomeSID != r.HomeSID || report.Recipient != r.Recipient {
		return nil, errors.New("report destination mismatch")
	}
	current, err := r.Resolve(ctx, report.MapID)
	if err != nil {
		return nil, err
	}
	if current.SID <= 0 || current.Marker == 0 || current.Route == 0 || current != req.Owner || current.SID != c.FromSid {
		return nil, errors.New("stale report owner route")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = r.Receive(ctx, report); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return true, nil
}
