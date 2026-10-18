# MMORPG 场景到 core 框架：学习与接入手册

本文面向准备使用 core 开发 MMORPG 的服务端工程师。目标是从一条玩法追到它的状态归属、锁、持久化和客户端可见性，并能运行一个最小消费者。它与 [SLG 手册](SLG-INTEGRATION.md)分别维护，公共框架概念相同，玩法优先级不同。

本会话已明确选择：**Player 静态绑定 game sid；跨服玩法由 Remote / Saga 协作，不迁移 Player 的权威归属**。同进程地图切换、跨服服务协作、按 sid 换机器是三个不同流程。文中的人数、伤害、期限和奖励都是可执行样例数值，正式游戏必须自行定义。

## 1. 先理解四种状态

| 状态 | 拥有者与框架能力 | 怎样使用 |
| --- | --- | --- |
| 角色资产、血量、任务、地图归属 | Player Entity 的 DAO；组件提供业务方法；Nest 串行修改 | 在 handler 内用生成的 mutator 写入，不能让网络端点裸改 DAO |
| 交易托管、怪物归属、副本事实 | 独立 Trade / Monster / Scene Entity，必要时拥有自己的 DAO | 已知参与者一次传入多实体 Sender，让 Nest 排序取锁、同事务提交 |
| 谁能看到谁 | `policy.Interest` 的地图 AOI、关系来源与同步 Profile | 由权威位置更新索引；它是派生索引，不能反向覆盖 DAO |
| 客户端已经看到什么 | `entitysync.Manager` 的会话、epoch、基线及 ObjectRef；`frame.Receiver` | 接收 Full 时清旧视图，连续应用 Delta；旧 epoch 忽略，缺口重建全量 |

当前实践的两个 Scene 带人数 / 完成事实，因此持久化；game-demo 的地形 Scene 可从配置重建，标记 `noPersist=true lifetime=runtime_rebuild`。这两者是不同业务模型，不能照搬注解。

共同的基础阅读顺序：[入门](../GETTING-STARTED.md) → [Nest / Entity](guide/02-nest-entity.md) → [DataEngine](guide/03-dataengine.md) → [同步](guide/04-sync.md) → 本文对应场景。主仓库是 core；ssr 仅供业务经验对照。

## 2. MMORPG 业务与框架对照

