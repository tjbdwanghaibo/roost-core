// Package scout 保存教学地图的直线行军规则；不依赖 tick 次数，不处理碰撞或战斗。
package scout

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

const DurationMillis int64 = 10_000

type Route struct {
	From, To   spatial.Point
	Start, End int64
}

func (r Route) Validate() error {
	// 坐标与时间跨度有界，插值乘法不会溢出；先比较再相减。
	if r.Start <= 0 || r.End <= r.Start || r.End-r.Start > 3_600_000 {
		return errors.New("invalid route time")
	}
	for _, p := range []spatial.Point{r.From, r.To} {
		if p.X < 0 || p.Y < 0 || p.X >= 1000 || p.Y >= 1000 {
			return errors.New("invalid route point")
		}
	}
	return nil
}

// At 返回整数坐标；端点精确钳制，离线只做一次有界计算，不循环补 tick。
// 生产业务时钟必须遵守跨进程不回退契约；低于出发时刻只得到起点。
func (r Route) At(now int64) (spatial.Point, error) {
	if err := r.Validate(); err != nil {
		return spatial.Point{}, err
	}
	if now <= r.Start {
		return r.From, nil
	}
	if now >= r.End {
		return r.To, nil
	}
	elapsed, duration := now-r.Start, r.End-r.Start
	return spatial.Point{X: r.From.X + (r.To.X-r.From.X)*elapsed/duration, Y: r.From.Y + (r.To.Y-r.From.Y)*elapsed/duration}, nil
}
