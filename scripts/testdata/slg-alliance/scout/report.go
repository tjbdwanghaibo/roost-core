package scout

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Report 是服务器内部的不可变投递信封；MapID + Key 是永久身份，不能由客户端构造。
// 一个教学地图固定一个收件人；生产多军队地图应在各行军事务固定收件人。
type Report struct {
	MapID, Recipient int64
	HomeSID          int32
	Key              string
	Target, At       int64
	Destination      spatial.Point
}

func (r Report) Identity() string { return fmt.Sprintf("%d/%s", r.MapID, r.Key) }
func (r Report) Valid() bool {
	return r.MapID > 0 && r.Recipient > 0 && (r.HomeSID == 1000 || r.HomeSID == 1001) && r.Key != "" && len(r.Key) <= 64 && r.Target >= 501 && r.Target <= 504 && r.At > 0 && r.Destination.X >= 0 && r.Destination.X < 1000 && r.Destination.Y >= 0 && r.Destination.Y < 1000
}
