# v147 接续交接：代码已完成，继续验证与性能（2026-11-14）

这是当前接手入口。维护者在本轮询问代码状态后，明确改为“让另一个 agent 处理后续的工作，写好交接文档”。因此已停止正在运行的生成器整包复验，未启动正式性能测量；不是测试已经全部通过，也不是取消原清单。

## 1. 先确认工作位置和范围

- 集成分支：`feat/v147`，工作树 `/Users/whb/roost/worktrees/v147`。**不要在 `/Users/whb/roost/roost-core` 的 main 上继续本批，也不要直接发版。** 当前最后代码/测试集成点为 `aff286349fb1bca215e8aa761665826d03628389`；之后的交接与报告提交以本文件所在分支的 `git log` 为准。
- 计划内代码和功能夹具已完成并合入。P1、P2 正式探针、P4～P10 及先决批不必重新实施。逐条对应、边界与证据见 [remaining 当前状态](evidence/known-limits-remaining-20261114/remaining.md)；原始纪律见[总交接](HANDOFF-2026-11-14-v147-leftovers.md)。
- **例外 B3：后台 checkpoint 只在实验分支** `experiment/v147-background-checkpoint`，工作树 `/Users/whb/roost/worktrees/v147-background-checkpoint`，当前 `300be3a0`。实验代码、配置和诊断信号处理器都不得合进集成分支；只交回 [实验报告](evidence/checkpoint-experiment-20261114/README.md)，M19 后由维护者判断。
- 不 push、不合 main、不打 tag、不发版。每批独立工作树，完成后由主 agent 串行 `--no-ff` 合回 `feat/v147`。用户已允许最多 4 个 agent 并行，不嵌套；正式性能期间所有其他 agent 停止编译、测试、race 和索引。
- Q1～Q17、N1～N4、Q4、#37、#154、CellWorld B/C/E、self_sync RetryLater 等未回答事项保持原样，不把实施授权解释为代替维护者选择。见 [decisions](evidence/known-limits-remaining-20261114/decisions.md) 和 remaining §5。

```sh
cd /Users/whb/roost/worktrees/v147
git branch --show-current
git status --short
git log -8 --oneline
export GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0
df -h /System/Volumes/Data
```

本机为 macOS arm64、Apple M5、10 逻辑 CPU、32 GiB，Go 1.27.3。交接前可用磁盘约 422 GiB；仍应重新检查。不要清理缓存、保留数据、实验工作树或分支。

## 2. 已完成验证，不要整套重复

权威计数、原始路径、失败及保留范围统一见[功能验收与交接状态](evidence/v147-final-validation-20261114/README.md)及其 `summary.json`。

| 项目 | 已有结果与限制 |
| --- | --- |
| 根 Go 初轮 | 165 包通过，25 包无测试；生成器 CLI 包触发缺省 10 分钟总超时。不能记为全仓 exit 0。 |
| 模块安装矩阵 | 42 模块 + 47 联动，5 个消费者、11 份 verify JSON；正序/逆序全安装各 89 项、1027 次 PASS，0 SKIP，build/vet/ID/生成漂移均过。跨阶段合计 3341 是重复执行数。 |
| 13 玩法默认/自定义 module | 13/13 脚本通过，各 182 次顶层 PASS，0 SKIP。首次沙箱 cache 写失败和重跑结果均保留。 |
| 13 玩法 REAL | 13/13 脚本通过，208 次顶层 PASS = 默认 182 + REAL 阶段 26；不是 208 个独立真实恢复场景。默认网络档与独立存储档不能拼成所有玩法的账号/Gate/存储联合验收。 |
| demo / Remote / alliance | demo 14 PASS；Remote 18 PASS + 3 个明确专用子进程/独立入口 SKIP；alliance 功能档 11 PASS。没有替代 Remote 完整故障矩阵。 |
| seamless | 默认 15 STEP，普通场景 Robot 2/2；地形档 7 STEP。小规模功能结果不是容量。 |
| `go generate ./...` | `fef75aee` 上 exit 0；生成前后 HEAD 相同，独立 `git diff --exit-code` 为 0、status 为空。之后 `aff28634` 只改测试/证据。除非继续改生成输入，不必为文档再跑一次。 |
| E2 原生弱网 | 原版 200 轮、1000 场景均通过；实际 max load 10.39，未覆盖历史 load 40～55。仅说明该窗口未复现，没有定位/修复历史偶发。见 [E2](evidence/v147-native-weak-20261114.md)。 |

