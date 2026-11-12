# activity：活动生命周期

`roost module add activity` 安装个人活动及共用引擎；`activity_world` 增加全服/服务器组宿主，`activity_alliance` 增加联盟权威宿主。三个宿主直接共用同一个 `activity.Component` Go 类型，各实体包只保留类型别名、组件注册和宿主适配。配置表默认为空，不会自动开启示例活动。

## 生命周期

标准阶段只有五个：**Initializing → Warmup → Open → Settling → Ended**。

| 阶段 | 进入后的含义 |
| --- | --- |
| Initializing | 冻结定义、时间锚点与期次，准备业务状态；允许等待可靠后台完成 |
| Warmup | 等待开启时间；零预热会在同一事务继续推进 |
| Open | 允许业务按规则操作；实际操作必须先调用 `CheckOpen` |
| Settling | 关闭新行为，完成本期业务；允许等待异步结算 |
| Ended | 本期完成，保存终态水位；循环在固定排期创建下一期 |

报名、分组、比赛是 `Flow`，例如 `Open + registration`，不扩充阶段枚举。`MoveFlow(token, Flow{Name, Due}, now)` 修改流程；`Due=0` 表示等可信事件，非零表示同阶段内的持久期限，到期调用 `Hooks.Flow`。回调返回下一流程及期限。阶段改变会清除旧流程；流程版本、阶段、期次、激活代际一起隔离迟到事件。

`activity.Component` 通过 `activity.Owner` 取得宿主的 `ActivityBook()` 和 `ActivityScope()`，不判断 Player/Alliance 等具体类型。宿主可提供 `ActivityCreatedAt()` 返回持久创建事实；只有 Player 实现 `SetActivityDeadline()`，把统一计算的期限接到 entitytimer，Remote 默认保存期限供权威扫描。实体包里的 ActivityComponent / WorldActivityComponent / AllianceActivityComponent 都是该类型的别名，仅为安装器分配的组件位置接线，不是旧 API 兼容层。

`Host` 是一次调用内的状态机入口，不在组件中缓存状态。状态、阶段游标和本实体业务修改都由生成 DAO 参与同一 Nest 事务。初始化/结算钩子失败不能被吞掉；成功进入不等于外部工作已经完成。时间到了也不会自动确认 `pending`。

## 定义与时间

工程中的 `configs/schema/activity.go`、`configs/table/activity.csv` 是规则源。主要字段：

| 字段 | 约束 |
| --- | --- |
| ID / Version | ID 为 1..128，不回收；规则变化提升 Version |
| Scope | `player`、`world`、`alliance` |
| Trigger | `fixed` 用 StartMilli；`created` 用可信的持久创建时间；`event` 用同事务业务事件及 Eligible |
| Event / Logic | event 必须指定事件和已登记的逻辑；缺失条件回调时拒绝激活 |
| WarmupMillis / OpenMillis | 预热可为 0，开启必须大于 0；各不超过 366 天 |
| Repeat / Period | `none`；`interval` 的 Period 是毫秒；`daily` / `weekly` 的 Period 是日数/周数 |
| MaxCycles / UntilMilli | 0 表示不限制；非零限制期数/整体结束时间 |
| Catchup | `queue` 保留每期责任；`skip_empty` 仅允许 Logic 为空的纯展示活动 |
| Disabled | 不再创建新实例；已激活实例继续执行被冻结的规则 |

日历规则冻结业务时区和日界。固定日期日历活动的 StartMilli 必须落在业务日界；创建/解锁触发的日历活动从“事实时间 + DelayMillis”之后的首个业务日界开期，恰好位于日界时使用该时刻。每周活动从首期锚点按 N 周重复；需要固定周一时，配置周一的固定锚点。日历按日期计算，跨 DST 不等于固定 24 小时。每个相邻窗口检查重叠，非法配置保留原状态并报错，不悄悄跳期。

