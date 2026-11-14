# 交接：v1.47.0 之前要收口的遗留（2026-11-14）

给接手的 agent。目标是把 [KNOWN-LIMITS](KNOWN-LIMITS.md)“二、遗留问题”清到只剩**框架限制**与**外部环境验收**，然后由维护者拍板剩下的决定，再发 v1.47.0。

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
    - 要改动数据或做故障注入，用自己的私有环境：`scripts/mirror-local.sh`，`ROOST_MIRROR_LOCAL_OFFSET` ≤ 38135。先 `lsof` 确认端口空闲：12500 / 14500 / 23000 / 24000 / 26000 / 28000 / 31000 都被别的 agent 用过。跑完 `clean`。
    - 生成的 game-demo 接测试环境时，`dataengine.effects.max_bytes` 与 `saga.stream_max_bytes` 要降到 ≤ 512 MiB。
15. **收尾清理**：每个 agent 结束后 `ps` 找残留的测试二进制、私有环境、`yes` 负载进程并停掉。删 worktree 之前先把 artifacts / pprof 复制出来。

### 分支、提交与文档

16. **在 feat/v147 上工作**：每批从 feat/v147 开 `tmp/<批名>` 分支和 worktree，做完 `--no-ff` 合回 feat/v147。不要在别的 agent 的 worktree 里改文件或做 git 写操作。
17. **并行上限 4 个 agent，不嵌套**；各批改的文件互不相交（remaining.md §2 已按文件分好）。共用文件 `integration_coverage_test.go` 和 CHANGELOG `[Unreleased]` 只追加。
18. **KNOWN-LIMITS 只由最后一个文档批改**：各批只在报告里给出“关闭 / 改写成什么”的原文，最后一次性写回。这样并行批之间不冲突。
19. **不推送、不发版**：v1.47.0 只在维护者明确说“发 v1.47.0”后发（合 main、写 NOTES、`scripts/release.sh v1.47.0`，打 tag 之后单独一个提交把最低 core 提到 v1.47.0）。删除操作（worktree、分支、数据、缓存）要维护者同意。
20. **安装外部工具要维护者同意**（protoc 已于 2026-11-14 同意并装好，libprotoc 36.2）。

## 二、未做的事

数字是 remaining.md 里的编号，详细做法、验证与量级以 remaining.md 为准。

### 1. 本机可修（B）——按批次，可并行

| 批 | 内容 | 量级 | 先决 |
| --- | --- | --- | --- |
| P1 | 换服真实进程演练 `scripts/test-demo-rehome.sh`：六个时间点 `kill -9` 后续做，核对单一所有者、计数不丢、源库删除（B-1） | L 5～6 h，可拆两段 | 无 |
| P2 | 联盟大成员数占用与 256 并发探针的开关（B-2）；nestwal / 投影器 checkpoint 后台写者实验（B-3，只在实验分支，不合并，结果交维护者） | 4 h + M-19 | 无 |
| P4 | C++ 两个 Gate 同一 SessionID 先 A 后 B 的端到端（B-14）；C++ protobuf ctest（B-15，protoc 已装：`ROOST_PROTOBUF_TESTS=ON bash scripts/test-client-cpp.sh`） | 3 h | 无 |
| P5 | MOBA 宿主与权威分进程的快照恢复、真实 strict WAL 提交延迟分布（B-4）；2026-10-20 / 22 一次性持久写入夹具入库 `scripts/perf/generated-tcp.sh`（B-16） | 6 h，建议拆两批 | 无 |
| P6 | 登录线模块真实依赖：两个 Gate、game 重启后重连、account 两副本、enter + 真实 naming、servertime 经真实 TCP、casual 配方（B-6） | L 4～5 h | 无 |
| P7 | 社交模块真实依赖：team 两进程 + 弱网、matchmaking 联合验收、alliance 双 sid 迁服 `kill -9`（B-7） | L 4～5 h | 与 P2 同目录不同文件 |
| P8 | 运营模块真实依赖：gm、purchase（跨 sid、Redis 重启补领）、activity 跨进程（B-8） | L 5 h | 等进行中的 mailbox、activity_world 合入 |
| P9 | room / turn 联合验收与 Robot 步骤、turn-pvp 配方（B-9）；合作 Roguelike、异步社交经营加 `_REAL` 档（B-5） | 2 × 4～5 h | 等进行中的回合超时（#200）合入 |
| P10 | 地形编辑 / 放建筑后 `kill -9` 从 WAL 恢复（B-11） | 3 h | 等进行中的 #146 合入 |