两处最后发现并已处理的问题：

1. **M13 邮件 Cluster 探针**：逗号种子被当成单地址。`d2457b26` / merge `fef75aee` 修 integration helper，真实 Cluster 修前红、修后四子项绿、单机回归绿、原二进制负对照红。生产驱动不变，1x 功能验证不算容量。[证据](evidence/mail-cluster-probe-20261114.md)。
2. **Room timer 停服预算旧断言**：P9 新增合法 Timer Mod，精简测试工程应为 28 Mods、126s/131s，CLI 默认另有 manager，为 29 Mods、129s/134s。`21c7183f` / merge `aff28634` 只修测试和证据；31 项相关用例通过，少计一 Mod 的变异红、还原后绿，保留独立 bootstrap 计数和固定算式。[证据](evidence/shutdown-assertions-20261114/README.md)。

真实组有一项保留限制必须传下去：offset18500 的私有镜像被该组执行者误用 `mirror clean` 删除；只涉及这次新建私有根，日志提前复制保存，共享数据未动。它的 DB 文件无法再查，不能写“全部原始数据库均保留”。其余保留环境用 `down`，标准测试对自己唯一前缀的清理另计。

## 3. 第一步：补完生成器整包（2026-11-14 已完成）

**已于 `d0d5a8b7` 补跑通过**：exit 0，1950.786s，406 顶层 PASS、0 FAIL；环境开关跳过的 4 个与结论见[功能验收报告](evidence/v147-final-validation-20261114/README.md)。下面是补跑前的记录，保留不改。

三个历史运行不能混为一个结果：

1. 初轮 `go test ./...` 缺省 10m 超时，其余包通过。
2. 当时参考旧维护 README，单包用 30m 重跑：出现 6 个停服预算旧断言失败，并最终超时；定向补测又发现第 7 个同源旧断言。现已由上面的修复处理。现行 `scripts/pretag.sh` 本来是 **60m**；维护 README 已同步修正。
3. 修复后在 `aff28634` 上以 `-count=1 -timeout 60m -v` 重跑，2026-11-14 **05:00:00 UTC** 开始。维护者要求交接后，**05:12:46 UTC** 主动停止本轮进程；已执行约 766s、123 个顶层 PASS，没有观察到失败，正在 `TestModuleMatchroomGenerated`。**未完成，不能写通过。** wrapper 原始 exit=-15 / shell 241 是 SIGTERM，不是新增实现失败。

已核对并停止自己启动的 PGID 19153 全部成员，最终无残留；没有后台性能任务、负载进程或本轮私有依赖在等接手。原日志与元数据：

```text
/private/tmp/v147-final-go-test-20261114.log
/private/tmp/v147-final-go-summary.json
/private/tmp/v147-final-roost-retry-20261114.log
/private/tmp/v147-final-roost-60m-20261114.log
/private/tmp/v147-final-roost-60m-20261114.json
/private/tmp/v147-final-roost-handoff-stop-20261114.json
/private/tmp/v147-final-go-generate-20261114.log
/private/tmp/v147-final-go-generate-20261114.json
```

下一位只需重跑这个包，不重复已通过的 165 包和三套生成矩阵。换新日志名，勿覆盖已有材料：

```sh
cd /Users/whb/roost/worktrees/v147
GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 \
  go test -count=1 -timeout 60m -v ./codegen/internal/roost \
  > /private/tmp/v147-roost-final-resume-UNIQUE.log 2>&1
```

将 `UNIQUE` 换成本次唯一标识，记录命令、HEAD、开始/结束时间、退出码与最后失败。42 模块规模下，历史整包约 34～36 分钟；60m 是包级预算，不是单个用例可以挂一小时。出现失败先查真实断言，不再盲目加时限或改生产逻辑迎合旧数字。

## 4. 第二步：正式性能测量（全部未开始）

**M1～M19、E1、E3 和 B2 大名单正式测量均 NOT RUN。** 已有小规模功能 smoke、二进制准备、E2 结果不能填入这些正式格子。

