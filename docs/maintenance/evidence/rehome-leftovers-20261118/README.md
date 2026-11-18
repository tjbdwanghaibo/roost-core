# 换服遗留“合服与在途换服”一条：本机能做的四件（2026-11-18）

KNOWN-LIMITS“合服与在途换服”原文四项：`MergeServer` 不检查在途换服；联盟 e2e `TestAllianceE2ESocialRehome` 的 handed-off
改法没跑过；Player 预加载工程的准入检查路径只有离线覆盖；Cluster 下 account 进程级装配与“源服一直没起来”的清扫中止只有契约测试。

平台 macOS ARM64，go1.27.3，同机独立进程。依赖：私有 mirror-local（偏移 15000：Mongo 42117–42119、NATS 29222–29224、
Redis 31379、Redis Cluster 32400–32405、toxiproxy 33474），自起 etcd（e2e 34979，演练 34989），应用端口平移 43000 /
e2e 端口基 37700。分支 `worktree-agent-ab0a4d33433fb6bb5`，基点 `8940ca73`，**没有推送、合并或打 tag**。

结论：四件都做完。第 3 件顺带发现并修了一个缺陷（预加载 Mod 没有把自己发布到 Registry，rehome 在 Player 预加载时
仍把本服库列一遍）。game-demo 的 Player 不能改成预加载（见 §3），第 3、4 件改在一个最小模块工程上做，写成了可复跑的
脚本 `scripts/test-rehome-preload-cluster.sh`。

## 1. 合服检查在途换服（代码，main 提交 `06e3e9b4`）

**方案核对**：换服方案（FEATURE-2026-11-11-player-rehome §7 分期表）三期列有“`sidmerge` 清单加‘无在途换服’”，是已定做法；
R2 证据 §6 第 3 条只说“是否加属于三期另一项、本轮没做”，没有相反的决定。

**位置**：放在 account 的 `MergeServer`，不放 `wiring/sidmerge`。理由：sidmerge 按设计只读配置、不连 account（“清单只读配置、
不连任何依赖”），在途换服只有 account 知道；`MergeServer` 是唯一写合服记录的地方，在写之前检查才是权威的一次。runbook
第 3 步（前置检查）加跑 `accountctl merge-server … -dry-run`，dry-run 同样拒绝，所以运维在迁数据之前就能看到。

**行为**：

- 写之前按在途索引列出以 `From` 为**来源或目标**的全部在途换服（不设条数上限：截断的列表证明不了“一个都没有”）。有就答
  `ErrTransferInProgress`、什么也不写；`MergeResult.TransfersInFlight` 列出它们（`accountctl merge-server` 本来就在拒绝时也打印
  结果）；错误文本点名至多 10 个并给出 `accountctl transfer-role list` / `transfer-role abort -player P -id ID -note …` 或等期限中止。
- 与被并走的服无关的换服（别处来去、去承载服 `Into`）不挡。
- 只在这次合服会写东西时检查（已经合完的同一对重跑仍幂等成功）。
- 角色存储没有在途索引时答 `ErrNoTransferIndex`（证明不了就不合）。生产的 Redis 角色存储总带索引。
- 检查与写不原子：检查之后才落下的发起照旧由提交时拒绝（目标服已并走）与期限中止兜底。停服窗口里 A、B 都是 maintenance、
  game 进程都停了，发起要求目标服 open、发起方是承载服，只剩人工发起的竞态。

**测试改动**（没有放宽断言、删除或跳过）：

- account 包测试缺省的进程内角色存储从 `versionstore.MemoryStore` 改成带索引的 `MemoryRoles`（与生产一致）；`wiring/gate`
  一处、login / portal 模块用例模板各一处同样改。要“没有索引”的 `TestListingTransfersNeedsTheIndex` 显式换回普通存储。
- `TestTransfersInvolvingAMergedAwayServerAreRefused` 的两条“合服期间在途”子用例原来直接 `mustMerge`；现在先断言 `MergeServer`
  拒绝，再用 `raceMerge` 直接写两条服务器记录模拟竞态，原来对提交兜底的断言不变。