活动激活后，整个序列使用冻结的定义版本；热更不会修改已生效的开启/结束时间。默认首次解锁持久生效，重复事件不重置时间。`Reopen` 只允许已彻底结束的序列，必须传 `oldGeneration + 1`；重复同代际请求返回现有实例。无限循环的规则不通过热更自动结束。

`Definition + Scope + Owner + Generation + Cycle` 是一期身份。Token 还包含 Phase 和 FlowVersion。Owner 是逻辑实体 ID，不是当前进程 SID。

## 业务接入

安装后的代码是业务代码，可以修改。主要入口：

- `ActivityComp().Reconcile(event, createdAt)`：按当前配置激活符合条件的定义并推进，随后更新期限。
- `ActivityComp().Host()`：直接使用 `Activate`、`Complete`、`Reopen`、`CheckOpen`、`MoveFlow`、`JoinShared`、`Views`。修改后调用组件的 `Rearm`；错误交还 Nest 回滚。
- 可信内部 Nest 入口 `ActivityEvent` / `ActivityComplete`；客户端仅有查询 PB `ActivityView`，没有“我已满足条件”的上报协议。
- `activity_world`、`activity_alliance` 的组件分别为 `WorldActivityComp()`、`AllianceActivityComp()`；共用相同的 Host API。

例如升级到 20 级后解锁个人活动，在 `game/entities/player/activity_growth.go` 登记：

```go
func init() {
    activity.Register("growth20", activity.Hooks{
        Eligible: func(owner entity.IThreadSafeEntity, event string) bool {
            p := owner.(*Player)
            return event == "profile.level_up" && p.Dao().GetProfile().GetLevel() >= 20
        },
        Enter: func(c activity.Context) (bool, error) {
            // 只改本次 Nest 已锁住实体的 DAO，或登记可靠 effect。
            // 返回 true 只用于 Initializing / Settling，表示等待后台业务完成。
            return false, nil
        },
    })
}
```

规则使用 `Scope=player, Trigger=event, Event=profile.level_up, Logic=growth20`。与 profile 同装时，升级事实、Eligible 和活动激活在同一 Player 事务里；拒绝提交会一起回滚。与 enter 同装时，首次建档建立 created 锚点，每次进入按持久 FirstAt 恢复。没有 enter 时，由业务提供真实创建时间，不能拿每次登录时间冒充。

不要把阶段名当成写权限。动作执行前调用 `CheckOpen(token, fctx.NowMilli())`，即使到期消息还在队列中也会拒绝过期动作。有界推进可能暂时停在尚未运行进入钩子的阶段，CheckOpen 同时检查这一事实。业务先校验，再修改玩法数据；无错误才调用 Rearm 并交给 Nest 提交。

## 异步完成及可选联动

`Hooks.Enter` 可在初始化/结算时返回 `pending=true`，同时写入被冻结的任务、名单或可靠意图。`Hooks.Resume(ctx, registry, snapshot)` 是可选的锁外 I/O 入口；业务确认完成才返回 `done=true`，随后以原 Token 调用正式 Complete。未登记 Resume 时，保留 pending，等待业务自己的可靠完成入口。

Remote 权威服务的扫描会在 Pulse 提交后运行 `ResumePending`；个人活动在进入后的 I/O 回调恢复。个人活动也可由可靠 effect 消费者直接调用 ActivityComplete，不依赖再次登录。业务如需离线准点执行，必须提供可靠后台驱动，当前个人惰性恢复不作此承诺。

Resume 可能因超时、进程退出、属主迁移或 Complete 失败而重入。外部副作用必须幂等：

- 同装 leaderboard：`activity.Board(token)` 为定义、宿主、代际、期次分榜，复用现有 rank 协议。分数应来自成功提交的业务事实；结算前冻结榜单/奖单，不能每次重试重新读取可变 Top 再发奖。
- 同装 mailbox：`activity.SendReward` 复用 `mailbox.SendTo`。以实例身份、动作和收件人生成稳定键；不含当前时间或流程版本。只有所有持久奖单都确认送达后才能 Complete，单封成功不代表整期成功。
- `activity.CoordinatorKey(token)` 连接现有 `service/activity` 跨服汇聚窗口。业务冻结预期服务器名单，调用 OpenActivity / NotifyPhase，通过 OwedDispatches 恢复欠账；持久应用结果后才 AckDispatch。它不替代本模块的生命周期，也不会自动部署 coordinator。

