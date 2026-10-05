# N08 codegen 证据（2026-10-05，macOS darwin/arm64，Go 1.27.0）

基线 `50e9a4e853641f348c879b85c282af9c5813f656`。[本轮记录](../../REVIEW-2026-10-05-n08-codegen.md)。路径中的 scratch 目录已替换为 `<scratch>`；大日志与生成工程留在本机 scratch，不提交。

## 文件

| 文件 | 内容 |
| --- | --- |
| `cli-interrupt-run.sh`、`cli-interrupt-fake-go.sh` | 正式 CLI 中断实验：替身 `go` 记下工作目录后长睡，等它运行后向 roost 发信号（`SIG=INT/TERM/HUP`），报告退出状态、工程旁的 `.roost-*` 与 go 子树。需要 `N08_SCRATCH` 下有 `demo/`（生成的工程）、`roost`（`go build ./codegen/cmd/roost`）、`fakego/go`；roost 由 `perl -e '$SIG{INT}="DEFAULT"; exec @ARGV'` 拉起，因为非交互 shell 的后台任务继承“忽略 SIGINT”，roost 会照 RR-20261004-13 的规则保持忽略 |
| `cli-interrupt-before.txt` | 修前：deps / generate / sync / upgrade / new 五条命令都 exit 130、无 go 残留，但各留下一棵 `.roost-deps-*` / `.roost-generate-*`（NC-70） |
| `cli-interrupt-after.txt`、`cli-term-hup-after.txt` | 修后：同五条命令 SIGINT、deps SIGTERM（143）、generate SIGHUP（129）均无残留 |
| `nc70-red.txt` / `nc70-green.txt` | `TestInterruptedCommandRemovesItsStagingTree` 修前红（只红在暂存树残留）/ 修后与 RR-20261004-13 进程树用例一起 `-race -count=3` 绿 |
| `nc71-red.txt` | `TestProjectDiffListsEveryFileTheNextSyncRewrites` / `TestUpgradeDryRunListsEveryFileTheUpgradeRewrites` 修前红：预览 11、实际 14，漏三份配置 |
| `nc72-red.txt` / `nc72-green.txt` | `TestCfggenHelpUsageCoexistsWithTheProjectGenerators` 修前红（registry “marked twice”）/ 修后绿 |
| `cfggen-runtime-gate.txt` | `sh codegen/scripts/cfggen-golden-runtime.sh -count=1 -race -v`（pin v1.20.0）5/5 通过，含新增 `TestIndexOptionsAndStringRefsRoundTrip` |
| `cfggen-runtime-gate-negative.txt` | 负对照：临时让生成器不写 `skipempty`，新用例红（`skipempty indexed zone 0: [2]`），随后恢复 |
| `cfggen-head-roundtrip/` | 当前 core（`replace` 到 worktree）上的往返探针：meta、数据与测试（`rt_test.go.txt`，改名避免被本仓编译）；9 叶子 `-race -count=3` 通过 |
| `nc73-red.txt` / `nc73-green.txt` | `TestIDToolsSeeErrcodeDefinitionsTheWayTheGeneratorDoes` 修前三处红 / 修后与既有 ID 用例一起绿 |
| `cli-id-errcode.txt` | 正式 CLI：别名导入定义被漏、注释被计入（修前）与修后结果 |
| `nc74-red.txt` / `nc74-negative.txt` / `nc74-green.txt` | `TestGenerateAndSyncIgnoreTheProjectsRuntimeOutput` 修前红 / 只改快照不改提交计划时运行期文件被删 / 修后绿 |
| `nc75-gate-red.txt` / `nc75-gate-green.txt` | `codegen/scripts/tablegen-runtime.sh`（pin v1.20.0）修前悬空 ref 两种情形 reload 被接受 / 修后 `-race` 绿 |
| `nc75-check-red.txt` | `TestCheckJSONEnforcesTheDeclaredRules` 修前 6 子测试红 |
| `shellcheck-game-demo.txt` | `shellcheck 0.11.0 -s sh` 对生成 game-demo 的 9 个脚本：仅 1 条 note SC2086（有意拆分） |

## 主要命令与结果

| 命令 | 结果 |
| --- | --- |
| `GOWORK=off go test -count=1 -json ./codegen/...`（修前基线） | 636 叶子：635 pass / 1 skip（`TestDoctorPlayerTCPPassesAfterAuthAndConfig`，编译期常量门） |
| 10 个具名环境 skip（来源 `docs/bugfix/evidence/noncore-bugfix-20261005-14/codegen-summary.json`） | 9 条在 macOS 实际执行通过：`TestCorePinScriptPrintsTheGeneratorMinimum`、`TestDevRunRegistersTheGameServerInTheAccountServicesRedisDatabase`、`TestDevRunKeepsAccountctlDefaultsWhenTheRedisKeysAreAbsent/{generated_values,keys_absent}`、`TestGeneratedComposeShapeCheckCatchesTheTmpfsShapeComposeUpRefuses`（真实 docker compose）、`TestShellRollbackRunsThePreviousReleaseUnderItsOwnUnit`、`TestShellReleaseSwitchStopsTheRunningProcessUnderItsOwnUnit`、`TestShellRollbackRejectsDotVersions`、`TestShellReleaseSwitchRestoresCurrentAndUnitWhenTheUnitCannotBeInstalled`；第 10 条见 O1 |
| 常量门临时置 true（不提交）：`-run 'TestDoctorPlayerTCPPassesAfterAuthAndConfig\|TestExplicitFirstBusinessWorkflowGeneratesAccessLifecycleAndEndpoint'` | 2/2 pass（7.7s） |
| Unix 信号 / 进程树：`-race -count=3 -run 'TestDoctorGoCommand\|TestDependencyCommand\|TestSyncProjectStagesNeverBecomeAChildsWorkingDirectory'` | 21/21 pass |
| v1.18.0 CLI 生成 pin v1.18.0 工程 → 当前 CLI `project upgrade -core v1.20.0` | 修前 dry-run 17 vs 实际 20（含 manifest）；修后 dry-run 20 与实际一致；build / vet / `go test ./...` / `generate --check` / doctor（22 OK）通过 |
| upgrade 失败回滚：语法错误 DAO 定义 / 失败的 go | 前者 manifest 回滚、零改动；后者半升级（O2），`roost project deps` 恢复后 build 通过 |
| DAO 改名 / 退役（game-demo 副本） | 旧输出删除、新输出生成、退役删除，`generate --check` 最新 |

修后完整矩阵（全部 `GOWORK=off`，最终 HEAD 见提交）在 bugfix 记录里列出。
