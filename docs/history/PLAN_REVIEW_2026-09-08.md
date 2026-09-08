# 2026-09-08：对《Core / Kit 实现下沉与缺陷收敛统一实施方案》的复核与后续工作清单

关联：[统一方案](../CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)、[M-00](M-00_BASELINE.md)、[M-00a](M-00a_SOURCE_HEAD_ALIGNMENT.md)、[09-08 提交复核](COMMIT_REVIEW_2026-09-08.md)、[账本](ledger.md)、[交接](HANDOFF_2026-09-07.md)。

复核方式：在 macOS 机器上 `git pull` 五仓 origin/main 后，读全部新文档、逐条对照仓库实际状态（go.mod、import 图、CI、工作流），并在本机重跑了边界测试。本文只做两件事：说清哪些结论成立、哪些与仓库事实不符；把后续工作按可执行粒度列出。

## 1. 结论先行

方案的**方法论部分是合理的，可以沿用**：U / M 双轨、批次门禁、证据模板、回退规则、"先 Remote Entity 试点再动 WAL / Data Engine / Saga"、Mod 留 Kit、不做转发别名。`TestCoreDependencyBoundary` 是一条真实有用的护栏（本机与 Linux CI 均通过）。

方案的**事实基础有四处需要修正**，其中两处会直接误导下一步：

| # | 方案里的说法 | 仓库实际 | 影响 |
| --- | --- | --- | --- |
| 1 | ENV-01：Skill 仍 require `cube-core v1.8.0`，多处 `cube-*` import；已用 `fe23185` 对齐并移除嵌套 replace | origin/main 的 Skill（`375c10c`）三个 go.mod **零** `cube-` 引用，根模块 require `roost-core v1.12.0`；Go 文件零 `cube-` import；嵌套模块的 replace 只指向 `../..`（自己），且注释写明目的是"用工作树 Skill 对着已发布 core/kit 测" | 另一台机器的 Skill 检出停在 `24ac8a9`（09-02），落后 main 六天、缺 U-0028 / U-0066 / U-0085 / U-0094 等提交。`fe23185` 是在过期基线上做的、**未推送**；直接推会冲突，而且"移除嵌套 replace"会改变 sync-e2e 的既定意图。ENV-01 不是仓库问题，是检出过期 |
| 2 | TOOL-01：Codegen 内部 `GOWORK=off`，外层 workspace 不能证明生成流程用了本地源码；需在 M-01 新设计 dev 解析通路 | `GOWORK=off` 是刻意的：生成工程必须可发布、不能带 replace。**source-head 验证通路已经存在**：codegen 的 `framework-compat` 工作流（push / 每日 03:17 / `workflow_dispatch` 可指定 core_ref、kit_ref、skill_ref）——生成 planet 工程 → 建临时 go.work 把 core / kit / skill 源码放进去 → `go test ./...` → 校验生成物无 replace → 在 dev compose 栈上真实启动进程 | M-01 不需要新造通路，需要的是：把这条工作流在本地可复现（一段脚本），并在迁移批次里把它当门禁跑 |
| 3 | 文档里的仓库名 `cube-core` / `cube-kit`、`D:/whb_s/...`、Windows Go 1.24 入口、`go/parser is not in std` | 是那台机器的本地目录名与环境故障，与五仓无关（模块名一直是 roost-*）；本机 macOS 与 Linux CI 上边界测试都通过 | 三份历史记录被机器细节稀释，接手者会被"工具链坏了"这条假阻塞牵着走。建议：M-00 / M-00a / 提交复核三份合并成一份"迁移前基线"，只保留可移植事实 |
| 4 | "本轮不自动提交、推送、发版" | 四个提交（`19e6928`、`4757b10`、`e4aeef8`、`8ae888f`）已直接推到 core main；提交信息 `doc update`、`go version` 不符合仓库惯例；未带署名行 | 规则与行为不一致本身不致命，但两个 Agent 同时改 ledger / HANDOFF 已经发生。方案第 8 节要求"ledger 由一个整合者更新"——现在就该指定 |

另有一条**方案说对了、我们此前做错的**：手工回退验证的计数把 `build failed` 也算成红（交接文档第 2 节的循环、账本第 9 节"count build failed lines too"）。编译失败只说明中和方式破坏了编译，不说明行为被测到。应按方案 §7.2 改为：`build failed` 记 invalid，换一种中和方式（例如保留变量引用）重试，只有 `--- FAIL` / 目标 panic 才算红。已在本文 §4.1 列为待办。

## 2. 对架构方向本身的评估（这是需要你拍板的部分）

方向："Core 承载核心逻辑与基础实现，Kit 收敛为配置 / 装配 / 生命周期"。

**可行性**（从 import 图看，问题不大）：