| 业务场景 | 对应能力 | 接入方式与源码入口 | 业务负责什么 |
| --- | --- | --- | --- |
| 登录、建角与首登 | account、玩家接入、Entity lifecycle | [EnterGame 模板](../../demo/game/controllers/player/enter_game.go.tmpl)：先核对角色绑定 sid，再 `GetOrCreate`，已有实体请求走 Sender | 渠道认证、区服和角色政策；PlayerID 来自已鉴权会话 |
| 移动、寻路与地图归属 | Scene runtime、Map 组件、Nest | [MovePlayer](../../demo/game/handler/move_player.go.tmpl)、[MapComponent](../../demo/game/entities/player/map_component.go.tmpl)：验证地形后写 DAO，再发布脏状态 | 移动速度、射程、视线与反作弊；AOI 距离不等于攻击授权 |
| 同进程切图 | 多实体 Nest + DAO undo | [持久切图](../../scripts/testdata/mmorpg/tasks.go.txt)：版本、原地图、新地图人数与归属一起修改 | 副本准入、容量、出生点、过期请求识别 |
| 切图后的视图切换 | 一个 Manager、多张地图各自的 Interest、held / ready | [多地图接法](../../scripts/testdata/mmorpg/scene_runtime.go.txt)：按已提交归属重建索引，失败保持 held，成功 Ready 后发新 Full | 驱动重试与生命周期；持久提交与网络交付不是同一事务 |
| 同屏观察与隐私 | Interest `SourceProfiles` + Player packer | 自身完整视图，空间来源使用 `near`；[真实客户端场景](../../scripts/testdata/mmorpg/scene_runtime_test.go.txt)核对金币 / 经验 / 背包不外泄 | 队伍、敌我、隐身、观战等权限及对应字段集合 |
| 组队、退出与解散 | 多 Player Sender + 关系订阅 | [MMORPGParty](../../scripts/testdata/mmorpg/tasks.go.txt)、[MMORPGDisband](../../scripts/testdata/mmorpg/gameplay.go.txt)；demo Scene `SetTeam` 展示关系来源 | 邀请、队长、人数、跨地图信息权限；样例为两人队伍，解散带预期队伍 ID |
| 面对面交易 | 独立 Trade DAO + 三实体 Nest | `MMORPGOffer` 预留双方资产，`MMORPGResolveTrade` 处理确认 / 取消 / 到期；交易实体 ID 是幂等身份 | 物品唯一性、容量、报价、确认规则、掉线政策；样例掉线选择取消 |
| 权威战斗与冷却 | 多实体 Nest、业务时钟、单调输入序号 | `MMORPGStrike` 校验存活、同地图、顺序、冷却，再改变怪物血量与击杀归属 | 战斗公式、技能命中、距离和预测；样例不是完整即时战斗系统 |
| Buff、死亡与复活 | 持久实体定时器 `Host` / `Driver` | `MMORPGBuff` / `MMORPGRevive` 与 [到期回调](../../scripts/testdata/mmorpg/timer_handlers.go.txt)；回调只修改自己已锁实体 | 叠加、驱散、刷新、死亡损失、复活点；离线到期加载后补触发一次 |
| 掉落、任务与副本结算 | Player / Monster / Scene 的同事务业务事实 | `MMORPGLoot` / `MMORPGQuest` / `MMORPGCompleteInstance`：领奖与领取事实一起写，死 Boss 才完成副本 | 掉落分配、任务条件、锁定进度、重置和奖励政策 |
| 背包、装备与属性 | Player 组件、嵌套 DAO、Nest rollback | [EquipItem](../../demo/game/handler/equip_item.go.tmpl)：背包扣新装备、换装、归还旧装备、刷新属性同事务 | 背包格数、装备绑定、强化与数值；装备派生层不能成为另一份权威 |
| 技能与配置 | `gameplay/skill`、configdata、不可变 Program | [技能指南](guide/08-skill.md)、[配置指南](guide/07-config.md)；Parse → Compile → Host → Runtime | Host 权威副作用、配置版本、跨 tick 状态与恢复；Skill Runtime 不自动加入 Nest undo |
| 匹配、聊天与战斗房间 | match / chat / session、Lockstep | game-demo 正式机器人执行聊天、组队匹配、房间战斗和状态同步 | MMORPG 世界状态同步不必采用 Lockstep；按具体玩法选择 |
| 公会与跨服读取 | Remote Entity、lease / fence、Mirror | demo Guild 的正式 Remote Sender / Mirror 接法；[Remote 指南](guide/05-remote-mirror.md) | 公会权限、命名、成员与跨服观察政策；Mirror 是快照，不是写权威 |
| 跨 sid 赠礼与补偿 | Saga 原生步骤、事务性 effect、邮件 | `roost add saga -native` / `roost add effect`；[双 sid 验收脚本](../../scripts/test-mmorpg-cross-sid.sh) | 收件人路由、补偿、人工收口与重试期限；Saga 不提供跨服瞬时原子交易 |
| 邮件、支付、活动与排行 | mail / platform / activity / rank、持久业务账本 | demo 真实端点与 `scripts/test-demo-generated.sh` | 订单与领奖身份、赛季、补发政策；有限服务去重环不替代长期资产账本 |
| 重启、搬服和观测 | 文件 WAL、Mongo、readiness、排空、schema、静态 sid 单实例 | [搬服](PLAYER-MIGRATION.md)、[schema](SCHEMA-MIGRATION.md)、[观测](OBSERVABILITY.md) | 部署容量、恢复目标、监控接收与运维；不删除 WAL 绕过恢复错误 |

## 3. 先跑业务纵切，再读实现

在仓库根目录使用 go.mod 声明的工具链：

```bash
bash scripts/test-mmorpg-generated.sh
# 希望检查生成工程时保留目录：
ROOST_MMORPG_KEEP=1 bash scripts/test-mmorpg-generated.sh
```

脚本从 `game` 模板创建独立工程，经公共 CLI 添加 Player / Scene / Trade / Monster DAO 和 Player 定时器，仅填业务定义、回调与 handler，再生成组件、访问器与 Sender。不是生成器内置了 MMORPG 规则。

业务入口有 [handler](../../scripts/testdata/mmorpg/gameplay.go.txt)、[行为测试](../../scripts/testdata/mmorpg/gameplay_test.go.txt) 和 [基础事务场景](../../scripts/testdata/mmorpg/tasks_test.go.txt)。当前顶层场景共 13 条，脚本要求全部实际通过且无 skip，随后执行 ID 与生成状态检查。

推荐按此顺序阅读与修改：

