# 交接：v1.47.0 之前要收口的遗留（2026-11-14）

**续跑交接**：用户已要求转交后续验收，详细状态、证据与命令见 [最终验证与性能交接](HANDOFF-2026-11-14-v147-validation-performance.md)。代码工作与功能矩阵已完成；生成器整包复验已于 `d0d5a8b7` 补跑通过（406 顶层 PASS，见[最终验收报告](evidence/v147-final-validation-20261114/README.md)）；M-1～M-19、E-1 / E-3 正式测量均未运行。维护者已答复 decisions.md 的 D1～D26，按答复实施的状态见[第四节“进度”](#进度2026-11-14-文档批之后)。

给接手的 agent。本轮状态以本节续跑交接、第二节和 remaining 当前状态为准。目标是把 [KNOWN-LIMITS](KNOWN-LIMITS.md)“二、遗留问题”在本机任务实证完成后收敛到**框架限制**与**外部环境验收**，然后由维护者拍板剩下的决定，再发 v1.47.0。

- 分支：`feat/v147`（worktree `/Users/whb/roost/worktrees/v147`），交接时 HEAD 见本文件所在提交。没有合进 main，没有打 tag。
- 逐条收口计划（114 条，含每条的类别、做法、验证与量级）：[remaining.md](evidence/known-limits-remaining-20261114/remaining.md)。表里的行号对应 HEAD `28055f06` 的 KNOWN-LIMITS，按标题找，不要按行号。
- 待维护者决定的问题：[decisions.md](evidence/known-limits-remaining-20261114/decisions.md) 加 remaining.md §5 的 N1～N4。**不要替维护者做这些决定**，也不要按推荐项先实施。

## 一、做事准则

### 范围与结论

1. **遗留必须清零**。每一条要么修掉（附证据），要么确认是设计边界、移到“一、框架本身的限制”，要么写明需要的外部环境与关闭它的那次运行。不允许新增“以后再说”的遗留；新发现的问题当轮给修复方案或给维护者的问题。
2. **限制与遗留分开**：框架本身的设计边界写进“一”，没做完、没验证的写进“二”。
3. **“尚未确认”不能随手写**：偶发失败要用循环复现确认或否证（在负载下跑足次数，修前修后各一组）；不能把失败的断言说成“预期”，没读断言之前不下结论；不能在一个会通过的规模上验收。
4. **不提 WANTED、不强行关闭**：自己能查的先查到代码级根因；查不到就如实写“尚未确认”和下一步。
5. **没有生产部署，不做兼容**：存储、协议、生成物的改动不为旧进程 / 旧数据 / 旧生成工程留兼容，CHANGELOG 写明“重新生成”。
6. **三大块优先**：nest 调度、dataengine、sync 是 core 的基础，改动以“三块各自局部聚集”为准，少分包、易读 Go、中文契约注释（见 roost-coding 规范）。
7. **模块与配置**：单实例 / 存活 / fail-stop 在 core app 统一做一次，模块继承；配置手写胶水最少、规则在运行期加载时统一检查一次；demo 是参考实现，不迁到生成器产物上。

### 证据

8. **先红后绿**：能构造就先写修前失败的用例；修后绿；再做至少一个变异（去掉修复或关键判断）证明用例能抓住它。证据（日志、JSON、复现命令）放在 scratchpad，结论与复现命令写进 CHANGELOG `[Unreleased]` 和对应契约文档。
9. **真实依赖用例要登记**：新的 `-tags integration` / 真实进程用例登记到根包的真实依赖入口清单（`integration_coverage_test.go`），否则根包测试会红。
10. **改了夹具或脚本，要跑一次真实脚本**，不能只跑编译。2026-11-14 改 alliance-e2e 夹具只编译没实跑，导致整套演练挂到 60 分钟超时。

### 测试节奏与机器

11. **每步只跑定向检查**，慢套件最后统一跑一次：`go test ./...`、`scripts/test-modules-generated.sh`、各玩法脚本、真实依赖档（`ROOST_*_REAL`）、demo / Remote / 联盟 / 无缝真实进程演练、`go generate ./...` 后 `git status` 为空。
12. **开跑前看磁盘**：`df -h /System/Volumes/Data`；空间不足 50 GiB 先看 `du -sh ~/Library/Caches/go-build`，清缓存要维护者同意。多 agent 编译时用 `GOWORK=off GOFLAGS=-p=2`。
13. **性能测量**（remaining.md §3 的 M 序列）只在机器空闲时跑，每段 ≤ 30 分钟、整轮约 30 分钟一段；开始前记 `sysctl -n vm.loadavg` 和 swap。CorpLink 常驻约 1 核，load 2～4 视为空闲。有别的 agent 在编译 / 跑 race 时不测。
14. **依赖环境**：
    - 共享环境 `~/.roost-it`（偏移 1000：Mongo 28117-28119、NATS 15222-15224、Redis 17379）只用来跑测试，不 reset、不批量删键或流。mongo-3 的端口 28119 被 BSPrintNotify 占着时，按脚本里的同一条 `mongod` 命令手工起 mongo-3，再跑 `up`。
    - 要改动数据或做故障注入，用自己的私有环境：`scripts/mirror-local.sh`，`ROOST_MIRROR_LOCAL_OFFSET` ≤ 38135。先 `lsof` 确认端口空闲：12500 / 14500 / 23000 / 24000 / 26000 / 28000 / 31000 都被别的 agent 用过。up/env/用例/down 使用同一受控生命周期，只停自己的进程；保留数据和缓存，不运行 clean/reset 共享或已保留环境。
    - 生成的 game-demo 接测试环境时，`dataengine.effects.max_bytes` 与 `saga.stream_max_bytes` 要降到 ≤ 512 MiB。
15. **收尾清理**：每个 agent 结束后 `ps` 找残留的测试二进制、私有环境、`yes` 负载进程并停掉。删 worktree 之前先把 artifacts / pprof 复制出来。

### 分支、提交与文档

16. **在 feat/v147 上工作**：每批从 feat/v147 开 `tmp/<批名>` 分支和 worktree，做完 `--no-ff` 合回 feat/v147。不要在别的 agent 的 worktree 里改文件或做 git 写操作。
17. **并行上限 4 个 agent，不嵌套**；各批改的文件互不相交（remaining.md §2 已按文件分好）。共用文件 `integration_coverage_test.go` 和 CHANGELOG `[Unreleased]` 只追加。
18. **KNOWN-LIMITS 只由最后一个文档批改**：各批只在报告里给出“关闭 / 改写成什么”的原文，最后一次性写回。这样并行批之间不冲突。
19. **不推送、不发版**：v1.47.0 只在维护者明确说“发 v1.47.0”后发（合 main、写 NOTES、`scripts/release.sh v1.47.0`，打 tag 之后单独一个提交把最低 core 提到 v1.47.0）。删除操作（worktree、分支、数据、缓存）要维护者同意。
20. **安装外部工具要维护者同意**（protoc 已于 2026-11-14 同意并装好，libprotoc 36.2）。

## 二、当前状态与未做的事

数字是 remaining.md 里的编号，详细做法、验证与量级以 remaining.md 为准。

### 1. 本机批次（B）——已实施的范围

P1、P2 正式探针、P4～P10 的本机功能工作已合入 feat/v147；不重复实施整批。逐 B 编号、精确证据和未关边界见 [remaining 当前状态](evidence/known-limits-remaining-20261114/remaining.md#当前收口状态2026-11-14)。重点：

- P1 嵌入式玩家 TCP 上六个换服 SIGKILL 点各两次，不含独立 Gate；committed 后源库旧档明列 Q4，需人工恢复，不能写成登录自动清理或旧副本定时器已全部隔离。
- P7 两 game Team / 弱网、匹配排队恢复、联盟双 sid 迁服已验；修复漏装 OnTeamJoining。#154 / Q4 未关。
- P8 三条真实绿、两变异红/复绿；活动使用由 casual 扩展、共 18 模块的独立工程，完整 demo 的 PlayerDao 64 字段上限仍保留；不是 WAL-only。
- P9 turn-pvp 真实账号 / Gate / 存储联合链与合作 / 经营直接 Sender 的 WAL-only 恢复是不同档，不能合称全部玩法都有联合链。
- P2 的 B3 后台 checkpoint 仅实验分支 `300be3a0`，实现与配置不合；M-19 报告到齐后再记录实验结论。

本轮代码工作与功能矩阵已完成，模块、13 玩法默认与 REAL、真实脚本组的准确计数统一见[最终验收报告](evidence/v147-final-validation-20261114/README.md)。`go generate ./...` 已在 `fef75aee` 退出 0，生成后 diff / status 均为空。

生成器整包复验仍待完成。此前 30m 重试除总超时外，还出现六处 shutdown 预算旧断言失败（实际 28 Mod、预期 27）；[断言修复](evidence/shutdown-assertions-20261114/README.md)已合，31 项定向测试及关键变异红绿通过。修复后的最后一轮 60m 整包复验因本次交接主动中止，不能计为通过或测试超时。接手后沿用现行 pretag 的 `-timeout 60m` 预算补跑，原始日志与续跑入口见[新交接](HANDOFF-2026-11-14-v147-validation-performance.md)。

### 2. 未确认的偶发（E）

E-1 AOI 微基准与 E-3 Gate 丢拍的正式测量均未运行，办法见 remaining.md §E。E-2 原版 200 轮 / 1000 场景全绿，实际 max load 10.39；只是该窗口未复现，没有定位 / 修复，也没覆盖历史 load 40～55 条件，见[原生弱网调查](evidence/v147-native-weak-20261114.md)。

### 3. 空闲机器上的测量（M）

**M-1～M-19 正式测量均未运行**；统一报告位置为 `docs/performance/benchmarks/v147-remaining-20261114/README.md`，收到实测后逐项回写。remaining.md §3，14 段，每段 ≤ 30 分钟，共约 5.3 小时，可分几天跑。另有各批报告里留下的测量命令：nest-game 心跳投递对照、独立 Gate + self_sync 重档、Block AOI 集中布局与扇出重档、类型调度 sync-business 对照（`ROOST_SYNC_SCHEDULE=periodic|on_change`）、游戏尺度分区视野（`ROOST_CELLWORLD_PERF_SCALE=game`）、机器人 100 / 200 人同秒与爬坡（每轮新环境）、CellWorld Robot 在 A+D 之后复跑。跑完把数字写回对应条目，“没测”改成结论。

### 4. 文档批（最后做，只有它改 KNOWN-LIMITS）

- remaining.md §A：31 段已按现行证据迁入“一”；A-5 快照、A-15 配方、A-23 换服前检查、A-25 归属、A-31 离散玩法修正了旧措辞。
- remaining.md 表里的 F：17 处已删除或改写，当前结果表保留出处；不删除仍未答的决定。
- 已回写各批功能结果，保留 M 未运行状态；后续按实测回写，关闭的条目移到“三、已修复的历史”（一行结论）。
- 本次交接只做轻量 diff 检查。生成器整包复验与 M 完成后，接手者再运行 `python3 scripts/check-doc-anchors.py` 和根包 `go test -count=1 .`，核对最终文档；性能期间不跑编译、race、索引或大扫描。

### 5. 外部环境（C）——只列清单，不在本机做

[external.md](evidence/known-limits-remaining-20261114/external.md)按 Linux 单机 12、安静独占机器 2、跨机/HA11、≥1h 长稳 5、云 Mongo2、Unity/设备 4 项分组，列现行入口与仍缺的拓扑/夹具/手工验收以及关闭证据。用户暂无生产部署，本轮 36 项保留，不用宿主本地进程冒充跨机。

### 6. 维护者决定（D）——不要动

decisions.md 的 Q1～Q17、#154、#37、CellWorld B / C / E、self_sync RetryLater（分支 `tmp/sync-fixes` 未合并；remaining.md §三第 1 条：放大已在 11-14 演练中确认会自我维持），以及 remaining.md §5 的 N1～N4。另有两条小的：合服回退要不要真正统计“合服期间新增”（现在按选项 C：保持行为、说明已改对）；超额降级放宽到 `MaxVisible` 即删记录、丢掉冷却历史（现已记为限制）。

## 三、交接时的先决批（已合入，不要重复做）

原交接前的批次已收尾、合入 feat/v147：

- **夹具与模块**（分支 `tmp/leftovers-fixtures`，**已合入** `cacf0d96`，KNOWN-LIMITS 已回写）：无缝演练加放建筑并核对进别的 Cell / Base 的主视野（#146）、turn-pvp 回合超时（#200）、mailbox 经 BusClient + Redis 的真实依赖用例、activity_world 换表确认、两条偶发（game-demo 主视野超限、地形转发编辑 `-race`）的负载循环。
- **小件**（分支 `tmp/leftovers-p3`，**已合入**，KNOWN-LIMITS 已回写）：出生号与运行期号共用 shard 位（运行期序号上限改为 `1<<33 - 1`）、cellworldhost Runtime 的 Start / Stop 回归、C# 地形确认用真实 entitysync 帧驱动。

夹具与模块的先决条件已满足，后续 P8 / P9 / P10 已完成各自本机验收。重复运行期号分配器已由 P10 删除；monster 场景数与整套生成模块的最终计数由统一验收报告确认。当前接续工作包括生成器最后一轮 60m 整包复验、尚未运行的 M-1～M-19 / E-1 / E-3，以及最终文档检查；`go generate ./...` 已完成，C 与 D 保留。详见[最终验证与性能交接](HANDOFF-2026-11-14-v147-validation-performance.md)。

## 四、维护者答复后要实施的（2026-11-14）

答复见 [decisions.md“维护者答复”](evidence/known-limits-remaining-20261114/decisions.md#维护者答复2026-11-14)。准则同第一节（先红后绿、变异、真实依赖用例登记、最多 4 个 agent、KNOWN-LIMITS 由最后的文档批统一改）。按文件互不相交分批：

| 批 | 决定 | 内容 | 主要文件 | 量级 |
| --- | --- | --- | --- | --- |
| R1 Sync | D1 | 已做：`tmp/sync-fixes` 的两个提交 cherry-pick 为 `2e1486b4`、`3c48c5f2`（分支 tmp/r1-sync）；两条回归与 5 个变异重跑；impl/04 §2C、ENTITY_SYNC、guide/04 按新行为（从被拒会话轮转）改写 | `framework/sync/entitysync/**` | S～M |
| R2 换服 / account | D7、D8 | D7：源服预加载时向 account 核对绑定，不属于本服的跳过并带版本守护删除；P1 的 committed 点验收这条窗口关闭。D8：account 加在途换服索引，按期限自动向前恢复或中止，`accountctl` 能列出；真实依赖用例 | `modules/rehome/**`、`service/account/**`、`wiring/` 相关 | M～L |
| R3 地形 | D11、D12 | Q17 A：按地图归属选库范围（普通场景 sid、分区场景 global），demo 的 `terrain_chunks` / `buildings` / `terrain_editors` 与 terrain 模块一致；N3 A：换服时建筑留在原地图，全服邮件离开前按旧范围拉一次；N4 A：坏块 / 装不进的块按基础包确认 | `demo/**/terrain*`、`modules/terrain/**`、`demo/game/controllers/player` 换服段 | M |
| R4 DataEngine / Remote / Saga | D16、D17、D18 | D16：原生正向档推荐配置开启准入限流，c128 追平复测 ≤ 30s；D17：“删空认领”运维入口（所有权代际守护）+ 累积计数指标；D18：缺省 WAL 目录 `data/wal/<服务>/<sid>`，更新生成配置、doctor 与文档（未上线不兼容旧目录） | `framework/saga` 配置与文档、`framework/remoteentity` / `wiring/remoteentity`、`codegen/internal/roost` 的 WAL 缺省 | M |
| R5 模块配置 | D19 | 模块清单可声明配置结构，安装器接入 Mod 配置声明，`roost config check` / `--check-config` 能查；先迁 profile、stamina、leaderboard、mailbox 的常量与 portal.servers | `codegen/internal/roost` 安装器与清单、相关 `modules/*` | L |
| R6 后台 manager | D22 | activity_world / activity_alliance 驱动、login 的 EnterStatus 协程、cellworldhost、rehome 静默 ticker 改成 manager，代码写在 `game/managers`，在 `game/managers/managers.go` 登记，由 manager Mod 统一启停；契约写进“一” | `modules/{activity_world,activity_alliance,login,cellworld,rehome}/**`、生成器的 managers 登记 | M～L |
| R7 portal / room | D23、D24 | D23：game 经总线定期上报到 account 的带过期注册表，portal 读它；D24：room 自动断线订阅与重启后在线状态重建（广播、R11 写进“一”） | `modules/{portal,room}/**`、`service/account` 注册表 | M～L |
| R8 game-demo | D25、D26 | D25：demo 的新玩家初始化搬进 `enter_first.go`，保留 demo 改写的 EnterGame；D26：副本 / 活动奖励加金币 | `demo/**` | M |
| R9 玩法模块化（进行中） | D21 | 13 类玩法逐类改成“装模块 + 只写模块没覆盖的部分”（按 compose.md），每类保持场景数与 REAL 档全绿；CellWorld 业务适配进模块排在后面 | `scripts/testdata/<genre>/**`、`modules/**` | L（逐类） |
| 文档批 | D2～D6、D9、D10、D14、D15、D20 及各批结果 | D2～D5、D9、D10、D14 写进“一”；D6 确认后移到“一”；D15 关闭（#37）；D20 发版清单加“上线前冻结协议号 / 错误码”；各批关闭的条目回写 | `docs/maintenance/KNOWN-LIMITS.md`、发版清单 | M |

不在本表：D13 先跑 M-15（性能测量）再定；后台 checkpoint 等 M-19。R2 与 R3 都碰换服，R3 只改 demo / terrain 的换服段，R2 改 rehome 模块与 account，开工前对一下文件。

### 进度（2026-11-14 文档批之后）

已完成并合入 feat/v147（没有合 main、没有打 tag）：

- **R1**（D1）：`9ee49da5`。
- **R4 的快速部分**（D16、D18）：`5fb152f3`。D16 开准入后的云 Mongo c128 追平复测没有跑（需空闲机器 + `ROOST_SCAN_MONGO_URI`，命令见 KNOWN-LIMITS“Saga：遗留”）；R4 里的 D17 未做。
- **R8**（D25、D26、D12）：`018f2da2`。D25 按选项 A 只把出生点写进 `enter_first.go`：demo 原来就没有初始道具 / 金币发放，钩子里也不发。
- **文档批**（分支 `tmp/v147-docs`）：kl-wording 的 R1 / R8 / R4 原文写回 KNOWN-LIMITS；D2～D6、D9、D10、D14 写进“一”，D15 关闭，D20 加进[维护手册“上线前冻结模块协议号 / 错误码”](README.md#上线前冻结模块协议号--错误码d20)；已决定、排在后面的条目在“二”改标“已决定（D#，2026-11-14），v1.47.0 之后实施”；写了 [v1.47.0 NOTES](../release/v1.47.0-NOTES.md)。

v1.47.0 发版之后仍要做的：

1. **R2**（D7、D8）：源服预加载核对 account 绑定并带版本守护删旧档；account 在途换服索引、按期限自动向前恢复或中止、`accountctl` 列出。
2. **R3**（D11 = Q17 A + N3 A）：地形 DAO 按地图归属选库范围（demo 与 terrain 模块一致）；换服时建筑留在原地图，全服邮件离开前按旧范围拉一次。
3. **R4 剩余**（D17）：“删空认领”运维入口（所有权代际守护）+ 累积计数指标。
4. **R5**（D19 B）：模块清单声明配置结构、安装器接入 Mod 配置声明。
5. **R6**（D22）：后台循环改成 `game/managers` 里的 manager。cellworldhost 现在特意不做成 manager（Node 要在实体心跳装好之后启动），实施时先解决启动先后。
6. **R7**（D23、D24 A）：portal 读 account 的带过期注册表；room 自动断线订阅与重启后在线状态重建。
7. **R9**（D21）：13 类玩法逐类改成模块组合，之后 CellWorld 业务适配进模块。
8. **等测量再定**：D13（M-15 之后，真要动先 C）、后台 checkpoint（M-19 之后）。
9. **测量与外部**：M-1～M-19、E-1 / E-3 正式测量；36 项外部环境验收（external.md）。
10. **发版**：维护者说“发 v1.47.0”后合 main、`scripts/release.sh v1.47.0`（不加 `--min-core`），打 tag 之后单独一个提交把最低 core 提到 v1.47.0。
