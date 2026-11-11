# monster：巡逻、追击与死亡重生

`roost module add monster` 自动安装 actor 与 scene，接入心跳、Player/Monster/Scene、AOI、PB 与模块 runtime。新 game 工程按默认规则在场景 1 的 `(80,80)` 放一只怪：模板 1、100 HP、速度 6、半径 12 的四角巡逻、感知半径 30，死亡 5 秒后重生。规则在归工程所有的 `game/monster/rules.go`，不需要用户先填表才能运行。

Monster 是 `noPersist=true lifetime=runtime_rebuild` 实体，不写 WAL/Mongo。重启会按当前启动规则重建，运行中临时 Spawn 的种群不自动跨重启恢复；需要长期种群计划时由业务保存规则并在启动时重放。每个进程最多 256 个出生槽位；静态 ID 编码本进程 sid 与不回收的序号，sid 必须在 1–65535 内，同一 sid 不能同时运行两个写者。它不参与跨进程分区、迁移或接力。

## 入口与边界

`game/monster/service.FromRegistry(registry)` 得到 Manager。以下入口会等待 Nest，必须在业务 worker 之外调用：

- `Spawn(ctx, sceneUniqueID, spec)`：可信服务端按不可变规则创建，返回完整 Monster EntityID。使用生成生命周期的带参数扩展创建；先在 Monster + Scene 事务中入场，再由场景提交后发布给 AOI。失败走同样的事务退出与生命周期销毁；清理失败保留记录并返回 ID/错误，调用方可重试 Destroy。
- `Damage(ctx, entityID, expectedLife, amount)`：服务端伤害入口。`expectedLife` 必须匹配当前 LifeSeq；同一命内每次攻击的幂等由 combat 自己的账本保证，不能把 LifeSeq 当攻击请求键。
- `Destroy(ctx, entityID)`：先在 Monster + Scene 事务中停止心跳、离场并释放成员槽位，再通过 lifecycle.Destroy 从 EntityManager 移除；移除失败保留记录供重试。
- `IDs()`：本进程的出生槽位快照。

客户端只有 `MonsterInspect(MonsterID)`，MonsterID 是 AOI subject 使用的完整 EntityID。PB/controller/Nest 从认证会话取得 Player，与 Monster 同时持锁，只允许查看当前同场景的存活怪；客户端没有出生、伤害或杀怪权限。公开 Sync 仅包含 Actor 与 Vitals，巡逻游标、目标、重刷截止及退休标志不公开。

`MonsterActorComp()` 提供 `Body()`、`Snapshot()`、`Damage(expectedLife, amount, now)`、`Decide(expectedLife)`。后两者仅可持锁调用；生命相关修改要带相应 Scene 参与者。出生、伤害、退出的正式 Sender 分别是 `MonsterSpawn`、`MonsterDamage`、`MonsterRemove`。下一步 combat 应在自己的多实体事务里重新检查场景、距离与 LifeSeq，不能把感知列表当伤害授权。

## AI 与回滚

移动、位置、朝向、跟随和定身门控共用 actor.Body。场景通过实例回调提供 `Space.Sense` 的**已提交玩家位置**；怪只在自己的锁里读值快照，不读取别人的 DAO 或拿别人的锁。感知半径不能大于场景 Sight，优先最近玩家，距离相同时以 EntityID 确定顺序。当前目标仍相同且 Follow 在运行时不重新发起，低速移动的小数进度不会每拍归零。目标离开或不可见时恢复四角巡逻。没有仇恨表、寻敌攻击或技能释放；战斗由 combat 接入。

每拍实际创建 `ai.Controller` 和 `actionflow.Behavior`，AI 经瞬时动作提交一次巡逻/追击意图。actionflow runner 不支持 Nest undo，所以它不保存跨拍状态，也不登记第二套移动心跳；真正持续的移动只留在支持 undo 的 actor.Body，目标、游标与生命留在 DAO。一次决策后续失败时，DAO 与 Body 同时撤销，不会留下后台动作。巡逻角点的地形碰撞由 actor/movement 处理，不提供全图寻路。