1. `scripts/test-mmorpg-generated.sh` 中的 DAO 字段，先画清玩家、交易和怪物分别拥有哪些事实。
2. `handlerMMORPGOffer`：找到扣除托管资产与记录交易身份的同一个 Nest 边界。
3. `handlerMMORPGResolveTrade`：分别推演单方确认、双方确认、取消、到期和重复请求。
4. 对照竞争用例，确认竞争的终态只能是交换完成或完整退款，不能只看两个请求返回值。
5. 阅读 `handlerMMORPGStrike` → `Loot` → `Quest`，确认重放不能再次命中或领奖。
6. 阅读 Buff 刷新 / 到期和死亡 / 复活用例，理解 DAO 期限与运行时索引的关系。

在这份生成消费者中，业务调用使用生成的多实体 Sender，例如：

```go
// ctx、client 与实体完整 ID 来自当前服务；actorID 由已鉴权会话转换，不能信任客户端自报。
s := syncsender.NewMmoGameplaySender(client)
err := s.MultiSync_MMORPGOffer(ctx, tradeEntityID, sellerEntityID, buyerEntityID, 10, 1, serverNowMillis)
// 两次分别由卖家、买家确认；不在持实体锁的 handler 内等待另一个玩家或远端 I/O。
err = s.MultiSync_MMORPGResolveTrade(ctx, tradeEntityID, sellerEntityID, buyerEntityID, actorID, 1, serverNowMillis)
```

这是已生成接口的调用片段，不是可独立运行的 main。完整装配在脚本和测试中。金币 / 道具上限、5 秒报价期限和最后一笔转账的单调序号仅是本例的业务选择；不要直接拿它们代替正式经济协议。

默认纵切的提交器是替身，主要验证生成代码、取锁、DAO undo 与业务交错。恢复有更深一层的可执行入口：

```bash
# ROOST_DATAENGINE_IT_MONGO_URI 必须指向独立 Mongo 副本集，先按集成说明准备。
ROOST_MMORPG_REAL=1 bash scripts/test-mmorpg-generated.sh
```

该模式为本次生成工程派生唯一 DAO 库，顺序运行两个独立 `go test` 进程，使用同一文件 WAL 与真实 Mongo：第一进程停在交易单方确认，第二进程冷加载后完成交易，重放击杀 / 掉落 / 任务领奖，并处理离线 Buff。两阶段必须实际通过，无 skip；成功后只删除本次唯一库。入口：[恢复夹具](../../scripts/testdata/mmorpg/persistence/persistence_test.go)。这是干净停启恢复，尚不是断电故障注入。

## 4. 地图切换与断线重连怎样接

```mermaid
sequenceDiagram
    participant C as 客户端
    participant B as 业务端点
    participant N as Nest / DAO
    participant S as Manager + 两张地图 Interest
    B->>N: 校验版本、准入，提交新地图归属
    N-->>B: 事务结果
    B->>S: 按权威归属 Reconcile
    S->>S: Hold，移除旧图兴趣，加入新图兴趣
    alt 索引或准入失败
        S-->>B: 保持 held，稍后按权威归属重试
    else 成功
        S->>S: Ready
        S-->>C: 更高 epoch 的 Full
    end
```

运行：

```bash
bash scripts/test-mmorpg-scenes.sh
```

脚本生成新的 game-demo，填入多地图业务接法，然后运行正式 TCP、线协议和 `frame.Receiver` 客户端，检查：旧图删除、目标图全量、相同坐标的跨图隔离、held 重试、旧 epoch 拒绝、重连恢复与公开 / 私有字段隔离。随后消费者 build / vet。

关键接法在 `mmoScenes.Reconcile`：它从 Nest 只读接口取得已提交位置；同一个连接由同一个 Manager 管理流与 epoch，两张地图分别维护自己的 Interest。不要把两个独立 Manager 的默认流无控制地拼到同一个客户端 Receiver 上。应用 Full 时必须清除旧对象，不能只把新对象追加上去。

同一场景内已有 demo 回归还覆盖：停读连接被关闭，健康连接继续消费；旧 session 的迟到断开不移除重连后的新成员；实体卸载 / 重载恢复观察者。运行生成 game-demo 的：

```bash
go test -race -count=1 ./internal/service/game ./game/scene/... ./cmd/loadtest
```

本机 loopback 客户端使用的是框架 Receiver；部分场景鉴权有替身，不能当作 Unity 实机、渠道认证或目标密度性能验收。队伍解散后的关系来源更新、更多可见性策略应与正式队伍服务联动，不能认为调用 `SetTeam` 就实现了完整队伍服务。

