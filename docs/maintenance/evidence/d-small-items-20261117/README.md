# D 类三条小项（2026-11-17）

来源：`docs/maintenance/evidence/open-work-d-triage-20261117/README.md` A 类第 35、26、40 条（KNOWN-LIMITS 第 922、892、931 行）。

## 1. account 新用例在真实 Redis 上跑（第 35 条）

结论：**全过，可以删掉 KNOWN-LIMITS 那一条**。没有改代码。

做法：`source ~/.roost-it/roost-dataengine-it/env.sh`，`ROOST_REVIEW3_BACKEND=redis ROOST_REVIEW_REDIS=$ROOST_DATAENGINE_IT_REDIS_ADDR`（env.sh 不导出 `ROOST_REVIEW_REDIS`，要自己从 `ROOST_DATAENGINE_IT_REDIS_ADDR` 取；地址在证据里已替换成 `<redis>`）。键前缀由 `redistest.Prefix` 生成（名字 + 纳秒时间戳），测试结束自动删除。

| 文件 | 内容 |
| --- | --- |
| `account-redis-new-cases.txt` | `go test -race -count=1 -v ./service/account/ -run '<lobby-relay-account 证据里那 12 条>'`：12 条全过 |
| `account-redis-full-package.txt` | `go test -race -count=1 -v ./service/account/ ./wiring/account/`（整包，真实 Redis）：两个包 ok，155 条 PASS（含子用例），0 SKIP，0 FAIL |
| `account-redis-negative-control.txt` | 反证：`ROOST_REVIEW_REDIS=127.0.0.1:1`（没有服务的端口）跑同样 12 条，走存储的 7 条全部失败 |

12 条里哪些真的经过 Redis（反证结果）：
- 走存储（`pendingContractService` / `freshAccount`），坏地址时失败：`TestGameTokensAreIssuedPerGameAndValidatedByAccount`、`TestABanRefusesGameTokensAtOnce`、`TestTheServerIndexRefusesNewServersWhenFull`、`TestWithoutServicesTheServerListIsUnchanged`、`TestServersBelongToTheOnlyGameService`、`TestTwoGameServicesShareOneServerNumbering`、`TestBadServicesAreRefusedAtStart`。
- 只验 token 的签名 / 域前缀 / 过期 / 游戏核对，用 `newService`（内存），与存储无关，两种后端都是同一段代码：`TestAGameTokenExpiresOnTheSystemClock`、`TestATamperedGameTokenIsRefused`、`TestOtherTicketsCannotPassAsGameTokens`、`TestOnlyTheConfiguredGamesGetTokens`、`TestAGameTokenOnlyOpensItsOwnGame`。这 5 条没有 Redis 版本是设计如此（不是遗漏）。

没覆盖（不在本条范围）：Redis Cluster（`ROOST_REVIEW_CLUSTER`，由 `wiring/scripts/integration/redis-cluster-suites.sh` 设置）这一轮没跑；portal 与真实进程演练仍按原证据归协调者。

## 2. C# 运行时用例在发版时不能静默跳过（第 26 条）

结论：`scripts/pretag.sh` 原来跑 `go test ./...` 与 `test-client-dotnet.sh` 时都**没有**带 `ROOST_REQUIRE_DOTNET=1`；没装 .NET 的机器上 `TestClientConfigCSharpRuntime` 只会 SKIP、`go test` 仍 ok，发版看不出少了这条覆盖。已改：

- `go test -timeout 60m ./...` 前加 `ROOST_REQUIRE_DOTNET=1`（加了一段中文注释说明原因）；
- 金样那一步 `ROOST_DOTNET_GOLDEN_ONLY=1 ./scripts/test-client-dotnet.sh` 也加 `ROOST_REQUIRE_DOTNET=1`（同样的道理：缺 dotnet 时脚本打印 SKIP 并以 0 退出，pretag 照样过）。这一步超出任务字面（任务只说 go test 全量），一并改是为了发版时 C# 两步口径一致；协调者觉得不该改可以只撤这一行。
- `docs/maintenance/README.md` 对应一句改成现状。

