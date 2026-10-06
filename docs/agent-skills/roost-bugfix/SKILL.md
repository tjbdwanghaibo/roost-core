---
name: roost-bugfix
description: "Roost（单仓 roost-core）bug 收敛一轮：把 review 登记在 docs/bug 里未修复的 RR 按先红后绿修掉，保留修前证据，写 docs/bugfix 记录并更新索引 / 交接 / CHANGELOG，按已有授权提交推送；不自动发版。触发词：roost bugfix、修复 review 发现的 bug、处理 docs/bug 未修复的问题、$roost-bugfix。"
---

# Roost bugfix：把 review 登记的 RR 问题收敛掉

审查那一半是 `roost-review`（只审查行为、可补必要中文注释，产出 `docs/bug/RR-*.md`）；本 skill 是修的那一半，修法记进 `docs/bugfix/`。
用户指定问题、模块或 review 轮次时，完成该范围的修复、回归、文档与已授权的提交推送。

## 0. 规则源与优先级

每轮开工先读，顺序即优先级，本 skill 与它们冲突时以它们为准：

1. `AGENTS.md`（入口，Codebase Memory 使用约定）；
2. `docs/agent-skills/roost-coding/SKILL.md`（共同规范，尤其"必须保持的执行契约"与"快池内不得阻塞等待"——修复不得违反）；
3. `docs/CORE-OPTIMIZATION-HANDOFF.md`（§4 用户已接受的边界、§5 未实施 / 未证明、§7 缺陷索引）；再按任务读它链接的 feature / bug / bugfix 记录，
   历史"待实施 / 未提交"以后续记录和当前源码为准。

`roost-optimize` 是同一规范的优化入口。若本轮改了 roost-coding 规范：仓库规则源、本机副本、roost-optimize 入口三处同步。
本 skill 的正本是仓库 `docs/agent-skills/roost-bugfix/`（AGENTS.md 指向它，两条工作线共用）；本机 `~/.claude/skills/roost-bugfix/` 是逐字镜像，改动只在仓库里做、再同步镜像。

## 1. 接手与范围

- 按 `go.mod` / Git remote 定位：`/Users/whb/roost/roost-core`，模块 `github.com/tjbdwanghaibo/roost-core`（Go 1.27.0）。
  `kit/`、`codegen/`、`demo/` 是顶层目录，一个 tag；`kit-src` / `codegen-src` remote 是搬迁遗留，别 pull、别 push。
- 每次先 `git fetch`；干净跟踪分支 `git pull --ff-only`。保留未提交或其他 agent 的工作，复用空闲隔离 worktree（agent worktree 可能停在旧提交，开工先对齐基线）；
  不擅自 reset、stash、force push 或改全局 Git 配置。
- 范围：只处理状态为"已复现 / 已确认，未修复"的 RR；稳定 RR / W 编号沿用，同根因残留追加到原项。用户对明确范围的"修复 / 实施"授权足够执行，
  历史 review 的"未实施"、旧 WANTED / CARRYOVER 的"待拍板"不是重复审批理由；业务语义缺失或要做破坏性迁移时先完成独立项，再提出具体选择。
- 授权边界：不把"继续 review"自动变成改码；**不自动发版、打 tag、部署**——用户明确要求或"不发版就测不了"时才发（§5）。push / tag 是公开操作，需当次明确授权。
- 开工记录基线 SHA 与选定清单；用户续跑时按上次清单推进，不重做已验证的独立项。故障或环境阻碍要留下准确原因与可复跑入口，不把部分交付写成全部完成。

## 2. 硬规则

- **读代码走 codebase-memory-mcp**：`list_projects` / `index_status` 确认项目与 generation（默认 Verify）→ `search_graph` → `trace_path`
  → `get_code_snippet`；每个引用到的文件 `check_index_coverage`，否定 / 穷尽结论加 scopes。
  generation 落后于 HEAD、metadata_changed / partial / skipped / unknown、或刷新被 "pre-coordination generation is active" 挡住
  （记忆 `codebase-memory-daemon-lock`）时，**以当前源码补证并在记录里写明限制**；不删未知锁、不停其他实例、不声称已刷新。
  字符串 / 配置 / 文档直接 `rg`。
