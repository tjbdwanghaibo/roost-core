# R2：换服 D7 A / D8 A 的实施证据（2026-11-17）

维护者 2026-11-14 已决定 D7 A 与 D8 A，本轮按决定实施，没有换方案。它们对应 KNOWN-LIMITS 二里的四条：

- 换服 committed 后源库旧档（Q4）
- committed 崩溃窗口（Q4）
- 向前恢复只由登录触发
- 期限只记录

平台是 macOS ARM64，go1.27.3。本轮只验同机独立进程，不涉及 Linux、跨机、独立 Gate 或容量。分支 `worktree-agent-a1f22e0285562330b`，基点 `265adb58`，**没有推送、合并或打 tag**。

`results.json` 汇总了四次真实进程演练的结论字段：run1 在修复前失败，run2、run3 全部通过，另有一次 D7 变异。它不含配置、凭据和日志全文。

## 1. 做了什么

| 提交 | 内容 |
| --- | --- |
| `dd4b3d05` feat(account) | **在途换服索引**：与角色记录在同一次原子写里维护。Redis 上用 `versionstore.RedisIndex` 有序集合 `<prefix>:transfers`，进程内用 `MemoryRoles`。<br>**新接口**：`ListRoleTransfers(from)` 和 `RoleBindingsAway(sid, ids)`，两者都上总线。<br>**期限**（`now >= DeadlineUnix`）**之后的提交**答 `ErrTransferStale`，并当场记为中止；中止只在同一次 CAS 里再看到“仍在途、已过期”才落地，与并发的提交是先落者生效。<br>**清扫**：account 进程每 30 秒清一次，每次至多 100 个。<br>**Cluster**：wiring/account 要求 Cluster 的 `key_prefix` 带 hash tag。<br>**真实 Redis 用例**：`TestAccountTransferDeadlinesAndIndexOnRedis`。 |
| `7f51bea8` feat(dataengine) | **本地起服预加载的准入检查**：`PreloadAdmitter` 和 `PreloadAdmitCapability(kind)`；被否决的计 `Skipped`、不加载；检查出错时起服失败。<br>**只列不加载**：`ListLocalEntityIDs`。 |
| `0fe42b5f` feat(demo) | `accountctl transfer-role list [-from SID]`。 |
| `57c710ee` feat(rehome) | **恢复循环** `RunRecovery`（`rehome_recover.go`），做两件事：<br>• **起服核对本服库里的玩家**。Player 预加载时走准入检查；不预加载时把本服库列一遍。account 绑在别处的不加载，按已迁走落定：立屏障、补搬并确认、按版本删本服的。<br>• **接着做在途换服**：每 30 秒把本服发起、迁出表里没有的在途换服交给 `Resume`。<br>**Directory** 加 `Transfers` 和 `BindingsAway`。<br>**模块场景** 50 → 61。<br>**`test-demo-rehome.sh`**：每个栅栏点重启后都不登录。 |
| `fdbb56e5` docs | PLAYER-MIGRATION §7、guide/02、OBSERVABILITY、FEATURE 换服方案，以及根包真实进程登记。 |
| `e0a3e8cd` fix(rehome) | **问题**：真实进程演练第一遍发现，manager Start 时就起了恢复循环，而 game-demo 要到 `Service.Init` 才发布自己的 Residency。恢复循环先用到迁出表，建出了缺省的 LocalResidency 并抢先发布写闸门，源服重启时起服失败。<br>**修法**：恢复循环改在 `service.started` 之后起。<br>**用例**：新增 1 条，模块场景 61 → 62。 |
| 本目录所在提交 | 证据。<br>`social_test.go.txt`（联盟 e2e）在 handed-off 点不再登录源服：登录会与恢复循环赛跑。<br>根包登记的范围文字同步更新。 |

### 与 D7 字面的出入