actor 心跳按场景时间域移动。死亡重刷使用**业务时间**的独立 1 秒心跳，不随场景倍速或时停改变等待时长；时钟回拨会推迟截止。

## 死亡、重生与停机

死亡在同一笔 Monster + Scene 事务中把 HP 置 0、Body.Leave 隐藏 AOI并启动重刷心跳。尸体保留同一 runtime 实体和 Scene 成员槽位，容量仍计数，因此不能在等待重刷时关掉它的场景。显式 Destroy 或模块 Stop 才释放槽位。

截止到达后，心跳 AfterCommit 只向容量 64 的队列投递 ID，不等待。队列满、临时失败或正在重试均由下一拍继续，worker 再发送 Monster + Scene 的重生事务。重生恢复出生位置/HP、增加 LifeSeq、清目标游标、重启 actor；旧命的迟到伤害和 AI 命令被 LifeSeq 拒绝。**重生复用同一 EntityID**，每命历史与击杀奖励不由本模块记录。

runtime 注册正式 ingress：先拒绝新操作、取消并等所有已接受操作和队列 worker退出，再按事务退出和 lifecycle逐个销毁。超时或销毁失败返回错误、保留可重试资源；依赖 scene 在 monster 停止成功后才停止。业务仍需按 scene 的协议排空玩家。

## 验证

`roost module verify monster` 固定 25 条、无子测试：正式出生/AOI/PB、巡逻、定身、低速追击、感知边界、决策失败回滚、死亡隐藏/截止/槽位、旧命拒绝、销毁、非法规则与 sid、停止准入、真实 heartbeat 的巡逻和重刷、出生失败清理、缺省 runtime/ingress、死亡失败回滚、退出回复丢失后续做、同ID不同实例提前拒绝清理，以及初生/异步重刷/伤害/退出分别遇到Monster或Scene同ID重建的8种确定交错。

`codegen/internal/roost/module_monster_test.go` 在自定义 Go module 的全新 game 上安装、重复安装、核对场景数并跑 build/vet/ID/漂移；十一种变异先证明可编译，再由旧命伤害、死亡隐藏、低速 Follow、退出恢复与实例身份交错场景击中。场景使用真实 Nest、实体 Manager/lifecycle、heartbeat、AI/actionflow、movement、Sync/AOI，持久提交与网络传输为替身。未验证真实数据库、TCP 联网、跨进程故障、长期稳定性或大规模性能。

Scene 的正式关图入口会按kind调用 Monster.Manager.DrainScene：先关闭目标图准入，再串行清理该Scene实例的全部怪物记录（包括死亡待重生槽），不关闭其他图的Monster入口。回调不持Scene.Manager管理锁，避免与Spawn查询Scene的锁顺序相反；失败保留记录和地图供重试。terrain先于monster停止时也经过这条链，不直接删除仍运行的怪物。

退出恢复区分两阶段：MonsterRemove的回复未知时保留记录并重做幂等Nest退出；明确成功后记下retired，只重试生命周期销毁。Scene对所有已登记模块调用按实例过滤的drainer，不以已Depart的成员表推断没有待清理记录。删除前核对EntityManager中的对象仍是记录实例；同ID的新实例必须拒绝接管。初生、异步重刷、伤害与退出还统一携带出生槽位固定的`monster.Target`：它是只含私有Monster/Scene指针的不可变本地值，生成Sender以普通Nest Params传递，三个handler在持有实际双实体锁后首先检查身份。外层Get检查与handler之间发生同ID重建也会拒绝，旧LifeSeq不能授权新实例；此值不能持久化、编码或跨进程传送。记录缓存原始SceneID，不在卸载后读取已清理的实例ID。