没有跑全量 pretag（按任务要求）。验证：

| 文件 | 命令 | 结果 |
| --- | --- | --- |
| `dotnet-required-run.txt` | `ROOST_REQUIRE_DOTNET=1 GOWORK=off go test -count=1 -v -run '^TestClientConfigCSharpRuntime$' ./codegen/internal/roost/` | PASS（4.90s，dotnet 10.0.302；输出里有 Load 读回与 8 种规则违反的报行 / 字段） |
| `dotnet-missing-required.txt` | 同上，但 PATH 去掉 dotnet 所在目录 | FAIL：`ROOST_REQUIRE_DOTNET=1 but dotnet is not on PATH`——证明开关在缺 dotnet 时确实变红，而不是 SKIP |

另外观察到（没改，尚未处理）：`codegen/internal/roost/player_client_test.go` 的 `TestClientSDKAgainstGeneratedPlayerTCP`、`infra/network/gateway/binding_test.go` 的跨 Gate C# 用例只看 `exec.LookPath("dotnet")`，**不认** `ROOST_REQUIRE_DOTNET`，缺 dotnet 时仍是 SKIP。前者随 pretag 的 `go test ./...` 跑；后者只在 `test-client-dotnet.sh` 里带 `ROOST_CLIENT_TEST_DLL` 跑（脚本自身已因开关失败，影响不大）。要不要让前者也认这个开关，交维护者定。

## 3. CellWorld Runtime 的 AddIngress 失败分支（第 40 条）

结论：**补上了场景**，模块场景 9 → 10；变异 4/4 被杀。没碰 CellWorld 业务适配进模块（D21 后半），也没做 Linux race 20 轮。

契约（读 `modules/cellworld/files/game/modules/cellworldhost/runtime.go.tmpl` 的 `Start` 与它的注释）：Node 就绪后 `app.AddIngress(registry, "cellworld", Node)` 失败（App 停机第一步已开始、关了输入）时，`Start` 取消 Node 的 ctx、等 `Node.Run` 返回，再 `fail`：清掉 `cancel` / `done`、从 Host 上 `detach`，返回 AddIngress 的错误。之后 Runtime 是“未启动”：StartSpawns 拒绝、Stop 无事可做、再 Start 报 already started。

新场景 `TestCellworldStartAfterInputsClosedTakesTheNodeDown`（`runtime_test.go.tmpl`）：先 `app.CloseRegisteredIngress` 关掉 Registry 的输入，用一个记订阅数的替身 Transport 建 Runtime，然后核对：Start 返回错误且点名 `"cellworld"`；Node 曾经就绪（确认失败落在 AddIngress 而不是就绪前）；订阅数回到 0（Node.Run 已退出）；`done` / `cancel` 为空；Host 上没有挂着这个 Runtime；Stop、Host.Close 无错；StartSpawns 拒绝；再 Start 报 already started。

模块 README 原来写“AddIngress 失败的分支没有用例（生成工程拿不到关闭 Registry 准入的入口）”——core 已导出 `app.CloseRegisteredIngress`，生成工程可以用它；README 已改为 10 条并写明覆盖内容。

| 文件 | 内容 |
| --- | --- |
| `cellworld-race.txt` | 全新 `-template game` 工程（replace 到本检出）装 cellworld 后，`go test -mod=mod -race -count=1 -v -run '^TestCellworld' ./game/modules/cellworldhost ./game/managers`：10 条全过 |
| `cellworld-module-verify.txt` | 同一工程 `roost module verify cellworld`：`10 passed (want 10), 0 skipped`，go build / go vet / id check / generate --check 全过 |
| `cellworld-mutation.txt` / `cellworld-mutate.py` | 在生成工程渲染出的 `runtime.go` 上做 4 条变异，每条只跑新场景，跑完恢复原文件 |

变异（全部被杀）：

