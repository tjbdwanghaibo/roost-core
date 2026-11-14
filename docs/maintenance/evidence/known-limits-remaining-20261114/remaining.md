# KNOWN-LIMITS 遗留收口状态与原始计划

> 下方“全表”、各类数量与 B 原方案是 `28055f06` 的历史基线，用于追踪 No.，不是当前未完成清单。当前状态以本节及 KNOWN-LIMITS 为准；M 运行参数有明确更正，C 以现行外部清单为准。

- 依据：`docs/maintenance/KNOWN-LIMITS.md` 第 617～873 行，分支 feat/v147，HEAD `28055f06`。
- 对照：旧归类（本机 scratchpad，未入库）（HEAD `067c14f7`，200 行）、`decisions.md`、`git log 067c14f7..HEAD`（106 个提交）、CHANGELOG `[Unreleased]`、“三、已修复的历史”2026-11-13/14 行，以及当前源码 / 脚本 / 文档（逐条核对，不照抄旧归类）。
- 原始分类时 codebase-memory-mcp 连不上（连接关闭），结构核对用源码 + grep；没有跑任何测试或压测。
- 计数口径：顶层 bullet、编号条、能力表每一行、只有段落没有 bullet 的小节各算一条，共 **114 条**；带子 bullet 的父条把子条拆成 `.1/.2/.3` 行（另 5 行）。只做导航的引言段（L643、L678、L703、L705、L752、L764、L825、L835、L853、L859）不计，所在小节清空后随之删除。
- 类别：A 移到“一”；B 本机可修；C 外部环境；D 维护者决定；E 未确认偶发；M 本机空闲时测；F 过时 / 已解决；**IP** 另一个 agent 正在做（只标记）。混合条目写成“A + C”等，各部分的去向分别给出。
- A 的现行去向见 §A，B 的原方案见 §B，C 见 §4，D 见 §D 与 §5，E 见 §E，M 见 §3，F 的证据写在表里。

## 当前收口状态（2026-11-14）