这些是可组合的接口及测试过的身份/发件适配，不是默认的“活动榜前十发金币”活动。没有活动规则时零业务运行。权威服务要调用 rank/mail/coordinator 时，需要给**实际调用它的服务**配置对应 `uses` 并登记业务钩子；只在 game 装了客户端，不代表联盟或全服进程也有该 capability。

## 归属与参与

个人活动由 Player 保存，两个玩家在不同时间解锁有独立锚点；换服沿用同一份已持久化 Player 数据，不根据新 SID 重建。联盟活动由联盟权威实体保存；入退盟不重置联盟活动，game Mirror 不推进。

全服实例归 `ActivityWorld`，实体唯一号是逻辑区服/服务器组号。`activity_scope.csv` 指定 `ID, BootstrapSID, CreatedAt`；由指定的 activity_world 进程建档，后续写权限由 Remote ownership 控制。多个 game 对同一 ID 查询同一时间线。迁移后应同步调整 BootstrapSID，不能用另一个 ID 重新开服；旧驱动会被路由和 Remote 准入拒绝。

个人解锁共享活动的参加资格时调用 `JoinShared`，只写 Participation，不创建个人定时器或重开共享活动。输入必须来自服务端授权查询；贡献/领奖仍向权威宿主核对实例和成员资格。模块不替业务决定退盟后的贡献或奖励归属。

## 恢复、容量和错误

- 一个 Player 共用一个活动持久定时器；模块复用框架的 entitytimer。回调没有 error 返回值，因此适配层将错误抛给 Nest，由 Nest 回滚阶段、业务修改和定时器删除；有正式回归，不能改成吞错后 `return 0`。
- 现有生成器禁止给 Remote Entity 挂本地持久定时器，本模块保留限制。全服/联盟由权威服务扫描自己的 ownership 清单，期限保存在活动 DAO。每秒至多扫描一页 128 个宿主、8 个并行 I/O 调用，每调用预算 5 秒；列表和路由校验在锁外，阶段推进仍在 Nest。大规模或慢依赖下扫描延迟会增长，不承诺 1 秒内全部到期。
- 每个宿主每次最多 32 次阶段/流程动作，定义之间轮转；剩余个人工作延后至少 1 秒继续。不会追放业务 tick。queue 保留旧期未决结算，后续期次固定排期，不因结算晚而整体漂移。
- 只保留活动定义的当前实例及已完成期次水位；业务需要历史成绩、领奖凭据或完整审计时，应持久保存自己的有界账本/奖单。不会因清理历史而删除 pending。
- 128 个定义号，最多 128 条共享参与资格。容量满明确返回错误；不会自动驱逐旧参与资格以冒险重发奖励。需要更多维度或长期历史时应设计业务归档和终态水位。
- 单调推进，不因业务时钟回拨重开已关闭阶段；Token 必须完整传递，旧期、旧流程和旧代际拒绝。
- 定义错误或钩子失败保留责任并返回/记录错误；不会把错误伪装成初始化、结算成功。关闭驱动先取消，再等待在途调用；超时可继续等待，不提前丢失依赖。

## 验证

`roost module verify activity activity_world activity_alliance` 验证生成工程的定向场景、build、vet、ID 和生成物漂移；无参数 verify 检查全部已装模块。

本轮证据、变异项和未运行项见 [R9 验收记录](../../docs/maintenance/evidence/module-activity-20261112/README.md)。内存 Remote/提交替身、BSON 恢复和进程内路由检查不等同真实 Mongo、WAL、NATS、跨机迁移或长稳验收。