详细准备、可复建来源、调度器与命令见[性能准备交接](evidence/v147-performance-preparation-20261114/README.md)；正式结果汇总入口为[本轮性能报告](../performance/benchmarks/v147-remaining-20261114/README.md)。原清单在 remaining §3。当前拆为 **16 段、117 个调用**，总计划约 5 小时；每段连依赖启动和收尾一起 ≤30 分钟，超时/失败后只续未完成项。

| 段 | 内容 |
| --- | --- |
| `s1-micro` | M2 移动、M9 关系视图、M18 组视图；E1/M5 三构建交替 10 轮 |
| `s2-heartbeat` | M1 回调 0/5/10/20µs × 热点 0/64；group2/4 × slice50/100µs 诊断构建 |
| `s3-sync` | M4 periodic/on_change；M8 16 视野 sync/async；M14 CellWorld demo/game |
| `s4-nest-game` | M3 batch/dispatch 各两轮 5m，1000 玩家/10000 Entity/每人10条消息/10Hz/5%变化 |
| `s5-gate-budget` | M6 独立 Gate+self_sync 15m；M7 健康1000/停读50、4096B、20/100Hz |
| `s6-catchup-mail` | M10 Gate追帧；M13 邮件单机/Cluster，各2000x×3 |
| `s7-block-aoi` | M16 集中布局 observedBlocks0/4 各10m，显式 short8/io8/queue65536/CPU0 |
| `s8-alliance-effects` | M11 effect workers16/1，各三轮；256并发、写额度512、有界重试8 |
| `b2-footprints` | 真实500人业务；1000/5000/10000仅合成DAO/DTO占用，不突破业务500人限制 |
| `s9-demo100`、`s10-demo200` | M12 100/200人，同批/每秒20/每秒10；六档每档新环境 |
| `s11-cellworld-robot` | M15 千人入场及60s稳态、5%活跃 |
| `s12-gate-pacing` | E3 100/s×30s×5，分客户端漏发与服务端信号 |
| `s13-generated-tcp` | M17 strict、strict+self_sync、pipelined开环、pipelined同实体，各5m |
| `s14-checkpoint128`、`s15-checkpoint256` | M19 同实验基线开关0/1交替各三轮；128/256并发，停顿栈诊断 |

同机准备仍保留：

```text
/private/tmp/v147-run-segment.sh
/private/tmp/v147-run-micro.py
/private/tmp/v147-timed-command.py
/private/tmp/v147-perf-runner-preparation/
/private/tmp/v147-perf-bin/
/Users/whb/roost/worktrees/v147/artifacts/perf/v147-remaining/
```

先按准备报告核对脚本版本、二进制 hash、工作树和端口，再看 `--plan`，不能盲用旧 `/tmp`。运行器支持 `--select`（完整匹配 label）、`--output-name`（新目录）和 `--plan`（只读）；失败即停，不能吞 exit。只有修复或明确分类后才继续后续格子，失败的原日志永远保留。

### 测量前提及容易误读的地方

- `GOMAXPROCS=0`，Nest 混合档明确 short8/io8/shared queue65536。原清单的 CPU4、未显式指定的4/32已纠正，不把不同参数叫同参数回归。
- 空闲窗口才测。先记录 load/swap/电源、实际 GOMAXPROCS、Git/构建来源；没有其他 agent 编译/race/索引。CorpLink 常驻约一核，load2～4可视为历史空闲范围；记录实际背景，不杀用户其他进程。
- 每段总预算1800s，预留120s依赖收尾。私有依赖采用独占目录/端口，`up → source env → 测试 → down` 在同一 shell 生命周期；**不 `clean`，不 reset 共享环境，不删保留数据和缓存**。
- 默认准备的私有 mirror offset18000，home `/private/tmp/v147-formal-mirror-20261114`；alliance etcd43379/应用43500，seamless etcd58379。M12另用每格新目录。重新查端口，不因旧计划写“空闲”就直接启动。
- M10 `LS_DRILL_RATES` 单位是 **KB/s**，用 `256,64`，不是旧文的262144/65536；脚本PASS不等于连接未断，必须读deadline与完成数据。
- Sync 用户目标是 **P99≤50ms**，保留>50ms数量和max。部分旧脚本遇到任何>50ms就exit1：如实分列脚本退出与用户目标，不能改门禁、删长尾或把失败吞掉。
- M8 16额外视野只属于诊断构建；生产仍8。E1去分支构建不能单独证明“结构体布局”因果；HB四构建、AOI对照都不合生产。
- M12统计的是完整Robot场景的完成数；日志中的unknown出现次数不是Saga事务数，不能把每秒完成整场景数写成消息TPS。保持原p95≤16s、错误/丢弃0门槛。
- M13统一新 `mail.test` 来自 `fef75aee`，SHA-256 `be360a92d2a7a28707d992cc990cb3e1e120d733aea780752dfc6e53bcffc89d`；旧二进制另存 `mail-before-cluster-357263ba.test`，不得换回旧helper测Cluster。
- M17固定开环1500/s是输入负载，不是容量承诺；核对最终冷加载Gold、成功回包、WAL排空、Sync与错误。M11分尝试数和逻辑终态；unknown不得盲重试。
- M19错误/慢样本有的只是日志而不导致测试FAIL，要读RPC error、≥3s时间簇与stack。实验 `ALLIANCE_E2E_STACKS=1` 只对所属进程发SIGUSR2，每阶段最多2次；栈输出本身有成本，原报告保留限制。