- **先红后绿**：每个修复先有一条在当前代码上确实变红的回归；优先控制并发事件顺序，不用任意 sleep；钩子不触发的超时不是红测。
  **红要落在承诺本身上**（记忆 `red-acceptance-must-be-proven`），修前失败文本抄进记录，不编造负对照。写不出红测试的降级为"观察"，不修。
- **一个 RR 一个修复单元**：当前范围 + 必要调用链内修根因，不顺手重构（无关重构另列，纯重构走 roost-coding 方案流程）；不以 demo 特判代替框架实现；
  优先复用 versionstore、Directory、索引、服务生命周期、codegen 等现有能力，不以新抽象、无界缓存或重试掩盖协议错误。代码风格按 roost-coding "写给人阅读的代码"。
- **快池内不得阻塞等待**：修复不能在快 worker 上同步等待快池任务或做 I/O 等待；"投递到快池并等待"只属于慢 worker；豁免清单见 roost-coding，之外的新等待入口须经评审。
- **分清已提交 / 未应用 / 结果未知 / 补偿失败**：未知结果不得直接回滚可能已提交的数据；清理原子校验不可复用身份，版本跨删键重建的重复不能当身份。
- **不许的"修法"**：跳过冲突 WAL、自动删坏记录 / 生产数据、放宽版本比较、伪造持久确认、改门禁 / 删样本 / 换口径让测试变绿。
- 状态、持久格式、公开 API 或 wire 改变时，同步装配 / 生成物 / 调用方与兼容说明；默认不执行生产数据迁移，提供安全恢复入口或可审查迁移方案。
- 隔离依赖只关闭自己创建的实例；临时文件只清理自己确认过内容的；凭据、大日志、二进制与缓存不提交。
- **提交链**：`gofmt -l` 空 → `GOWORK=off go vet ./pkg && GOWORK=off go test ./pkg -count=1 -race && git add <显式路径> && git commit && git push`，用 `&&` 串。**不要 `git add -A`**：排除 `artifacts/`、压测二进制、env.sh / 凭据、本地大日志。
- **不要信 `set -e` 里的 python heredoc 断言**——zsh 下断言失败后续步骤照跑；python 改文档后用 `&&` 接 git，或分两步跑。
- 并行修复用 worktree 隔离时：共享索引（bug / bugfix README、交接、CHANGELOG）只由主会话统一改，各组只写自己 RR 的记录，合并用 cherry-pick。
- **反复出问题要上报方向判断**（维护者 2026-10-05）：同一模块 / 同一机制在近期多轮里反复出缺陷，或某次 bugfix 之后又在同一处出 bug（修复被打回、补修再补修），不要只是继续打补丁——在汇报里单列一段给维护者：列出该模块近期的问题与修复链（编号、提交），判断根因是实现细节还是前提 / 设计 / 实现方向有问题，给出是否需要修正思路、简化或改变实现方向的建议（候选方向与代价）。信号：同一不变量第二次被打破；修复在增加状态 / 分支 / 重试而不是减少；状态机交错类问题反复；修复依赖越来越多的时间 / 预算假设。先例：game-demo PlayerOwners 的按玩家租约连续多轮出问题，维护者确认前提不成立后改为静态绑定 + App 单实例锁（`docs/feature/APP-SINGLETON-LOCK-2026-10-05.md`）。

## 3. 一轮的步骤

1. **拉代码**：fetch / `--ff-only`，`git log --oneline -10` 看 review agent 或另一条工作线的新提交；普通收尾以本地编译及适用验证为准，不等待或轮询 GitHub CI，不自动把在线 CI 红转成本轮任务。
2. **读规则源**（§0），再读发现：`docs/bug/README.md` 头部散文与表（新在上）→ 未修复条目的 `docs/bug/RR-*.md`；
   老轮次 `REVIEW-*.md` / `REPRO-*.md` 聚合报告仍可能是主记录；
   也看 `docs/bug/WANTED.md`、`docs/bug/CARRYOVER.md` 与交接 §5 里被 review 升级成 RR 的项。