**先红后绿**：在基点上只加 `TestAMergeIsRefusedWhileATransferInvolvesTheMergedServer`（不引用新字段）：

```
--- FAIL: TestAMergeIsRefusedWhileATransferInvolvesTheMergedServer (0.00s)
    merge_test.go:433: merge (dry-run true) with transfers in flight from and onto 2 = <nil>, want ErrTransferInProgress
```

**变异**（各自撤掉一行，跑 `./service/account/`）：

- M1 拒绝分支不生效（`if len(flights) > 0 && false`）：本用例 `= <nil>, want ErrTransferInProgress`；兜底用例两条
  `merging the target / source of a transfer in flight = <nil>`。
- M2 只看来源不看目标：`the refused merge lists [1000001:out], want [1000001:out 1000002:in]`；兜底用例
  `merging the target of a transfer in flight = <nil>`。
- M3 没有索引时放行：`merge without an index = <nil>, want ErrNoTransferIndex`。

**真实 Redis**：

- 共享隔离环境的单机 Redis（`remote-acceptance.lock` 不存在；只 source env；前缀 `revn06:account:<ns>`，用例自己清理），
  `ROOST_REVIEW3_BACKEND=redis go test -race -run 'Merge|Transfer|Index' -v ./service/account/`：35 条 PASS，ok
  （`account-contract-redis-single.txt`）。
- 私有 Redis Cluster（`ROOST_REVIEW_CLUSTER`，前缀带 hash tag），`-run 'Merge|Transfer|Deadline|Sweep|RoleBindingsAway|ListRoleTransfers|Index'`：
  36 条 PASS（`account-contract-redis-cluster.txt`）。

**文档**：PLAYER-MIGRATION §6.1 第 3、7 步、§6.4、§7.5；FEATURE 换服方案分期表；`service/account/admin.go` 接口注释。

## 2. `TestAllianceE2ESocialRehome`（真实依赖，没有改动）

```
source <私有 mirror-local>/roost-dataengine-it/env.sh
ALLIANCE_E2E_RUN='^TestAllianceE2ESocialRehome$' ALLIANCE_E2E_ETCD_PORT=34979 ALLIANCE_E2E_PORT_BASE=37700 \
  ALLIANCE_E2E_LOGS=<目录> bash scripts/test-alliance-e2e.sh
```

结果：`--- PASS: TestAllianceE2ESocialRehome (40.56s)`，`alliance actual cases: passed=1, skipped=0`（`e2e-social-rehome.log`）。

源服 game-1001 的日志（`e2e-social-rehome-game1001-rehome.log`）证实两点都是不登录、由恢复循环完成的：

- handed-off 点 SIGKILL 后重启：`resuming a transfer this process did not finish player_id=100002`，随后
  `the player is bound to another server; this process keeps its fence`；用例等到 account 改绑 1000 之后才登录核对。
- committed 点 SIGKILL 后重启：`a player stored on this server is bound to another server; his copy here is not loaded`、
  `checked=1 bound_elsewhere=1`。

用例本身对 committed 点仍写着 “Q4 old-source timers/writes not certified”（脚本头注释“Q4 保留”）：用例不核对源库旧档是否删除，
这一点由 `test-demo-rehome.sh` 覆盖；文字是旧的，本轮没改。

## 3. Player 预加载工程的准入检查（真实进程；发现并修了一个缺陷，main 提交 `d9b47375`）

**game-demo 做不了**：先在 `test-demo-rehome.sh` 里加选项把 game-demo 的 Player 标成 `preload=startup`，生成器拒绝两次
（`game-demo-preload-attempt.log`）：alliance 也加载 Player，预加载要写 `service=game`；写了之后 game-demo 的
`alliance_credit`（Player 上的 saga 原生步骤）又被拒——“native steps on service-owned kinds are not supported”。这个选项已撤回。

