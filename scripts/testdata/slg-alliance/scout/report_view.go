package scout

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// ReportView 只公开玩家查看报告所需的字段；归属、owner 代际和内部回执不进入客户端协议。
type ReportView struct {
	ID          string        `json:"id"`
	Target      int64         `json:"target"`
	At          int64         `json:"at"`
	Destination spatial.Point `json:"destination"`
}

// ProjectInbox 的 recipient/home 必须来自服务端已认证会话绑定，不能取自客户端参数。
// 输入来自生成 Sender 的已提交 Inbox 快照。整批校验后发布，不跳过损坏或串号条目。
// 空 Inbox 返回 []；读取/校验失败应保持 Sync 会话 held，重试不写永久回执。
func ProjectInbox(raw string, recipient int64, home int32) ([]ReportView, error) {
	if recipient <= 0 || (home != 1000 && home != 1001) || len(raw) > 128*1024 {
		return nil, fmt.Errorf("invalid inbox identity or size")
	}
	var inbox map[string]Report
	if err := json.Unmarshal([]byte(raw), &inbox); err != nil {
		return nil, err
	}
	if inbox == nil || len(inbox) > 128 {
		return nil, fmt.Errorf("invalid inbox collection")
	}
	views := make([]ReportView, 0, len(inbox))
	for key, r := range inbox {
		if !r.Valid() || key != r.Identity() || r.Recipient != recipient || r.HomeSID != home {
			return nil, fmt.Errorf("invalid inbox report")
		}
		views = append(views, ReportView{ID: key, Target: r.Target, At: r.At, Destination: r.Destination})
	}
	slices.SortFunc(views, func(a, b ReportView) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return views, nil
}