3. **分析**：图谱定位符号与调用链（generation 旧就读源码）；核对 RR 给的行号 / 基线 SHA 是否仍对；对照执行契约想清楚不变量再定方案，没采用的方案连同取舍写进 bugfix 记录。
4. **红测试**：新建或扩充回归（`*_promises_test.go` 或按包既有命名），文件头注释 `RR-…：` 一段中文说清承诺与旧行为；跑一次确认红，抄下失败文本。
   审查附录的测试拿来改：去掉 `Review` 前缀、按包内 harness 重写。新 API 的红：用 `git stash push -- <impl files>` 或暂时截掉新 API 用例，只走旧 API 拿红文本。
5. **修**：最小改动，注释写"为什么这样、之前为什么错、RR 编号"。
6. **验证矩阵**（全部 `GOWORK=off`，从模块根跑；按影响面选，通过后只因新增修改或未解风险扩大检查）：
   - 错误、TTL/版本、取消/关闭改动按[组合契约复核](../roost-coding/references/fix-contract-review.md)补调用方/邻近分支及恢复后状态；先列行为场景再给包测试数量。“保留资源”必须验再次停止最终收敛，明确退化不得仅写未验证。合并状态与独立验收状态分列。
   - 目标包 `-race`，再跑受影响的相邻包（如 kit 的 Mod 转发 core Assembly）；并发变更必跑 race。
   - **每批都跑一次根包 `GOWORK=off go test -count=1 .`**：分层边界（`TestCoreDependencyBoundary`）、CI / 故障矩阵 / Redis 门变量的覆盖门禁都在根包里，只跑目标包看不见。保留本地编译、行为回归和门禁证据；推送后确认远端包含提交即可，不等待或轮询 GitHub CI。
   - 静态：`go vet`；三大模块加 `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`。
   - 协议 / 生成器 / 存储变更：`bash scripts/test-sync-modes-generated.sh`；
     有隔离 Mongo / Redis / NATS 时 `scripts/test-dataengine-generated.sh`、`scripts/test-remote-generated.sh`；
     故障矩阵 `scripts/test-remote-matrix.sh` 会注入故障，**单独跑**（约 40 分钟，独占集成环境）。单测不能代替这些。
   - codegen 改了生成形状：重生成 `codegen/.../testdata` fixture，跑 `go test ./codegen/...`；生成接口执行自身 `go:generate` 与 check / 消费者验证。
   - 性能相关修复：同机、同配置、同负载、同编译模式前后对照（`scripts/perf/*.sh`，每次新 label 目录），profile / race 与正式延迟测量分开、不和索引重建并跑；保留退化样本，没收益就撤回或如实记代价。
   - `kit/service` 下的 Redis 用例需 `-tags integration`（`REDIS_ADDR=127.0.0.1:6379 go test -tags integration -count=1 -p 1 ./kit/service/...`）；新加 Redis 存储 / 索引要把键空间登记进 `kit/service/integration` 的 `everyNamespace`（U-0263）。
7. **文档**（§4）。
8. **提交推送**：一笔 `fix(<pkg>)：RR-… 一句话`，文档随修复一起提，显式选路径；fetch 后整合远端变化再 push，不改写他人提交。
   推完不等 CI（记忆 `release-no-ci-wait`），确认远端包含本次提交即可。
9. **一轮登记很多条时先分流**：能靠"补一处判断"收敛的先做完并提交；需要先定契约的不赶工，在 `docs/bugfix/README.md` 末尾写清每条为什么没修、要先定什么。

## 4. 文档与索引

