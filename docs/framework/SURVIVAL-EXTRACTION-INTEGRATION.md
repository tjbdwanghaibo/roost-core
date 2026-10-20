# 多人合作生存／撤离：物资归属、制作与永久兑付

本篇采用四人同局、四个相邻区域的教学规则：区域 0 是安全撤离区，1／2／3 分别产木材、矿石、食物；玩家采集、制作药剂、承受环境伤害，完成撤离读条后将封存物资按固定价格兑换永久金币。不是完整开放世界或射击游戏。先读[入门手册](../GETTING-STARTED.md)，运行[正式生成消费者](../../scripts/test-survival-extraction-generated.sh)，再沿下表理解状态归属。

## 学习：业务与框架对应关系

静态 sid 的 Player 保存永久金币。World 类 `SurvivalRaid` 拥有一次探险的名单、付费准入事实、公共资源、玩家位置／生命／背包、制作、死亡掉落、撤离清单、期限和命令账本。一个局内物品只在 Raid 内改变归属；永久资产仅在正式入场与兑付事务中跨到 Player。

| 业务场景 | core 能力／调用 | 事务契约 |
| --- | --- | --- |
| 建档与成局 | 生成 Player／SurvivalRaid DAO，`Sync_SurvivalPlayerInit`、`Sync_SurvivalInit` | 玩家首次建档送 100 金币；名单和配置按首次创建固定，重试不重置存档 |
| 付费入场 | `MultiSync_SurvivalEnter(PlayerID, RaidID, now)` | 扣 5 永久金币与 Joined 同事务，获得固定初始物资；同 Raid／Player 重试只查原事实，不重复收费或复活 |
| 共享采集 | `Sync_SurvivalCommand`，Raid Nest 锁 | 位置必须匹配资源点，每次取一件；节点扣减、背包增加和 Seq 回执原子提交 |
| 移动与容量 | 服务端位置与 NextMove，背包固定容量 | 只移动到相邻区域，移动间隔至少 100ms；客户端不能上传坐标覆盖或自报背包 |
| 制作与取消 | 局内 CraftAt＋生成实体 timer | 2 木材＋1 食物预扣，制作在途仍保留 3 格；到期得到药剂，提前取消归还材料 |
| 环境与治疗 | 服务器期限触发 Raid handler | 安全区免伤；野外每次到期扣 1 HP；药剂消耗与最多回复 3 HP 同事务 |
| 死亡与拾取 | Raid 内固定死亡包，仍由同一锁管理 | 单次生命，死亡转移全部背包；未完工制作先退料入包；同一件掉落竞争只允许一人取走 |
| 撤离通道 | ExtractAt 与服务器位置 | 仅安全区、无制作时可开始；移动／主动取消中断；开始读条不发奖，完成后封存清单 |
| 永久兑付 | `MultiSync_SurvivalClaim(PlayerID, RaidID)` | 只按服务端封存清单及冻结单价兑付；Player Gold 与 Raid Claimed 同事务，死亡兑付为零 |
| 重连与私有视图 | 持久 Epoch，`Sync_SurvivalConnect/Receipt/View` | 旧连接不能写或断开新连接；新会话查旧 Seq 原回执；不公开他人背包、制作、撤离清单和账本 |
| 崩溃恢复 | strict WAL、Mongo 水合、TimerDriver 循环 | 同时恢复制作与撤离在途事实；先核对完整冷状态，再由驱动完成到期事件和下一期限 |

源码阅读顺序：[规则与配置](../../scripts/testdata/survival-extraction/rules.go.txt) → [状态、视图和守恒](../../scripts/testdata/survival-extraction/state.go.txt) → [命令与时间](../../scripts/testdata/survival-extraction/match.go.txt) → [Nest handlers](../../scripts/testdata/survival-extraction/handlers.go.txt) → [timer 回调](../../scripts/testdata/survival-extraction/timer_handlers.go.txt)。`.go.txt` 会复制进正式生成工程后编译运行，不是伪代码。

### 接入与调用