D7 原文是“起服预加载时核对”，但 game-demo 和模块缺省工程的 Player 不预加载（按需加载）。只挂在预加载上的话，这两类工程的旧档永远不会被核对，所以另加了一条：**Player 不预加载时，恢复循环起来后经 `ListLocalEntityIDs` 把本服库列一遍（只读 `_id`），每 500 人问一次 account**。

代价有两点：

- 起服后要做一次 `_id` 列举，外加 ⌈N/500⌉ 次 account 调用。这些在 Background 里跑，不挡起服。
- 核对做完之前，如果有业务路径加载了旧档玩家，他的定时逻辑可能写这份旧档（只写源库）。落定时交出屏障会把它卸载。

## 2. 先红后绿与变异

修前与变异的原始失败文本在下面逐条摘录；只有几行说明的条目注明了。

### account（D8）

**修前红**：在基点 `265adb58` 上只加用例。

```
--- FAIL: TestACommitAfterTheDeadlineIsRefusedAndAbortsTheTransfer (0.00s)
    transfer_test.go:1671: commit after the deadline = {PlayerID:1000001 ServerID:2 ServingServerID:2 BindEpoch:1 TransferSeq:1 Transfer:<nil> LastTransfer:0x5dcb42f19310}, <nil>; want ErrTransferStale
```

迟到的提交把角色改绑到了 2。`ListRoleTransfers`、`SweepExpiredTransfers` 和 `RoleBindingsAway` 修前都不存在，用例编译不过（API 级红）。

**变异**：

- **M-A1** `transferExpired` 恒为 false：
  - 迟到提交、清扫、Server 定期清扫三条红。
  - 例如：`sweep at the deadline = 0`。
- **M-A2** 索引条目不收录：
  - 清扫、列举红（`bus: transfers in flight = [], want [...]`）。
  - 真实 Redis 上红：`the transfer index on real Redis = map[], want the three roles scored by their deadlines`。
- **M-A3** `Server.run` 不调清扫：`TestTheAccountServerSweepsExpiredTransfers: did not abort the expired transfer`。

### DataEngine 准入

**修前**：API 不存在。

**变异 M-E1**，去掉“否决的跳过”：

```
--- FAIL: TestPreloadLocalEntitiesSkipsWhatTheAdmitterRefuses (0.00s)
    local_preload_test.go:235: report = {...Listed:6 Loaded:6 ... Skipped:0} ..., want 6 listed, 4 loaded, 2 skipped
```

### rehome 模块

在生成工程（enter + rehome + gm）里改一行、跑相关用例、再恢复：

- **M-R1** `AdmitPreload` 全放行：`admitted [7094301 7094302], want [7094302]: a player bound elsewhere must not be loaded` 等 3 条。
- **M-R2** 认出的旧档不入队：驱动器层 4 条红；controllers 层 `TestRehomeSourceRestartDropsTheCopyLeftByACrashAfterTheCommit: on the restarted source: [] want [close.. handoff.. copy overwrite=false.. drop at 1000]`。
- **M-R3** `resumeInFlight` 把所有换服当作已在做：
  - `TestRehomeRecoveryResumesATransferInFlightWithoutALogin: 0 background parts were started, want 1`。
  - 迟到提交那条也红。
  - controllers 两条：`on the restarted source: []`。
- **M-R4** manager 不起恢复循环，等于修前行为：controllers 层的“重启后不登录”4 条全红，事件为空，即没有登录就什么都不发生。
- **M-R5** controllers 不发布 `DriverCapability`：同上 3 条红。
- **M-R6** 准入检查发布到错误的 kind：`the preload admitter for Player = <nil> (false), want this process's driver`。
- **M-R7** 恢复循环在 manager Start 里直接起，即 run1 的缺陷：`TestRehomeRecoveryWaitsForTheServiceToPublishItsOwnResidency: before Service.Init the recovery loop already published *rehome.LocalResidency as the write gate`。

### accountctl

**修前**：`transfer-role list` 不存在，答 `want begin, commit or abort`。

**变异**：`-from` 被忽略。

