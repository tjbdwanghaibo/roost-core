# 实现学习：实体加载单飞与 Activity 派发交接

基线：Core `a2e8fa0`、Kit `5116f2a`、Codegen `fde74d1`。本文解释当前实现和实施边界；建议部分尚未实现。

## 2026-09-20 验收回顾

当前 Core `8c589a6` 已让两层 loader 的 leader 在 panic 时完成 flight、唤醒 waiter 并清理 map，RR-20260919-08 原触发通过；Nest single/broadcast 也显式处理 nil entity，RR-09 原触发通过。首 caller context 驱动共享 load、Shutdown generation 与真实 store 在途恢复仍是后续审查范围，不因原 bugfix 通过而关闭。

当前 Core/Kit/Codegen 已实现 `(group, gameSID)` owed index、RPC `AttemptDispatch` 与 game runner drain；attempt 在领取 payload 时消耗，sweeper 不再假装交付，RR-10 原触发通过。老部署升级后仍需回填 owed index；离线多窗口、ACK 回复丢失和双 runner 的真实 Redis/NATS 故障矩阵尚未重跑。

## 实体冷加载的两层 singleflight

`entity.ManagerAccess.Get` 先查共享 `EntityManager`，未命中时读取当前 loader，再按 fullID 进入 `loadEntityShared`。它让同一实体的并发请求共享一次 loader 调用。Dataengine 的 `EntityRepository.LoadEntity` 自己又做一层同样的 singleflight，然后执行 builder 查找、持久 DAO 读取、schema migration、RestorePersisted、实体创建和 manager 发布。

```text
Nest / guard / remoteentity
  → ManagerAccess.Get
    → manager.Get
    → ManagerAccess.loadEntityShared
      → EntityRepository.LoadEntity
        → repository flight
        → read/migrate/restore/build
        → EntityManager.Add
```

两层的意义不同：外层保护任意注入 loader，内层保护 repository 的昂贵恢复链。正常路径共享 value/error；waiter 自己取消只停止等待，不删除 leader 的 flight。

### 完成所有权

singleflight 的核心资源不是 map，而是 `done` 的完成责任。leader 一旦把 flight 放进 map，就必须在任何退出路径上完成它。当前两层只处理普通 return；panic 会绕过 map 删除和 channel close，形成 RR-20260919-08。

可复用的完成模式应满足：

1. leader 独占一次 `finish`；
2. 先把 value/error 写成稳定结果，再从 map 删除同一个 flight；
3. 最后 close(done)，建立 waiter 读取结果的 happens-before；
4. panic、普通 error、context error 都走 finish；
5. 旧 leader 不能删除后来同 fullID 新建的 flight，因此删除时校验 map 中仍是当前指针。

panic 是错误协议的一部分：框架可以选择恢复并返回 `ErrEntityLoadPanic`，也可以在完成 flight 后让 leader 继续 panic；不能让 waiter 永远没有结果。日志应带 fullID、kind、loader generation 和堆栈。

### context 与关闭 generation

当前首个 caller 的 ctx 驱动共享 load。这样实现简单，但 leader 的短 deadline 会让更长寿命的 waiter一起失败。更稳定的模型是为 flight 创建由运行期管理的 context，caller context 只控制等待；运行期 Shutdown 取消/等待本 generation 的所有 flight。

Runtime 当前先 `ready=false` 并注销 loader，再 Flush/Close projector/outbox。已经越过 Ready 检查的 repository load 不在这个关闭屏障内。实施 generation 后，旧 generation 完成时必须满足以下之一：Shutdown 已等待它结束；或完成者发现 generation 已过期，不把实体发布到共享 manager。这个观察尚缺真实 store 交错测试，不能当作已确认修复。

## Getter 的缺失契约

生产 `ManagerAccess.Get` 允许 `(nil,nil)` 表示未找到；Nest multi 路径已经容忍它，single/broadcast 没有，形成 RR-20260919-09。框架应选定一个统一契约：

- 推荐让 Getter 对缺失返回 `ErrEntityNotFound`，减少消费者遗漏；
- 兼容期所有调用者仍同时检查 `err` 与 nil，避免第三方 Getter 继续返回 `(nil,nil)`；
- 测试至少包含真实 ManagerAccess，不能只用会主动返回 error 的 mock。

broadcast 的失败隔离边界应包住“Get 后的所有单实体操作”，包括 Touch/guard/handler/commit/release。这样一个缺失或损坏实体不会阻止后续 id。

## Activity 当前状态流

Kit 为每个完成活动创建 per-game Dispatch，保存 token、attempt、backoff 与 pending/acked/exhausted 状态；DeliveringActivities 让后台跨轮重新发现未终结活动。游戏完成 mail/本地 settled record 后，用 token ACK，顺序能够承受 ACK 前重放。

缺失的是交付所有权：

```text
Kit sweep: AttemptDispatch → 只修改 attempt/backoff，没有 send
Game poll: LookupDispatch → 只猜当前/上一窗口，没有 owed 枚举
```

两个循环彼此独立。服务端把“把 payload 从 store 取出来”当成一次 delivery，游戏端却不消费该返回值；因此 attempt 不是实际交付次数。游戏离线超过一个窗口后也不会再猜到旧 key。

## 用现有框架原语补齐闭环

推荐保持拉取模型，因为游戏侧已有结算与 ACK 顺序，Kit 也已有持久状态：

1. dispatch 创建事务同时写入 `gameSID → due dispatch keys` 的持久有界索引；若底层不能原子跨 key，使用现有 CAS/repair sweep 保证可重建。
2. Coordinator 增加游标/limit API 列出某 game 的 due keys；用现有 servicerpc 生成客户端与路由，不手写另一套传输。
3. 把 `AttemptDispatch` 变成游戏 runner 的 RPC 领取操作。CAS 成功返回 payload 的时刻才递增 attempt；回复丢失由相同 token、Lookup 与 ACK 状态对账。
4. 游戏按 owed 索引排空，不使用本地时间推导 key。settle 保持 mail → World settled record → ACK。
5. Kit sweep 只推进窗口、修复缺失 dispatch/index、退休 acked/exhausted 和上报滞留，不代表游戏做 delivery。

如果选择主动推送，也必须为 Server 注入明确的 deliverer，并在传输接受 payload 后才计 attempt；当前没有该 collaborator。无论推还是拉，都需要保证“尝试次数”对应真实交接，而不是定时器次数。

## 验收矩阵

| 场景 | 要证明的不变量 |
| --- | --- |
| game 离线多个窗口后上线 | 旧 dispatch 可枚举并最终 ACK |
| 领取 RPC 回复丢失 | token/attempt 可对账，不重复发奖 |
| mail 成功后进程退出 | settled 重放幂等，dispatch 保持 pending |
| ACK 回复丢失 | 重查得到 acked，索引最终退休 |
| 两个同 SID runner 并发 | CAS 只授予一个有效 attempt/相同 token |
| Kit/Game 分别重启 | owed 索引、dispatch、World settled 均可恢复 |
| 人工 reopen exhausted | 新预算可见，旧 ACK 不能关闭新一轮 |
| 大积压分页 | limit 有界且游标持续前进，坏项不饿死健康项 |

实现完成前，现有 activity 状态机测试只能证明 CAS/退避/终态转换，不能证明结果实际送达游戏服。
