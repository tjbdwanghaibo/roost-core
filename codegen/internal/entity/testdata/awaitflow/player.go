// Package awaitflow 是不依赖外部服务的正式生成链路示例。
package awaitflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// 此包是独立消费工程，kind 231 只在本包注册。
const PlayerKind entity.EntityKind = 231

func init() { entity.MustRegisterEntityKindCategory(PlayerKind, 1) }

//roost:entity entityKind=PlayerKind businessPool=long noPersist=true
type Player struct {
	*entity.EntityBase
	// 临时内存示例；真实持久字段须放入 DAO 并使用持久事务。
	reward int
}

// ClaimReward 复制查询输入，work 不捕获 Entity；resume 参数重新受 Guard 保护。
//
//roost:nest target=PlayerKind durability=memory
func ClaimReward(player *Player, rank int) error {
	playerID := player.GUId()
	return nest.Await(func(ctx context.Context) (int, error) {
		if !fctx.InIOWorker() || entity.CurrentGuardScope() != nil {
			return 0, errors.New("query outside I/O")
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		// 真实业务在这里用 playerID 调排行榜 RPC；示例返回确定的数值。
		if playerID == 0 {
			return 0, errors.New("invalid player")
		}
		return 100 - rank, nil
	}, func(player *Player, reward int, err error) error {
		if err != nil {
			return err
		}
		if !fctx.InLongWorker() || !entity.GetEntityGuard().GuardedEntity(player) {
			return errors.New("resume without long-pool Guard")
		}
		if player.reward != 0 {
			return errors.New("already claimed")
		}
		player.reward = reward
		return nil
	})
}

// StampVisit 记下这次访问的业务时间。nowUnixMilli 是业务时间参数：生成的分发包装在执行时从 fctx 填入这条
// 消息冻结的业务时间，Sender 的签名里没有它；handler 仍可以在单测里直接传值。
//
//roost:nest target=PlayerKind durability=memory
func StampVisit(player *Player, note string, nowUnixMilli int64) (int64, error) {
	if note == "" {
		return 0, errors.New("empty note")
	}
	if frozen := fctx.NowMilli(); nowUnixMilli != frozen {
		return nowUnixMilli, fmt.Errorf("handler got business time %d, but its message froze %d", nowUnixMilli, frozen)
	}
	return nowUnixMilli, nil
}