```
--- FAIL: TestTransferRoleListShowsTheTransfersInFlight
    operator_test.go:254: transfers from 1001 = [{PlayerID:424242 ServerID:1000 ServingServerID:1000 ...}], want none
```

## 3. 真实依赖与真实进程

### 单机 Redis：共享隔离环境

先确认 `remote-acceptance.lock` 不存在。只 source 了 env，键前缀是 `itest:accountdeadline:<ns>`，用完由 `redistest.Cleanup` 清理。

`TestAccountTransferDeadlinesAndIndexOnRedis`：PASS。

### Redis Cluster：私有 mirror-local

offset 17500，3 主 3 从，用完 clean。

- `ROOST_REVIEW_CLUSTER=… go test -run 'Transfer|Deadline|Sweep|RoleBindingsAway|ListRoleTransfers' ./service/account/`：25 条 PASS。D8 的 5 条都跑在 Cluster 上，前缀带 hash tag。
- `go test -tags integration ./infra/storage/versionstore/`：ok。

### P1 真实进程演练（`scripts/test-demo-rehome.sh`）

环境：私有 mirror-local（Mongo 副本集、NATS、Redis），offset 17500；应用端口平移 30500；自起 etcd 41579；每次用完 clean。

| 运行 | 结果 |
| --- | --- |
| run1（`57c710ee`，修复前） | **失败**。accepted 点 SIGKILL 后源服重启起不来：`service game init: game: publish player write gate: registry: capability "access.player.write_gate" already registered by *rehome.LocalResidency`。首次起服没有可恢复的工作，所以没有暴露。修复见 `e0a3e8cd`。 |
| run2（`e0a3e8cd`） | 7 / 7 PASS。 |
| run3（`e0a3e8cd`） | 7 / 7 PASS。 |
| mutant-d7（committed 点；模板里把认出的旧档改为不入队，跑完恢复） | **按预期失败**：`FAIL: source documents not dropped without a login`。 |

run2 和 run3 的每个点，重启后都**没有任何登录**。脚本只读 account 绑定和两个库，直到绑定指向 1001、源库无档，才登录核对：

- **accepted / quiesced / leaving / handed-off / checkpoint**：kill 时 account 上换服在途。重启后源服日志先出现 `resuming a transfer this process did not finish`，绑定随后变为 1001、源档删除。整个过程无人登录（D8）。
- **committed**：
  - kill 时 account 已改绑到 1001，源库与目标库都有档，版本都是 7。
  - 重启后源服日志出现：
    - `a player stored on this server is bound to another server; his copy here is not loaded and is dropped once confirmed`
    - `checked … bound_elsewhere=1`
  - 不登录，源库那份即被删除（D7）。之前这个点明列 `KNOWN_LIMIT Q4`、要求源档保留，现在要求源档删除。
- **之后的核对**：所有点都核对了背包 11、钱包 93（重发扣款不再扣）、到达次数 1、源服拒绝入场、两侧 WAL 排空。

复跑方法见脚本头注释。依赖和演练要在同一个 shell 里启动：

```sh
export ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=17500
bash scripts/mirror-local.sh up && source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
DEMO_REHOME_OUT=/abs/out DEMO_REHOME_PORT_SHIFT=30500 DEMO_REHOME_ETCD_PORT=41579 bash scripts/test-demo-rehome.sh
bash scripts/mirror-local.sh clean
```

## 4. 验证命令与结果

所有 Go 命令都带 `GOWORK=off`。

**仓库内包**：

- `go vet`：service/account、infra/storage/versionstore、wiring/account、wiring/dataengine、framework/dataengine/engine，全部通过。
- `go vet -tags integration`：wiring/integration、service/account、infra/storage/versionstore，全部通过。
- `go test -race -count=1`：
  - service/account、infra/storage/versionstore、wiring/account、wiring/dataengine：ok。
  - framework/dataengine/engine（`-run 'Preload|ListLocalEntity'`）：ok。
- 根包 `go test -count=1 -run 'TestCI|TestRedisCluster|TestOptIn|TestGeneratedProject|TestRealProcess|TestFaultMatrix|TestAuthCredentials' .`：ok。