- `docs/bug/RR-….md`（review 写的主记录）：只追加——状态改"已修复"、加修复链接；原始失败与结论不覆盖，推翻的部分在末尾写"更正"。
- `docs/bugfix/RR-….md`：问题链接 → 根因（指到行）→ 决策理由 / 未采用方案 → 文件与行为 → 兼容性与迁移（行为收紧写清）→ 实际执行的回归命令与结果（修前红文本、修后绿）→ 未验证项 / 风险。
- 两个索引：`docs/bug/README.md` 与 `docs/bugfix/README.md` 头部各加 / 改一行（新在上）；审查有时只在散文头提新 RR、没加表行，标已修复时把表行补上。
- `docs/CORE-OPTIMIZATION-HANDOFF.md` §7 加行；修复改变了 §3～§5 的结论时同步改那里。`docs/review/PROGRESS.md` 是跨轮进度，B 线在用，按轮更新。
- 行为 / 配置 / API 变化：`CHANGELOG.md` `## [Unreleased]` 加条（一句粗体 + RR + 两三句机制 + 记录链接），`docs/USER_GUIDE.md` 等使用说明同步。
  运维可见的现象才加 `docs/TROUBLESHOOTING.md` T 行（现象 | 原因 | 判别 | 处置，编号接续最后一行）。
- `docs/history/ledger.md` 是 09-22 以前的账本，不再强制；维护者要求恢复时再补。
- 准确区分：已定位 / 已修复 / 已验证 / 用户接受 / 已提交 / 已推送 / 已发布。commit / push 不等于发布或部署；不能以包测试绿代表全部完成。
- 复核残余不新编号、不新 T：原 RR 的 bug / bugfix 记录末尾各加"## 复核后的补修"，T 行处置列追加一句，两个 README 那一行改成"已修复（含残余补修，未发版）"，
  CHANGELOG 单独一条，交接 §7 该行备注。

## 5. 编号与位置速查（2026-09-30）

| 东西 | 在哪 | 当前接续 |
| --- | --- | --- |
| RR 编号 | `docs/bug/README.md` 头部散文与表，`RR-YYYYMMDD-NN` | 最新 RR-20261004-01（A 线）；B 线另有 `RR-<日期>-NC-NN` / `RR-<日期>-CG-NN` 带前缀的编号段。**两条工作线共用日期段**（09-30：01/02/04/05/06～10 属 B 线，03 属 A 线），登记前先 fetch 看头部，撞号就顺延；修的一侧不自造 RR |
| 用户直接提出、无 RR 的缺陷 | `docs/bugfix/U-xxxx-<topic>.md` 或 roost-coding 的 `<issue-id>` | U 最后 U-0278（09-22） |
| W 候选 | `docs/bug/WANTED.md`（`W-YYYY-MM-DD-NN`） | — |
| T 行 | `docs/TROUBLESHOOTING.md` | 最后 T-44，按最后一行接续 |
| M / REFACTOR（重构，不占 RR） | 旧 `docs/bugfix/M-*.md`，现行 `docs/feature/REFACTOR-YYYY-MM-DD-<topic>.md` | 旧 M 最后 M-18 |
| 发布 | `codegen/ci/framework-release.yaml` `release:` → `scripts/pretag.sh vX.Y.Z` → `git tag -a` → push tag | 最新 **v1.21.0**（10-06，tag 指向 `4881f2b7`）；之后在 main 的修复未发版 |
| 版本同步点 | `codegen/internal/roost/manifest.go` 的 `Core` 默认版本（现 v1.18.0）与 `.github/workflows/framework-compat.yml` 的 `minimum` 行（`-roost-core-version v1.18.0`） | 发版时两处同步；生成的 game-demo 用到本版新增 API 时下限要升，先用上一版 tag 实编生成工程判断 |

发版细节：发版仍须单独授权，构建验收以本地编译及适用验证为准，不等待或轮询 GitHub CI；pretag 要在干净 worktree 跑（仓库根的 `artifacts/` 里保存的源码备份会让 `go build ./...` 失败）；发版前在最终 HEAD 跑一次故障矩阵 `scripts/test-remote-matrix.sh`（v1.17.2 起的做法，结果目录写进 CHANGELOG 或交接）；
发完在 bug README / 交接把"未发版"改成版本号，并对着 tag 做一次无 go.work 的生成 + 编译（`GOWORK=off go run ./codegen/cmd/roost project new X -module example.com/X -out <scratch>/X -template game-demo` → `GOPROXY=direct GONOSUMDB=github.com/tjbdwanghaibo go get github.com/tjbdwanghaibo/roost-core@vX.Y.Z` → `go build ./... && go vet ./...`）。proxy 对新 tag 有几分钟延迟，用 `GOPROXY=direct` 重试，不要因此改代码。