## 5. 保持 Player 归属的跨服协作

每个角色从 account 取得 `ServerID`。`EnterGame` 在装载之前核对认证会话中的绑定，登录错服应返回 `player_elsewhere` 与 owner sid。Player 不随一次跨服交互搬到别的 game 进程；本服业务经 Saga 请求对方或邮件服务完成交互。

```text
sid A 的玩家 → 本地 Nest 扣款 / 记回执 → Saga / outbox
              → sid B 或邮件步骤 → 成功收尾
              → 失败时补偿原玩家；结果未知先查询 / 恢复，不盲目再扣款
```

公共生成器支持 `roost add saga <name> -steps ... -native ...` 与 `roost add effect <name>`；参数和骨架接法见 `roost help saga`、`roost help effect` 及 [Saga 指南](guide/06-saga.md)。步骤中只执行本步骤声明的实体事务，不能在持锁期间同步访问另一个服务。

已有隔离 game-demo 的 account、支撑服务和两个 game（sid 1000 / 1001）启动后，配置实际地址再运行：

```bash
export ROOST_MMORPG_DEMO_ROOT=/path/to/generated/game-demo
export ROOST_MMORPG_PREFIX=your_project_prefix
export ROOST_MMORPG_DATABASE=your_player_database
export ROOST_MMORPG_ENDPOINTS='1000=127.0.0.1:7000,1001=127.0.0.1:7001'
# 同时设置隔离资源地址 ROOST_DATAENGINE_IT_MONGO_URI、ROOST_DATAENGINE_IT_NATS_URL。
bash scripts/test-mmorpg-cross-sid.sh
```

脚本只创建本次开发测试账号，不起停服务、不改角色 sid。检查真实账号会话错服拒绝、双向共 4 笔赠礼完成、重复邮件领奖不增加资产、1 笔无效收件人的补偿，以及 Mongo 中背包最终为 `8 / 2 / 8 / 2`。Saga 库约定为 `<ROOST_MMORPG_DATABASE>_saga`；若工程自定义了另一命名，先调整脚本参数接法，不对生产库运行。

Remote / Mirror 则用于公会等跨服实体：读取镜像不代表拥有写权，写操作走正式 owner / lease / fence 路径。真实功能入口 `scripts/test-remote-generated.sh` 与上面的 Saga 验收分别执行，不能互相替代。

## 6. 学习时最容易混淆的边界

| 常见误解 | 正确接法 |
| --- | --- |
| 改了 SceneID 就完成切图 | 持久事实、兴趣订阅、客户端基线都要更新；派生步骤失败后按权威重试 |
| AOI 看见某人就能攻击 / 交易 | AOI 是交付策略，handler 仍需验证权限、距离、存活、同图与状态 |
| 收到一次确认就能交换资产 | 两个确认与报价身份 / 版本绑定，终态保留去重依据；取消与确认必须竞争同一事务事实 |
| strict 成功等于 Mongo 已落库 | strict 的边界是 WAL durable；需要投影可见时等待 DataEngine 的正式屏障 |
| 关闭连接或 ctx 超时就撤销请求 | 已准入事务可能仍完成；先辨认结果未知，再按业务身份查询或恢复 |
| 把 Skill Runtime 放进组件就自动回滚 | Host 副作用和 Runtime checkpoint 必须由业务设计一致的持久边界；本例 DAO 战斗没有声称已完成持久技能 Host |
| 框架已有 Lockstep，所以 MMO 必须帧同步 | 开放世界可做服务器权威状态同步；竞技副本可独立选 Lockstep |
| 真实 TCP 场景通过就说明容量合格 | 功能与容量分开；玩家密度、带宽、分位时延、跨机故障和长稳需要目标负载证据 |

## 7. 当前完成范围与练习

已提供业务纵切、多地图网络纵切、真实两进程恢复及静态 sid 跨服协作入口；装备、聊天、邮件、支付、匹配、排行和 Lockstep 由正式 game-demo 场景学习。当前实际执行结果维护在 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md)，不以本文的清单长度代替验收。

练习时可以依次增加：三人队伍及队长转让、报价修改使双方确认失效、物品实例与绑定交易、掉落分配、多个副本重置、权威射程与技能 Host。每次先写出业务规则与失败语义，再映射到现有框架接口；缺少这些规则不是框架 bug。真实客户端引擎、目标规模长稳、跨机器故障仍单独验收；按已确认路线，动态 Player 跨 sid 迁移不在本接入方案内。