**`roost module verify`**（生成工程，`replace` 指向本 worktree）：

| 模块组合 | 结果 |
| --- | --- |
| enter + rehome + gm | rehome 62/62；rehome+enter 5/5；rehome+gm 4/4；enter 29/29；gm 9/9。build、vet、id check、`generate --check` 都 ok。 |
| enter rehome mailbox team alliance room matchroom matchmaking | 全部组合都与预期条数一致，其中 rehome 62，带 +rehome 的组合各 2，alliance 55，team 35。build、vet、id check、`generate --check` 都 ok。 |

**codegen**：`go test -timeout 60m -run 'TestModuleGMScenariosAndMutations|TestModuleAllianceGenerated|TestModuleTeamGenerated|TestModuleMatchmakingGenerated' ./codegen/internal/roost/`。期望值已改为 rehome 62，结果 ok（365.6s）。

**game-demo**（新生成）：`go vet ./...` 通过；`go test -race` 跑 cmd/accountctl、game/modules/rehome、game/controllers、game/managers、internal/service/game，全部 ok。

**未运行**：全量 `go test ./...`、pretag、`scripts/test-modules-generated.sh` 全量，以及联盟 e2e（`scripts/test-alliance-e2e.sh`，本轮改了它的 `social_test.go.txt`）。

## 5. 尚未确认

- **联盟 e2e 没有运行**：`TestAllianceE2ESocialRehome` 的 handed-off 点改成不登录、等 account 改绑。改法与 P1 演练相同，但这个用例本身尚未确认。
- **Player 预加载（`preload=startup`）的工程走准入检查这条路**，只有单测和 controllers 层用例覆盖。真实进程演练用的 game-demo 不预加载 Player，走的是列库那条路。所以“预加载时 account 问不到 → 起服失败”在真实进程上尚未确认。
- **Redis Cluster 下 account 的 Mod 级装配**（`key_prefix` 带 hash tag、清扫在真实 account 进程里跑）尚未确认。已确认的只有契约测试跑在 Cluster 上；演练用的是单机 Redis。
- **account 清扫中止“源服一直没起来”的换服**，只有单测和真实 Redis 契约用例覆盖，真实进程演练没有构造这种情形（每个点源服都重启了）。
- **大库起服核对的耗时**（例如十万量级玩家的 `_id` 列举，加每 500 人一次 account 调用）没有测量。

## 6. 要维护者定的点

1. **目标服已有别的版本时，源档怎么处理**。这发生在目标服在源服重启之前已经写过他，或者运维在模块外提交后目标服新建了他。现在的做法：源档保留、屏障立着（本进程不加载、不写）、记 Error，留给运维；只看版本号判断不了哪一份才是他的。
   - **A（推荐，现状）**：保留并记 Error，运维对账。
   - **B**：以目标服为准直接删源档。代价是目标服若是“新建的空档”，源服那份真实数据就丢了。
2. **Player 不预加载时的起服列库**（与 D7 字面的出入）：
   - **A（推荐，现状）**：保留它。代价是起服后一次列举，外加每 500 人一次 account 调用，在后台跑。
   - **B**：要求装 rehome 的工程把 Player 设为 `preload=startup`，只走准入检查。代价是起服内存与时长随玩家数增长。
3. **`MergeServer` 遇到在途换服**。索引有了，但合服时怎么处理指向被合并服的在途换服还没有定，现在靠提交时拒绝与期限中止兜底。是否在 sidmerge 清单里加“无在途换服”，属于三期的另一项，本轮没做。

## 7. 拟写的 CHANGELOG / KNOWN-LIMITS 文本

本轮没有改这两个文件，由协调者合入。

### CHANGELOG `[Unreleased]`