**改用最小模块工程**：缺省模板 + `roost add entity player` + `roost module add enter rehome gm`，Player 标 `preload=startup`，
`roost project sync` 装上 `dataengine.preload`。account 的种子工具直接开 account 存储造角色，Player 文档用 mongosh 写进
`<库>_1000.player`（`_schema:1`、`_version:3`）。

**发现的缺陷**：第一次起 game 1000（`drill-red-before-fix-game1000.log`）：

```
dataengine preload mod: preloaded a kind kind=1 listed=2 loaded=1 skipped=1        ← 准入检查已核对
rehome: a player stored on this server is bound to another server ... player_id=60443813   ← 第二次
rehome: checked the players stored on this server against the account service checked=2 bound_elsewhere=1  ← 又列了一遍库
```

根因：`rehome_recover.go` 的 `checkStoredPlayers` 用 `app.Lookup[*wiringde.LocalPreloadMod](registry, mods.ModDataEnginePreload)`
判断 Player 是否预加载，而 `wiring/dataengine/local_preload.go` 的 `LocalPreloadMod.Provide` 只记下 Registry、从不发布自己，
查找永远落空。后果：预加载工程起服时同时走两条核对路径（重复的 `_id` 列举与 ⌈N/500⌉ 次 account 调用、同一份旧档两次入队，
落定本身幂等，没有数据错误），与现行契约“预加载时经准入检查，不预加载时列一遍本服库”不符。R2 的单测与 controllers 用例没有
把真实的 `LocalPreloadMod` 装进 Registry，所以没测到。

修法：`Provide` 以 `mods.ModDataEnginePreload` 把自己发布到 Registry（DataEngine Mod 也是这样发布自己的）。

- 红：`TestLocalPreloadModIsPublishedUnderItsName`：`lookup "dataengine.preload" after Provide = <nil>, false; want this mod, preloading kind 215`。
- 绿：`go test -race ./wiring/dataengine/` ok。
- 真实进程修后（`drill-manual-fixed-round.txt`）：只有一次 `bound to another server` 与 `listed=2 loaded=1 skipped=1`，没有
  `checked the players stored`；旧档以版本 5 出现在 `<库>_1001`、从 `<库>_1000` 删除。
- 变异 M-P1（工程里 `AdmitPreload` 全放行，`drill-manual-mutant-admit-all.txt`）：`listed=2 loaded=2 skipped=0`——旧档被加载。
  旧档之后照样被落定删除，所以“不加载”只能由预加载报告的 `skipped` 断言，脚本按它判定。
- 变异 M-P2（撤掉这次修复，跑脚本，`drill-mutants/unpublished.log`）：`FAIL: PRELOAD: the startup check listed the database instead of using the admitter`。

“预加载时 account 问不到 → 起服失败”在真实进程上仍做不出来：`ServingGuardMod` 排在 DataEngine 之前，account 不可达时它先
拒绝启动（fail-closed），走不到预加载。这条仍只有单测覆盖（`framework/dataengine/engine` 的 `TestPreloadLocalEntitiesSkipsWhatTheAdmitterRefuses`：准入检查报错时
预加载返回那个错误）。

## 4. Cluster 下 account 进程级装配与“源服一直没起来”的清扫中止（真实进程）

与第 3 件同一个工程、同一个脚本：account 的 `redis.cluster_addrs` 指向私有 Cluster（3 主 3 从）。

- 不带 hash tag 的 `key_prefix`：进程退出，`app run failed … account.key_prefix ("…-acct-untagged") requires a non-empty closed first
  hash tag for Redis Cluster`（`drill-green/account-untagged.log`）。