- Kit 内部依赖极浅：几乎每个包只依赖 `kit/mods`（capability 名常量）；`dataengine → nestwal`、`saga → nats, nestwal`、`room / lockstep / robot → nettransport`；测试依赖 `mongo/mongotest`。没有环，搬迁顺序可以按 nestwal → nats/mongo/redis 客户端 → remoteentity → dataengine → saga 排。
- Core 已经依赖 mongo-driver v2，所以 `mongotest` 迁入 Core 的测试支撑包在依赖上是顺的。

**代价**（方案提到了，但没有量化）：

| 代价 | 数字 | 说明 |
| --- | --- | --- |
| Core go.mod 新增重依赖 | nats.go、go-redis v9、etcd client/api v3、quic-go、kcp-go | 今天 Core 只有 chi / viper / mongo-driver / x-net / x-time。合并后**每个只想用 Core 契约的消费者**都会拉进这些驱动。这与"Core 是契约层、轻依赖"的初衷相反，是最大的架构代价 |
| 消费方 import 路径改动 | service：`kit/versionstore` 54 处、`kit/mods` 34 处、`kit/servicerpc` 9、`kit/redis` 5；skill：`kit/syncstream` 2；codegen 模板：`kit/dataengine` 4、`kit/nats` 3、`kit/mongo` 3、`kit/saga` 2、`kit/room` 2、`kit/manager` 2、`kit/statslog` 1、`kit/mods` 2 | 方案禁止转发别名，意味着这些消费方和**所有已生成的业务工程**在同一个发布窗口内被迫改 import。这是破坏性变更，语义版本上要么 v2 大版本，要么一次协调的"五仓同日发版" |
| 生成工程模板 | codegen 的 render 模板里写死了 kit 包路径 | 需要同批改模板 + golden 测试 + `framework-compat` 真启动 |

**建议**（三选一，我倾向 A）：

- **A. 分层下沉，不搬驱动**：把与外部系统无关的算法与状态机下沉 Core（versioned lock / fence、remoteentity 的 wrapper / batch / ownership 状态机、nestwal 的日志格式与恢复、dataengine 的事务 / projection / outbox 状态机、saga 引擎已在 Core），驱动客户端（nats / redis / etcd / mongo 连接与 Lua 脚本执行）和 Mod 留 Kit，通过 Core 定义的窄接口注入。Core 不新增重依赖，消费方路径改动最小（只有直接用 kit 算法包的地方）。这与方案 3.1 "Core 可以依赖通用第三方驱动"有出入，但保住了 Core 轻依赖。
- **B. 按方案全量下沉，发 v2**：Core / Kit 同步升 v2 模块路径（`/v2`），一次性改所有消费方与模板；旧 v1 冻结只修 Bug。工程量最大，但语义最干净。
- **C. 按方案全量下沉，不升大版本、不留别名**：五仓同日协调发版，业务工程用 `roost upgrade` 一次性改 import。风险最高：任何一个消费方漏改就是编译失败，且无法灰度。

无论选哪个，都应先在**分支**上做 M-02 试点并跑通 `framework-compat`，量出真实改动面，再定发布策略；不要在 main 上分批合入半迁移状态（方案 §4.2 也是这个意思）。

## 3. 其他核对结果（成立的、可以放心用的）

- Core examples go.mod 1.25 → 1.27：正确，CI 绿。
- `TestCoreDependencyBoundary`：AST 扫根模块全部 Go 文件（含 `_test.go` 和非当前 build tag），跳过嵌套 go.mod，拒绝 kit / skill / service / codegen / cube-*。本机与 CI 通过。它只看直接 import，方案自己也写明了；够用。
- B-14 的补充观察（Saga Command 含 `time.Time`，`Validate` 只要求非零）：成立，`time.Time` 只在年份越界时 Marshal 失败，仍是健壮性观察，不改结论。
- 方案 §6 对历史待办的并入方式、§7 证据要求、§10 回退与停止规则：与账本第 1 节协议一致，无冲突。

## 4. 后续工作（按先后，每条可直接开工）

### 4.1 立刻做的卫生项（半天，谁先上机谁做）

1. **另一台机器同步五仓**：`git fetch && git status`，Skill 从 `24ac8a9` 快进到 `375c10c`；`fe23185` 不要直接推——先看它在新基线上还剩什么实质内容（身份对齐已无必要；`TestFrameworkModuleIdentity` 可以保留为护栏，但要去掉"移除嵌套 replace"那部分，或给出新的理由）。rebase 后作为独立提交、按仓库惯例写信息再推。
2. **指定 ledger 整合者**：两个 Agent 都在改 `ledger.md` / `HANDOFF`。约定：U / B / T 编号与矩阵只由做 U 单元的那台机器改；M 批次状态只写在方案文档与 M-xx 记录；另一方只加"见 xxx"一行链接。
3. **合并三份机器基线记录**：M-00、M-00a、COMMIT_REVIEW 合成一份 `M-00_BASELINE.md`，只保留可移植事实（模块、HEAD、命令、退出码、未验证项），删掉 D:/ 路径、cube-* 目录名、Windows 工具链故障。历史文件不删，但在开头标"已合并"。
4. **修正回退计数规则**：交接文档 §2 的循环与账本第 9 节改成 `build failed` = invalid（换中和方式重试），只有 `--- FAIL` / 目标 panic 计红；在 `revertsample.py` 的报告里已经是 skip 分类，手工循环跟上。
5. **打 tag 后验证远端**：09-07 codegen v1.14.0 第一次 `git push` 因网络失败但本地退出 0；把 `gh api repos/<o>/<r>/git/ref/tags/<v>` 写进交接文档的发布步骤。
6. **提交信息与署名**：所有机器统一仓库惯例（中文一句话 + 空行 + `Co-Authored-By` 行，由实际执行的 Agent 填自己的身份）。