## 5. 最后收尾及文档责任

1. 将生成器最终结果追加到统一功能报告，保留三轮历史状态；不再重复功能矩阵，除非后续代码变化影响它们。
2. 实跑完M后，逐格写原始指标/失败/判据；更新性能报告、remaining和KNOWN-LIMITS，实验只交报告。禁止先写“已完成”再补数据。
3. A31/F17文档归并已完成；C36外部验收只列条件/入口，见 [external](evidence/known-limits-remaining-20261114/external.md)，本机没有运行Linux、云、跨机、设备或≥1h长稳。D事项仍待维护者决定。
4. 文档反复出现的范围错误已纠正：P1六点迁服用**嵌入TCP**，不含独立Gate，committed旧源副本Q4未关；P6跨sid占名不等于MMORPG跨sid专项；turn配方联合链不代表全部玩法联合链；31项shutdown定向数不能写32。
5. `compose.md` 只允许小事实更新，不改结构。KNOWN-LIMITS仍由单一文档负责人改，避免多agent互相覆盖。
6. 最后运行 `python3 scripts/check-doc-anchors.py`、受本轮文档影响的根包 `GOWORK=off GOFLAGS=-p=2 go test -count=1 .`，检查diff/生成漂移/工作树、凭据和大产物。若本次交接已有文档检查结果，后续改文档后再按影响检查，不冒称无需复验。
7. 最终交付仍停在`feat/v147`，等维护者明确授权再push/main/tag。不要替用户发v1.47.0。

## 6. CBM与接手定位

本轮使用Codebase Memory Verify/Tier2，实际项目为 **`Users-whb-roost-worktrees-v147`**，根目录是本集成工作树；不要因为上层 `/Users/whb/roost` 没有项目而索引错仓。最近确认generation为`2026-11-14T02:55:37Z`，晚于此的变化用精确源码补证，不能把它称为最新全覆盖。

接手先`index_status`/`list_projects`确认实际代际，结构查询优先`search_graph → trace_path → get_code_snippet`，对全部证据文件检查coverage；metadata_changed/模板缺口直接读源码。需要刷新时在正式性能窗口之前或结束后做，不与测量并跑。遗留工作树、实验patch、原始日志均保留；同机原始材料丢失时按仓内入口重建，不编造曾经的运行结果。

## 7. 本次交接检查

文档合入后，`python3 scripts/check-doc-anchors.py` 检查 383 份文件、0 个坏链接；`git diff --check` 通过。根包初查因 B3 实验提交不在集成分支祖先中而失败：这是有意只合报告、保留实验分支，故在已有 `documentedCommitExemptions` 中为该提交的短/完整 SHA 登记明确原因，保持原守卫及其历史坏引用自检。没有合入实验实现，也没有删除精确实验来源来避开检查。

随后 `GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 go test -count=1 -timeout 3m .` 通过（10.803s）。该次只是根包文档/入口守卫，不代表中止的生成器整包已补完。原始输出保留为 `/private/tmp/v147-handoff-doc-anchors-20261114.log`、`/private/tmp/v147-handoff-root-doc-check-20261114.log`（初查失败）及 `/private/tmp/v147-handoff-root-doc-check-fixed-20261114.log`（通过）。接手完成剩余工作并改文档后，仍按影响做最终检查。