- 带 hash tag `{<工程>-acct}`：account 起来，`redis mod: connected … cluster=true`，`account server: handlers registered methods=26`。
- 种子造一个从不启动的 1002 发起、到 1001、期限 2 秒的换服。account 进程的清扫（30 秒一次）在期限之后约 26 秒中止它：
  `account: aborted a role transfer past its deadline`、`aborted transfers past their deadline count=1`；角色仍绑 1002，
  `last_transfer.committed=false`，`note` 为 `expired: the deadline … passed before the transfer was committed`
  （`drill-green/never-binding.json`、`account-sweep.log`）。
- 变异：撤掉 Mod 里的 Cluster 前缀检查（`drill-mutants/untagged.log`）：`FAIL: CLUSTER: account kept running with a key_prefix without a hash tag`
  ——不检查时进程照常起来，要到第一次换服才会在服务端报 CROSSSLOT；撤掉 `Server.run` 里的清扫调用（`drill-mutants/nosweep.log`）：
  `FAIL: SWEEP: the never-started transfer was not aborted as expired`。

## 5. 可复跑的脚本

`scripts/test-rehome-preload-cluster.sh`（夹具 `scripts/testdata/rehome-preload/seed.go.txt`，复用 `scripts/testdata/demo-drill/lib.sh`
的 drill_* 函数；已登记进根包 `realProcessDependencyEntries`）：

```sh
export ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=15000
bash scripts/mirror-local.sh up && source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
REHOME_PRELOAD_OUT=/abs/out REHOME_PRELOAD_PORT_SHIFT=43000 REHOME_PRELOAD_ETCD_PORT=34989 bash scripts/test-rehome-preload-cluster.sh
bash scripts/mirror-local.sh clean
```

绿：两次（修后的手工一轮之外，脚本 run1、run2 各 PASS，`drill-green/` 是 run2）；约 50 秒（生成与编译走缓存）。三个变异都按预期失败。
结束时删本工程的库、流、单机与 Cluster 上的键（核对过私有环境里没有残留）。

## 6. 验证命令与结果

所有 Go 命令带 `GOWORK=off`。

| 命令 | 结果 |
| --- | --- |
| `go test -race -count=1 ./service/account/` | ok |
| `go test -race -count=1 ./wiring/gate/ ./wiring/account/ ./wiring/dataengine/` | ok |
| `go vet ./service/account/ ./wiring/gate/ ./wiring/dataengine/` | 干净 |
| `go test -count=1 -timeout 60m -run 'TestModuleLoginReplacesTheAuthenticatorAndAccountStubs\|TestModulePortalInstallsOnTheHostedAccountService\|TestModulePortalInstallsOnALobbyProjectWithoutGame' ./codegen/internal/roost/`（改了 login / portal 用例模板） | ok（50.8s；portal 用例内的 `VerifyModules` 跑 login 33、portal 15、portal+login 1、portal+enter 2 条场景） |
| `go test -count=1 .`（根包） | ok |
| 第 1 件真实 Redis 单机 / Cluster | 见 §1 |
| 联盟 e2e | 见 §2 |
| `scripts/test-rehome-preload-cluster.sh` | 见 §5 |

**未运行**：全量 `go test ./...`、pretag、`scripts/test-demo-rehome.sh`（没改它）、`scripts/test-demo-merge.sh`（走 `merge-server`，
演练里没有在途换服，预期不受影响，尚未确认）、rehome 模块的 `roost module verify`（没改 rehome 模板）。

## 7. 尚未确认 / 限制

- **预加载时 account 不可达 → 起服失败**：真实进程上被 `ServingGuardMod` 先挡住（见 §3），只有单测覆盖。
- **大库起服核对的耗时**（R2 遗留）没有测量；本轮修复去掉的是预加载工程上的重复列库，不涉及非预加载工程的列库代价。
- **game-demo 不能预加载 Player**：这是生成器规则（共用 sid 的服务都能加载的 kind 要点名 service，点名了 service 的 kind 上不能有
  saga 原生步骤）与 game-demo 结构的组合，不是本轮要改的；预加载路径的真实进程验收落在模块工程上。
- **合服检查与写不原子**（框架限制，见 §1）。
- 平台只覆盖 macOS 同机；Linux、跨机没有跑。

