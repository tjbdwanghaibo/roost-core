// Package awaitflow 是不依赖外部服务的正式生成链路示例。
package awaitflow

import (
	"context"
	"errors"
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
