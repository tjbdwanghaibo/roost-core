# N07 第二批：configdata 经 GM 的真实热更新与 handler 内快照一致性

2026-10-05，分支 `revn07b`，基线 `3d4fe9f3`（origin/main，N07 第一批之后）。本机 macOS / Go 1.27.0，`GOWORK=off`。接力清单 [N07 行](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [第一批](REVIEW-2026-10-05-noncore-n07.md) · [跨轮进度](PROGRESS.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n07b/README.md)。

结论：登记并修复 2 条（NC-64 P3 文档、NC-65 P2），均有修前红与修后绿，未发版。handler 内快照一致性没有缺陷，新增一条生成工程具名回归作为控制。第一批的 C-O1 / C-O2 / C-O3 与 event 未接线都没有改行为；端到端里没有找到它们的真实触发路径（证据见下）。另有 1 条移交 N08。N07 **场景部分完成**，不计 completed/15。

## 读代码的限制

图谱项目 `Users-whb-roost-roost-core`（根是主检出，generation 停在 09-30）。`ActiveSnapshot` 的入边在图里为 0（调用方都在 `.tmpl` 与生成物里，不索引），`demo/game/scene/runtime/refresh.go.tmpl` 标为 parse_partial。所以本批的调用链全部用基线源码 `rg` 与直接阅读补证，行号指 `3d4fe9f3`。

## 环境与做法

- 生成工程：`roost project new n07bgd -template game-demo`，`replace` 到本 worktree；`db/def` 的 `db=game` 改成 `db=n07b_game` 后 `roost generate`；修复后另生成 `n07bgd2` 复验。
- 依赖：隔离环境 `~/.roost-it/roost-dataengine-it`（Mongo 28117–28119、NATS 15222–15224、Redis 17379），只 source 不打印；私有 etcd `127.0.0.1:23791`（scratch 目录）。前缀：Mongo `n07b_game / n07b_remote_entity / n07b_saga`，NATS `prefix: n07b`、流 `N07B_EFFECTS / N07B_SAGA / N07B_SYNC`，Redis `roost:n07bgd:*` 与 `snapshot_l2_key_prefix: n07b`，ops `127.0.0.1:19761`。game 进程启动需要 global（activity 绑定），所以起 global + game 两个进程。
- 收尾：列出后只删以上三个库、一个 Redis 键（`roost:n07bgd:global:route:1000`，隔离 Redis 里只有这一个键）、三个流；停掉私有 etcd。
- 显式 `Store.Rollback` 在生成工程里没有任何运维入口，本批在 scratch 工程的 `gm.go` 里临时加了 `zz.probe.config.rollback`（不提交），只为把 rollback 路径跑在真实进程里。

## 调用 / 数据 / 资源所有权链（基线源码）

- 写入：`gm.config.reload`（`demo/internal/service/game/gm.go.tmpl:413-439`）→ `Store.ReloadWithReason`（`configdata/configdata.go:781`，持 `s.mu`）→ `build`（读 `config_data.dir` 下 JSON，`kit/configdata/configdata.go:36-41`）→ `commit`（`:838`：Validate → BeforeApply → 发布 current / defaultStore / `fctx.SetRuntimeConfig` → lifecycle emit → AfterApply，失败 `revert`）。
- 监听者：只有两个，顺序固定——kit `configdata.metrics`（`kit/configdata/configdata.go:57`，Provide 时登记）与游戏 `featureflags`（`flags.go.tmpl:52`，Service.Init 时登记）；生成工程与 kit 里没有 `PhaseConfigReload` 的 lifecycle hook（`rg` 全仓 + 生成工程）。
- 读取：开关读 `featureflag.DefaultStore()`（AfterApply 里用 `store.Current()` 整表 Replace）；场景刷新 `RefreshSystem.Due`（`refresh.go.tmpl:60`）在 spawner 自己的 goroutine 上每秒读 `configdata.ActiveSnapshot()`——该 goroutine 没有请求上下文，回落到 `Current()`；handler 内的生成 getter（`codegen/internal/tablegen/main.go:766`）读 `ActiveSnapshot()` → 请求上下文的 `Config`。
- 快照钉住：Nest 准入 `prepareClientMessage`（`nest/client.go:198-219`）把调用方上下文的 Config（没有上下文时取 `fctx.RuntimeConfig()`）写进 `msg.Context`；执行时 `ensureNestContext`（`nest/nest_dispatch.go:234-251`）用它建请求上下文；慢阶段→快阶段续行带同一份快照（`nest/remote_dispatch.go:39,54,125`）；冷加载 `runEntityLoad` 也带快照（`entity/manager_access.go:315,358`）。kit 的玩家接入 / bus / worker 每条消息新建上下文（`bus/bus.go:843,891`、`worker/*.go`），没有长寿命上下文。

## 场景矩阵

### 热更新端到端（真实进程，[e2e-reload](../bugfix/evidence/noncore-bugfix-20261005-n07b/e2e-reload.txt)）

| # | 情形 | 操作 | 结果 |
| --- | --- | --- | --- |
| H0 | 运维流程 | 按 GM 说明只改 `configs/table/feature_flag.csv`（purchase→false）再 reload | reload `ok`、版本前进，开关没变 → **NC-64** |
| H1 | 成功 | 改 `configs/data/spawn.json` count 3→5、`feature_flag.json` purchase→false，reload | 版本 3、flags 版本 3、`off=[purchase]`；3 秒内 alive 3→5；`configdata_version 3` |
| H2a | 失败 | 同一次编辑里 spawn / 开关改动 + `item.json` 截断 | 返回 `unexpected end of JSON input`；开关、版本、gauge 都不变（整次 reload 原子拒绝） |
| H2b | 失败 | `item.json` 重复 id | `table item duplicate key 1001`；不变 |
| H2c | 失败 | 删掉 `feature_flag.json` | `open ... no such file`；开关保留上一批（不会读成“全关”） |
| H2d | 失败 | spawn count→7 + `item.json` 截断 | 3 秒后 alive 仍 5：失败的一代没有被 scene 读到 |
| H2e | 校验缺口 | spawn 行删掉 `template`（schema `required:"true"`），直接改 JSON | **reload 接受**，按 template 0 刷了 2 只怪 → 移交 N08（运行时不查 required） |
| H3 | rollback | 探针 `Store.Rollback` | 回到版本 3、开关重新发布（`off=[purchase]`，AfterApply 在 rollback 时照常跑，flags 注释“rollback needs no handler”对显式 Rollback 成立）；gauge 回 3；杀一只后按回滚表的 20 秒重生规则不补（alive 6 > count 5）；第二次 rollback 拒绝 `previous snapshot not found` |
| H4 | 运维流程 | 改 CSV（monster_spawn→false）→ `roost generate` → reload | 开关翻了；但 generate 在被跑进程往工程目录写日志时拒绝执行（见“移交 N08”） |
| G1/G2 | 修后 | 本分支全新生成：说明指向 `configs/data/feature_flag.json`，照做翻开关 | 通过（[e2e-green](../bugfix/evidence/noncore-bugfix-20261005-n07b/e2e-green.txt)） |

### handler 内快照一致性

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| K1 | 同一 handler 两次 `generated.ItemByID`，中间 reload（attack 15→99） | 新增 `TestAHandlerReadsOneConfigGenerationAcrossAReload`（生成工程，真实 NestMgr 1 worker + 真实 Store） | 两次都读 15、同一代；控制 |
| K2 | 同上，中间 Rollback | 同一用例 | 两次都读准入时的一代（99）；控制 |
| K3 | reload 后的下一个请求 | 同一用例 | 读到新代；控制 |
| K4 | 判别力 | 把生成的 `ItemByID` 改读 `configdata.Current()` 后同一用例变红：`{first:15 second:99 ...}` | [mutation](../bugfix/evidence/noncore-bugfix-20261005-n07b/snapshot-mutation-control.txt) |
| K5 | 准入在 rollback 前、执行在 rollback 后 | scratch 探针 V3 | 读被回滚的那一代（99）且前后一致 → 观察 C-O9 |
| K6 | 调用方自带上下文，绑定后才 reload | 探针 V5 | 读调用方那一代；一致 |
| K7 | 加载期 getter | `runEntityLoad` 带快照（源码） | 一致；NC-65 的修复在加载时读 item 表，沿用这条 |

探针原文 [snapshot-probe](../bugfix/evidence/noncore-bugfix-20261005-n07b/snapshot-probe.txt)。

### 属性与配置的交界

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| A8 | 穿着铁剑的玩家重新加载 | 新增 `TestFinalIsRecomputedFromWornGearWhenThePlayerLoads` | Final 掉回 Base、`attr_final` 为空 → **NC-65** |
| A9 | 热更改了物品 attack，在线玩家 | 源码：`RefreshGear` 只在 equip handler 调用 | 不自动重算 → 观察 C-O8 |

## 第一批观察的触发路径核对（不改行为）

- **C-O1**（AfterApply 失败回退前请求读到被撤回的一代）与 **C-O2**（flags 钩子无 Rollback 分支）：生成工程只有 metrics、featureflags 两个监听者，featureflags 在最后；metrics 不会失败；featureflags 只在快照里没有开关表时失败，而缺文件在 build 阶段就失败（H2c），不进 AfterApply；没有 config.reload 的 lifecycle hook。真实进程里没有触发路径，维持观察。
- **C-O3**（行内切片 / map 浅拷贝）：生成工程的三张表（item / spawn / feature_flag）全是标量字段，无触发路径。
- **event 未接线**（E-O4）：生成工程 `rg SetEventBus|NewEventBus` 为空，维持。

## 新观察（未改行为）

- **C-O5 失败的 reload 不留痕迹**：H2a～H2d 四次失败，game 日志里 0 行、`configdata_reload_total` 没有失败计数（kit 只在 AfterApply / Rollback 里计数，build / validate 失败到不了监听者）；只有 GM 调用方拿到错误。建议在 Store 或 GM 命令处记一条 WARN，或给 Store 加失败回调，由维护者定。
- **C-O6 指标标签带运维自由文本**：`configdata.reload.total` 的 `reason` 标签来自 GM payload，每个不同的 reason 一条新序列（H3 时已有 5 条：manual、probe、s0-csv-only、s1-success、s2e-missing-required）；kit README 只承诺 `{result}`。频率低（人工 reload），记观察；收口可去掉 `reason` 标签、改记日志。
- **C-O7 版本号说明**：`demo/deploy/dev/observability/README.md.tmpl` 写“热更后版本 +1”，实际失败也烧号（E2E 3→8），rollback 回退版本。措辞问题，未改。
- **C-O8 热更不重算在线玩家的 Gear**：见 A9；要么写明“下次换装 / 加载生效”，要么在 reload 后经 Nest 广播重算，属于设计选择。
- **C-O9 准入在 rollback 前的请求读被回滚的一代**：这是“请求钉住准入时的一代”的直接后果，与“同请求不撕裂”是同一个选择；回滚坏配置时已排队的请求仍按坏配置执行。若要“rollback 之后不再执行坏一代”，需要在执行时比对代际并拒绝，代价是请求失败，由维护者定。
- 回滚后，在坏一代里按 template 0 刷出的怪继续存在（refresh 不剔除多余成员，H3）；与 refresh 的“只补不删”设计一致。

## 移交 N08（不在本批修）

- **generate 的输入快照不跳过 `.dev/`**：`codegen/internal/roost/generate.go:452` 只跳过 `.git / bin / dist / log / .testcache / .roost-*`。`make dev-run` 把五个进程的日志写在 `.dev/*.log`，进程运行期间 `roost generate`（也就是“改 CSV 后让开关生效”那一步）每次都报 `project inputs changed while code generation was running: .dev/game.log; rerun the command`（[复现](../bugfix/evidence/noncore-bugfix-20261005-n07b/n08-generate-devdir.txt)：后台向 `.dev/game.log` 追加时跑 generate）。默认 WAL 目录 `data/wal/dataengine` 也在工程内，有流量时同理（未单独复现；不能按目录名跳过 `data`，`configs/data` 同名）。
- **运行时不查 `required`**（H2e）：tablegen 生成的 `required:"true"` 只在 CSV 转换时检查，直接改 JSON 再 reload 会被接受。归 N08 的 required / ref 检查一并处理。

## 验证

见[证据 README](../bugfix/evidence/noncore-bugfix-20261005-n07b/README.md)。未跑：Linux / Windows；真实客户端看复制帧；五进程全链路（只起 global + game）；`glsvet`（没改三大模块）。

## 方向判断

attribute 模板是近期第三轮出问题：RR-20260917-06（框架半缺失）→ 第一批 NC-60 / 61 / 62 → 本批 NC-65。NC-61 与 NC-65 是同一个前提的两面：**属性容器是组件内存，框架只认识 DAO**——NC-61 是事务回滚不认识容器，NC-65 是加载重建不认识容器（Gear 的来源是持久的装备，Final 的去处是 nopersist 的复制字段，两头都要组件自己记得接）。修复没有增加状态或重试（都是在一处补“从 DAO 重建 / 把结果写回”），但每加一个层、一个派生字段就要再记一次。第一批给的候选 (b)（容器提供 `Checkpoint/Restore`，模板一处接 Nest）能覆盖回滚一侧；加载一侧的对应做法是把“由持久事实重建全部非持久层并写回复制字段”收成组件的一个 `rebuild()`，`OnInitFinish`、热更后的重算（C-O8）、回滚恢复都调它。建议维护者在 (b) 的基础上一起定，仍不建议把 Final 持久化。

configdata 本身没有反复出问题的迹象：本批的两个确认项都在模板 / 说明层，Store 的提交协议在真实进程里的成功、失败、回滚行为都与契约一致。

## 停点与下一步

- **本批完成**：H0～H4、G1/G2、K1～K7、A8/A9，第一批观察的触发路径核对；NC-64 / NC-65 修复。
- **未做**：五进程全链路下的热更（只起 global + game）；客户端侧复制帧；Luban 外部表（`examples/lubanreal`）真实工具链；C-O5 / C-O6 / C-O8 / C-O9 待维护者。
- **下一入口**：N07 剩余只有维护者决定项（event 接入 / 移除、C-O1 契约、属性容器重建与回滚的统一入口）；转 N08，并带上本批“移交 N08”的两条。