### 4.2 M-01 收口（1 天）

1. **本地复现 `framework-compat`**：写 `roost-codegen/scripts/source-head-check.sh`，做与工作流相同的事（生成 planet → 临时 go.work 挂本地 core / kit / skill → test / vet / glsvet → 校验无 replace → 可选 compose 启动），参数为三仓本地路径。M 批次门禁直接调它。
2. **测试支撑归属**：`kit/mongo/mongotest` 迁入 Core（建议 `roost-core/mongo/mongotest`，Core 已有 mongo-driver 依赖），Kit 的 dataengine / nestwal / saga / remoteentity 测试改 import；这是唯一一处 Core 测试会反向依赖 Kit 的地方，先解决它，M-02 才能开始。作为独立子批，跑 `TestCoreDependencyBoundary` + 五仓测试。
3. **依赖图产物**：把本文 §2 的 kit intra-import 图与外部消费统计写成脚本（`go list -json` + 过滤）放进 `scripts/`，每批迁移前后各跑一次存入批次记录。
4. **决定 §2 的 A / B / C**，写进方案第 3 节。这一条是 M-02 的前置，不定就不要动文件。

### 4.3 M-02 Remote Entity 试点（3～5 天，分支上做）

1. M-02a 行为保护：确认 U-0011 / U-0023 / U-0031 / U-0049 / U-0093 / U-0025 的测试仍在、仍绿；补方案列出的新场景（同 ID 并发创建与取消、引用释放与驱逐、容量满、所有权转移、续租失败、停止时等待者退出、快照乱序、首次 / 重试判据一致）——先在**现有 Kit 位置**补，红→绿，这些本身就是 U 单元。
2. M-02b 迁移：按选定的 A / B / C 搬文件；`remote_entity_mod.go` 留 Kit；混有 Mod 测试的文件按职责拆。
3. M-02c 消费者与启动：codegen 模板 + service / skill import；`source-head-check.sh` 全绿；`framework-compat` 手动触发 core_ref / kit_ref 指向分支。
4. 性能：迁移前后同机器跑 kit 现有 `scripts/perf`，记录延迟 / 吞吐 / 分配。
5. 全部通过后再合 main，五仓协调发版（策略见 §2）。

### 4.4 U 轨道（与 M 并行，另一台机器或空档做，遵守账本协议）

沿用交接文档 §4 的顺序，做了两处修正：

1. **B-25**：改采样器按文件名跳过 `*_gen.go` 并在报告里单列（半小时）；codegen 侧的"生成 + 编译 + 运行"测试并入 4.2-1 的脚本，不单独做。
2. **B-18 entitysync、B-19 redis**：各一个 C2 单元。
3. **B-14**：按函数分别复核可达性；当前不改公开 API，保持观察，除非 M-05 迁 Saga 时顺手把摘要函数改为返回错误。
4. **87 个未审格**：脚本扫标记为"脚本扫"，不升级为回退验证。
5. **nightly 高位包**（09-07 报告）：core `robot/action` 15/20、`nest` 13/20、`syncstream` 11/20；kit `nestwal` 14/20（committer 重试区间、短写——这两条与 M-03 直接相关，优先）、`etcd` 14/20、`mongo/mongotest` 13/20；service `rank` 15/20、`match` 14/20；skill `combatcomponent` 11/20。
6. **MigrationRunner 三次冲突耗尽**：构造 `SystemCommitter` 替身，M-04 之前完成。
7. **故障矩阵新切片**：Mongo 事务级 toxic、etcd 断连、JetStream 重投递风暴——放在 M-03 / M-04 之后，作为迁移后回归的一部分。

### 4.5 明确不做

- 不在 main 上合入任何"Core 已搬、消费方未改"的中间状态。
- 不为减少 nightly GREEN 删守卫；不可达 / 冗余守卫只记账本。
- 不在两台机器上同时编辑同一个包或同一份账本。

## 5. 一句话给另一台机器

先 `git pull` 五仓（Skill 落后六天），把 `fe23185` 在新基线上重看；然后读本文 §2 等待 A / B / C 的决定，期间只做 §4.1 与 §4.2-1、4.2-3（不需要决定就能做的部分）。
