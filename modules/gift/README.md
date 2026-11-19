# gift：玩家间送礼

## 能力

`SendGift(ToPlayerID, ItemID, Count)` 在发送者的 Player 锁内检查背包，提交 `gift_item` 启动意图，返回 Saga 业务键。成功只表示已发起，不表示已送达。键取发送者、完整认证会话 ID 和帧序号，同一帧重投不会再送一次。`GiftStatus(SagaID)` 只向发送者返回状态、当前步骤和最后错误；记录暂未建立、已经过期或属于其他人一律答 `unknown`。

扣物和退款是原生 Nest 步骤：背包变更、操作回执 `PlayerDao.SagaReceipts` 和完成结果在同一事务。投递是 Mongo 步骤：按收件人当前承载服查 Player DAO，再发带道具附件的邮件；邮件服务用 `gift:` 加步骤幂等键消除重复投递。不在本服创建或加载收件人。收件人未建档等业务拒绝触发退款；基础设施错误不是业务拒绝。

## 能力边界

使用带 `server_id` 的认证会话、account 角色绑定，以及每服自己的 Player 库。不要给客户端任意指定发送方或路由的入口。发起前核对当前归属；执行扣物和退款时再次核对：已迁服按新 sid 改投，迁移中等待，本地副本正在卸载则重试。送达后不回收邮件。退款耗尽重试会进入 `manual_required`，不能据此自动再发一份物品。

## 复用方式

```sh
roost module plan gift
roost module add gift
roost module verify gift
```

在业务服务配置里按自己的重启窗口设置 `saga.steps.gift_item.debit.max_attempts`（正向与补偿共用）。game-demo 的开发、生产示例和部署配置沿用 15 次，在单次超时 5s 和既有退避下覆盖约 95s 的重启窗口。普通工程沿用自己的 Saga 配置，安装器不会覆盖已有预算。协议号由安装器分配；game-demo 为兼容已有客户端固定为 `SendGift=10013`、`GiftStatus=10014`。

## 联动

依赖 `bag`、`mailbox` 和 `rehome`；安装器声明 `gift_item`、步骤路由与共享的 Player 回执，不需要手动 `roost add saga`。普通工程通过 `game/modules/gift/runtime.Start` 自动安装消费者。自带驻留表的 demo 只在宿主写闸门就绪后调用 `StartSteps` 一次，提供 `PlayerOwners.AdmitBound`，停机时排空同一组消费者；没有第二份步骤实现。

## 验证范围

模块 7 条：完整会话键与重放、当前归属检查、收件人存在与不存在、跨服按 DAO 映射查库。game-demo 另有两个 sid 的归属/换服演练、固定消息号、发送方 sid 与退款预算回归。这些测试不代表真实集群宕机恢复或吞吐测量。
