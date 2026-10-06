# B6：roost CLI 信号统一接管（含 N08 留项 O2 / O3 / O6）与 C9 联网测试开关

> **状态（2026-10-06 核对）**：已实施（`f556049c`，含 N08 O2 / O3 / O6 与 C9），已随 v1.21.0 发布。下文“未发版”是实施当时的状态。

2026-10-06，分支 `b6cli`。来由：[DECISIONS-PENDING 第四轮 B6 / C9](../review/DECISIONS-PENDING-2026-10-05.md#维护者决定2026-10-06第四轮)，[N08 方向判断与 O1 / O2 / O3 / O6](../review/REVIEW-2026-10-05-n08-codegen.md#方向判断)。

## 1. 问题

“同级暂存树 + 外部 go 进程 + 信号”这一个组合连续出了四次缺陷：RR-20261004-12（生成器 chdir 让子进程继承暂存树）→ RR-20261004-13（go 命令按进程树取消，进程组让 Ctrl-C 只到 roost，于是 `runCommandTree` 接管信号、杀树、重发）→ cb11be90（重发后要 `reraisedSignalGrace` 等信号生效，否则调用方先跑了回滚和退出）→ RR-20261005-NC-70（死于信号不跑 defer，暂存树残留，于是加进程级暂存树登记表）。每次都在 `runCommandTree` 上加状态。根因是暂存树的生命周期没有单一所有者：创建在命令里、清理靠 defer、信号却在最底层的 go 命令包装里被接住并让进程死掉。N08 O6 记下了这条路还没覆盖的窗口：生成器运行、`copyProject`、提交阶段收到信号时没有任何处理，进程立即死、暂存树残留，提交阶段还可能留下一半提交。

## 2. 目标契约

1. **只在 CLI 入口接管信号**（`roost.Main`，`cmd/roost` 与 `cmd/project` 都经它）：SIGINT / SIGTERM / SIGHUP（启动时已被忽略的不接，保留 nohup 语义）。第一次信号只取消命令的 ctx。
2. **命令按 ctx 走正常错误路径**：暂存树由创建者的 `defer os.RemoveAll` 删除，依赖文件按既有 `rollbackDependencyUpdate` 回滚，manifest 按既有 `commitManifestSyncResult` 回滚。go 子进程树由 `runCommandTree` 在 ctx 取消时按进程组杀掉（RR-20261004-13 的机制不变）。
3. **提交点之前查 ctx，提交本身不中断**：`commitSyncChanges` / `renameProjectStage` 开始后跑完（或按自身失败回滚），信号在命令返回后才生效。这样中断只有“未提交”和“已完整提交”两种结果，不再有半份提交。
4. **命令返回后重新抛出信号**：入口打印一行 `roost: interrupted by <sig>: <命令的错误>`（命令在信号前已成功则不打印），恢复默认动作后向自己发同一信号，进程照旧死于该信号，退出码（shell 看到 128+n）不变。万一进程里别处也处理了该信号、没有死，等待 `reraisedSignalGrace` 后以 128+n 退出——这个等待只剩入口这一处。
5. **第二次信号强制退出**：回滚期间再按 Ctrl-C，立即以该信号结束（暂存树可能残留，等同于用户主动放弃清理）。go 进程树在第一次信号时已被杀。
6. `runCommandTree` 只负责“按进程树运行、ctx 取消时杀树、Wait 有界”：删掉其中的信号接管、重发与 `reraisedSignalGrace` 等待，删掉 NC-70 的暂存树登记表（`removeOnInterrupt` / `removeInterruptedStage`）。

## 3. 改动面

- `codegen/internal/roost/interrupt.go`（新）：`Main`、`runInterruptible`（接管 / 取消 / 第二次强制 / 重抛）、`dieOf`。
- `command_tree.go`：去掉信号相关代码与登记表；`watchedInterrupts` 搬到 `interrupt.go`；平台文件只留进程组与杀树（`treeInterruptSignals` 改名 `interruptSignals`，Windows 仍为空：Ctrl-C 直接到达同一控制台组里的 go，入口不接管，行为与之前一致）。
- ctx 贯穿：`RunContext` → `runProject` / `runGenerate` → `NewProjectContext`、`SyncProjectContext`、`commitManifestSyncResult`、`UpdateFrameworkDependencies`、`GenerateTransactional`、`TidyProjectDependencies`、`DoctorWithOptions`；内部 `copyProject`（每个条目查 ctx）、`runGenerators`（每个生成器前查 ctx）、`generate` / `checkGenerated`、doctor 的 go 命令。`Run` / `NewProject` / `SyncProject` / `Generate` 保留为 `context.Background()` 包装（`roost add`、模板脚手架与大量测试在用；它们没有 go 命令窗口，信号在入口推迟到返回后生效，暂存树照样由 defer 删除）。
- doctor：ctx 取消后不再打印半份报告，返回中断错误。
- 测试钩子 `stagePhaseHook(ctx, phase)`：`copy`（复制完第一个文件后）、`generator`（每个生成器开始时）、`commit`（提交点检查之后、提交之前），nil 时不做任何事，只给 O6 用例在确定的阶段发信号。
- O2：`project upgrade` / `project sync` 依赖解析失败的错误带上 `roost project deps --root <dir>`（与 `project new` 一致）。
- O3：生成器 / tidy / 迁移在暂存树里报的错误，把暂存路径换回工程路径（`errors.Is` / `As` 保留）。
- C9：`roost_test.go` 的常量 `publishedDataEngineGeneratorDependencies` 换成 `ROOST_NETWORK_TESTS=1`；`.github/workflows/framework-compat.yml` 加一个联网 job 只跑这两条；根包加一条门禁，钉住该 workflow 设置变量且 `-run` 覆盖这两条用例。

不碰 tablegen / cfggen / 配置生成、kit/service、app。

## 4. 兼容

- 退出码与“死于信号”不变；新增的是 stderr 上一行中断说明（之前进程直接死、什么也不打印）。
- 中断后的工程状态更强：任何阶段都不留暂存树、不留半份提交（第二次信号强制退出除外）。
- Windows：不接管信号，与之前相同（Ctrl-C 直接到达 go 与 roost）。只要求 `GOOS=windows go vet`。
- 包内导出函数签名变化（`UpdateFrameworkDependencies`、`GenerateTransactional`、`TidyProjectDependencies`、`DoctorWithOptions` 加 ctx；删掉零调用的 `Doctor`），包在 `internal/` 下，仓外无调用方。生成物、持久格式无变化。
- 默认 `go test` 不联网（C9 门默认关）。

## 5. 验证计划

- 先红：O6 三个阶段用例（复制 / 生成器 / 提交）在旧机制上（钩子以 `context.Background()` 接入，入口 = 旧 `Run`）红在“暂存树残留”上。
- 既有用例照样通过：cb11be90 的 `TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost`、NC-70 的 `TestInterruptedCommandRemovesItsStagingTree`（子进程改经新入口 `Main` / `runInterruptible`，断言不变），RR-20261004-13 其余进程树用例、RR-20261004-12 生成器 cwd 用例。
- Unix 信号用例 macOS 上 `-race -count=3`；整包 `go test ./codegen/...`；`GOOS=windows go vet`；根包；`go build ./... && go vet ./...`。
- C9：本机 `ROOST_NETWORK_TESTS=1` 实跑两条用例。

## 6. 实施状态

**已实施，未发版**（基线 `4f0bab75`）。图谱：项目 `Users-whb-roost-roost-core` generation 停在 2026-09-30，`command_tree*.go` not_tracked，`dependencies.go` / `generate.go` / `doctor.go` / `project.go` metadata_changed；本方案全部以当前源码逐行核对。

### 6.1 最终形状

- `interrupt.go`：`Main` → `runInterruptible(stderr, command)`：`signal.Notify` 只登记启动时未被忽略的 `interruptSignals`；监视 goroutine 收到第一次信号记下并 `cancel()`，第二次信号直接 `dieOf`；命令返回后 `signal.Stop`，有信号则打印 `roost: interrupted by <sig>: <err>` 并 `dieOf(sig)`（`signal.Reset` → 向自己发信号 → 等 `reraisedSignalGrace` → 兜底 `os.Exit(128+n)`）。`reraisedSignalGrace` 只剩这一处。
- `command_tree.go`：157 行 → 42 行；只剩 `exec.CommandContext` + 进程组 + `Cancel` 杀树 + `WaitDelay`。删除：信号接管、中断分支、重发与等待、`interruptStages` / `removeOnInterrupt` / `removeInterruptedStage`（NC-70 的登记表），两处 `defer removeOnInterrupt(stage)()`。
- 取消检查点：`copyProject` 每个条目、`runGenerators` 每个生成器前、go 命令（`runCommandTree`）、提交点前（`GenerateTransactional` / `SyncProjectContext` / `updateFrameworkDependenciesTransactional` 的 `commitSyncChanges` 之前，`NewProjectContext` 的 `renameProjectStage` 之前）。提交与提交之后的模板脚手架不查 ctx。doctor 在 ctx 结束后返回 `doctor interrupted, no report`。go 命令被取消时错误为 `go <args> interrupted: context canceled`（`errors.Is(err, context.Canceled)` 成立）。
- `cmd/roost`、`cmd/project` 改调 `roost.Main`；`Run` 保留为无取消的包装（测试与库调用）。
- O2：`project upgrade` / `project sync` 的依赖失败错误带 `after the cause is fixed run roost project deps --root <dir>`；doctor 的 `compile:go-list` 修复建议补一句“失败或被中断的 upgrade 之后先跑 roost project deps”。
- O3：`inProjectPaths` 包一层只改 `Error()` 文本、保留 `Unwrap` 的错误，用在 generate 的生成器 / tidy、sync 的 shutdown 刷新 / 模板 / 生成器、deps 的迁移 / 依赖解析、`generate --check` 的临时副本。复制阶段的错误不改写（那时暂存路径就是写入目标）。
- C9：`networkTestsEnv = "ROOST_NETWORK_TESTS"`；`framework-compat.yml` 新 job `codegen-network`（`GOWORK=off`、`ROOST_NETWORK_TESTS=1`，`-run` 点名两条，出现 `--- SKIP` 即失败）；根包 `TestNetworkCodegenTestsRunInSomeWorkflow` 钉住门变量名、两条用例存在、某个无条件步骤设变量并点名跑它们（负对照：把变量改成 `"0"` 即红）。根包既有 Redis 门禁的正则只匹配 `*REDIS*` 与 `ROOST_*_BACKEND`，与本变量无关。

### 6.2 红绿

O6 三阶段（`interrupt_phases_promises_test.go`）在旧机制上（钩子以 `context.Background()` 临时接入旧代码，`Main` 临时等于旧 `Run`）：

```
--- FAIL: TestInterruptInEveryStagePhaseRemovesTheStageAndDiesOfTheSignal/copy
    roost project deps interrupted in phase copy left its staging tree .roost-deps-3831054622 beside the project
--- FAIL: .../generator
    roost generate interrupted in phase generator left its staging tree .roost-generate-3263551163 beside the project
--- FAIL: .../commit
    roost generate interrupted in phase commit left its staging tree .roost-generate-1806799051 beside the project
    roost generate interrupted during its commit did not finish it: stat …/planet/internal/registry/generated.go: no such file or directory
    roost generate interrupted during its commit left a partial commit: generated files are stale: internal/registry/generated.go
```

“死于 SIGINT”在旧机制上本来就成立，红只落在暂存树与提交完整性上。O2 / O3（`stage_error_messages_promises_test.go`）把修复临时关掉时：

```
generate error names a path that is gone when it is read, want …/planet/internal/broken/broken.go:
    generator registry: registry: parse …/.roost-generate-488902455/internal/broken/broken.go: …
sync error names a path that is gone when it is read, …/.roost-sync-139422663/internal/broken/broken.go …
project upgrade dependency failure does not say how to converge ("roost project deps --root …/planet"):
    project upgraded but framework resolution failed: resolve framework dependencies: go get …@latest: exit status 1
project sync dependency failure does not say how to converge …
```

旧二进制（`4f0bab75` 构建）在正式 CLI 上复现 O3：`generate` / `project sync` 的错误分别指向 `.roost-generate-52068927/internal/bad/bad.go`、`.roost-sync-1715783186/…`。

### 6.3 验证（全部 `GOWORK=off`，macOS darwin/arm64，Go 1.27.0）

- 信号 / 进程树 / 暂存树用例 `-race -count=3`：`TestInterruptInEveryStagePhase…`（3 子测试）、`TestInterruptedCommandRemovesItsStagingTree`（NC-70，2 子测试，子进程改经 `Main` 跑正式命令行）、`TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost`（cb11be90，子进程改经 `runInterruptible`）、其余 RR-20261004-13 进程树用例、RR-20261004-12 生成器 cwd 两条、O2 / O3 两条 → `ok`（36.9s）。断言均未改动。
- 整包 `go test -race -count=1 ./codegen/internal/roost/` → `ok`（106.5s）；`go test -count=1 ./codegen/...` → 全部 `ok`。
- `go build ./... && go vet ./...`；`GOOS=windows go vet ./codegen/internal/roost/ ./codegen/cmd/...`；根包 `go test -count=1 .` → `ok`；`gofmt -l` 为空。
- 正式 CLI（本分支构建的 `roost`，PATH 上替身 go 起长寿孙进程后 wait，向 roost 进程组发信号）：`project deps` / `generate` / `project sync` / `project upgrade` × SIGTERM / SIGHUP 八格全部 exit 143 / 129（死于信号）、工程旁无 `.roost-*`、go 孙进程已不在、工程文件逐字未变；stderr 一行如 `roost: interrupted by terminated: tidy generated dependencies: go mod tidy interrupted: context canceled`。SIGINT 由上面的用例覆盖（非交互 shell 的后台作业启动时忽略 SIGINT，roost 按 nohup 语义不接管它，属预期）。
- C9：本机 `ROOST_NETWORK_TESTS=1` 跑两条用例：默认代理 `goproxy.cn` 上 `go mod tidy` 失败（`roost-core@v1.20.2` 的 sumdb 查询 404——tag 发布约 25 分钟，代理未同步）；按发布说明改 `GOPROXY=direct GONOSUMDB=github.com/tjbdwanghaibo` 后两条都通过（8.1s）。默认 `go test` 下 `TestDoctorPlayerTCPPassesAfterAuthAndConfig` 跳过、另一条走离线分支。

### 6.4 未做 / 残余

- Windows 不接管信号（与之前相同，只做了 vet）；Windows 上实跑 Ctrl-C 与 taskkill 进程树仍属 N08 剩余项。
- 第二次信号强制退出时暂存树可能残留（设计如此）；`roost add` 等无 go 命令窗口的命令不加 ctx，信号推迟到它们返回后生效。
- 强杀（SIGKILL）、断电、磁盘故障下的提交 / 回滚仍未覆盖。