CLI 创建 Player DAO、SurvivalRaid Entity／DAO／timer；Raid 使用 World 分类，Player 使用 Other 分类，多实体入场和兑付由 Nest 按框架锁序执行。不要手改生成的 Sender 或 DAO。Create 内存实例不代表建档已持久，先调用正式 Player 初始化事务。

```go
api := syncsender.NewSurvivalExtractionSender(engine)
// playerID 来自已鉴权会话；RaidID 已由服务端成局流程分配。
if err := api.MultiSync_SurvivalEnter(ctx, playerID, raidID, serverNow.UnixMilli()); err != nil {
    return err
}
v, err := api.Sync_SurvivalView(ctx, raidID, playerID)
if err != nil { return err }
_, err = api.Sync_SurvivalCommand(ctx, raidID, playerID, survival.Command{
    Seq: v.Self.NextSeq, Epoch: v.Self.Epoch, Kind: "move", A: 1,
}, serverNow.UnixMilli())
return err
```

`move` 的 A 是目标区域；`gather` 的 A 是资源索引 0～2；`loot` 的 A 是死亡玩家席位 0～3、B 是物资索引 0～3；其余命令的 A、B 为 0。物资顺序是木材、矿石、食物、药剂。`craft`、`cancel_craft`、`heal`、`extract`、`cancel_extract` 都通过同一个正式命令 Sender。

PlayerID、规则、服务器时间、接管票必须来自可信服务端入口，不能直接透传客户端字段。首次初始化返回第一席视图只用于内部成局确认。每个玩家的成功命令有永久递增 Seq；同身份重试必须逐字段相同。收到未知提交结果，保留 RaidID／PlayerID／Epoch／Seq 查询，不能换 Seq 再扣费、采集或领奖。每席账本 128 条满后拒绝新玩家操作，保留旧回执，后台期限和最终兑付仍可继续。

### 物资守恒与回滚

所有业务写 handlers 使用 `rollback=undo durability=strict`。规则先解码事务局部副本，校验成功才写 DAO；制作、计时器或双实体扣费／兑付后的拒绝由真实 Nest undo 撤回。strict 成功保证 WAL 持久，Mongo 可以尚未投影。

状态校验计算整局物资账：资源节点＋所有背包＋所有死亡包＋封存撤离清单＋在途制作原料＋已制作所消耗原料，应等于初始公共资源与已准入玩家初始物资。药剂存量＋已使用数量应等于制作完成数量。不会仅检查各个余额非负。

在途制作占用原料原本的 3 格，玩家不能利用预扣临时腾空背包、采满物资后让取消或死亡退料溢出。死亡包仅创建一次，不复活、不重复返还。完成撤离后清空局内背包，把原物资写进不可变 Extracted 清单；兑付不删除清单，因此重试能返回同一金额。当前演示以金币兑换物资，未实现局外逐件库存或装备耐久。

### 时间与恢复顺序

默认探险最长 120 秒，环境期限 5 秒，制作 3 秒，撤离读条 2 秒。数值用于测试，不是生产容量建议。下一 timer 取结束时间、环境时间、四人制作与撤离时间中的最早值。

最早期限已到时拒绝新的玩家操作，等待 timer 提交后重试原命令。原成功回执查询不受期限阻塞。回调检查持久 timer ID、参数与期限，按固定顺序有界处理：

1. 完成已经到期的制作与安全区撤离。制作和撤离开始时均要求完成时间严格早于 Raid End；二者不能在同一玩家身上并存。
2. 当前时间超过 Raid End 时，未撤离的在场玩家死亡，未付费席位标记 missed；已到期的合法安全区撤离保留成功。
3. 尚未结束且环境到期时，只计算一次环境伤害，再从当前服务端时间安排下次环境期限；不追赶离线期间每个伤害 tick。

这是“硬结束期限＋离线伤害暂停”的业务选择。原始 EndAt 永不延长；掉线仍按驱动处理，不能靠重复请求续命。不是墙钟持续伤害、服务端历史回放或离线无敌的通用保证。若所有席位均已终结，停止 timer；个人完成撤离后可以先兑付，无需等队友。

正式服务注册实体／handlers，在加载前设置时钟，Nest 可用后 Bind／Start 驱动；退出先 Stop 驱动，再排空 Nest／Runtime。基础测试通过 `DeliverDue` 控制顺序，真实恢复使用驱动自己的投递循环。