本机功能实现 / 演练与正式性能、外部环境、维护者决定分开判定。A 的 31 段已迁入 [KNOWN-LIMITS 一](../../KNOWN-LIMITS.md#一框架本身的限制)，F 的 17 项按下表收敛；B 的结果如下。**M-1～M-19、E-1 / E-3 正式测量均未运行**，B-2 大形状成本与 B-3 checkpoint 实验不由功能冒烟关闭。E-2 完成原版 200 轮调查，实际 max load10.39，未复现不是修复。C 的 36 项继续待外部环境；D 与 N1～N4 原样保留未答。

| B / 对应 No. | 本机结果与证据 | 仍保留的边界 |
| --- | --- | --- |
| B-1 / 11、61、65、68 | [P1 换服](../rehome-20261114/README.md)：嵌入式玩家 TCP 上六个 SIGKILL 点各两次、checkpoint 负对照，不含独立 Gate；account / 玩家入口唯一归属，资产、计数、WAL 恢复 | committed 后源库旧档明确 KNOWN_LIMIT，源服入口归属检查拒绝（P1 为 player_elsewhere），不保证 Resume 清理；旧内存 / 定时器权威写未全部隔离，Q4 与人工恢复保留；非跨机 |
| B-2 / 12⑥、15.2 | [联盟探针](../../../performance/benchmarks/alliance-probes-20261114/README.md)：真实成员 ≤500、合成 1k/5k/10k、内存/命令计数与精确嵌套 setKey；RPC 1..256 参数修正和 c4 功能实跑 | M-11 / 正式大形状成本 pending；heap 不是 RSS，空 L1 不是冷 Mongo；Found 固定 1/16，变量只作用于 Info/Join；PASS 要另核 failed/slow |
| B-3 / 14 | 后台 checkpoint 实验仅在 `experiment/v147-background-checkpoint`（`300be3a0`） | 不合实现/配置；M-19 off/on 对照 pending，全体超时根因未确认 |
| B-4 / 31① | [MOBA](../v147-moba-20261114.md)：分进程权威 / 宿主、快照与交接、已提交头门、旧宿主隔离及变异 | 下游记录器；非真实客户端/跨机 HA/正式负载容量 |
| B-5 / 114 | [合作/经营](../session-real-20261114/README.md)：各两个 WAL-only SIGKILL 窗口，原 WAL 恢复、第三冷进程领奖幂等；丢 WAL 变异红 | Sender 真实存储，不含 TCP/account/Gate/NATS 联合链；与默认 TCP 分开统计 |
| B-6 / 40、46～50 | [登录](../v147-login-20261114.md)：双 account/game/Gate、重启重连、真实 naming、casual 购买与 self_sync 解码，两轮与丢推送变异 | naming 完整故障阶段、Redis 故障、生产渠道/跨机/长稳未验；时间推送先后只是观察；非 WAL-only |
| B-7 / 38、54、56 | [社交](../social-real-20261114/README.md)：两 game 共享 Team+toxiproxy、匹配 queued SIGKILL/重试、联盟双 sid handed-off/committed SIGKILL；漏装 OnTeamJoining 红绿与变异；当批模块 594/594，联盟脚本 11/11 | 排队取消后先 Clear、unknown 不放行；不改跨 sid 匹配，Q4/#154 未关；基础 alliance55 不是 594 |
| B-8 / 34、52、53 | [运营](../operations-real-20261114/README.md)：三条真实绿、两项变异红/复绿；GM HTTP 邮件+rehome、purchase 跨 sid/Redis AOF 重启、活动权威迁移+SIGKILL | 非 WAL-only；GM/purchase 在完整 demo，活动在由 casual 扩展的共 18 模块工程；PlayerDao64 字段限额不变；真实支付/外部奖励/长稳/容量未验 |
| B-9 / 36、39、48 | [Room/Turn](../turn-real-20261114/README.md)：真实 portal/account→match→Room/Turn、Gate/NATS/Ingress、SIGKILL、旧 Epoch 拒绝与 timeout 只补一次；demo Robot/dev-smoke；另有正常停机 timer 档 | 两档各自范围；非 WAL-only/跨机；Q13 后续保留 |
| B-10 / 73④ | [地形确认帧](../terrain-confirm-source-20261110/README.md)：Go entitysync 真实帧→.NET EntityViews/TerrainChunkSync 已通过 | 不经 TCP/Gate/demo Interest，Unity 仍 C-U2，坏块 N4 未答 |
| B-11 / 74 | [地形 WAL](../terrain-wal-crash-20261114/README.md)：四块笔刷/两块建筑，strict 确认且 Mongo 空后 SIGKILL，版本/hash/占位/账本/幂等，丢 WAL 红 | 不含网络同步送达/跨机/容量；T09/T10/Unity 等仍保留 |
| B-12 / 76 | CellWorld Runtime 六场景已合（Start/Stop、失败复位），见 [模块说明](../../../../modules/cellworld/README.md) | AddIngress 失败分支仍缺用例，业务 Entity 适配仍在 demo |
| B-13 / 77 | 运行期序号上限`2^33-1`及边界回归已合；P10 删除 game-demo 未使用的重复分配器 | 不把编号修复当容量证明；最终 monster 场景计数随统一矩阵报告 |
| B-14 / 86 | [C++跨 Gate](native-acceptance.md)：独立 C++客户端真实 TCP/NATS，两 Gate 同 Session 迟到 Bind、两绿/变异红 | Gate/Game 同 Go 测试进程；非跨机/Unity/设备全矩阵 |
| B-15 / 87 | [C++ protobuf](native-acceptance.md)：libprotoc36.2，8/8 CTest | 与 E-2 弱网偶发调查分开 |
| B-16 / 103、112 | [GeneratedTCP](../v147-generated-tcp-20261114.md)：正式夹具入库，12 个 10s 配置与开环冒烟，丢 DAO 变异红 | M-17 四档正式测量 pending；客户端/服务模块同一测试进程，不是旧 10 服务 Main 拓扑 |

M-13 的 [Cluster 探针修复](../mail-cluster-probe-20261114.md)已合：仅 integration helper 装配地址，单机 / Cluster 四子项以 1x 通过，保留红绿对照；不计为正式性能。

本轮代码工作与功能矩阵已完成，准确计数由[最终验收报告](../v147-final-validation-20261114/README.md)汇总。`go generate ./...` 已在 `fef75aee` 退出 0，生成后 diff / status 均为空。

生成器整包复验仍待补跑：30m 重试曾出现六处 shutdown 预算旧断言失败（实际 28 Mod、预期 27）及总超时；[断言修复](../shutdown-assertions-20261114/README.md)已合，31 项定向测试与关键变异红绿通过。修复后的 60m 整包复验先因用户要求交接而主动中止，随后于 `d0d5a8b7` 补跑通过（exit 0，406 顶层 PASS、0 FAIL）。详见[最终验证与性能交接](../../HANDOFF-2026-11-14-v147-validation-performance.md)。M-1～M-19 / E-1 / E-3 均未运行，后续正式测量统一回写 `docs/performance/benchmarks/v147-remaining-20261114/README.md`，取得实测前不关闭 M。

### F：17 项过时表述的处理

| No. | 本次处理 / 仍保留 |
| --- | --- |
| 7 | 删除 MOBA D22 路由档未跑；Linux SLG 修后复跑仍 C-L2 |
| 15.3 | 删除额度 512 仍大量 retryable 来源不明；既有三轮零，M-11 仍 pending |
| 16 | 删除上线后兼容政策未定；维护手册已有必须兼容决定，Linux 拓扑仍待 |
| 19 | 明确 Linux 两条生成 Saga 功能不含 SIGKILL 恢复/性能，C-L3 保留 |
| 30 | deadline 分类指标已实现；M-10 仍待，修正 kB/s 单位 |
| 32① | robot 阻塞写用例期限时序已修，race2600 次；②E-2 只本轮未复现 |
| 58 | Sweep limit200 已修重跑；Cluster/异步复制容量仍 C-X5 |
| 68 | 删除 Redis Cluster 没跑与两 game 没演练；Q4 仍未关 |
| 73⑤ | 无缝演练已跑，不再作遗留；.NET/Unity 与 N4 分别判定 |
| 93 | 旧 Saga 默认串行已随`298e4396`替换；同盘/长尾外部调查保留 |
| 94 | 旧 v1.27 单变量比较任务移除；历史数据保留原报告，C-L10 保留 |
| 97 | observedBlocks4 不收敛已修`fa936c0d`；M-16 正式复验待测 |
| 100 | 删除旧 HEAD/v1.27 各两轮重跑要求；历史 Mongo 慢原因及 C-X11/S5 保留 |
| 105 | 独立 Gate 组同步已有专用测试`9aa30cf8`；M-18 容量待测 |
| 107 | 删除已迁历史的 Sync P99 重复待办，不伪造根因 |
| 108 | 删除已迁历史的 Remote P99 重复待办，不伪造 Mongo 根因 |
| 110 | 旧 Saga Mongo 中间档随旧实现删除，不作现行待测 |

<a id="一全表"></a>

## 一、全表（原始基线）

| No. | 标题路径（行） | 条目 | 类 | 去向 | 理由 |
| --- | --- | --- | --- | --- | --- |
| 1 | Nest/实体心跳：遗留（L623） | 批量作业参数的未测部分 ①②③ | M + C | ①③ → M-1；② → C-L1 | 夹具已带 `HB_CB_US` / `HB_HOT*`（`397aeda1`），①按 §3G 改 `tryBatchGroup` / `tryBatchSlice` 常数测；② 要 Linux |
| 2 | 同上（L624） | 热点实体下的冲突转单目标率 | M | M-1 | `HB_HOT` / `HB_HOT_SHARE` 与 `c_converted_per_beat` 已加（`397aeda1`），只差测量 |
| 3 | 移动与动作、行为与技能：遗留（L628） | 移动的每拍分配 | M | M-2 | `BenchmarkMoveBeatAllocations` 已加（`9869bd33`），没跑没记数；allocs 对负载不敏感，也可随时跑 |
| 4 | 同上（L629） | 事务内门控的进出场景 | D | Q1 | 设计取舍，未变 |
| 5 | 同上（L630） | `state` 的同步与接线 | D | Q2 | 未变 |
| 6 | 同上（L631） | 没有迁移 Lockstep 房间与 testdata 夹具 | D | Q3 | 未变 |
| 7 | 同上（L632） | MOBA 路由档在 D22-1 之后没有实跑 | F + C | F：MOBA 路由档与七玩法真实档 2026-11-14 全部重跑通过（“三”首行、`050680be`）；C：`ROOST_SLG_REAL=1` 在 Linux 复跑 → C-L2 | 本机部分已关闭，只剩 Linux |
| 8 | 压测工具的心跳驱动：遗留（L636） | 正式测量只覆盖 macOS 单机、每格 2～3 次 | A + C | A-1；C-L1（含 ticker 档 0.2 s 停顿是否复现） | 已测数字是框架代价的口径；Linux 复测与停顿归因要独占 Linux |
| 9 | 同上（L637） | nest-game 两种心跳投递方式对照没有测 | M | M-3 | 开关已加（`c67afd78`），只差测量 |
| 10 | 起服预加载与按服分库：遗留（L645） | 换服崩溃窗口没有自动清理 | D | Q4（与 No.65 同一问题） | 未变 |
| 11 | Remote：遗留（L649） | 交出实体的验证范围（两个真实进程每阶段 kill -9） | B | B-1 | 与 rehome 演练同一件事，合并 |
| 12 | 同上（L650） | 路由登记的长时运行与大联盟 ①⑥ | C + B | ① → C-S1；⑥ → B-2 | ① 要 ≥1h 浸泡；⑥ 大联盟文档 / 内存本机可测 |
| 13 | 同上（L651） | 空认领不清除 | D | Q5 | 未变 |
| 14 | 同上（L652） | 128 / 256 并发写偶发“全体约 3 s 超时” | B + C | B-3（checkpoint 落盘移出投影主循环的实验与本机 A/B）；C-Q1（安静机器、Mongo 分盘） | 剩下的停顿全在 I/O；“是否移出主循环”本机可做实验再交维护者定 |
| 15 | 同上（L653） | 单实例分散 Join 1000/s（主条） | A | A-2 | 已做的改动与缺省值是 Remote 容量口径 |
| 15.1 | 同上（L654） | 本机到不了 1000/s | C | C-Q2 | 要目标硬件 |
| 15.2 | 同上（L655） | effect 消费者并发对 RPC 的影响（未确认） | B + M | B-2 加开关；M-11 | 本机安静时可比；当时的探针在仓库外，要先把开关放进正式探针 |
| 15.3 | 同上（L656） | 写额度 512 后仍有大量“可重试”，来源未确认 | F | 删除 | `ffe2b98d`：`TestAllianceE2EJoinRetryableSources` 额度 512 三轮 retryable=0、unknown=0；额度 128 时全部是写额度准入。条目文字过时 |
| 16 | Saga：遗留（L660） | 上线前缺口 | C + F | C-X1；F：“上线后的兼容政策未定”——维护手册 §2 L23 已写“维护者宣布上线后改为必须兼容” | 只剩目标 Linux 拓扑 |
| 17 | 同上（L661） | 正式实现性能只在 macOS 单机测过（主条） | A + C | A-3；C-X2 | 已测数字归“一”Saga；Linux / 跨机外部 |
| 17.1 | 同上（L662） | 云 Mongo 只有三档单样本 | C | C-M1 | 云 Mongo |
| 17.2 | 同上（L663） | 参与方投影追平超过 §12 的 30 s | D | Q6 | 未变 |
| 17.3 | 同上（L664） | 原型 c128 余额 false 是否同源 | D | decisions.md“#37” | 已在 decisions.md |
| 18 | 同上（L665） | 托管 Mongo 的 `getParameter` | C | C-M2 | 要托管实例 |
| 19 | 同上（L666） | Saga 专项只在 macOS 上验证 | C + F | C-L3；F：“是否在 Linux 跑过尚未逐项核对”已可核对——LINUX-FUNCTIONAL-20261102 L40 只有“生成 Saga 2 PASS”，kill -9 恢复与性能没在 Linux 跑 | 改写为确定的“没跑” |
| 20 | 同步：尚未确认的偶发与疑点（L672） | game-demo 主视野超限用例重负载失败一次 | IP | 另一 agent 的复现循环 | — |
| 21 | 同上（L673） | 类型调度的 demo 性能验收 | M | M-4 | 夹具已能测生产接线（`bfed012a`），只差对照 |
| 22 | 同上（L674） | `BenchmarkAOIBatchRecompute/.../workers=4` 慢 7～11% | E | E-1（在 M-5 里跑） | 只能空闲时交替测 |
| 23 | 自身视图、嵌入 TCP 与 AOI：未测部分（L680，第 7 条） | 嵌入 TCP + self_sync 重档；整轮重试放大 | D + M | D：decisions.md“self_sync RetryLater 按 subject 作废 + 轮转”；M-6 | **条目文字过时**：CHANGELOG `[Unreleased]` L72 记 `TestSelfSyncResidentBudgetRetryDrill` 冒烟已确认放大会自我维持（预算 16 KiB 时 200 轮全部重试、0 轮提交），“没有测到不等于不存在”应改为“已确认，方向待定” |
| 24 | 同上（L682） | 缺省 256 MiB 在停读 / 更高推送量下是否够用 | A + M | A-4；M-7 | 演练入口已加（`c3b17b19`），只差 1000 连接档 |
| 25 | 同上（L683） | Interest 单写者 ⑧ | M | M-8 | 逐 tick 采样已加（`13614afa`），只差 16 视野复跑 |
| 26 | 同上（L684） | AOI 视野的大联盟 | M | M-9 | `BenchmarkRelationViewAllianceFlush` 已加（`734d6312`），条目仍写“没有压测” |
| 27 | Lockstep：遗留（L688） | 深缓冲链路子例 Linux 未复跑 | C | C-L4 | 判定为夹具问题，只差 Linux 复跑 |
| 28 | 同上（L689） | 最新 Linux 复验（v1.40.1）深缓冲仍断 | C | C-L4（与 27 同一次运行） | 合并 |
| 29 | 同上（L690） | TCP 快照用例的平台假设；Linux 全仓拖到 90 s | C | C-L4 | 合并 |
| 30 | 同上（L691） | 独立 Gate 慢链路先到期的是哪个期限 | M + F | M-10；F：代码侧已能区分（`25a3cd16` 新指标 `player_tcp_write_deadline_total{deadline}`，`fb214da1` 演练报出种类） | 只差 3000 帧 256 / 64 KB/s 正式演练 |
| 31 | 同上（L692） | 快照恢复未测、未做 ①②⑦⑧ | B + C + A | ① → B-4；② → C-U1；⑦ → C-X3；⑧ 与“game-demo 战斗未接” → A-5 | ⑧是设计 §13 未做项 |
| 32 | 同上（L693） | 高负载两个偶发 ①② | F + E | ① F：`1e595e9b` 定位为用例期限时序并修正（重负载 race 2600 次全过）；② → E-2 | ② native `send_stall` 仍无现场 |
| 33 | 生成器与工具：遗留（L699） | 两个服务同 sid 同目录争 WAL 目录 | D | Q7 | 未变 |
| 34 | 业务模块（L707） | gm 联动边界 | A + B | A-6；B-8 | 设计部分归“一”；真实 ops HTTP / 跨进程 mail / 真实交出实体本机可做 |
| 35 | 同上（L708） | 业务模块目录 | A + D | A-7；N1 | “待维护者决定”先后顺序不在 decisions.md |
| 36 | 同上（L709） | room 的边界 | A + B | A-8；B-9 | Mongo / WAL 联合验收本机可做 |
| 37 | 同上（L710） | 运行期接线的边界 | A + D | A-9；N2 | 2026-11-14 新增的待决定，不在 decisions.md |
| 38 | 同上（L711） | team 的边界 | A + B | A-10；B-7 | 跨进程共享队伍、toxiproxy 弱网本机可做 |
| 39 | 同上（L712） | turn 与定时器边界 | A + B | A-11；B-9 | Gate / NATS / 真实存储联合验收、模块 Robot 步骤 |
| 40 | 同上（L713） | enter 模块的边界 | A + B | A-12；B-6 | 与真实 naming 的组合、多实例本机可做 |
| 41 | 同上（L714） | game-demo 只用了 enter 的协议、域与首次初始化 | D | Q8 | 未变 |
| 42 | 同上（L715） | 模块的规则大多是 Go 写的业务文件 | D | Q9 | 未变 |
| 43 | 同上（L716） | mailbox 经 NATS BusClient 与 Redis 没跑过 | IP | 另一 agent | — |
| 44 | 同上（L717） | demo 删写死的号后模块号重排 | D | Q10 | 注意：交接 HANDOFF-2026-11-12-module-requirements §7 推荐“先不做” |
| 45 | 同上（L718） | wallet 金币没有玩法来源 | D | Q11 | 未变 |
| 46 | 同上（L719） | login 的边界 | A + B + C | A-13；B-6（多个 Gate、重启后重连）；C-X4（跨机） | 设计部分归“一” |
| 47 | 同上（L720） | portal 的边界 | A + D + B | A-14；Q12（动态目录上报）；B-6（account 多副本、naming 组合） | — |
| 48 | 同上（L721） | 配方登录验收边界 | A + B | A-15；casual → B-6，turn-pvp → B-9 | 完整购买 / 对局、独立 Gate、多实例、重启恢复本机可做 |
| 49 | 同上（L722～723） | game-demo 接入登录链路的边界 + 验证范围 | A | A-16 | 2026-11-14 已全部实跑，剩下的只是验证口径 |
| 50 | 同上（L724） | servertime 的边界 | A + B | A-17；B-6 | 真实 TCP 推送先后、独立 Gate 下 game 重启连接是否保留本机可测 |
| 51 | 同上（L726） | activity_world 自带场景是否读工程配置表 | IP | 另一 agent（换表） | — |
| 52 | 活动模块的驱动与验收边界（L730、L732） | activity 驱动与验收 | A + B + C | A-18；B-8；C-S2 | 真实 Mongo / WAL / NATS 本机可做；长稳外部 |
| 53 | purchase 模块的恢复与验收边界（L736） | purchase | A + B | A-19；B-8 | 真实 NATS / TCP / Mongo / WAL、跨 sid、故障恢复本机可做 |
| 54 | R7 匹配模块的当前边界（L740） | matchmaking | A + B | A-20；B-7 | Redis / Mongo / NATS 联合验收本机可做 |
| 55 | 同上（L742） | room §5 后续与 R11 延后 | D | Q13 | 未变 |
| 56 | 联盟（alliance）（L746） | alliance 模块验收范围 | A + B | A-21；B-7 | 真实依赖、双 game sid 迁服 kill -9、全模块矩阵本机可做 |
| 57 | game-demo 的移动、怪物与场景时间（L754） | 机器人 100 / 200 人同秒起跑 | M + E | M-12（顺带验证“前一轮 saga 积压被继承”的推断） | 要空闲机器、每轮新环境 |
| 58 | 框架服务与 game-demo：未测部分（L758） | 匹配队列 | C + F | C-X5；F：后半句“Sweep 参数错误未生效”已于 2026-10-22 修正重跑（见 L866 本身） | — |
| 59 | 同上（L759） | game-demo 活动结算榜 | A + C | A-22；C-X5 / C-S3 | “无自动对账、要 Redis 6.2”是设计 |
| 60 | 同上（L760） | 全服邮件账本规模没有压测 | M | M-13 | `BenchmarkBroadcastLedgerAtCapacity` 已加（`26d99deb`），条目仍写“没有压测” |
| 61 | 单个玩家换服（L766） | 只有离线验证 | B | B-1 | 合并 No.11、65、68 |
| 62 | 同上（L767） | 向前恢复只由登录触发 | D | Q14 | 未变 |
| 63 | 同上（L768） | 受理之后中止也会断开会话 | D | Q15 | 未变 |
| 64 | 同上（L769） | game-demo 换服前的业务边界 | A + D | A-23；N3（名下建筑与全服邮件换服后归哪个服） | 对战座位 / 撮合 / 地形账本 11-14 已做（`637af954`、`f8c4c2b4`） |
| 65 | 同上（L770～772） | 搬档的崩溃窗口与验证 | D + B | 崩溃窗口 → Q4；验证 → B-1 | — |
| 66 | 单个角色换服（account）：遗留（L776） | 方案 §6 第 1～4 项待确认 | D | Q16 | 未变 |
| 67 | 同上（L777） | 期限只记录、没有列举与清扫 | D | Q14 | 合并 |
| 68 | 同上（L778） | 验证边界 | F + B | F：Redis Cluster 已跑（`31777bad`，并进 `redis-cluster-suites.sh`）；真实进程演练 → B-1 | — |
| 69 | 地形 / 无缝世界：遗留（L784） | game-demo 地形 DAO 仍是 global | D | Q17 | 未变 |
| 70 | 同上（L785） | 转发编辑 `-race` 失败一次 | IP | 另一 agent 的复现循环 | — |
| 71 | 同上（L786） | 分区场景建筑进主视野只有编译与单元覆盖 | IP | 另一 agent（#146） | — |
| 72 | 同上（L787） | 游戏尺度分区视野成本没实测 | M | M-14 | 入口已加（`ba870e55`） |
| 73 | 同上（L788） | 地形确认来源 ③④⑤ | D + B + C + F | ③ → N4；④ .NET 用真实帧驱动 → B-10，Unity → C-U2；⑤ F（2026-11-14 已跑，条目自述） | — |
| 74 | 同上（L789） | 地形首批参数实施边界 | A + B + C | A-24；真实 Mongo / WAL 崩溃演练 → B-11；T09 目标容量、跨机、长稳、Unity → C-X6 / C-U2 | TERRAIN-STATUS L64 仍“待补” |
| 75 | 同上（L790） | CellWorld 千人容量的复测 | M + C + D | M-15（A + D 之后的 Robot 千人复跑）；C-L5（≥16 核 Linux 或 Cell / Base 分机、Redis HA）；D：decisions.md“CellWorld 容量 B / C / E” | 本机给不出 50 ms 结论，但复跑本身本机可做 |
| 76 | 同上（L791） | CellWorld 模块的验收范围 | B | B-12 | 仍只有 3 条（`runtime_test.go.tmpl`） |
| 77 | 同上（L792） | 出生号与运行期号共用 shard 位 | B | B-13 | 已核对仍冲突：monster `sequenceBits=34`、demo `runtimeid.SequenceBits=34`，序号 ≥ 2^33 时与 `SpawnID` 的 `1<<33` 段重叠 |
| 78 | 同上（L793） | 无缝世界的其余产品规则；Team 归属 | A + D | A-25；decisions.md“#154 Team 归 game 服务是否确认” | Team 已按 §14.3 B 实施（`649f2c78`） |
| 79 | 同上（L794） | cellworld 的 Linux 原始失败样本 | C | C-L6 | — |
| 80 | LevelComponent 与 UGC：遗留（L800） | UGC 物理闯关 | A + C | A-26；C-U3 | — |
| 81 | 同上（L801） | UGC 分频显示 | C | C-U3（Unity / WAN）、C-X7（多房间容量） | — |
| 82 | 同上（L802） | UGC 多房间与长稳 | A + C | A-27；C-L7、C-S4 | 已测部分是口径；Linux / 小时级外部 |
| 83 | 客户端：遗留（L808） | Native 客户端 | A + C | A-28；C-U4 | TLS / WS / UDP / KCP 没实现是框架限制 |
| 84 | 同上（L809） | 地形 C# | C | C-U2 | — |
| 85 | 同上（L810） | 场景时间域 Unity 适配只在 .NET 验证 | C | C-U1 | 适配层已接（`4a6f92f3`） |
| 86 | 同上（L811） | 同一 SessionID 跨 Gate 的客户端覆盖 | B + C | C++ 跨 Gate 端到端 → B-14；Unity / IL2CPP / Unreal → C-U4 | — |
| 87 | 同上（L812） | C++ protobuf 用例没跑 | B | B-15 | 已核对：本机仍没有 `protoc` |
| 88 | 同上（L813） | 客户端引擎全矩阵 | C | C-U4 | — |
| 89 | 平台与外部验证（L819） | 跨机 HA 与 Linux 物理故障 | C | C-X8（E01～E28） | — |
| 90 | 同上（L820） | 独立 Gate 多进程 / 跨机矩阵 | C | C-X9 | — |
| 91 | 同上（L821） | 单个玩家改绑 sid 与 Player 跨机演练 | C | C-X10 | 同机两进程部分由 B-1 承接 |
| 92 | Robot 推广验收（L827） | 100/s 修后仍有 19 / 15 个输入丢拍 | E + C | E-3；Linux / 跨机 / 移动设备 → C-L8 | — |
| 93 | 10-23 候选诊断边界（L831） | 全局停顿与 200 ms 抖动、Mongo 容量 | A + C + F | A-29；C-L9；F：“Saga 两个消费 worker、默认串行”属旧协调器，已删（`298e4396`） | 维护者已定保留生产默认、不再压测 |
| 94 | 能力与验证边界（L839） | 重档总体 | C + F | C-L10（独占服务器）；F：相对 v1.27 的单变量对照已无意义（L862 自述 10-22 对 v1.29 无增加） | — |
| 95 | 同上（L840） | Sync 偶发长尾 | C | C-L11 | 维护者 10-15 方案 A |
| 96 | 同上（L841） | 独立 Gate | C | C-X9 | — |
| 97 | 同上（L842） | Block AOI | M + A + F | 集中布局 / 扇出重档复验 → M-16；独立客户端进程 → A-30；扇出 4 不收敛 F（`fa936c0d`） | — |
| 98 | 同上（L843） | 自身视图（self_sync） | M + D | 同 No.23（M-6、decisions.md self_sync） | 重复 |
| 99 | 同上（L844） | Lockstep 追帧 / 快照恢复 | M + C | 独立 Gate 纯追帧页 → M-10；真实移动网络 → C-X3 | 演练入口已加（`316fb8b9`） |
| 100 | 同上（L845） | Remote 容量 | C + F | C-X11 / C-S5（跨机 RTT、故障、24 h）；F：“每边 ≥2 次交替排除 HEAD 尾部”是对 2026-10-21 HEAD 与 v1.27 的对照，基线已过时，10-22 复测 p99 287 ms 已给结论 | — |
| 101 | 同上（L846） | 投影 checkpoint 与 outbox 唤醒同盘 | C | C-L12 | — |
| 102 | 同上（L847） | 匹配队列容量 | C | C-X5 | 重复 No.58 |
| 103 | 同上（L848） | 生成工程完整链路持久写入 | B + M + C | 夹具入库 → B-16；self_sync / pipelined 开环对照 → M-17；独占宿主 → C-L10 | 当时的夹具不在仓库 |
| 104 | 同上（L849） | schema 升级 / Player 跨机演练 | C | C-X10 | 重复 No.91 |
| 105 | 游戏接入验收：遗留（L855） | 组同步的容量与独立 Gate 用例 | F + M | F：组同步自己的慢消费者与独立 Gate 用例已加（`9aa30cf8`）；容量 → M-18 | 条目文字过时 |
| 106 | 待补测（L861） | 1. Sync 长尾的归因 | C | C-L11 | 重复 No.95 |
| 107 | 同上（L862） | 2. Sync P99 +0.6～1.1 ms 来源 | F | 删除 | 条目自述已移到“三” |
| 108 | 同上（L863） | 3. Remote 80TPS 夜间 p99 偏高 | F | 删除 | 同上 |
| 109 | 同上（L864） | 4. 投影 checkpoint 同盘 | C | C-L12 | 重复 No.101 |
| 110 | 同上（L865） | 5. Saga Mongo 步骤中间档位 | F | 删除 | 自述随 `298e4396` 不再适用 |
| 111 | 同上（L866） | 6. 匹配队列 | C | C-X5 | 重复 No.58 |
| 112 | 同上（L867） | 7. 生成工程完整链路持久写入 | B + M | B-16、M-17 | 重复 No.103 |
| 113 | 同上（L868） | 8. demo 按秒爬坡 | M | M-12 | 重复 No.57（DESIGN-TODO“先测量再优化”第 2 项） |
| 114 | 新增离散玩法纵切（L872） | 三个离散玩法 | IP + A + B | 回合超时 timer 装配 → IP（#200）；A-31；真实持久恢复 → B-5 | 三个脚本都没有 `_REAL` 档（已核对） |

## 二、各类数量

按“部分”计（一条混合条目的每个部分各计一次；重复条目照计，括号里是去重后的独立工作 / 问题数）：

| 类 | 部分数 | 去重后 | 说明 |
| --- | --- | --- | --- |
| A 移到“一” | 31 | 31 段文字（A-1～A-31） | 多数是模块与验证口径 |
| B 本机可修 | 30 | 16 项（B-1～B-16） | 分 9 个批次，见 §2 |
| C 外部环境 | 49 | 约 30 次运行，7 类环境 | 见 §4 |
| D 维护者决定 | 28 | 25 个问题：Q1～Q17 共 17 + decisions.md“整理中新增”4 + 新问题 N1～N4 | 新问题见 §5 |
| E 未确认偶发 | 4 | 3 个循环（E-1～E-3），No.57 的推断放在 M-12 里验 | 另有 2 个 IP |
| M 本机空闲测量 | 22 | 18 次运行（M-1～M-18），外加 B-3 的 A/B（M-19） | 约 5.3 h，见 §3 |
| F 过时 / 已解决 | 17 | — | 直接删或改一句 |
| IP 进行中 | 6 | No.20、43、51、70、71、114（部分） | 只标记 |

按行看：114 条里纯 D 16 条、纯 C 20 条、纯 M 9 条、纯 B 5 条、纯 IP 5 条、纯 F 3 条、纯 A 2 条、纯 E 1 条，其余 53 条是混合（大多是“A + B”或“A + C”）；另有 5 个子行（15.1～15.3、17.1～17.3 中的 D / C / F）。

## 三、几条值得先看的发现

1. **No.23 / No.98 的事实已变**：self_sync 整轮重试放大在 2026-11-14 的演练冒烟里已确认会自我维持（CHANGELOG `[Unreleased]` L71～72），KNOWN-LIMITS L680 / L843 还写“未测到 / 没有测到不等于不存在”。这正是 decisions.md 里“self_sync RetryLater 改为按 subject 作废 + 轮转（tmp/sync-fixes 8c86065f / a0f07234，未合并）”要定的事，建议把它排在 D 的最前面。
2. **No.15.3 已经解决**：`ffe2b98d` 的探针在当前代码上额度 512 时 retryable=0，KNOWN-LIMITS 还挂着“来源尚未确认”。
3. **No.77 的冲突是真的（理论上）**：monster 模块与 demo `runtimeid` 都允许 34 位序号，`cellworldhost.SpawnID` 占用 `home<<34 | 1<<33 | seq(≤50000)`；同一 sid 的运行期序号到 2^33 以上就会与出生号重叠。代价很小（S），建议直接把两处上限改成 `1<<33 - 1`。
4. **多条测量“入口已加、条目没改”**：No.3、24、25、26、30、60、72、105 的夹具 / 基准在 11-14 都已提交，条目仍写“没有压测 / 没有用例”；M 跑完后按结果改写或删除。
5. **Q10 与交接文档的推荐相反**：旧归类给 Q10 推荐 A（安装器加号锁定），而 HANDOFF-2026-11-12-module-requirements §7 写的推荐是“先不做；出现客户端兼容需求时再支持保留号”。问维护者时两种都列上。
6. **模块真实依赖验收是 B 的大头**（No.34、36、38、39、40、46～48、50、52～54、56、114）：四个模块 / 配方的 `_REAL` 档都没有，约 4 个批次；其中 mailbox、activity_world、turn-pvp 与正在进行的 IP 碰同一批文件，要等它们合入。

## A：移到“一”的文字

31 段已按当前证据修正后迁入 [KNOWN-LIMITS 一](../../KNOWN-LIMITS.md#一框架本身的限制)，本处保留去向而不再复制另一份现行事实。特别修正 A-5 的 MOBA 检查点/宿主恢复、A-15 配方联合验收、A-23 两种匹配的换服边界、A-25 未答#154、A-31 默认与 REAL 两层口径。

| A / No. | 目标精确标题 |
| --- | --- |
| A-1 / 8 | 压测工具的心跳驱动（D20 第 4 期） |
| A-2 / 15 | Remote |
| A-3 / 17 | Saga |
| A-4 / 24 | 嵌入 TCP 出站 |
| A-5 / 31 | Lockstep |
| A-6 / 34 | 业务模块的既定设计 |
| A-7 / 35 | 业务模块的既定设计 |
| A-8 / 36 | 业务模块的既定设计 |
| A-9 / 37 | 业务模块的既定设计 |
| A-10 / 38 | 业务模块的既定设计 |
| A-11 / 39 | 业务模块的既定设计 |
| A-12 / 40 | 业务模块的既定设计 |
| A-13 / 46 | 业务模块的既定设计 |
| A-14 / 47 | 业务模块的既定设计 |
| A-15 / 48 | 业务模块的既定设计 |
| A-16 / 49 | 业务模块的既定设计 |
| A-17 / 50 | 业务模块的既定设计 |
| A-18 / 52 | 业务模块的既定设计 |
| A-19 / 53 | 业务模块的既定设计 |
| A-20 / 54 | 业务模块的既定设计 |
| A-21 / 56 | 业务模块的既定设计 |
| A-22 / 59 | 框架服务与 game-demo |
| A-23 / 64 | 单个玩家换服（rehome）：既定设计 |
| A-24 / 74 | 地形与无缝世界 |
| A-25 / 78 | CellWorld 固定分区（v1.47.0） |
| A-26 / 80 | UGC 物理闯关 |
| A-27 / 82 | UGC 物理闯关 |
| A-28 / 83 | Native 客户端（2026-11-01） |
| A-29 / 93 | 验收与运维 |
| A-30 / 97 | sync-business 夹具的范围 |
| A-31 / 114 | 游戏接入验收 |

## B：本机可修的方案

以下是原始实施方案；当前完成范围与残留以上方 B 状态表为准，不按旧预计工作量重新执行。

量级：S < 1 h，M 1～4 h，L > 4 h。验证一律先红后绿（能构造的话），最后按“full-tests-once-at-end”只跑一次慢套件。

| ID | 条目 | 做法（文件 / 脚本） | 怎么验证 | 量级 |
| --- | --- | --- | --- | --- |
| B-1 | No.11、61、65（验证部分）、68（进程演练）、91 的同机部分 | 新脚本 `scripts/test-demo-rehome.sh`（照 `test-demo-merge.sh`：mirror-local 私有偏移、生成 game-demo、account + game 1000 / 1001 + alliance），新夹具 `scripts/testdata/demo-drill/rehome*.go.txt`：机器人在 1000 建号、发起换服，在受理 / 断开排空 / 离开钩子 / `HandOffEntity` / account 提交 / 删源库六个点后各 `kill -9` 一次源服（或 account），重启后经登录续做；核对单一所有者、背包 / 钱包计数不丢、源库被删或记 Error；记 checkpoint 等待时长。`integration_coverage_test.go` 登记入口 | 脚本连跑两遍退出 0；变异：去掉 HandOff 的 checkpoint 等待 → 应红 | L（5～6 h） |
| B-2 | No.12⑥、No.15.2 的开关 | `scripts/testdata/alliance-e2e/perf_test.go.txt` 加 `TestAllianceE2ELargeAllianceFootprint`（`ALLIANCE_E2E_PERF=1`，1k / 5k / 10k 成员的一个联盟：BSON 文档字节、Mirror 常驻堆、冷加载耗时）；给 256 并发重试探针加 `ALLIANCE_E2E_EFFECT_WORKERS`（改生成的 `dataengine.effect_consumer.workers`）与 `ALLIANCE_E2E_RETRY=1`（调用方退避重试），使 M-11 能在正式入口上复现 11-05 的对照；`scripts/test-alliance-e2e.sh` 注释、`integration_coverage_test.go` | 两条用例冒烟各一次（小规模）；结果写进基准页 | M（2.5 h） |
| B-3 | No.14 | 实验分支：`framework/nestwal/wal.go` 的确认路径与 `framework/dataengine/engine` 投影器主循环里同步做的 `storeCheckpoint`（rename / fsync）改为单个后台写者、只合并最新 fence，保持“checkpoint ≤ 已投影”与 `syncActive` 在前的顺序；不合并到 main，结果交维护者 | nestwal / engine 全包 `-race`、崩溃恢复用例全过；变异：后台写者越过未刷段 → 现有用例应红；A/B 见 M-19 | L（4 h 实现 + M-19） |
| B-4 | No.31① | `scripts/testdata/moba/`：宿主与权威分属两个进程时经已提交检查点的快照恢复；宿主交接与快照恢复同时发生；`TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL` 记录真实 strict WAL 提交延迟分布（P50 / P99） | `ROOST_MOBA_REAL=1 bash scripts/test-moba-generated.sh` 全过；变异：恢复时不等已提交门 → 红 | M（3 h） |
| B-6 | No.40、46、47、48（casual）、50 | `scripts/test-demo-login.sh` 加阶段：两个独立 Gate 同时在线、game 重启后客户端重连（嵌入 TCP 与经 Gate）、account 两个副本；enter + 真实 naming 的新建 / 换名（`modules/enter`、`modules/naming` 的联动场景改用真实 naming）；servertime 经真实 TCP 的推送与 EnterGame 应答先后、独立 Gate 下重启 game 后连接是否保留（`modules/servertime` 用例 + 演练一步）；casual 配方补购买、独立 Gate、多实例、重启恢复（`modules/recipes/casual`） | 脚本两遍退出 0；新用例先红（例如去掉推送排序）再绿 | L（4～5 h） |
| B-7 | No.38、54、56 | team：mirror-local 上两个 game 进程共享队伍 + toxiproxy 弱网（`modules/team` 联动场景的真实依赖档）；matchmaking：Redis / Mongo / NATS 联合验收（`modules/matchmaking`）；alliance 模块：`scripts/test-alliance-e2e.sh` 缺省集合加双 game sid 迁服 `kill -9` 与模块矩阵跑一次 | 新档全过；team 弱网下邀请 / 查询不变式成立 | L（4～5 h） |
| B-8 | No.34、52、53 | gm：真实 ops HTTP → 跨进程 mail → 真实交出实体的演练（`modules/gm`）；purchase：真实 NATS / TCP / Mongo / WAL、跨 sid、Redis 重启后的补领（`modules/purchase`）；activity：真实 Mongo / WAL / NATS 与跨进程迁移（`modules/activity*`）。**等 mailbox 与 activity_world 的 IP 合入后再开工**（同碰 mail / activity 文件） | 新档全过；purchase 故障注入后游标、待发、Player 一致 | L（5 h） |
| B-9 | No.36、39、48（turn-pvp）、114（turn-pvp 部分） | room / turn 的 Mongo / WAL / TCP / Gate / NATS 联合验收（`modules/room`、`modules/turn`、`modules/recipes/turn-pvp`），给 turn 模块加 Robot 步骤接到 game-demo `cmd/loadtest`；turn-pvp 配方补对局、独立 Gate、重启恢复。**等 #200 合入后再开工**（同碰 turn-pvp） | 新档全过；Robot 步骤在 `make dev-smoke` 里跑通 | L（5 h） |
| B-10 | No.73④（.NET 部分） | `client/dotnet/Roost.Client.Terrain.Tests`：用 `Fixtures/sync_fixture_test.go` 生成真实 entitysync 帧驱动 `EntityViews` → `TerrainChunkSync`，核对确认时序 | `bash scripts/test-client-dotnet.sh` 全过；变异：确认早于块到达 → 红 | M（2 h） |
| B-11 | No.74（崩溃演练） | `demo/internal/service/game/terrain_test.go.tmpl` 照 `TestTerrainEditLedgerRealMongo` 加 SIGKILL 子进程用例：编辑 / 放建筑后只在 WAL 里的一笔，`kill -9` 后回放，块版本与建筑与崩溃前一致；更新 TERRAIN-STATUS L64。**与 #146 同碰 demo 地形文件，等它合入** | `bash scripts/test-demo-generated.sh` 的地形真实档；变异：恢复丢 WAL → 红 | M（3 h） |
| B-12 | No.76 | `modules/cellworld/files/game/cellworldhost/runtime_test.go.tmpl` 加 `New` 的 Cell 分支、`Start` / `Stop`、`Start` 失败后回到未启动的回归 | `roost module verify cellworld`（3 → 6 条）；变异：Start 失败不复位 → 红 | M（2 h） |
| B-13 | No.77 | `demo/game/runtimeid/runtimeid.go.tmpl` 与 `modules/monster/files/game/monster/service/runtime.go.tmpl` 的序号上限改为 `1<<33 - 1`（把第 33 位留给 `cellworldhost.SpawnID`），各加一条越界用例；注释写明保留段 | 越界用例先红后绿；demo 重新生成、`roost module verify monster` | S |
| B-14 | No.86（C++ 部分） | `client/cpp/tests` + `scripts/test-gate-client.sh`：两个 Gate、同一 SessionID 先连 A 再连 B 的端到端（照 C# 已有用例） | `bash scripts/test-gate-client.sh`；变异：不带旧 SessionID → 红 | M（2.5 h） |
| B-15 | No.87 | 装 protoc（`brew install protobuf`，**需维护者同意下载**），`ROOST_PROTOBUF_TESTS=ON` 跑 `scripts/test-client-cpp.sh` | ctest 全过后删条 | S |
| B-16 | No.103、112 | 把 2026-10-20 / 22 的一次性夹具入库：`scripts/perf/generated-tcp.sh` + `scripts/testdata/generated-tcp/`（CLI 生成工程、strict handler 金币 +1、嵌入 TCP、50 连接 / 同一实体两档），开关 `SELF_SYNC=1`、`DURABILITY=strict|pipelined|async`、`OPEN_LOOP_RATE` | 10 s 冒烟各档出数；正式测量见 M-17 | M（3 h） |
| B-5 | No.114 | 合作 Roguelike、异步社交经营加 `ROOST_COOP_ROGUELIKE_REAL=1` / `ROOST_SOCIAL_SIM_REAL=1`（照 `scripts/testdata/card-rpg` 的 SIGKILL 恢复用例，`scripts/test-coop-roguelike-generated.sh`、`test-social-sim-generated.sh`）；turn-pvp 的 `_REAL` 放进 B-9 | mirror-local 上两档全过；变异：恢复丢 WAL → 红 | L（4～5 h） |

## E：未确认偶发的复现 / 否证

- **E-1（No.22）**：同一台空闲机器上编两个测试二进制：`git worktree add /tmp/aoi-base b4699758`，两边 `GOWORK=off go test -c -o aoi-<side>.test ./framework/sync/entitysync/policy`；交替 10 轮 `./aoi-<side>.test -test.run '^$' -test.bench 'AOIBatchRecompute/dense=false/coverage=false/workers=(1|4)$' -test.count 1 -test.benchmem`，`/Users/whb/go/bin/benchstat`；再加第三个二进制（HEAD 去掉那两次分支判断）区分“分支”与“代码布局”。空闲下不显著 → 关闭为噪声；显著且第三个不显著 → 记“代码布局”，按 A 写进“一 / 广播与 AOI”。约 12 分钟（在 M 序列 S1 里）。
- **E-2（No.32②）**：`cmake -S client/cpp -B <scratch>/cpp-build && cmake --build <scratch>/cpp-build`；后台 `for i in 1..8; yes >/dev/null &` 造负载（load 40+），`for i in $(seq 200); do ctest --test-dir <scratch>/cpp-build -R native_weak --output-on-failure || { lsof -nP -iTCP > fail-$i.lsof; netstat -an > fail-$i.netstat; break; }; done`，失败时保留现场；同时在 `client/cpp/tests/weak.cpp` 的 `send failed` 分支打印 errno 与已发字节（只为本循环，不入库）。200 轮不红 → 记“未复现（条件）”关闭；红 → 按现场判夹具时序（`weak.py` 的 `time.sleep(0.8)` 与 SO_RCVBUF 4096）还是实现。约 30 分钟。
- **E-3（No.92）**：私有依赖（`source ~/.roost-it/roost-dataengine-it/env.sh`）上 `ROOST_PERF_LABEL=gate-drop-<n> ROOST_GATE_LOAD_RATE=100 ROOST_GATE_LOAD_SECONDS=30 bash scripts/perf/gate-mixed.sh` 连跑 5 轮；先给夹具的丢拍计数按原因打标签（准入拒绝 / 期限到期 / 心跳跳拍 / 客户端节拍落后，`wiring/gate` 的 Gate 负载用例），按标签定来源。客户端节拍落后为主 → 归压测端、改写进“一 / Robot 推广验收的范围”；服务端原因 → 转 B。约 10 分钟测量 + S 级打标签。

## D：维护者决定（问题清单）

已在 decisions.md 的直接引用其编号；推荐项标【推荐】。

- Q1（No.4）、Q2（No.5）、Q3（No.6）、Q4（No.10、65）、Q5（No.13）、Q6（No.17.2）、Q7（No.33）、Q8（No.41）、Q9（No.42）、Q10（No.44，见 §三第 5 条）、Q11（No.45）、Q12（No.47）、Q13（No.55）、Q14（No.62、67）、Q15（No.63）、Q16（No.66）、Q17（No.69）：问题与选项沿用旧归类 §三，事实已核对未变。
- decisions.md“#37 Saga 原型 c128 余额 false”（No.17.3）：A 关闭（原型已删、逐账户证据缺失、正式实现门槛为 0）【推荐】；B 保留为“尚未确认”。
- decisions.md“#154 Team 归 game 服务”（No.78）：A 确认现状（已实施）【推荐】；B 改为实体服务独占。
- decisions.md“CellWorld 容量 B / C / E”（No.75）：先做 M-15 复跑再决定；排查结论里 B 收益最大但破坏自包含契约。
- decisions.md“self_sync RetryLater 按 subject 作废 + 轮转”（No.23、98）：已有冒烟证据（见 §三第 1 条），A 合并 tmp/sync-fixes 的方案【推荐，因为放大已确认会自我维持】；B 维持 C29、只在文档写明配预算的方法。
- 新问题 N1～N4 见 §5。

## 2. B 的批次（每批约 3～4 h，按领域分，文件互不相交，可并行；遵守“最多 4 个并行 agent”）

此表保留原分工；功能批已经执行，B-3 仅实验 / M-19 待测，不重复整批。

| 批 | 领域 | 条目 | 碰的文件 | 时长 | 先决 |
| --- | --- | --- | --- | --- | --- |
| P1 | 换服真实进程演练 | B-1 | `scripts/test-demo-rehome.sh`（新）、`scripts/testdata/demo-drill/rehome*`（新） | 5～6 h（可拆成“脚本骨架 + 前三个 kill 点”与“后三个 kill 点 + checkpoint 计时”两段） | 无 |
| P2 | DataEngine / Remote 测量与实验 | B-2、B-3 | `scripts/testdata/alliance-e2e/perf_test.go.txt`、`framework/nestwal/wal.go`、`framework/dataengine/engine/`（B-3 只在实验分支） | 4 h（B-3 的 A/B 测量算 M-19） | 无；B-3 动三大块之一，不合 main |
| P3 | 小件：cellworld / 出生号 / 地形 C# | B-12、B-13、B-10 | `modules/cellworld/files/**`、`demo/game/runtimeid/`、`modules/monster/files/**`、`client/dotnet/Roost.Client.Terrain.Tests/**` | 4～4.5 h | 无 |
| P4 | 原生客户端 | B-14、B-15 | `client/cpp/**`、`scripts/test-gate-client.sh`、`scripts/test-client-cpp.sh` | 3 h | B-15 要先拿到装 protoc 的同意 |
| P5 | MOBA 快照与持久写入夹具 | B-4、B-16 | `scripts/testdata/moba/**`、`scripts/perf/generated-tcp.sh`（新）、`scripts/testdata/generated-tcp/`（新） | 6 h（建议拆成两批各 3 h） | 无 |
| P6 | 登录线模块真实依赖 | B-6 | `scripts/test-demo-login.sh`、`modules/{enter,naming,servertime,login,portal}/**`、`modules/recipes/casual/**` | 4～5 h | 无 |
| P7 | 社交模块真实依赖 | B-7 | `modules/{team,matchmaking,alliance}/**`、`scripts/test-alliance-e2e.sh`、`scripts/testdata/alliance-e2e/{game,robot}_test.go.txt` | 4～5 h | 与 P2 同目录不同文件（P2 只碰 `perf_test.go.txt`） |
| P8 | 运营模块真实依赖 | B-8 | `modules/{gm,purchase,activity,activity_alliance,activity_world}/**` | 5 h | 等 mailbox、activity_world 两个 IP 合入 |
| P9 | 回合 / 房间与新玩法真实档 | B-9、B-5 | `modules/{room,turn}/**`、`modules/recipes/turn-pvp/**`、`scripts/testdata/{turn-pvp,coop-roguelike,social-sim}/**`、对应 `scripts/test-*-generated.sh` | 2 × 4～5 h | 等 #200 合入 |
| （后续）P10 | 地形崩溃演练 | B-11 | `demo/internal/service/game/terrain*` | 3 h | 等 #146 合入 |

共用文件：`integration_coverage_test.go`（新入口登记，只追加）与 CHANGELOG `[Unreleased]`（各批只追加自己的段）。**KNOWN-LIMITS 只由最后一个文档批改**（A 的 31 段移到“一”、F 的 17 处删除 / 改写、各批与 M 的结果回写），约 3 h，在 D 答复之后做，避免并行冲突。

## 3. M：空闲机器上的运行序列

**状态：M-1～M-19 正式测量均未运行。** 下列是计划入口，实际报告须记录生效参数与错误，不以开关冒烟为完成。2026-11-14 更正：M-10 单位为十进制 kB/s，使用 256/64（原 262144/65536 大 1024 倍）；M-16 显式 8/8/queue65536/CPU0（省略时旧默认 4/32，不作同参数对照）；M-17 开环固定 1500/s、四档各 5min，不是容量保证。统一使用`GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0`，每段≤30min，等待无编译/race/索引负载。

前提：没有别的 agent 在跑 race / 编译 / 索引；每段开始前 `sysctl -n vm.loadavg`、`sysctl vm.swapusage` 记入结果（CorpLink 常驻约 1 核，load 2～4 视为空闲）；用到依赖的段 `source ~/.roost-it/roost-dataengine-it/env.sh`（偏移 1000）或 `bash scripts/mirror-local.sh up`（偏移 ≤ 38135）。按维护者的节奏分成每段 ≤ 30 分钟的 14 段，可分几天跑；每段结束检查孤儿进程。

```sh
cd /path/to/roost-core
export GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0

# ── S1 微基准（约 25 min，无依赖）──
go test ./gameplay/movement -run '^$' -bench MoveBeatAllocations -benchmem -count 6            # M-2（No.3），约 2 min
go test ./framework/sync/entitysync/policy -run '^$' -bench RelationViewAllianceFlush -benchtime 50x -count 3   # M-9（No.26），约 4 min
go test ./framework/sync/entitysync/policy -run '^$' -bench GroupViewFlush -benchtime 200x -count 3            # M-18（No.105），约 3 min
# M-5 = E-1（No.22）：base b4699758 / HEAD / HEAD-去分支 三个二进制交替 10 轮，见 §E，约 12 min

# ── S2 心跳投递（约 15 min，无依赖）── M-1（No.1①③、No.2）
for round in 1 2; do for cb in 0 5 10 20; do for hot in 0 64; do
  HB_DELIVERY=1 HB_MODE=c HB_CB_US=$cb HB_HOT=$hot HB_HOT_SHARE=$([ $hot = 0 ] && echo 0 || echo 0.5) \
  HB_OUT=artifacts/perf/hb-remaining/delivery.jsonl \
    go test -run TestDeliveryRun -count=1 -timeout 0 ./gameplay/heartbeat/internal/bench
done; done; done                                                          # 16 次 × ~20 s
# ①：临时工作树里改 framework/nest/try_batch.go 的 tryBatchGroup ∈ {2,4}、tryBatchSlice ∈ {50µs,100µs}，各编一个 bench 二进制，
#    HB_MODE=c HB_CB_US=10 交替 2 轮（4 × 2 × ~20 s + 编译）

# ── S3 Sync 夹具（约 30 min，无依赖）──
for s in periodic on_change periodic on_change; do ROOST_PERF_LABEL=sched-$s-$RANDOM ROOST_PERF_COUNT=1 ROOST_SYNC_SCHEDULE=$s bash scripts/perf/sync-business.sh; done   # M-4（No.21），约 12 min
# M-8（No.25）：按 HB-DRIVEN-LOAD §4 / INTEREST-SINGLE-WRITER §7.1 编 16 视野诊断构建（只把 SyncApplyMaxExtraViews 改 16，不入库），
for apply in sync async sync async; do ROOST_PERF_LABEL=i8-v16-$apply-$RANDOM ROOST_PERF_COUNT=1 bash scripts/perf/sync-aoi.sh \
  -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -views-per-player=16 -apply-mode=$apply; done   # 约 8 min，读 server.json 的 log_backlog_per_tick
for scale in demo game demo game; do ROOST_CELLWORLD_PERF=1 ROOST_CELLWORLD_PERF_SCALE=$scale ROOST_CELLWORLD_PERF_COUNT=1000 \
  ROOST_CELLWORLD_PERF_NPCS=2250 ROOST_CELLWORLD_PERF_ACTIVE=5 ROOST_CELLWORLD_PERF_SECONDS=60 \
  go test ./gameplay/cellworld -run '^TestCellWorldMeasure$' -count=1 -timeout 5m -v; done   # M-14（No.72），约 6 min

# ── S4 nest-game 心跳投递对照（约 25 min，无依赖）── M-3（No.9）
for d in batch dispatch batch dispatch; do ROOST_PERF_LABEL=hbdel-$d-$RANDOM ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 \
  ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat ROOST_NEST_GAME_HEARTBEAT_DELIVERY=$d ROOST_NEST_GAME_DIRTY=5 ROOST_NEST_GAME_DURATION=5m \
  ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_EMBEDDED=1 ROOST_NEST_GAME_HZ=10 ROOST_NEST_GAME_IO_WORKERS=8 \
  ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_SHORT_QUEUE=65536 \
  ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_SYNC=1 bash scripts/perf/nest-game.sh; done   # 4 × 5 min（历史口径 15 min，按“每轮 ~10 min”缩短）

# ── S5 独立 Gate + self_sync 重档 + 出站预算演练（约 25 min，私有 NATS）──
ROOST_PERF_LABEL=gate-selfsync-$RANDOM ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SELF_SYNC=1 \
  ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 \
  ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_DURATION=15m \
  bash scripts/perf/nest-game.sh                                           # M-6（No.23、98），15 min
ROOST_TCP_BUDGET_DRILL=1 DRILL_HEALTHY=1000 DRILL_STALLED=50 DRILL_FRAME_BYTES=4096 DRILL_RATE=20 DRILL_SECONDS=120 \
  go test -run TestOutboundBudgetDrill -count=1 -timeout 10m -v ./infra/network/gateway/   # M-7（No.24），约 3 min；再加一档 DRILL_RATE=100

# ── S6 Gate 追帧演练 + 全服邮件账本（约 15 min，私有 NATS / toxiproxy / Redis + mirror-local Cluster）──
LS_DRILL_FRAMES=3000 LS_DRILL_RATES=256,64 go test -tags integration -run TestRealGateLockstepCatchupOverASlowLinkDrill \
  -count=1 -timeout 20m -v ./wiring/gate/                                   # M-10（No.30、99），读 player_tcp_write_deadline_total{deadline}
REDIS_ADDR=127.0.0.1:17379 go test -tags integration ./service/mail -run '^$' -bench BroadcastLedgerAtCapacity -benchtime 2000x -count 3
# Cluster入口已修d2457b26，仅integration helper；单机/Cluster四子项1x功能通过，非M-13正式结果。
REDIS_ADDR=$ROOST_MIRROR_LOCAL_REDIS_CLUSTER go test -tags integration ./service/mail -run '^$' -bench BroadcastLedgerAtCapacity -benchtime 2000x -count 3   # M-13（No.60）

# ── S7 Block AOI 重档复验（约 25 min，私有 NATS）── M-16（No.97）
for blocks in 0 4; do ROOST_PERF_LABEL=blockaoi-c$blocks-$RANDOM ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 \
  ROOST_NEST_GAME_LAYOUT=concentrated ROOST_NEST_GAME_OBSERVED_BLOCKS=$blocks ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_ENTITIES=10000 \
  ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_HZ=1 ROOST_NEST_GAME_DIRTY=1 \
  ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_SHORT_QUEUE=65536 \
  ROOST_NEST_GAME_DURATION=10m bash scripts/perf/nest-game.sh; done

# ── S8 effect 消费者对 RPC 的影响（约 30 min，mirror-local；先做 B-2）── M-11（No.15.2）
for r in 1 2 3; do for w in 16 1; do ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RETRY=1 ALLIANCE_E2E_EFFECT_WORKERS=$w \
  ALLIANCE_E2E_RUN='^TestAllianceE2EJoinRetryableSources$' bash scripts/test-alliance-e2e.sh; done; done   # 6 × ~4 min

# ── S9 + S10 game-demo 100 / 200 人（约 2 × 25 min，每轮新环境）── M-12（No.57、113）
# 每轮：新建私有mirror目录/端口并up/env（不clean共享或已保留数据）→ 生成game-demo、唯一命名空间、单IP上限512 → make dev-run
#   → go run ./cmd/loadtest -count 100（再 200）同秒起跑；再 -ramp-step 20 -ramp-interval 1s 与 -ramp-step 10 -ramp-interval 1s 两档爬坡
#   → 记成功数、saga“仍未知 / 等待”数、World 热点 nest: sync timeout；每档之间不复用环境（验证“上一轮 saga 积压被继承”）

# ── S11 CellWorld 千人复跑（约 15 min，私有依赖）── M-15（No.75）
ROOST_SEAMLESS_OUT=$PWD/artifacts/perf/cellworld-1000-$RANDOM ROOST_SEAMLESS_ROBOT_ONLY=1 ROOST_SEAMLESS_CAPACITY=1 \
  ROOST_SEAMLESS_ROBOTS=1000 ROOST_SEAMLESS_ROBOT_SECONDS=60 ROOST_SEAMLESS_ROBOT_ACTIVE=5 bash scripts/test-seamless-demo.sh

# ── S12 输入丢拍（约 10 min，私有依赖）── E-3（No.92），见 §E

# ── S13 生成工程持久写入对照（约 25 min；先做 B-16）── M-17（No.103、112）
#   strict / strict+SELF_SYNC=1 / pipelined 开环 / pipelined 同一实体，各5min；开环档OPEN_LOOP_RATE=1500
#   ROOST_GENERATED_TCP_SECONDS=300、每档独立ROOST_GENERATED_TCP_OUTPUT，实际参数以正式报告为准

# ── S14 B-3 实验的 A/B（约 30 min，mirror-local；先做 B-3）── M-19（No.14）
#   main 与实验分支交替各 3 轮 TestAllianceE2ERPCConcurrency（128 / 256 并发），数“全体超时簇”与停顿栈里 storeCheckpoint 的份数
```

合计约 5.3 小时（14 段，S1～S7 约 2.7 h 不依赖任何 B；S8、S13、S14 要先做对应的 B）。

## 4. C：按环境分组

**36 项仍待外部环境，不在本机关闭。现行入口、拓扑/设备缺口与关闭证据统一见 [external.md](external.md)。** 下表是原始环境分类索引，其中“运行”是当时的计划摘要；不能直接把 Remote 时长、Saga 脚本或本机进程脚本当成已具跨机/小时级装配。执行以 external 逐项核对后的入口为准。

| 环境 | 条目（ID） | 关闭它的命令 / 运行 |
| --- | --- | --- |
| **Linux 单机（独占，≥16 核为佳）** | C-L1 No.1②、No.8（心跳参数与 HB 正式测量、ticker 0.2 s 停顿） | HB-DRIVEN-LOAD §4 的 1～4 段原样（各格 ≥3 次交替），外加 M-1 的同一组 `TestDeliveryRun` |
| | C-L2 No.7（SLG） | 隔离依赖上 `ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh` |
| | C-L3 No.19（Saga） | `go test -tags integration -run 'TestRealJetStreamCoordinator\|TestRealSaga' -count=1 ./framework/saga/`（含 Kill9Replays）+ `ROOST_PERF_LABEL=linux-saga bash scripts/perf/saga-js-scan.sh` |
| | C-L4 No.27～29（Lockstep） | `go test -run TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive -count=20 -v ./framework/sync/lockstep/`，再全仓 `go test ./...` 一次看是否仍拖到 90 s |
| | C-L5 No.75（CellWorld 50 ms 判定） | ≥16 核 Linux 或 Cell / Base 分机，CELLWORLD.md“2 Cell 正式容量配置”与跨机 Robot 命令；Redis HA 另见 C-X |
| | C-L6 No.79 | `go test -race -count=20 ./gameplay/cellworld/` |
| | C-L7 No.82 | PHYSICS-ROOM-SERVICE“共享索引与多房间测量”同参数 1 / 4 / 8 房间 + RSS |
| | C-L8 No.92 | `scripts/perf/gate-mixed.sh` 100/s × 30 s × 5 |
| | C-L9 No.93 | `ROOST_NEST_GAME_TAIL=metrics` 30 min × 2（重档参数），看全局停顿与 200 ms 抖动是否随宿主消失 |
| | C-L10 No.94、103 | 独占服务器上重档 30 min（V1290 参数）与 B-16 夹具 strict 50 连接 |
| | C-L11 No.95、106 | `ROOST_NEST_GAME_TAIL=metrics` 15 min × ≥2，按方案 A |
| | C-L12 No.101、109 | NIGHT-20261022 第 4 项同参数（正向 8/s × 15 min），WAL 在普通盘与 tmpfs 各一次，加逐次 fsync 计时（`nestwal_fsync_duration`） |
| **安静独立机器 / 目标硬件（Mongo 三成员分机或分盘）** | C-Q1 No.14 | alliance-e2e 128 / 256 并发探针 8 轮，抓 L652 列的栈与指标 |
| | C-Q2 No.15.1 | 同探针 128 / 256 并发，目标 1000 次/秒 |
| **跨机 / HA（多台 Linux）** | C-X1 No.16 | 目标拓扑（Mongo / NATS R3 分机）资产守恒 + 准入限流持续负载与追平 |
| | C-X2 No.17 | 协调器、参与方、Mongo 分机，`saga-js-scan.sh` 生产推荐档各 ≥3 样本 |
| | C-X3 No.31⑦、99 | 真实移动网络 / 跨机下的快照恢复与纯追帧页 |
| | C-X4 No.46 | 多个 Gate 跨机、跨机重连 |
| | C-X5 No.58、59、102、111 | 跨机 Redis Cluster：分区 / 主节点切换丢写、容量；结算榜 ZADD GT 并发 |
| | C-X6 No.74 | T09：1w × 1w 地图、3000 / 单服、10w 人口径的容量验收；跨机 |
| | C-X7 No.81 | 多房间容量（WAN） |
| | C-X8 No.89 | EXTERNAL-VERIFICATION E01～E28 |
| | C-X9 No.90、96 | Gate / Game 多进程跨机矩阵 |
| | C-X10 No.91、104 | rehome / Player 跨机、卷迁移、客户端重连（B-1 是同机版本） |
| | C-X11 No.100 | Remote 80TPS 跨机 RTT / 故障 |
| **长稳（≥1 h 浸泡）** | C-S1 No.12① | 路由登记长跑，记守卫加载量（`ROOST_REMOTE_DURATION=1h` 口径） |
| | C-S2 No.52 | activity 宿主扫描长跑 |
| | C-S3 No.59 | 结算榜 24 h |
| | C-S4 No.82 | UGC 小时级 + 房间反复创建销毁 + 真实 Gate / NATS / TCP 下内存 |
| | C-S5 No.100 | Remote 24 h |
| **云 / 托管 Mongo** | C-M1 No.17.1 | 云主机 + 云 Mongo（RTT ~9 ms），三档各 ≥3 样本 |
| | C-M2 No.18 | `ROOST_MONGO_URI=... go run ./cmd/mongocheck`，看 getParameter 是被拒还是返回 |
| **Unity / 原生设备** | C-U1 No.31②、85 | Unity Editor / IL2CPP：C# `SHA256.Create` 与 restore 路径、`RoostSceneTime` |
| | C-U2 No.73④、74、84 | Unity / IL2CPP 跑地形 C# 测试工程与 `EntityViews` → `TerrainChunkSync` |
| | C-U3 No.80、81 | Unity 编辑器跑 UGC 新交互与分频显示 |
| | C-U4 No.83、86、88 | Windows / Linux / Android / iOS、Unreal、Godot；移动前后台、WAN；跨 Gate 的 Unity / IL2CPP / Unreal |

## 5. 不在 decisions.md 里的 D 问题

- **N1（No.35）CellWorld 业务适配进模块与 13 类玩法改成模块组合，先做哪一件？** A 先把 13 类玩法改成模块组合（每迁一类先看业务能否落进现有模块，交接 §5“配方”），CellWorld 适配后做【推荐：玩法夹具是发版门禁、收益面更广，CellWorld 仍在做容量】；B 先迁 CellWorld 业务适配（LOD / 队伍视图 / SceneReady / 刷怪 / 副本 / 地形）；C 两件都不做，写进“一”作为 demo 的范围。
- **N2（No.37）没迁到模块 runtime 的后台循环（activity_world / activity_alliance 驱动、login 的 EnterStatus 协程、cellworldhost、rehome 静默 ticker）怎么定契约？** A 把“后台工作放在 App Mod 或 `runtime:`，停机按 Mod 逆序”定为契约写进“一”，不另做 ingress 登记【推荐：符合“App 级统一保证”原则，量小】；B 给模块加 App 级 ingress 登记 / 超时 / 排空契约并逐个迁移（L）。
- **N3（No.64）玩家换服后，名下建筑与全服邮件归哪个服？** A 建筑留在原地图（归地图所在服，随 Q17 的地图归属），全服邮件按新服范围重新拉取（交接 §5 已写“离开前按旧范围拉一次”）【推荐】；B 建筑随玩家迁移；C 都不处理，写进“一”。与 Q17 一起定。
- **N4（No.73③）原始地形包里的坏块、分区场景里拥有者装不进的块，客户端永远等不到确认，怎么办？** A 按基础包确认（不改协议）【推荐：零协议成本，坏块本来只能按基础包显示】；B 新增“不可用”标记（改协议与 C#）。

另：B-3（No.14）的实验结果出来后会多一个问题——“是否把 checkpoint 落盘移出投影器主循环合入 main”，届时补问，现在不列。