## 8. 要维护者定的点

1. **没有在途索引时合服怎么办**：
   - **A（现做法，推荐）**：拒绝（`ErrNoTransferIndex`）。生产的 Redis 存储总带索引；包了一层的存储（例如加指标的装饰器）
     丢了索引时合服会报错而不是静默跳过检查。代价：测试与调试用的进程内存储要用 `MemoryRoles`（本轮已改仓内用例与模块模板）。
   - **B**：放行并记 Warn。代价：存储被包一层时检查悄悄消失。
2. **是否给 `wiring/sidmerge` 的清单也加一项“在途换服（由 account 的 merge-server 检查）”的说明**：现在只在 runbook 第 3、7 步写了；
   sidmerge 清单（`checklist` / doctor `merge-readiness`）没有这一项。推荐不加（清单里的项是 sidmerge 自己检查或要人工确认的，
   这一项由 account 强制，dry-run 已在第 3 步）。

## 9. 拟写的 CHANGELOG / KNOWN-LIMITS 文本

本轮没有改这两个文件，由协调者合入。

### CHANGELOG `[Unreleased]`

```markdown
### Changed
- account：`MergeServer`（`accountctl merge-server`，含 `-dry-run`）在被并走的服是在途换服的来源或目标时拒绝（`ErrTransferInProgress`），
  `MergeResult.TransfersInFlight` 列出这些换服，错误给出 `transfer-role list / abort` 的提示；角色存储没有在途索引时答 `ErrNoTransferIndex`。
  合服 runbook 第 3 步加跑 `merge-server -dry-run`。测试用的进程内角色存储请用 `account.NewMemoryRoles()`（带索引）。

### Fixed
- DataEngine：`LocalPreloadMod` 在 Provide 里把自己发布为 `dataengine.preload`。原来不发布，rehome 判断不出 Player 已起服预加载，
  预加载工程起服时除了预加载的准入检查，还会再把本服库列一遍、再问一遍 account。

### Added
- `scripts/test-rehome-preload-cluster.sh`：最小模块工程（enter + rehome + gm、Player `preload=startup`、account 走 Redis Cluster）的
  真实进程演练：不带 hash tag 的 `account.key_prefix` 在 Mod 初始化被拒；起服预加载的准入检查否决 account 绑在别处的旧档；
  源服从不启动的在途换服由 account 进程的清扫按期限中止。不进 pretag。
```

### KNOWN-LIMITS

**删除**“合服与在途换服”整条（约 908 行）。四项都已做完：合服检查（本证据 §1）、联盟 e2e（§2）、预加载准入检查（§3）、
Cluster 装配与清扫中止（§4）。

**改**“涉及已合服的服”一条（约 650 行）的末句：

```markdown
`MergeServer` 在被并走的服是在途换服的来源或目标时拒绝并列出它们（2026-11-18，[证据](evidence/rehome-leftovers-20261118/README.md)）；
检查在写之前做一次、不与写原子，检查之后才落下的发起（停服窗口里只可能是人工发起）由提交时拒绝（目标服已并走）与期限中止兜底。
```

**新增或保留为框架限制 / 外部环境验收**（放在“单个玩家换服”节）：

```markdown
- **预加载时 account 不可达**：Player 预加载工程起服时准入检查问不到 account 会让起服失败；真实进程上 `ServingGuardMod` 先于 DataEngine
  拒绝启动，这条路径只有单测覆盖。（框架限制）
- **game-demo 的 Player 不能预加载**：alliance 也加载 Player，预加载要写 `service=game`，而 game-demo 在 Player 上有 saga 原生步骤，
  生成器不允许；预加载路径的真实进程验收在 `scripts/test-rehome-preload-cluster.sh` 的模块工程上。（框架限制）
- **外部环境**：换服的真实进程演练只在 macOS 同机跑过（单机 Redis 与 Redis Cluster 各一次），Linux、跨机、独立 Gate 尚未确认。
```