## 验收与复跑

```bash
bash scripts/test-survival-extraction-generated.sh
ROOST_SURVIVAL_EXTRACTION_REAL=1 \
ROOST_DATAENGINE_IT_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0' \
bash scripts/test-survival-extraction-generated.sh
```

Mongo 必须指向隔离测试副本集；脚本生成本次唯一数据库与 WAL，成功清理本次库，失败保留证据。`ROOST_SURVIVAL_EXTRACTION_KEEP=1` 保留成功消费者。默认提交器是替身，只有显式启用的真实模式证明持久恢复。

| 可执行场景 | 验证内容 |
| --- | --- |
| `TestSurvivalExtractionAdmissionAtomicAndIdempotent` | 入场费／Joined 双实体 undo；四人并发重复入场只扣一次 |
| `TestSurvivalExtractionSharedNodeCompetitionAndRetry` | 四人共享节点、重复采集不增发，耗尽与错位置拒绝 |
| `TestSurvivalExtractionCraftCapacityCancellationAndHealing` | 原料预扣、容量预留、取消退款、完工、药剂治疗防重 |
| `TestSurvivalExtractionDeathDropsAndConcurrentLoot` | 六次环境伤害死亡、物资入包、三人竞争同一食物只一人成功，死亡不兑付 |
| `TestSurvivalExtractionChannelCancellationAndAtomicClaim` | 移动取消读条、完成前不领奖、封存清单、双实体兑付 undo 与八次并发领奖 |
| `TestSurvivalExtractionReconnectPrivateInventoryAndDeadline` | 旧连接隔离、新连接查原回执、私有视图、期限准入与离线制作 |
| `TestSurvivalExtractionExpiryCannotReopenOrReenter` | 长时停机后硬结束、保留原合法撤离、重复入场不复活或再收费 |
| `TestSurvivalExtractionUndoRestoresMaterialsJobAndTimer` | 制作写入后的真实 Nest undo 恢复物资、任务和 timer |
| 四项规则测试 | 未完工制作在死亡时退料、全局守恒、离线伤害有界／EndAt 固定、严格解码与账本上限、未准入拒绝、配置热更不改旧价格／时长 |
| `TestSurvivalExtractionRealCrashRecovery` | 15 条 strict WAL 未投影时 SIGKILL；制作、采集、撤离同时在途；恢复后核对完整状态，再由驱动完工／撤离／一次伤害；下一期限三人撤离；第三进程冷加载后重试准入、全部命令和四人兑付 |

真实恢复入口见[三进程测试](../../scripts/testdata/survival-extraction/persistence/recovery_test.go)。最终四人的永久金币分别为 100／99／98／98：100 初始金币减去每人 5 入场费，再加各自封存清单 5／4／3／3。测试先核对冷事实，再发送重试，避免重复初始化掩盖丢档。

12 项基础测试及真实恢复实际通过 race，无跳过；正式消费者 build/vet、ID 和 `generate --check` 同时验证。测试杀死的只是本用例创建的子进程，不代表磁盘断电、WAL 损坏或提交回复未知的故障注入。

## 问题归属与未覆盖边界

本消费者使用现有 Entity／DAO／Nest／configdata／entitytimer／DataEngine 完成关键后端链路，未确认新增框架缺陷。物资守恒、移动范围、死亡退料、离线伤害暂停、安全区读条优先级、入场费与兑付价格均属业务规则。

尚未接入真实客户端／公网鉴权／接管票签发、匹配与队友补位、建造与开放世界地图、AOI／视野、枪械命中／物理移动／反作弊、PvP／队友伤害、完整技能 Host、局外逐件仓库／装备携入携出、自动结算、长期归档、跨 sid 奖励和权威宿主 HA。这里的四人是同进程 Sender 并发，不是四客户端网络或容量验收。Player 保持静态 sid；跨服通过 Remote／Saga 原服协作，本地 MultiSync 不提供分布式原子性。归档必须保留准入和兑付身份，不能删 Raid 后用同 ID 重开来绕过账本。