```markdown
### Added
- account：在途换服索引与 `ListRoleTransfers(from)` / `RoleBindingsAway(sid, ids)`（上总线）；account 进程每 30 秒清扫过期的在途换服（D8 A）。`accountctl transfer-role list [-from SID]`。
- DataEngine：本地起服预加载的准入检查（`wiringde.PreloadAdmitter`，`PreloadAdmitCapability(kind)`；被否决的计 `skipped`，指标 `dataengine.local_preload_total{result="skipped"}`）与只列不加载的 `Mod.ListLocalEntityIDs`、`ErrLocalEntityKind`、`LocalPreloadMod.Kinds()`。
- rehome 模块：源服重启后的恢复循环（`service.started` 之后起）——起服核对本服库里的玩家，account 绑在别处的旧档不加载、补搬确认后按版本删除（D7 A）；之后定期按 account 的在途索引接着做本服发起的换服，不再只靠玩家登录（D8 A）。模块场景 50 → 62。

### Changed
- account：期限（`DeadlineUnix`，`now >= DeadlineUnix`）之后的 `CommitRoleTransfer` 答 `ErrTransferStale` 并把这一次记为已中止（`LastTransfer.Note` 以 `expired:` 开头）；原来期限只记录。
- account 角色存储：Redis 上多一个有序集合 `<key_prefix>:transfers`，与角色记录同一个脚本维护；**Redis Cluster 上 account 的 `key_prefix` 必须带 hash tag**（如 `{acct}`），否则 Mod 初始化报错。升级：先停旧 account 再起新的；已有的在途换服不在索引里（未部署，不做迁移），需要时用 `accountctl transfer-role abort` 了结或清空。
- `scripts/test-demo-rehome.sh`：六个 SIGKILL 点重启后都不登录，committed 点要求源档删除（原来按 KNOWN_LIMIT Q4 要求保留）。

### Fixed
- rehome：提交之后、删源库之前源服崩溃留下的旧源档不再被起服载入内存、不再需要运维对账（KNOWN-LIMITS Q4）。
```

### KNOWN-LIMITS

**删除这四条**：

- “换服 committed 后源库旧档（Q4）”
- “committed 崩溃窗口（Q4）”
- “向前恢复只由登录触发”
- “期限只记录”

**新增或保留为框架限制**，放在“单个玩家换服”节：

```markdown
- **提交后源档与目标服已有别的版本冲突**：源服重启时起服核对认出旧源档、补搬到目标服时，目标服已有**别的**版本（目标服在源服重启之前已经写过他、或模块外提交后目标服新建了他）——源档保留、屏障立着（本进程不加载、不写）、记 Error，由运维对账；只看版本号判断不了哪一份是他的数据。见[R2 证据](evidence/r2-rehome-d7-d8-20261117/README.md)。（框架限制；处理方式待维护者确认，见证据 §6 第 1 条）
- **Player 不预加载时的起服核对窗口**：起服后恢复循环把本服库列一遍再问 account（每 500 人一次），做完之前若有业务路径加载了旧档玩家，他的定时逻辑可能写这份旧档（只写源库，不影响目标服），落定时交出屏障卸载它。代价随本服库玩家数增长。（框架限制）
- **合服与在途换服**：`MergeServer` 不检查指向被合并服的在途换服；靠提交时拒绝与期限中止兜底。（遗留，属三期“sidmerge 清单加无在途换服”）
```

**还要改的提法**：KNOWN-LIMITS 里另有两处把 Q4 写成“保留”。

- P7 社交那段（“双 sid 迁服 handed-off / committed SIGKILL 已验（其中 committed 保留 Q4）”）：应改为“那一轮 committed 保留 Q4，2026-11-17 起由起服核对清理”。
- 2026-11-14 收口那条（“P1 六个真实换服 SIGKILL 点（Q4 保留）”）：是历史记录，可加一句“Q4 已于 2026-11-17 由 D7 关闭，见 R2 证据”。

**外部环境验收**：真实进程演练只在 macOS 同机单机 Redis 上跑过；Cluster 上的 account 进程级装配、Player 预加载工程的准入检查路径、联盟 e2e 的 handed-off 点改法都尚未确认（见 §5）。