### 2. 未确认的偶发（E）

E-1 AOI 微基准慢 7～11%、E-2 C++ 弱网用例、E-3 Gate 丢拍来源：复现 / 否证办法见 remaining.md §E。E-1 放在 M 序列 S1 里跑。

### 3. 空闲机器上的测量（M）

remaining.md §3，14 段，每段 ≤ 30 分钟，共约 5.3 小时，可分几天跑。另有各批报告里留下的测量命令：nest-game 心跳投递对照、独立 Gate + self_sync 重档、Block AOI 集中布局与扇出重档、类型调度 sync-business 对照（`ROOST_SYNC_SCHEDULE=periodic|on_change`）、游戏尺度分区视野（`ROOST_CELLWORLD_PERF_SCALE=game`）、机器人 100 / 200 人同秒与爬坡（每轮新环境）、CellWorld Robot 在 A+D 之后复跑。跑完把数字写回对应条目，“没测”改成结论。

### 4. 文档批（最后做，只有它改 KNOWN-LIMITS）

- remaining.md §A：31 段移到“一”，目标小节已写好。
- remaining.md 表里的 F：17 处删除或改写（各有证据）。
- 回写各批和 M 的结果、关闭的条目移到“三、已修复的历史”（一行结论）。
- 小节清空后删掉只做导航的引言段。
- 跑 `python3 scripts/check-doc-anchors.py` 和根包 `go test -count=1 .`。

### 5. 外部环境（C）——只列清单，不在本机做

remaining.md §4，按 Linux 单机、安静独立机器、跨机 / HA、长稳 ≥1h、云 / 托管 Mongo、Unity / 原生设备分组，每项写了关闭它的命令。KNOWN-LIMITS 里这些条目保留，写明环境与命令即可。

### 6. 维护者决定（D）——不要动

decisions.md 的 Q1～Q17、#154、#37、CellWorld B / C / E、self_sync RetryLater（分支 `tmp/sync-fixes` 未合并；remaining.md §三第 1 条：放大已在 11-14 演练中确认会自我维持），以及 remaining.md §5 的 N1～N4。另有两条小的：合服回退要不要真正统计“合服期间新增”（现在按选项 C：保持行为、说明已改对）；超额降级放宽到 `MaxVisible` 即删记录、丢掉冷却历史（现已记为限制）。

## 三、交接时仍在进行的（不要重复做）

由交接前的会话收尾、合进 feat/v147：

- **夹具与模块**（分支 `tmp/leftovers-fixtures`）：无缝演练加放建筑并核对进别的 Cell / Base 的主视野（#146）、turn-pvp 回合超时（#200）、mailbox 经 BusClient + Redis 的真实依赖用例、activity_world 换表确认、两条偶发（game-demo 主视野超限、地形转发编辑 `-race`）的负载循环。
- **小件**（分支 `tmp/leftovers-p3`）：出生号与运行期号共用 shard 位（运行期序号上限改为 `1<<33 - 1`）、cellworldhost Runtime 的 Start / Stop 回归、C# 地形确认用真实 entitysync 帧驱动。

这两批合入后，P8 / P9 / P10 的先决条件才满足。合入情况看 `git log --oneline feat/v147`。