## 6. 本轮新增规则（2026-09-28～30）

- **写许可定容**：`remote_entity.max_concurrent_writes ≥ 目标写 TPS × 可容忍的依赖停顿秒数 + 基线在途（约 15）`；80 TPS 用 256，默认 128 不变。
  规则源 `kit/README.md` remoteentity"独立资源预算"。
- **集成环境在 `~/.roost-it/roost-dataengine-it`**（`ROOST_IT_HOME=$HOME/.roost-it`、`ROOST_IT_PORT_OFFSET=1000`，
  脚本 `kit/scripts/integration/dataengine-env.sh`）。旧 `/tmp/roost-dataengine-it` 仍被另一会话的 demo 使用：**不要停、不要重置、不要对它跑脚本**；
  同一 shell 不要同时 source 两份 env.sh。env 文件含凭据，只 source，不输出、不提交。C01 / B30 与其他 Remote 验收共用 `remote-acceptance.lock`，只能串行。
  环境按共享模式使用（A5）：integration 一律加 `-run`，故障一律自建代理 / 进程，全局运维命令持锁，锁存在时不跑真实依赖用例；细则见 `reference/lessons.md`“环境与工具”。
- **合并时按行并集的自动合并只用于 `docs/bug`、`docs/bugfix` 索引**这类只追加的文件；USER_GUIDE 判别表等正文冲突手工合并，合并后核对表行编号连续。
  规则源 ARCHIVE-2026-09-30 §7。
- **删 worktree 前先把 `artifacts/perf/*`、pprof、日志复制到主检出的 `artifacts/perf/remote/`**；`--force` 只能在复制之后用；报告写结果目录绝对路径。
  规则源 C01-RUNBOOK §7、记忆 `worktree-artifacts-before-remove`（已丢过两次证据）。
- **长跑脱离终端**：`nohup caffeinate -i bash <script> > <scratch>/x.out 2>&1 &`，每次新 label 目录，不与压测 / 索引重建并跑。
- **两条工作线并行**（A 线本机 core / Remote / Sync，B 线 Windows 机器 Service / Codegen）：不碰对方文件，推送前 fetch + rebase；索引头部两线都在改，撞号顺延。

## 7. Wanted 与相邻 skill

- `docs/bug/WANTED.md` 是实现侧写给 review agent 的候选表（`W-YYYY-MM-DD-NN`，无状态、不进矩阵）。修 RR 时看到"签名承诺了但实现没履行 / 跨包契约对不上"，
  **不要顺手改**，写一条（位置到 SHA+行、现象、为何可疑、会红的测试草稿、候选修法），等 review 三选一（登记 RR / 判非问题 / 再观察）。判为 RR 的按正常流程修，判非问题的删条目。
  roost-coding 已授权的历史 bug **复现确认后**直接修，不因旧"待拍板"重复请示。
- `roost-review`：只审查行为、可补必要中文注释，登记 RR 与分流 Wanted，本 skill 的另一半。
  `roost-coding`（共同规范）/ `roost-optimize`（优化入口）：重构 / 性能优化按它们走，纯重构先写 `docs/feature/REFACTOR-*.md`。
  `roost-consolidate`：合仓已完成，只剩历史参考，不要再按三仓 / cherry-pick 到 `consolidation-v3` 的流程工作。

## 8. 汇报格式

按 RR 列：修了什么、根因在哪一行、测试怎么红怎么绿（命令 + 结果）、改了哪些文档、提交号；未验证项、兼容限制单列。
最后一句说明是否发版（及下一个版本号），并区分"已提交 / 已推送 / 已发布"。
若本轮触发"反复出问题要上报方向判断"（§2），汇报里单列"方向判断"一段。

反复用到的事实（各包 harness、踩坑手法、环境）见 `reference/lessons.md`。
