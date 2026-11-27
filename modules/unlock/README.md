# unlock：Player 功能解锁记录与当前开关

`roost module add unlock` 安装独立可选组件。它只回答两个问题：**该玩家是否已有首次认证记录**、**在本次请求的配置快照下这条记录能否放行**。不自动接管任何玩法；不安装就没有这些字段、事件或检查。没有客户端自解锁协议、通用远程授权 handler 或跨服权限中心。

## 配置和服务端调用

`configs/table/unlock.csv` 每行一个长期功能 ID（1..4096），发布后不复用，也不要把旧号改为另一种门槛。样例 `1,true,fact,tutorial.completed,0` 只声明接受哪种事实；不会自动赋予任何玩家。`enabled` 是**本服配置表**中的临时准入开关，未设为真就不能进入。停开、删行和恢复配置都不改玩家记录；暂停期间仍可显式记录已发生的可信事实。改 CSV 后照常生成和部署表数据，热载按 Nest 消息固定快照生效，不做集群同步切换。

仅在锁定该 Player 的**可回滚** Nest 事务中调用（例如 `//roost:nest rollback=undo`）：

```go
// 教程结算方已经从服务端权威记录验证了“该玩家完成教程”和原始发生时间。
// 不可把客户端的 fact、时间、等级或任意 featureID 直接透传给授予入口。
first, err := p.Unlock().GrantFact(1, "tutorial.completed", completedAtUnixMilli)
if err != nil { return err } // 必须传回 Nest；first 是本事务首次写入，尚不是提交成功
_ = first
```

真正受限的玩法在自己的 handler **同一事务内、写业务状态之前**调用 `if err := p.Unlock().Require(id); err != nil { return err }`。这是只读检查：未知/下架/不可识别/缺配置条件 → `ErrUnlockUnknown`，暂停 → `ErrUnlockDisabled`，没有有效认证 → `ErrUnlockLocked`。不会查询顺便建档，不依赖客户端缓存。`Record(id)` 返回历史值副本和是否存在，**不等于当前准入**，即使现在停开或下架也能读到原记录。错误来源名、无效/未来的原始毫秒时间、等级不足等认证失败返回 `ErrUnlockProof`，不写记录。

首次 `GrantFact` 保存 `OccurredAt`（可信来源证明的原始 Unix 毫秒）和 `RecordedAt`（首次认证的事务业务时刻）。调用方负责提供原始完成事实，不按重放顺序猜最早时间。重复有效授予返回 `false, nil`，不覆盖时间、不再次发事件。玩家私有账本 `PlayerDao.Unlock` 持久化、不随 DAO 同步下发；每玩家至多 4096 个键。

## 可选 profile 例子

`roost module add unlock profile`；配置中可加 `2,true,profile.level,,5`。业务在 Player 锁内显式调用 `p.Unlock().CertifyCurrent(2)`。组件从**该玩家的 profile 组件**读取权威等级，不收外部传入的等级，未装桥或缺少门槛不放行。今天才认证到玩家已达 5 级，并不能证明他在哪天第一次升到 5 级：此时 `RecordedAt` 是本次认证时刻，`OccurredAt=0` 表示未知。不会自动监听升级、倒推历史或批量扫描旧玩家。

## 状态变化与边界

首次写记录在同一 Player 上发布 `event.FeatureUnlocked{FeatureID, RecordedAt, OccurredAt}`。监听可用 `//roost:listen`；在事务中同步运行，**不是提交后通知**。监听返回错、外层业务返回错、提交被拒，记录和监听写的该实体状态一并回滚，之后可以重试。监听里不要直接推送、跨服调用、发外部奖励；需要提交后外部投递应另外设计持久 effect 和去重。

本模块固定的是“曾认证”这个历史事实，不是替业务决定永久可玩或动态资格。需要按当前条件实时进出、撤销/过期、迁移门槛、跨服可见性或全服分布式开关时，应单独明确产品规则和一致性要求；不要覆盖旧记录来模拟这些语义。

验收：`roost module verify`；`go test -race ./game/handler -run '^(TestUnlock|TestWithUnlockProfile)'`。用真实生成 Player / DAO、配置重载、Nest undo 事务与同实体事件，提交器为内存替身；包含 BSON 往返重建实体，不代表真 Mongo、跨进程或物理落盘验收。
