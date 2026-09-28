# 2026-09-28 第八轮独立审计（本地 main `d66eeeb`）

范围：RR-20260928-07～11，外加 `462c8d7`（DEPLOYMENT k8s 章节）。非修复方审计员在导出副本上核验（各修复父提交为修前）。图谱已过期，结论来自源码与实跑；未连外部服务、未跑 docker。

## 修复核验结论

5 条修复“修前红、修后绿”全部成立，约束符合。RR-11：`requireSync >= Strict` 只作用于 {2, 3}（更大值在 `canonicalizeRecord` 已被拒），`WAL.Append` 非测试调用方 3 处、Durability 3 进入只有 broadcast 与带 Remote 批次两条，未找到把不该等 fsync 的记录变成等 fsync 的路径，快路径 `Enqueue` 不变；
同机 A/B 复跑方向一致（broadcast pipelined 吞吐下降、strict 与快路径无显著变化）。RR-09：16 处 Durability 判定清单完整。RR-08：Durability 0 路径未变，拒绝范围表与源码一致。RR-10：临时根演练覆盖无记录 release、连续回滚、从 legacy 升级失败自动回滚，均按设计。
RR-07：新生成 game-demo 10 个服务 Secret 内嵌 config 与 prod 示例逐字节相同，手改 Secret 保留。USER_GUIDE §4 判别表 1～15 连续无重复，`replySentinels` 逐行对齐。`462c8d7` 与生成的 README / deploy.sh 一致。

门禁（审计员）：build / vet（含 integration tag）/ glsvet / gofmt，整仓非 race 120 包，nestwal / nest / remoteentity / dataengine / kit 相关包 `-race -count=20`，新回归 `-race -count=50`，codegen 定向回归，均通过。

## 新发现

- [RR-20260928-12](../bug/RR-20260928-12.md)（P4 推断，RR-10 引入）：回滚时新版本进程按旧 unit 的 TimeoutStopSec 被停。
- [RR-20260928-13](../bug/RR-20260928-13.md)（P4）：CRLF 的 Secret 示例被静默跳过；Secret 不可解析时 `add transport tcp` 与 `add mod` 处理不一致。

## 疑点（文档，随 RR-12 / 13 一并处理）

- D1：判别表第 4 行“strict / async 已持久”——自带 remoteentity 在 async 下 `Commit` 返回推测回执、不走第 4 行，async 回复时本地也未 fsync；RR-11 后带 Remote 批次的 pipelined 已持久却未列出；第 2 行“四种形态”未列 RR-08 的 Overloaded 形态（表后已写）。
- D3：`docs/DEPLOYMENT.md` §4 与生成的 `deploy/shell/README.md` 仍写“45 秒 SIGTERM 预算”（TimeoutStopSec 按 Mod 生成）。
- D4：rollback.sh 版本号校验接受 `.` / `..`（修前即有，readiness 失败后恢复原版本，基本无害）。
- D5：roost-coding 快池豁免清单只点名“strict 锁内 fsync”，建议补“以及回退到 strict 路径的 pipelined”。

## 处理结果（2026-09-28，fix-r11-b21）

基线 `3a58811`。图谱过期（09-26），以源码与实跑为准；未连任何外部服务。

- RR-20260928-12：已修复，见[修复记录](../bugfix/RR-20260928-12.md)。install.sh / rollback.sh 改为 stop（当前 unit）→ 切 `current` → `use_unit` → start；`use_unit` 失败时恢复 `current` 与 unit，并以非零退出。
- RR-20260928-13（含 D2）：已修复，见[修复记录](../bugfix/RR-20260928-13.md)。Secret 示例与停机块刷新都支持 CRLF，并按原行尾写回。认不出 Secret 结构时，各命令统一处理：stderr 打印 WARN，不改 Secret，命令照常完成。
- D1：USER_GUIDE §4 判别表只改了第 2、4 行的文字，编号 1～15 不变。第 4 行改为 strict 与随 strict 路径提交的 pipelined（RR-11 起等 fsync）已持久，并注明 async 不走本行：自带 remoteentity 在 async 下 `Commit` 返回推测回执（`remoteentity/batch.go` Durability 1 分支），回复时本地未 fsync。第 2 行补上 RR-08 的 `ErrRemoteOverloaded` 形态。
- D3：生成的 `deploy/shell/README.md`（`renderShellReadme`）逐个列出各 Service 的 `TimeoutStopSec`，取值与 install.sh 的 `STOP_TIMEOUT` 同源（`serviceShutdownPlan(...).grace`）；`docs/DEPLOYMENT.md` §4 改为写公式，并指向 README。回归：`assertGeneratedShutdown` 断言 README 给出的每个 Service 的值与 install.sh 相同，且不含“45 秒”。修前在 game-demo 的 10 个服务上全部失败（`deploy/shell/README.md does not give chat the stop budget 28s install.sh uses` 等）。
- D4：rollback.sh 拒绝 `.` / `..`，报 `invalid version`，退出码 2。回归 `TestShellRollbackRejectsDotVersions` 并进 RR-12 的提交。
- D5：roost-coding 快池豁免清单补上“回退到 strict 路径的 pipelined”（RR-20260928-11）。本机 `~/.codex/skills/roost-coding/SKILL.md` 已同步；`docs/agent-skills/roost-optimize/` 入口只引用共同规范，没有豁免清单，所以不改。

验证（三笔提交之后，`GOWORK=off`）：

- `go vet ./codegen/...` 与 `go test -count=1 ./codegen/...` 全部 ok（`codegen/internal/roost` 266.5s）。RR-12 的 3 个新用例加上 RR-10 回归 `-count=50` 通过；RR-13 的 2 个新用例 `-race -count=50` 通过。glsvet（nest / entity / dataengine/engine / sync/entitysync）通过，gofmt 为空。
- `bash scripts/test-sync-modes-generated.sh`：ok。
- 新生成 game-demo（`replace` 指向本 worktree）：`go build ./...`、`go vet ./...` 与 `go test ./...`（17 个包 ok）都通过；`roost config check --all` 通过；`project doctor` 49 OK、无 WARN / FAIL；`project diff` 为 0 个文件。README 里的停机预算为 account / activity / chat / global / mail / match / platform / rank / session 各 28s、game 119s，与 install.sh 一致。
- 旧工程：用基线 `3a58811` 的生成器建一个 game-demo，再用修后生成器 `project sync`，结果 `updated=3`。更新的是 `deploy/shell/install.sh`、`deploy/shell/rollback.sh` 与 `deploy/shell/README.md`，三个文件与新生成工程的同名文件逐字相同（项目名替换后）。之后 `project diff` 为 0，`go build ./...` 通过。配置与 Secret 示例都不变，RR-13 只影响 `add` 入口与 CRLF 文件的停机块刷新。
