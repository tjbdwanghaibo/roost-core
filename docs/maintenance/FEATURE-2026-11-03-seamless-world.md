# 功能设计：多进程无缝大世界（2026-11-03）

**状态：思路稿（维护者 2026-11-03：“可以先不做”“把这个方案思路先记到文档里”）；同日维护者要求“可以开一个分支先实现一版，并测试一下”，第一版原型只在分支 `feat/seamless-world-prototype` 上（§5 设计、§6 实测），未合入 main，不是现行能力。现行能力以 [MMORPG 接入手册](../framework/MMORPG-INTEGRATION.md) 为准。**

登记在 [DESIGN-TODO“按需求触发的功能”](DESIGN-TODO.md#按需求触发的功能)。

## 0. 问题

开放大世界无缝 MMO 的常见形态：多个 map server 各负责一片区域，玩家在区域间走动不加载、不断线。难点都在区域交界处：

| 难点 | 要回答的问题 |
| --- | --- |
| ① 跨界权威迁移 | 实体从 A 区走到 B 区，写权威怎样从 A 进程交给 B 进程：A 停写、状态交接、B 接管、连接不换或无感换，不双写、不丢最近修改 |
| ② 边界可见性 | A 区边上的玩家要看到 B 区边上的实体：边界带实体实时镜像到邻区（Ghost / 代理） |
| ③ 跨界交互 | 技能、子弹、AOE 打到另一区的目标，由谁结算、时序与失败语义 |
| ④ 分区与负载 | 区域怎样切，热点区域怎样拆分 / 合并 |

## 1. 当前 core 的位置（2026-11-03，v1.40.1）

| 能力 | 状态 |
| --- | --- |
| 同进程多地图，每图一个 Interest，共用一个 Manager 与连接，切图先提交归属再换订阅 | 已提供（[MMORPG §5](../framework/MMORPG-INTEGRATION.md#5-地图切换与断线重连怎样接)） |
| 单进程大地图 AOI / Block AOI | 已提供，容量见 KNOWN-LIMITS“广播与 AOI” |
| Player 按 sid 静态归属，跨服协作走 Saga / Remote | 已提供（[MMORPG §6](../framework/MMORPG-INTEGRATION.md#6-保持-player-归属的跨服协作)） |
| ① 动态跨进程迁移 | **没有**。底层零件：Remote 的 owner / lease / fence 与 `TransferRemoteOwnership`、DataEngine WAL 排空后冷加载、ownerroute；没有串成秒级无感交接 |
| ② 跨进程边界镜像 | **没有**。Interest 只在进程内；Remote Mirror 是给低频共享实体（公会、联盟）的快照复制，延迟撑不住 20Hz 移动 |
| ③ 跨进程实时交互 | **没有**。Nest 多实体事务只在单进程内；跨进程只有 Saga（最终一致，不适合战斗） |
| ④ 世界分区目录 / 动态负载 | **没有** |

另有与本题相关的现行边界：玩家接入层只有 TCP；服务端心跳最短 50ms（≤20Hz）。

## 2. 业界成熟方案

产品现状以记录时所知为准。

| 流派 | 代表 | 做法 | 评价 |
| --- | --- | --- | --- |
| A. Cell 分区 + Ghost | BigWorld（《坦克世界》等）；开源 KBEngine（C++）、GoWorld（Go，支持实体跨 game 进程迁移） | BaseApp 管玩家持久状态与连接，不随地图走；CellApp 管空间模拟，大地图切成 Cell。实体在所属 Cell 是 Real（唯一写权威），靠近边界在邻 Cell 留只读 Ghost 供邻区 AOI；跨界时 Real 迁到新 Cell、旧 Cell 变 Ghost；改别区实体是把消息转发给 Real 所在进程，不做跨进程事务；CellAppMgr 按负载动态挪 Cell 边界 | 最经典、长期商用验证；难点是迁移时的消息时序与 Ghost 一致性 |
| B. 空间数据库 + 多 Worker | Improbable SpatialOS（及 Unreal GDK）、Hadean | 世界状态放在中心化分布式实体组件库；每个组件同一时刻授权一个 Worker 写，Worker 按区域订阅；边界与权威交接由平台负责 | 思路干净，但运维与延迟成本高；Improbable 已基本退出游戏引擎业务，不宜作为选型 |
| C. Server Meshing + 复制层 | Star Citizen | 状态集中在独立复制层，游戏服务器是无状态模拟节点，只认领负责区域的实体权威；先静态网格再动态网格 | A 的现代化版本，状态与模拟彻底分开；工程量巨大、仍在演进 |
| D. 用产品设计绕开 | 多数商业 MMO | 区域间加载点 / 传送门；大陆连续 + 热点按人数分层（WoW 的 layering / sharding）；单分片 + 时间膨胀（EVE：一个星系一个节点，跳跃即加载，过载时减慢游戏时间） | 成熟、风险低；多数“看起来无缝”的 MMO 是“区域内无缝 + 区域间过渡 + 热点分层” |

选型参考：

| 需求 | 建议 |
| --- | --- |
| 同屏几百～一千人，地图可分区 | D：区域内单进程无缝，区域间过渡带，热点分层 / 分线。**roost 现在能做** |
| 大地图连续、跨区实时互动（国战、攻城跨边界） | A：Cell + Ghost；开源实现可参考 KBEngine、GoWorld |
| 超大规模且有专门平台团队 | C 方向，自研成本很高 |

## 3. 用现有 core 的做法（不需要框架改动）

1. **大区单进程 + 切图**：一个大区一个进程，区域间用传送门 / 加载切图。MMORPG 纵切已验证。
2. **伪无缝**：相邻区域放在同一进程，多图共用一个 Manager，切图只换订阅不换连接；进程边界放在天然阻隔处（海、山、城门），越过时切一次图。
3. **分线**：同一张地图开多条线，每条线一个进程，换线即换服。

## 4. 将来要做时的起点：沿 A 流派，Player 不迁移

与 roost 结构最贴近的拆法：

| 角色 | roost 对应 | 说明 |
| --- | --- | --- |
| BaseApp | 现有 Player 进程（静态 sid、DataEngine 持久、连接） | 不动。Player 的“静态 sid”正好是 BaseApp 角色，避开最难的持久权威迁移 |
| CellApp | **新增**空间模拟层 | Cell 内的 Real / Ghost 是**非持久**空间代理实体；跨界迁移的只是空间代理 |
| Ghost 同步 | **新增**低延迟跨进程通道 | 大概率经 NATS 点对点；不用 Remote Mirror（为低频共享实体设计，延迟不够） |
| 跨区交互 | **新增**路由 | 对齐 A 的“消息转发给 Real”，不做跨进程事务；明确时序与失败语义 |
| 分区目录 | 可在 ownerroute 上扩展 | 区域 → Cell 进程映射、扩缩容与迁移 |

实施前要先定的事：目标规模（同屏人数、区域数、跨界频率、跨界交互比例）；空间代理与 Player 持久状态的边界（哪些字段在 Cell、哪些在 Player，Cell 崩溃后从哪里重建）；Ghost 的生灭条件与允许的陈旧度；跨界交互的结算方（目标的 Real）与超时、结果未知的处理；Cell 进程故障时的接管方式。

这是牵涉 Nest、DataEngine、Sync 三块的大改，按 roost-coding 的重构流程先写完整方案再实施。

## 5. v1 原型设计（2026-11-03，分支 `feat/seamless-world-prototype`）

目的：用最小、可验证的一版把 §0 的 ①②③ 串起来跑通，量出交接与 Ghost 的延迟量级，暴露真正难的地方；不追求容量与上线质量。代码在新包 [`gameplay/cellworld`](../../gameplay/cellworld/README.md)，**不改 Nest、DataEngine、Sync 三块的现有代码与行为**。

### 5.1 包放在哪里

`gameplay/cellworld`。理由：它是建在基础设施之上的玩法层空间模拟（类似 `gameplay/movement`、`gameplay/skill`），v1 只依赖 `infra/base/spatial`（块索引与几何）、`infra/network/nats` 的契约接口 `IClient`、`gameplay/heartbeat` 的档位表；不在三大块的包表里，不新增跨块依赖边。没有放进 `framework/sync/`：Ghost 通道不是 Entity Sync（没有 Profile、基线、可靠增量这些语义），放进去会让 Sync 块出现一个与 entitysync 并列、语义不同的同步机制。将来接入 Interest / entitysync 时由本包 import 它们（gameplay → framework 方向本来就允许）。

### 5.2 “实施前要先定的事”的 v1 选择

| 事项 | v1 选择 | 性质 |
| --- | --- | --- |
| 目标规模 | 不定产品目标；测 1×2、2×2 网格，每 Cell 1k / 5k 个移动实体，玩家 5～10 m/s 跨界。数据只说明量级 | 原型简化；上线前按产品给的同屏人数、区域数、跨界频率、跨界交互比例重测 |
| 分区 | 静态二维等大方格：配置给出原点、格边长、行列数与“格 → Cell 进程 sid”映射，所有进程读同一份配置；格内坐标半开 `[min, max)`。不做动态负载、不挪边界 | 原型简化；动态分区（CellAppMgr）上线前另行设计，可在 ownerroute 上扩展目录 |
| 空间代理与 Player 持久状态的边界 | Cell 只持有非持久空间代理：ID、种类（玩家 / NPC）、所属 BaseApp sid、位置、速度、HP、版本。HP 在原型里由 Cell 结算（为了跨界交互有东西可结算），不回写 Player 持久数据 | **上线前必须解决**：哪些战斗字段在 Cell、何时回写 Player（DataEngine），Cell 崩溃从哪里重建 |
| Cell 进程内的执行模型 | 每个 Cell 一个独占 goroutine 的事件循环：消息、心跳、控制请求都串行在这个 goroutine 上，状态无锁；传输回调只解码后非阻塞投进有界收件箱，满了丢弃并计数（交接与伤害靠重传容忍丢失）。心跳周期必须是 `heartbeat.Periods` 的档位，缺省 50ms（20Hz）。**不跑在 Nest 上**，空间代理不是 Nest Entity，没有事务与回滚 | 原型简化；上线前决定 Cell 是否改成 Nest 实体 + 心跳驱动（复用事务、回滚、快池纪律） |
| Ghost 生灭与陈旧度 | Real 的位置落在某邻格“外扩 G 的矩形”内即向该格发 Ghost；每拍发完整状态（不是增量），按目标格合批、分块；离开边界带发显式删除；接收端 Ghost 超过 GhostTTL（缺省 1s）没收到更新就删，删除留墓碑防旧消息复活。要求 G ≥ 视野半径 R（邻区实体在视野内就一定有 Ghost）、G < 格边长（只镜像到相邻格）。允许的陈旧度：一拍 + 传输延迟（§6 实测） | Ghost 全量每拍发是原型简化；上线前改增量 / 分级频率并与 Sync 的 LOD 对齐 |
| 消息顺序 | 每个实体的版本是（epoch, seq）：epoch 是迁移代际，每次交接 +1；seq 每拍 +1、交接时随状态带走。接收端只接受更新的版本，旧消息丢弃。不依赖 NATS 的投递顺序 | 定案方向 |
| 跨界迁移（handoff） | 见 §5.3。迟滞：越出本格超过 H（测量配置取 2m；要求 H < G，且视野半径 R + H ≤ G）才交接，边界附近抖动不来回交接 | 定案方向，参数待调 |
| 跨界交互 | 只做一种：伤害。攻击者所在 Cell 用本地视野（含 Ghost）检查距离，把伤害请求转发给目标 Real 所在 Cell 结算；结算方按 HitID（攻击者 ID + 攻击序号）去重，去重表随交接带走。目标在交接中时返回“重试 + 新 owner 提示”，攻击者 Cell 在期限（缺省 2s）内重试，过期向 BaseApp 报“结果未知”。见 §5.4 | 定案方向；结算方不复核攻击距离（信任攻击者 Cell）是原型简化，上线前要按反作弊要求复核 |
| BaseApp | Player 所在 game 进程（静态 sid）里的 `cellworld.Base`：记住每个玩家当前所在 Cell 与 epoch（只接受更大的 epoch），把移动意图（速度 + 单调输入序号）发给当前 owner；owner 在视野消息里带回已应用的输入序号，落后时 Base 重发最新意图（交接窗口里丢的输入自愈）。Cell 每拍把玩家视野内实体（含邻区 Ghost）整份推给 Base | 原型简化：视野是整份集合，没有接入 Entity Sync / Interest，也没有推到客户端；上线前把视野结果接成 Interest 订阅来源或由 Cell 直推 Gate |
| 通道 | NATS core pub/sub（至多一次，不用 JetStream：空间代理非持久，延迟优先）；每个进程一个收件主题 `roost.cellworld.<world>.<sid>`，消息带类型。经 `infra/network/nats.IClient` 接口，测试用真实 nats-server 与进程内替身 | 定案方向；鉴权（谁能往哪个 Cell 发）上线前补，参考 Gate 的 NATS ACL |
| Cell 故障 | Cell 崩溃后它的 Real 与 NPC 全部丢失（非持久）。Base 对玩家做静默看门狗：超过 SilenceTimeout（缺省 1s）没收到视野，就按最后位置向该位置的 Cell 重新进入，epoch 提示取已知 +1；邻格里旧 Ghost 靠 TTL 消失 | 原型简化；**上线前必须解决**：NPC / 怪物的重建来源、Cell 接管（热备或从 Player 侧重建）、重新进入的防重复 |

### 5.3 交接状态机

角色：实体在某 Cell 里是 **Real**（唯一写权威）、**Frozen**（已发出交接、等结论，只读）、**Ghost**（只读镜像）。每个 Cell 另有一张 `seen` 表：实体 → 本格见过的最高交接 epoch 及是否接受（带 TTL，覆盖重传窗口）。

1. 旧格 O：Real 越出本格超过 H，目标 N = 位置所在格。O 先把自己的记录改为 Frozen、epoch+1（=E），**之后不再写它**（输入丢弃、伤害回“重试”），再向 N 发 `Offer{完整状态, E, 已应用输入序号, 伤害去重表, O 的 Ghost 目标集}`。一个 epoch 只对应一个目标，重传只发给同一个 N。
2. N 收到 Offer：
   - 本地已是该实体的 Real / Frozen 且 epoch ≥ E，或 `seen` 记录的 epoch > E：说明 E 已被接受、甚至已经再交接过 → 回“已接受”（幂等，重复 Offer 不会重装）。
   - `seen` 恰好是 E：按当时的结论原样回复。
   - E 比见过的都新：位置确实在本格、容量未满就安装为 Real（有本地 Ghost 则就地转正），记 `seen`，回“已接受”，并通知玩家的 Base（owner 变更）；否则记“拒绝 E”并回拒绝。
3. O 收到“已接受 E”：位置仍在本格边界带内就降为 Ghost（owner = N），否则删除。也接受两种隐式结论：收到 N 发来的 epoch ≥ E 的 Ghost 更新（只有 E 的 Real 能发出）、收到 epoch > E 的 Offer（实体已经从 N 再交接回来）——都说明 N 接受过 E。
4. O 收到“拒绝 E”：N 承诺以后不会再接受 E，O 以 epoch E 恢复为 Real，退避一段时间后再试（下一次用 E+1）。
5. 超时：O 每 OfferRetry（缺省 100ms）重传；到 HandoffAbort（缺省 3s）仍无结论，O **删除**本地记录（不恢复为 Real，因为无法证明 N 没装），给 Base 发 `Lost{E, 最后位置}`。Base 若已从别的 Cell 收到 epoch ≥ E 的消息就忽略；否则等 LostGrace 后按最后位置重新进入（目标就是 N：N 若已有 Real 则幂等返回；若有存活 Ghost 说明 Real 在别处，拒绝并提示 owner）。

保证与限制：同一 (实体, epoch) 至多一个 Cell 取得 Real；O 先 Frozen 后发 Offer、N 收到后才安装，所以任何时刻至多一个 Real（Frozen 不是 Real）。消息丢失、重复、乱序只会推迟结论，不会双写。代价：N 不可达超过 HandoffAbort 时实体会丢失，由 Base 重新进入补回（非持久状态回到 Base 最后看到的值）。

### 5.4 跨界伤害

`Base.Attack(attacker, target, damage)` → 攻击者 Real 所在 Cell：攻击者必须是本格 Real；目标必须在本格视野里（Real / Frozen / Ghost）且距离 ≤ AttackRange。目标是本格 Real 就地结算；否则登记待决请求，把 `Damage{HitID, 目标, 伤害}` 发给目标的 owner（Ghost 的来源格或 Frozen 的交接目标）。结算方：是 Real 就查去重表（命中则回“已结算”带当前 HP，不再扣）、死亡则拒绝、否则扣血记表；Frozen / Ghost / 不认识则回“重试 + owner 提示”。攻击者 Cell 每 AttackRetry 重发直到收到结论或到期，到期回 Base“结果未知”（可能已结算）。去重表随交接带走；去重窗口必须长于重试期限，表满（窗口内同一目标挨打过多）时拒绝新伤害而不是淘汰旧记录，保证不重复结算。

### 5.5 验收设计

单元（状态机用手动投递的确定性夹具，不起 goroutine）、进程内多 Cell（进程内替身传输可注入丢失 / 重复 / 延迟）、真实 nats-server（私有端口，测试自起自停）上的多 Cell、多 OS 进程（测试二进制自 exec，每个 Cell 一个进程）。全部用例检查：同一 (实体, epoch) 只有一个 Cell 取得 Real、Real 区间不重叠、Base 看到的 epoch 单调、边界带内邻区实体在视野里不连续缺失超过约定帧数、每个 HitID 至多结算一次。测量用 `ROOST_CELLWORLD_PERF=1` 开启。

## 6. v1 实测结果与已知缺口（2026-11-03）

### 6.1 用例与结果

全部在 macOS（Apple M5）本机运行，`GOWORK=off go test -race -count=1 ./gameplay/cellworld/` 通过；真实 NATS 与多进程用例要求 PATH 里有 nats-server（实测 2.14.5），测试自起私有端口、用完杀掉。

| 层 | 用例 | 断言 |
| --- | --- | --- |
| 单元 | 分区映射、边界带、迟滞、版本序；交接正常 / 丢 3 次 Offer 与 2 次回复且其余全重复 / 回复全丢靠 Ghost 隐式确认 / 超时只删不恢复（扣住的 Offer 迟到后对方安装，仍只有一个 Real）/ 超时后重进取下一 epoch / 被拒恢复并用下一 epoch 重试 / 回复扣住时来回越界 / 斜穿四格交接链 A→B→D；Enter 幂等与“有存活 Ghost 就拒绝”；Ghost 旧版本与墓碑 | 每次身份变化时同步检查：同一实体同一时刻至多一个 Real、同一 (实体, epoch) 只取得一次、epoch 严格递增。这批用例先对空桩运行，14 条红（记录在提交 7c71f762 之前的本地运行），实现后转绿 |
| 单元 | 跨格伤害：全部消息重复 + 同一攻击重发；距离 / 可见性拒绝；目标冻结期间“重试”改投新 owner；结算后回复丢失、目标迁走，去重随交接带走；回复全丢报“结果未知”；去重表满拒绝 | 每个 HitID 至多扣一次血；Applied 都真的结算过；Unknown 可能已结算 |
| 进程内多 Cell | 直线穿越、边界抖动（12 次交接）、斜穿四格交点、跨界攻击、Offer 延迟 300ms 的攻击、Cell 间 20% 丢包 + 10% 重复 + 0～30ms 乱序下的来回穿越与攻击、Cell 崩溃后重进 | 视野连续：静止 NPC 在视野半径内时连续缺失 ≤ 1 帧（实测 0），视野间隔 ≤ 3 拍 + 50ms（实测最坏约 100ms，即交接时 2 帧）；至多一个 Real；伤害只结算一次。两处变异（确认后不降级 / 不发 Ghost）分别让断言变红，说明断言能抓到双写与边界不可见 |
| 真实 NATS | 同一组场景（直线、抖动、斜穿、攻击），每个 Cell 与 Base 各一条连接 | 同上，约定放宽一拍（缺失 ≤ 2 帧、间隔 ≤ 4 拍 + 50ms）；实测缺失 0、最坏间隔约 101ms |
| 多 OS 进程 | 两个 Cell 各一个子进程（测试二进制自 exec），父进程跑 Base：来回穿越后跨界攻击 10 次 | 子进程 pid 互不相同且不是父进程；按各进程事件的系统时间排序后跨进程至多一个 Real、epoch 单调（玩家最终 epoch 3）；10 次伤害各只在一个进程结算一次，最终 HP 70 |

### 6.2 测量（原型量级）

2×2 网格、每格一个 OS 进程、单机 nats-server；40 个玩家经 Base 随机游走，NPC 3 m/s 随机游走、跨格交接。20s 窗口、两轮，原始输出与环境见 [benchmarks/cellworld-v1-20261103](../performance/benchmarks/cellworld-v1-20261103/README.md)。下表是两轮、四格的范围：

| 每格 NPC | 每格 CPU（一个核） | 每拍耗时 p50 / p99 | 每格出 / 入 | Ghost 延迟 p50 / p99 | 交接延迟 p50 / p99 | 视野延迟 p50 / p99 | 窗口内交接 / 格 |
| ---: | ---: | --- | --- | --- | --- | --- | ---: |
| 1k | 4.3～6.3% | 0.9～1.6ms / 2.5～4.6ms | 230～370 msg/s、310～400 KB/s / 70～75 msg/s、210～260 KB/s | 1.5～1.9ms / 3.8～5.9ms | 1.6～2.3ms / 2.8～14.8ms | 0.22～0.28ms / 1.1～1.2ms | 60～79 |
| 5k | 13.4～17.1% | 3.7～5.1ms / 5.9～9.8ms | 295～438 msg/s、1.4～1.7 MB/s / 137～141 msg/s、1.1～1.2 MB/s | 2.6～3.5ms / 6.0～6.7ms | 4.5～6.0ms / 6.3～11.1ms | 0.15ms / 0.74～0.77ms | 330～356 |

两档都没有被拒、放弃、重传、收件箱丢弃或 Real 冲突。观察：

- **交接延迟 ≈ 目标格一拍的处理时间**：Offer 在目标格的事件循环里排在正在执行的 tick 后面，5k 时 p50 从 1k 的约 2ms 涨到约 5ms，跟每拍耗时同步增长。格子更大时需要把 tick 切片或让交接消息优先。
- **带宽主要是全量 Ghost**：5k 时每格约 600 个实体在边界带里，每拍每个邻格发一份完整状态，出流量约 1.5 MB/s/格；增量与按距离降频是第一个要做的容量优化。
- **玩家视野在交接时最多断 2 帧**（旧格最后一份视野到新格第一份视野约 100ms），没有实体从视野里缺失；这依赖 R + H ≤ G（构造时校验）。
- 这是同机数据：Cell 与 nats-server 在同一台机器、进程间走本机回环，跨机的网络延迟要另测。

### 6.3 已知缺口

| # | 缺口 | 性质 |
| --- | --- | --- |
| 1 | Cell 不跑在 Nest 上，空间代理不是 Nest Entity：没有事务与回滚、没有快池纪律，战斗逻辑一复杂就需要 | **上线前必须解决**：定 Cell 的执行模型（Nest 实体 + 心跳驱动，或保持事件循环但定义自己的事务边界） |
| 2 | 视野只到 Base，是整份集合；没有接 Entity Sync / Interest，也没有推到客户端 | **上线前必须解决**：视野接成 Interest 的订阅来源，或 Cell 直推 Gate |
| 3 | HP 等战斗状态只在 Cell、非持久，不回写 Player；交接放弃（3s）时回到 Base 最后看到的值 | **上线前必须解决**：定字段归属与回写时机 |
| 4 | NPC / 怪物在 Cell 崩溃时全部丢失，没有重建来源 | **上线前必须解决** |
| 5 | Cell 进程没有单实例保证：同一 sid 起两个 Cell 进程会出现两个 Real；故障检测只靠 Base 的视野静默看门狗（1s） | **上线前必须解决**：按“App 级统一保证”，Cell 进程继承 core app 的单实例锁与 fail-stop，不在本包另做租约 |
| 6 | 玩家 Real 没有租约：Base 进程消失后，Cell 里的玩家 Real 一直留着 | **上线前必须解决** |
| 7 | 重进防重复只有一道：目标格有存活 Ghost 就拒绝。视野与 Ghost 同时丢失超过宽限期时理论上能造出第二个 Real | **上线前必须解决**（例如重进前向邻格 / 目录确认） |
| 8 | 结算方不复核攻击距离（信任攻击者所在格）；没有鉴权，任何连上 NATS 的进程都能往 Cell 主题发 Enter / Damage | **上线前必须解决**：复核与 NATS ACL（参考 Gate 的 ACL 用例） |
| 9 | 伤害去重窗口按各 Cell 的系统时间过期，跨机时依赖时钟一致 | **上线前必须解决**（跨机前改成相对时间或拍号） |
| 10 | 线上格式没有版本号 | **上线前必须解决** |
| 11 | Ghost 每拍全量、没有增量 / LOD | 原型简化（容量阶段第一项） |
| 12 | 交接延迟随目标格每拍耗时增长 | 原型简化（容量阶段：tick 切片或交接优先） |
| 13 | 静态分区，一个进程一个格，没有动态负载 / 热点拆分 | 原型简化 |
| 14 | 攻击只能打本格视野里的目标（R ≤ G）；远程、大范围 AOE 没有设计 | 原型简化（需要单独设计跨多格的范围交互） |
| 15 | Leave 尽力而为；配置不经 `app.LoadConfig` 声明 | 原型简化 |
| 16 | 测量是单机、20s 窗口、两轮；没有跨机、长稳、10k 以上 | 原型简化 |

### 6.4 对后续方向的判断

- 交接协议本身（先冻结后 Offer、epoch 唯一目标、幂等回复、隐式确认、只删不恢复）在丢失 / 重复 / 乱序 / 来回 / 链式交接下守住了“至多一个 Real”，协议规模小（`handoff.go` 约 160 行），可以作为正式实现的起点。
- 真正的工作量不在交接，而在 6.3 的 1～6：Cell 的执行模型与事务、战斗状态的持久归属、NPC 重建、Cell 单实例与故障接管。它们都牵涉 Nest / DataEngine / App，建议下一步先由维护者定 1（Cell 是否上 Nest）与 3（战斗字段归属），再决定是否进入正式方案。
- 如果产品需求是 §2 的“区域内无缝 + 区域间过渡 + 热点分层”，现有 core 已能做，不需要这条线；只有大地图连续且跨区实时互动是硬需求时才值得继续。

## 7. v2 设计：分区 Scene（2026-11-03，分支 `feat/seamless-world-prototype`）

维护者 2026-11-03 的决定：BigWorld 的 Cell 是 **Scene 的一种模式**（不另造“世界”）；角色在分区 Scene 里的部分是不持久的 Nest 实体 **SceneActor**，心跳驱动；战斗字段（位置、HP、Buff、冷却）放在 SceneActor 上随交接走，Player 留在原 sid、固定时机回写；Cell 崩溃按 Player 最近快照重建，Monster 按刷怪配置重建；不用 Remote 托管 + `TransferRemoteOwnership`（交接延迟太高）。补充（同日）：分区模式新增的状态尽量放进 Nest 实体（Scene、SceneActor、Monster），用心跳与事务驱动，**不新增带锁的进程级管理器**；现有管理器保持原样，普通 Scene 行为不变。

本节先写形状与取舍，代码按 §7.9 的阶段落地，实测与剩余缺口见 §8。

### 7.1 两种 Scene，一套成员接口

| | 普通 Scene（缺省，行为不变） | 分区 Scene（声明了分区配置） |
| --- | --- | --- |
| 进程 | 一个进程 | 每格一个进程（Cell 进程），格 → sid 静态映射（v1 的 `Grid`） |
| 角色在场景里的代表 | Player 实体本身是成员（Interest subject，AOI 跟随它） | Player 所在格的 **SceneActor**（不持久 Nest 实体，ID = `BuildEntityID(玩家 uniqueID, KindSceneActor)`，跨格交接 ID 不变）；Player 只留在 Base 进程 |
| NPC | Monster（ephemeral） | Monster 是所在格的 Real，跨格交接 |
| 客户端看到什么 | 本进程 Manager 里的 Player / Monster subject | Base 进程 Manager 里的 **mirror subject**（§7.3），内容来自玩家当前所在格的 witness |
| 成员接口（业务调） | Join / Ready / Leave / Move / SetView / 攻击 | 同名接口；分区模式下由服务层桥把调用转成发往当前 owner 格的消息（§7.6） |

“Base 进程”就是 Player 绑定的 game 进程（静态 sid，DataEngine、连接都在这里，v1 的 BaseApp）。Cell 进程也是普通 game 进程（同一个二进制，配置里声明它托管哪个格）：它可以同时是一些玩家的 Base。

### 7.2 Sync 补的能力：远端只读对象作为 subject（mirror）

**问题**：分区模式下客户端连接在 Base 进程，视野内容的权威在 Cell 进程（本格 Real + 邻格 Ghost）。Base 要把这些“不是本进程 Nest 实体”的对象，和本进程的 Player 实体一起，经同一个 Manager、同一条连接推给客户端：全量 / 增量 / 删除，按 (epoch, seq) 丢旧（玩家交接时旧格和新格的 witness 会交错到达），收不到更新就过期删除。

**现有抽象**（读源码确认，codebase-memory 本次连不上）：Manager 的 subject 是 `*entity.SubjectSyncState`——“不含观察者的内容状态机”，`entity.NewSubjectSyncState` 是公开构造，不绑实体时 `withEntityLock` 直接执行 packer、`commitGate` 为空即就绪、`MarkDirty` 直接标脏并唤醒；`Manager.Register / Unregister / RegisterAfterRetirement` 只看状态和 ID；Interest 的 `Show / Move / Hide` 只认 subject ID 与坐标。也就是说 **Manager 与 Interest 本来就不要求 subject 是 Nest 实体**，缺的只是“谁来维护这份远端内容”的那一层：版本序、墓碑、过期、按版本决定全量还是增量、登记 / 显示 / 移动 / 退役的固定顺序。

**最小改动点**：在 `framework/sync/entitysync/policy` 新增一个文件 `mirror.go`（类型 `Mirrors[T]`）组合现有公开 API，**不改 Manager、Interest、SubjectSyncState 的任何现有代码**：

```go
type MirrorVersion struct{ Epoch, Seq uint64 } // 字典序；epoch 是迁移代际，seq 跨 epoch 单调

type MirrorConfig[T any] struct {
	Manager  *entitysync.Manager
	Interest *Interest                    // 空间可见性：mirror 是 Show 进 AOI 的目标
	Position func(T) spatial.Point
	Snapshot func(T, entity.SyncProfile) (entity.FrozenSyncPayload, error)
	Delta    func(T, entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) // 可空：只发全量
	TTL      time.Duration                // 超过它没有 Apply 就删除（发 remove）
}

type MirrorItem[T any] struct {
	ID        int64; Namespace string; Kind uint32
	Version   MirrorVersion
	Content   T
	Mask      uint64 // 相对上一个已接受版本改了哪些字段；0 = 全量
}

func NewMirrors[T any](MirrorConfig[T]) (*Mirrors[T], error)
func (m *Mirrors[T]) Apply(item MirrorItem[T], now time.Time) MirrorOutcome // Created / Updated / Stale / Tombstoned
func (m *Mirrors[T]) Remove(id int64, ver MirrorVersion, now time.Time) bool  // 显式删除，留墓碑
func (m *Mirrors[T]) Expire(now time.Time) int                                 // 过期清理，返回删除数
func (m *Mirrors[T]) Get(id int64) (T, MirrorVersion, bool)
func (m *Mirrors[T]) Close()
```

语义：

- 新对象：建一个不绑实体的 `SubjectSyncState`（packer 读 mirror 当前内容），`Register`（同 ID 还在退役就 `RegisterAfterRetirement`，remove-before-create 不变）→ `Interest.Show`。顺序与 demo `ShowSubject` 相同（先登记再显示）。
- 更新：版本不新于已接受的丢弃（计数），新的替换内容；`Mask == 0` 或配置没给 `Delta` → `MarkFullDirty`，否则 `MarkDirty(mask)`；位置变了 `Interest.Move`（直接移动，不走提交门：mirror 没有事务）。增量的正确性前提是 mask 相对 **上一个被接受的版本**；跳过了中间版本（丢消息）时调用方要给全量——cellworld 的 witness 每拍是全量，不靠增量链（§7.5）。
- 删除 / 过期：`Interest.Hide` → `Manager.Unregister`（订阅者下一帧收到 ObjectRemove）；墓碑保留 2×TTL，不新于墓碑版本的迟到更新不会让对象复活。
- 锁：一把短锁保护 mirror 表（与 Interest 登记侧同类），调用 Manager / Interest 都在锁外按 ID 串行（同 ID 的 Apply / Remove 不交错）；可以在 Nest 快池上调用（不等待、不做 I/O）。

**为什么不另开第二个 Manager**：(1) 客户端一条连接一个 Sync 流（`frame.Receiver` 一份基线），两个 Manager 就是两条流、两套基线，客户端要合并；(2) 同一个对象可能先是 mirror、后来又是本进程实体（或反过来），同一 Manager 里是同一个 subject ID，跨 Manager 只能 remove + create，视野不连续；(3) 每会话每拍一帧、冷创建预算、慢会话规则都按 Manager 计，拆成两个就各算各的；(4) `ClaimSyncOwner` 已经规定一个内容状态只喂一个 Manager，mirror 与 Player 放在同一个 Manager 里不需要例外。

**不做的**：不在 Cell 进程上给 Ghost 建 subject（Cell 进程没有这些客户端）；不改 entitysync 的帧格式、Profile、预算。

### 7.3 Cell 侧：状态全在 Nest 实体上

| 状态 | 放在哪 | 驱动 |
| --- | --- | --- |
| 本格区域表：邻格 Ghost（只读镜像，不是实体）、交接 `seen` 记录、墓碑、跨格伤害待决表、本格 Real 的位置索引 | **Scene 实体**持有的 `cellworld.Region`（手写 DAO：事务内的修改登记逆操作，回滚即还原；与 `skill/combatcomponent.CombatDao` 同形） | Scene 的 `scene.cell` 心跳（50ms）+ Scene 上的 Nest handler |
| 角色 / NPC 的格内状态：位置、速度、HP、Buff、冷却、角色（Real / Frozen）、epoch / seq、已应用输入序号、伤害去重表、交接中的目标与 Offer | **SceneActor / Monster 实体**的 DAO（nopersist） | 实体自己的 `cell.actor` 心跳 + 实体上的 Nest handler |

框架包 `gameplay/cellworld` 不定义实体（实体由生成器与 game 定义），只提供：

```go
// 实体实现它：读 / 写自己的格内状态（写经 DAO setter，登记 undo 与同步脏标）。
type Actor interface {
	entity.IThreadSafeEntity
	CellState() ActorState
	SetCellState(ActorState)
}
// Region 由 Scene 实体持有；只在 Scene 的锁内调用。
func NewRegion(RegionConfig) (*Region, error)
// 心跳回调与 handler 体（game 在自己的 Nest handler 里调用这些函数）：
func (r *Region) Beat(now time.Time)                       // 刷新索引、发 Ghost 批、过期、伤害重试、算 witness
func (r *Region) Receive(m *Message, now time.Time) Effects // Ghost / Offer / 回复 / 伤害 / 进入
func ActorBeat(a Actor, g *Grid, now time.Time) ActorEffects // 积分、判交接、冻结、重传、到期放弃、租约到期
```

**执行模型**：

1. 传输回调只解码，按目标投 Nest：发给角色的（Input、OfferReply、Damage、Lease）→ `DispatchMulti(handler, [sceneID, actorID])`；发给区域的（Ghost 批、Offer、Enter、DamageReply）→ `Dispatch(handler, sceneID)`。全部 handler 都先锁 Scene（World 档 2）再锁角色（Other 档 5），锁序固定。
2. 角色心跳只锁角色本身：积分位置、判交接、冻结（epoch+1）——它不能反过来锁 Scene（违反锁序），所以“我在哪、我冻结了”这类给区域的报告在 **AfterCommit** 里投进区域的有界收件队列（无锁的 channel，满了丢弃计数；下一拍重报），由 Scene 心跳 / 下一次 Scene handler 在 Scene 锁内取出应用。Ghost 批在 Scene 心跳里组好、AfterCommit 发出。
3. 外发（Publish）一律在 AfterCommit：回滚的事务不外发。

**代价（§8 实测）**：每个角色每拍一笔 Nest 事务（v1 是一个 goroutine 里的循环），Ghost 与 witness 晚到至多一个 Scene 心跳（≤ 50ms）；交接 Offer 在目标格是一次 Scene handler，不再排在 tick 后面（v1 缺口 12 反而变好）。

### 7.4 交接以 Nest 事务为边界

- 旧格 O：角色心跳发现越界（迟滞 H）→ 同一笔事务里 `role = Frozen, epoch+1`，Offer 编码进 DAO；**提交后**才发 Offer（AfterCommit）。事务回滚 = 没冻结 = 没交接。冻结期间心跳按 OfferRetry 重传、到 HandoffAbort 放弃（销毁角色，给 Base 发 Lost）。
- 新格 N：Offer → Scene handler：查 `seen`、本格是否已有该 ID 的实体、容量；接受则在**同一笔事务里**经 `EntityManager.Create`（handler 内新建走 `CreateInScope`，由事务捕获：回滚 / 拒绝即撤销发布）建出 SceneActor / Monster 并写入状态（Real，epoch = E），`seen[E] = 接受` 也在这笔事务里（Region 手写 DAO，回滚还原）。**提交后**才回“已接受”、通知 Base。事务失败 → 记 `seen[E] = 拒绝`（另一笔事务）并回拒绝，旧格恢复 Real。所以“交接提交成功才算移交；回滚不移交”。
- O 收到“已接受 E”（或隐式确认：N 发来 epoch ≥ E 的 Ghost）→ 角色 handler 事务里确认冻结的 epoch 仍是 E，标记已移交；提交后销毁实体（`EntityManager.Destroy`，不在业务 worker 上等：交给 Region 的销毁队列，在 Scene 心跳之外的一个 goroutine 上执行，与 demo spawner 同样的“决定在事务里、建 / 毁在 worker 外”），区域在 Ghost 表里留一份最后状态（仍在边界带内时）。
- v1 的协议不变量（先冻结后 Offer、一个 epoch 一个目标、幂等回复、隐式确认、超时只删不恢复）原样保留；变化只是每一步落在哪笔事务里。

### 7.5 视野：witness → Base 的 mirror

Cell 的 Scene 心跳给本格每个玩家 Real 算 witness（半径 R_feed 内的本格 Real + Ghost，含自己），发给该玩家的 Base。Base 收到后在 Player 的 Nest handler 里：只接受 epoch 不回退的 witness，把每个对象 `Mirrors.Apply`（版本 = 对象在 Cell 上的 (epoch, seq)，内容 = 位置 / HP / 种类等客户端可见字段，全量）。客户端的可见集合由 Base 的 Interest AOI 按 mirror 位置决定（玩家主视野跟随自己的 SceneActor mirror），要求 R_feed ≥ Base 的离开半径，对象离开视野之前位置一直在更新；不在任何 witness 里的 mirror 按 TTL 过期删除。交接时旧格、新格的 witness 交错到达，按对象版本取新的，视野里的对象不会 remove 再 create。

### 7.6 Base 侧：租约、回写、重建

- 玩家的格内路由状态（当前 owner sid、epoch、输入序号、攻击序号、最近快照版本）放在 **Player 实体的 nopersist 字段**，Base 的看门狗是 Player 的心跳（不再是 v1 `Base` 里带锁的玩家表）。
- **租约**：Player 心跳每 LeasePeriod 给 owner 格发 Lease（顺带重发落后的输入）；SceneActor 心跳发现 `now > leaseUntil`（LeaseTimeout，缺省 3s）就自行离场销毁。Base 进程消失后 SceneActor 在 LeaseTimeout 内被清理（v1 缺口 6）。
- **回写**：SceneActor 在脱战、离开场景、交接放弃、战斗中每 N 秒（缺省 5s）把战斗字段快照（HP 等）+ 版本 (epoch, seq) 发给 Base；Base 在 Player 的 Nest handler 里只接受比已存版本新的快照，写 Player 的持久字段（正常 DataEngine 落库）。幂等：同一版本重复到达不再写。
- **重建**：Enter 带 Player 的最近快照；Cell 崩溃后 Base 静默看门狗重进时新格按快照建 SceneActor（最多丢一个回写周期的战斗状态）。Monster 由新起的 Cell 进程按刷怪配置重建。

### 7.7 App 级保证

Cell 进程是普通 roost App：`App.Singleton(kitredis 的 SingletonStore)`，`singleton.enabled=true`。同 sid 起第二个 Cell 进程在 startup_wait 内拿不到锁即 `ErrSingletonHeld` 启动失败；失锁 fail-stop。cellworld 不再自己做租约 / 单实例判断（v1 缺口 5）。

### 7.8 game-demo 进程内管理器在分区模式下的处理

| 管理器 | 文件 | 管什么 | 锁 | 与 Nest 事务的关系 | 分区模式 |
| --- | --- | --- | --- | --- | --- |
| Scene 桥 `Scene` | `internal/service/game/scene.go.tmpl` | entitysync Manager + Interest；成员表 `members`（Join 代际、ready、opening）；队伍视图 `teams` | `transitions`（Join / Ready / Leave / settle 串行）、`teamMu`（队伍视图）、`mu`（成员表） | 不在 Nest 锁内持有；`MoveSpace` 把移动作为提交门事实排进 Interest | 一分为二：**(b)** 客户端可见性、成员表、队伍视图属于玩家——留在 **Base** 进程，照旧是这一个桥（Manager 里加 mirror，§7.2）；**(a)** Cell 进程不需要这个桥（Cell 上没有客户端会话），格内可见性由 Region 的索引负责 |
| 队伍视图 | 同上 `SetTeam` / `teams` | 匹配成功的两人互相可见 | `teamMu` | 无 | **(b)** 留在 Base；队友若在别的 Base 进程、或不在本玩家 witness 里，跨格 / 跨进程的队伍可见用 v1.34 的 `GroupView` / `selfsync.Groups` 从 Base 推（本轮原型不接，§8 缺口） |
| `PlayerOwners` | `playerowner.go.tmpl` | 本进程为哪些玩家服务（驻留表）、写准入、闲置卸载 | `mu` | Admit 在 Nest 业务池上做一次加锁读写 | **(b)** 留在 Base 不动；Cell 进程不托管 Player，不需要它 |
| spawner | `spawner.go.tmpl` | 把 Scene 刷怪节拍的决定变成 Monster 实体 | `mu`（monsters 表）、`busy` | 决定在 Scene 心跳事务里，AfterCommit 交给它；建实体在 worker 外 | **(a)** 每个 Cell 进程一份，只刷本格（刷怪表按格过滤：刷怪点在本格矩形内才刷）。跨格游走的 Monster **(c)** 走交接，交接后在新格的 Monster 由新格的 Region 管，spawner 只记本格生出来的；被交接走的从 spawner 表里移除（经 Region 的通知，不加跨进程锁），在别的格死亡时由那个格的 Refresh 记账——**原型简化**：跨格死亡不回原格补刷，§8 缺口 |
| Refresh（刷怪表计数） | `game/scene/runtime/refresh.go.tmpl` | 每组该有几只、已有几只 | `mu` | Scene 心跳读 Due，spawner 报 Spawned / Died | **(a)** 每格一份 |
| Terrain / PathFind | `game/scene/runtime/terrain.go.tmpl`、`pathfind.go.tmpl` | 地图格子与占位、寻路 | `RWMutex` | 心跳内读 | **(a)** 每格一份（同一张地图，每个进程整张加载；占位只对本格 Real 有意义）。跨格寻路只算到本格边界，原型不处理跨格占位 |
| `actor` 包级变量 | `game/actor/actor.go.tmpl` | 心跳驱动、Lookup、WorldTime、Ground | 原子指针 | — | **(a)** 每进程一份，不变 |
| 战斗（lockstep 对战房间） | `battle.go.tmpl`（及 matchmaker） | 匹配成功后的帧同步对战房间 | `mu`、房间 goroutine | 不在 Nest 内 | **不属于三类**：它不是场景内战斗，是独立房间，和分区无关，保持原样（在匹配它的进程里开房） |
| 场景内战斗（攻击 / 伤害） | v1 `cellworld` combat | 攻击者格做前置检查，目标 Real 结算，HitID 去重 | — | — | **(c)** 不加跨进程锁，统一“转发给目标 Real 结算”，去重表与 HP 落在 SceneActor / Monster 的 DAO 上 |

分类后没有发现必须在分区模式下重写的管理器：Scene 桥在 Base 侧原样使用（只多一类 subject），Cell 侧不实例化它；成员进出串行化（`transitions`）只管 Base 上的会话与订阅，交接事务在 Cell 上，两者不交叉。**新增的分区状态全部在 Scene / SceneActor / Monster / Player 实体上**，进程级只增加：Region 的收件 channel 与销毁队列（无锁的单消费者队列，不是带锁的管理器）、Sync 的 `Mirrors`（Sync 框架组件，锁的性质同 Interest 登记侧）。

### 7.9 实施阶段与验收

1. **A. Sync mirror**（`policy/mirror.go`）：单测先对空桩跑红、实现后转绿；全部 `framework/sync/...` 测试 + `-race`。
2. **B. cellworld v2**（框架）：`Region` / `Actor` / `ActorBeat`；v1 的确定性用例按新执行模型改写（断言含义不变）；新增真实 Nest 引擎 + EntityManager + 心跳驱动的进程内多格用例与两 OS 进程用例（NATS 私有端口、Redis 私有端口给单实例锁）；Base 侧 mirror 推给测试客户端（`frame.Receiver` 解帧）检查视野连续。
3. **C. game-demo 分区配置**：Scene 支持分区、Monster 交接、Player 经 SceneActor；缺省不分区，`scripts/test-demo-generated.sh` 等全部通过。
4. 每阶段测试通过即提交并推分支；某一步发现方向问题先停下来写进汇报。