| 变异 | 新场景的红 |
| --- | --- |
| M1 忽略 AddIngress 的错误 | `Start succeeded although the App no longer accepts inputs` |
| M2 失败时不停 Node（去掉 `cancel()` 与 `<-r.done`） | `Node.Run still holds 2 subscriptions after the refused Start` |
| M3 失败时直接 `return err`、不调 `fail` | `left the stopped Node's done / cancel behind` + `still attached to the host` |
| M4 `fail` 里去掉 `host.detach(r)` | `the refused Runtime is still attached to the host` |

M4 是三条失败分支共用的一行；原有的 `TestCellworldFailedStartLeavesRuntimeNotStarted` 不检查 Host，新场景补上了这一点（只在 AddIngress 那条核对）。

`scripts/test-modules-generated.sh`：`all_modules` 里没有 cellworld，`all_counts` 也没有它的数，不需要改。模块目录核对 `TestModuleCatalogEmbedsEveryTemplateAndValidates`、`TestModuleEntityPackagesHoldOnlyComponentWiring`、`TestModuleVerifyCountsTopLevelScenariosOnly`、`TestModuleSpecValidationNamesWhatIsWrong` 已跑过（ok）；装全部模块的慢门禁（`TestModulesInstallOntoAGeneratedProjectAndVerify` 等）留给协调者最后的全量。

## 尚未确认

- account：Redis Cluster 后端这一遍没跑。
- dotnet：pretag 改动没有经全量 pretag 跑过（按任务不跑）；`TestClientSDKAgainstGeneratedPlayerTCP` 不认开关（见上）。
- cellworld：只在 macOS 本机跑；全部模块一次装完的慢门禁未跑。

## 建议写进 KNOWN-LIMITS 的文字（协调者统一写）

- 第 922 行 **account 新用例没在真实 Redis 上跑**：删除。（依据：本目录 `account-redis-*.txt`，单机真实 Redis 下 12 条新用例与整包 155 条全过，反证 7 条走存储的用例在坏地址下失败。）
- 第 892 行 **C# 运行时用例依赖本机 dotnet** 改写为一句说明：
  `TestClientConfigCSharpRuntime` 需要 .NET SDK：没有时 SKIP，设 `ROOST_REQUIRE_DOTNET=1` 时失败；pretag 的 `go test ./...` 与 C# 金样步骤都带这个开关，所以发版机器必须装 .NET 10 SDK（`TestClientSDKAgainstGeneratedPlayerTCP` 尚不认这个开关）。
- 第 931 行 **CellWorld Runtime** 改写为：
  模块场景 10 条，含 Start / Stop、失败复位（就绪前退出、ctx 先到、就绪后 AddIngress 失败）与 manager 托管，已验；业务 Entity 适配仍只在 demo。Linux 原始收尾样本未在本机复现，C-L6 需目标 Linux race 20 轮。

## 建议写进 CHANGELOG `[Unreleased]` 的文字（协调者统一写）

- **scripts**：pretag 的 `go test ./...` 与 C# 金样步骤带 `ROOST_REQUIRE_DOTNET=1`：发版机器缺 .NET SDK 时 pretag 失败，不再让 `TestClientConfigCSharpRuntime` 静默 SKIP。
- **cellworld 模块**（只对新安装的工程）：新增场景 `TestCellworldStartAfterInputsClosedTakesTheNodeDown`（Node 就绪后 AddIngress 失败时 Start 停下 Node、从 Host 拿下、回到未启动），`roost module verify cellworld` 9 → 10 条。

## 协调者补充（2026-11-17）

`TestClientSDKAgainstGeneratedPlayerTCP`（`codegen/internal/roost/player_client_test.go`）也认 `ROOST_REQUIRE_DOTNET=1`：把 dotnet 所在目录从 PATH 去掉后，带开关直接失败（`ROOST_REQUIRE_DOTNET=1 but dotnet is not on PATH`），不带开关仍 SKIP；有 dotnet 时带开关通过（154s）。KNOWN-LIMITS 二的“C# 运行时用例依赖本机 dotnet”因此删去（发版机器要装 .NET 10 SDK 写在维护手册与 CHANGELOG）。
