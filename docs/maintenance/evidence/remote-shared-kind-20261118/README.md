# Remote 共享 kind：首写认领一条插入就是共享记录；facebook 的 Ensure 简化（2026-11-18）

任务来源：维护者 2026-11-18 对 [nest-readonly-claim-20261118](../nest-readonly-claim-20261118/README.md) 第九节两个决定点的答复——
(1) 选 A：Remote 框架加 kind 级“共享”声明，首写认领一条插入就直接是共享记录，facebook 的 `Ensure` 随之简化；
(2) facebook 的读守卫保留，只改过时注释。基线 main `8940ca73`。codebase-memory-mcp 本会话连不上，以下都按当前源码读出。

## 一、设计（先定，再实施）

### 问题

facebook 建实体是“认领 → 切共享”两步（`ClaimRemoteOwnership` → `EnterRemoteSharedMode`）：共享是运行期的所有权状态，
只能由 owner 在认领之后切。两个 game 并发首投同一个人时，插入输的一方读回独占记录被 fence（facebook 报 `ErrOpening`，
这份邀请不进收件箱）；赢的一方在两步之间崩溃时，记录停在它的独占，别的 game 没有时限地 `ErrOpening`。

### 声明放在哪

沿用 `snapshots=none` 的先例（Remote 托管 kind 级选项，同一套注解、同一个生成位置）：

- 注解：`//roost:entity … remote=managed ownership=shared`。键 `ownership`，取值 `exclusive`（缺省）/ `shared`；`shared` 只能写在
  `remote=managed` 上，拼错的值、写在本地实体上都在生成前拒绝（`codegen/internal/entity/parse.go` `validateMarkerValues`）。
  选键名 `ownership` 而不是 `shared=true`：与 `snapshots=published|none` 同为枚举，读起来是“所有权模式”，以后要别的模式不必再加布尔键。
  **名字可以改**（见“要维护者定的点”）。
- 生成：`register<Entity>Entity` 在登记 builder 的同一个 once 里调 `entity.DeclareRemoteSharedKind(kind)`（与
  `DisableRemoteSnapshots` 同处）。运行期判据 `entity.RemoteKindShared(kind)`（`framework/entity/remote_shared_kind.go`，
  进程级登记表，与快照开关同形）。只在登记过该 kind 的进程里成立：认领只发生在会构建实例的进程里（写准入要加载实例），
  这些进程都登记了它。

### 认领怎么变

- `WriteAuthority.ClaimAuthorityState(ctx, id, owner, shared bool)`（加 `shared` 参数；只有 `MongoCommitter` 实现）：
  `shared` 为 true 时插入的第一代记录 `_owner_shared=true`、代际 1/1——**一条 InsertOne，原子，没有独占中间态**；owner 字段只是
  第一个认领者，共享模式下 `GrantWrite` 不看 owner。插入撞上已有记录时读回：读回的是共享记录就照样返回（不 fence），是别人的
  独占记录仍 fence。
- 写准入的首写认领与 `ClaimRemoteOwnership` 共用 `remoteEntityWrapper.claimOwnership`（按 `RemoteKindShared(w.kind)` 传 `shared`），
  认领结果用同一个 `acceptsClaimedLease` 判：本节点的，或共享 kind 的共享记录。之后 `isMarked()` 为真，准入照常走共享路径
  （分布式锁 + 写许可、落后的本地实例从权威重载）。
- 与 `confirmExistsBeforeClaim`（不存在不认领）：不变、在前。内存里没有实例、冷加载答不存在时不认领，请求答
  `entity_not_found`；业务先 `GetOrCreate` 进内存再写，第一次写时认领。共享 kind 上读一个还没建的实体同样不认领。
- 非持久所有权存储（Redis marker，自行 `SetOwnershipStore` 的装配）：没有一条原子写出共享记录的认领（只能认领后再切，
  正是要去掉的中间态），认领共享 kind 返回 `ErrRemoteWriteCapabilityDisabled`。正式装配都是 Mongo 持久权威，不受影响。

### 共享 kind 还许不许切回独占 / 转移（最小语义）

- **退出共享：拒绝**。`LeaveRemoteSharedMode` 对共享 kind 不取锁、直接返回 `ErrRemoteOwnerTransition`。理由：声明承诺“每条记录
  都是共享的”；退回独占就又是“只有 owner 能写、别的进程被 fence”，正是声明要消除的状态。一行判断，不新增状态。
- **转移所有权：不变**。只有记录上的 owner 能转，转完仍是共享；共享实体不进 `ListOwnedRemoteEntities`，缩容 / 迁移本来就不枚举它。
- **进入共享**：记录生来共享，`EnterRemoteSharedMode` 按现有规则答 fence（已是共享），不用改。
- 空认领：共享 kind 认领后不提交留下的是共享的空认领，不卡任何人；`DeleteEmptyClaimExpected` 只删非共享的，`empty_claim_total{left}`
  只在独占路径计，都不变。

### 未声明的 kind

行为逐字不变：`ClaimAuthorityState(…, false)` 与修前同一条插入、同一个读回判断；`acceptsClaimedLease` 对未声明的 kind 等于原来的
`OwnerSid == localSid`；`LeaveRemoteSharedMode` 的新判断只对声明的 kind 生效。本包全部既有用例（含 `TestRequestOnAnEntityThatDoesNotExistClaimsNothing`
的“认领 → 切共享”路径）不改仍绿。

### 没有选的做法

- 运行期按实体参数选共享（在 `PrepareRemoteWriteBatch` / `ClaimRemoteOwnership` 上加选项）：同一个 kind 可能被两种方式认领，
  又回到“谁先到谁定模式”；kind 级声明一次定死。
- 存储层（`MongoCommitter.ClaimOwnership`）自己查 kind 登记表：隐式依赖进程级登记，读 `ClaimAuthorityState` 的调用方看不出来；
  改成调用方显式传 `shared`。

## 二、先红（修前行为：声明 API 已加、认领未改）

`framework/remoteentity/write_admission_test.go` 新增三条（进程替身：两个 / 三个 Manager 共用 Mongo 权威替身与 Redis 版本锁替身，
正式 `ManagerAccess` loader + 真 Nest 引擎；共享 kind 用本包唯一声明为共享的 kind 147）：

- `TestSharedKindFirstWritesFromTwoProcessesBothSucceed`：A、B 都 `GetOrCreate` 进内存，B 已缓存“没有记录”（等价于两边都在对方
  插入前查过），A 首写、B 首写；断言 A 首写后记录直接共享、代际 1、版本 1，B 不被 fence，两边读到 `a=A1;b=B1@2`，再写再读；
  另 8 组真正并发（goroutine 同时发）的首写都成功。
- `TestSharedKindClaimerCrashLeavesOthersWriting`：A 准入认领后放弃事务、不再出现（崩溃），B 照常新建写入，同 sid 重启的 A 读到并接着写。
- `TestSharedKindOwnershipOperations`：`ClaimRemoteOwnership` 一条插入就共享、别的进程再调答出同一条记录；退出共享被拒且记录不变；
  共享 kind 上读不存在的实体答 `entity_not_found` 且不认领；非持久存储上认领拒绝。

`red-framework.log`（修前，断言落在“记录生来共享”本身）：

```
write_admission_test.go:1670: ownership record after A's first write: {… Owner:1000 Shared:false Marker:1 Route:1 …} found=true; want shared from the claim (owner 1000, epoch 1, version 1)
write_admission_test.go:1740: ownership record left by A's claim: {… Version:0 … Owner:1000 Shared:false …} found=true; want a shared record without commits
write_admission_test.go:1771: A claim of a shared kind: {OwnerSid:1000 MarkerEpoch:1 RouteEpoch:1 Shared:false} err=<nil>; want shared at epoch 1
```

`red-framework-consequence.log`（修前，临时把两条记录断言改成 Logf 看后果，已改回）——就是 facebook 的 `ErrOpening`：

```
write_admission_test.go:1673: B first write after A's claim: remote entity: writer fenced: entity=… owner=1000 marker=1 route=1 shared=false
write_admission_test.go:1747: B write after A crashed holding the first claim: remote entity: writer fenced: entity=… owner=1000
```

真实 Mongo 副本集 + 真实 Redis（共享隔离环境 `~/.roost-it/roost-dataengine-it/env.sh`，库名 `shared_kind_it_<随机>`、锁键
`roost-remote-it:<用例名>:<随机>`，用后删库删键）：`ownership_redis_integration_test.go` 的 `TestRealSharedKindFirstWritesFromTwoProcesses`
（12 个实体，每个两个进程同时首写 + 同 sid 重启读写）。修前认领（把共享插入与读回接受临时撤掉，已恢复）`it-red-unshared-claim.log`：

```
ownership_redis_integration_test.go:153: entity 2400: concurrent first write: remote entity: writer fenced: entity=… owner=1000 marker=1 route=1 shared=false
--- FAIL: TestRealSharedKindFirstWritesFromTwoProcesses (0.28s)
```

修后 `it-green.log`：PASS（2.37s，-v 核对没有 SKIP）。

## 三、变异（修后代码上逐条撤一处，跑完恢复）

框架（`mutants-framework.log`，脚本按原文件恢复并断言一致）：

| 编号 | 关键行 | 结果 |
| --- | --- | --- |
| M1 | `ClaimAuthorityState` 插入 `Shared: shared` → `Shared: false` | RED 三条 `TestSharedKind*`（记录不是共享的） |
| M2 | 读回判断 `d.Owner != owner && !(shared && d.Shared)` → `d.Owner != owner` | RED `TestSharedKindFirstWritesFromTwoProcessesBothSucceed`：B `writer fenced … shared=true` |
| M3 | `acceptsClaimedLease` 去掉“或者是共享 kind 的共享记录”那一半（`lease.Shared && RemoteKindShared(kind)`） | RED 两条：B `invalid ownership claim`、B 的 `ClaimRemoteOwnership` 被 fence |
| M4 | `LeaveRemoteSharedMode` 的共享 kind 拒绝 → `false && …` | RED `TestSharedKindOwnershipOperations`：退出共享成功了 |
| M5 | 非持久存储的共享 kind 拒绝 → `false && shared` | RED `TestSharedKindOwnershipOperations`：认领成功了 |

codegen（`mutants-codegen.log`）：C1 模板去掉 `entity.DeclareRemoteSharedKind(...)`、C2 解析不读 `ownership`、C3 去掉“只能写在
remote=managed 上”的拒绝，各自 RED `TestOwnershipSharedDeclaresASharedKind`。

facebook 生成工程（`mutants-facebook.log`，scratchpad 里新工程装 team，带上 facebook）：F1 生成的 wire 去掉
`entity.DeclareRemoteSharedKind(EntityKindFacebook)`、F3 实体标记去掉 `ownership=shared` 后 `roost generate`，各自 RED
`TestFacebookIsASharedKind`；F2 读守卫 `if !found || !lease.Shared {` 撤掉 RED `TestFacebookUnopenedInboxIsEmptyWithoutNest`。
F1、F2 固化进 `codegen/internal/roost/module_team_test.go` 的 `recipeMutationMustFail`。

## 四、facebook 模块改动

- 实体 `game/entities/facebook/entity.go.tmpl`：标记加 `ownership=shared`，注释写明“第一次写它时一条插入就是共享、不能退出共享，
  Ensure 只装进内存”。
- `game/modules/facebook/facebook.go.tmpl`：
  - `Ensure` 只 `GetOrCreate` 进本进程内存、返回号；去掉 `GetRemoteOwnership` / `ClaimRemoteOwnership` / `EnterRemoteSharedMode`
    三步与 Remote 运行时依赖；删除 `ErrOpening`（不再有“别的 game 正在打开它”）。
  - 读守卫 `if !found || !lease.Shared { return nil, nil }` 保留，注释改成真实理由：没有记录时收件箱必定为空，只读一次所有权就答空、
    不经 Nest（省掉写准入里的一次元数据读与一次聚合冷加载）；记录不是共享的（共享 kind 正常不会有）经 Nest 读会被 fence，答空而不是
    错误码；记录在、还没有提交（别人第一次写到一半）冷加载答不存在，同样答空；框架不会因为读而认领。
- `facebook_test.go.tmpl`：`TestFacebookEnsureOpensSharedOnce` 与 `TestFacebookEnsureReportsAnotherProcessOpening` 换成
  `TestFacebookEnsureOnlyLoadsIntoMemory`（只装进内存、装不进照实报错）与 `TestFacebookIsASharedKind`（生成的登记声明了共享 kind）；
  `TestFacebookInvitesNeverClaimsAnUnopenedFacebook` 改名 `TestFacebookUnopenedInboxIsEmptyWithoutNest`（断言不变，名字与注释不再写
  “否则被认领”）；投递失败用例里的 `ErrOpening` 换成一般的装载失败。**场景数不变：9**（handler 4 + 模块 5），清单 `pass: 9`、
  `scripts/test-modules-generated.sh`、`module_{team,matchmaking,alliance}_test.go` 的计数都不用改。
- `modules/facebook/README.md`：实体、`Ensure`、`Invites`、谁建、不要改（`ownership=shared`）、验证范围。
- 真实依赖用例 `scripts/testdata/alliance-e2e/social_test.go.txt` 的 `TestAllianceE2ESocialTeamInvite` 末尾加一段“两个 game 同时第一次
  投给同一个人”：game 1001 的 E 建第二个队，game 1000 的 A 与 E 两条连接同时邀请从没收到过邀请的 D，断言两份都在 D 的收件箱里。
  用例名与默认集合计数不变。

## 五、真实依赖

| 命令 | 结果 |
| --- | --- |
| `source ~/.roost-it/roost-dataengine-it/env.sh`；`go test -tags integration -run '^TestRealSharedKindFirstWritesFromTwoProcesses$' ./framework/remoteentity/` | 修后 PASS（`it-green.log`）；修前认领 FAIL（`it-red-unshared-claim.log`） |
| 同 env，`ALLIANCE_E2E_ETCD_PORT=34879 ALLIANCE_E2E_PORT_BASE=36900 ALLIANCE_E2E_EXPECT=1 ALLIANCE_E2E_RUN='^TestAllianceE2ESocialTeamInvite$' scripts/test-alliance-e2e.sh`（私有 etcd，脚本自起自停） | PASS，passed=1 skipped=0，13 个进程 0 条 ERROR 日志；D 的收件箱列出两份（`e2e-green.log`） |
| 同上，facebook 模块退回基线（两步 Ensure、实体不声明共享），core 用新代码（跑完恢复，`cmp` 核对） | **也 PASS**（`e2e-oldfacebook-not-red.log`）：两条连接同时发邀请，但两边的投递各在自己的 Team 事务之后，没有撞进“查到没有记录 → 认领”的窗口。真实依赖下修前没有复现出红；确定性的红在框架层（二）与真实 Mongo / Redis 的框架用例（`it-red-unshared-claim.log`）。新加的这段在真实依赖下只证明“两份都进收件箱”，不证明修前必红 |

## 六、验证

| 命令 | 结果 |
| --- | --- |
| `GOWORK=off go vet ./framework/remoteentity/ ./framework/entity/ ./codegen/internal/entity/ ./codegen/internal/marker/ ./codegen/internal/roost/`（另 `-tags integration` vet remoteentity） | 干净 |
| `GOWORK=off go test -race -count=1 ./framework/remoteentity/ ./framework/entity/ ./framework/nest/ ./framework/dataengine/engine/ ./gameplay/movement/ ./wiring/dataengine/ ./wiring/nest/ ./wiring/remoteentity/`（remoteentity 与全部引用它的测试包） | 全 ok |
| 共享隔离环境，`go test -tags integration -run '^(TestRealSharedKindFirstWritesFromTwoProcesses\|TestRealDeleteEmptyClaimIsGuardedByTheOwnershipGeneration\|TestRealListOwnedRemoteEntitiesFiltersByOwnerAndKind\|TestRealRemoteOwnershipClaimAndTransfer\|TestRealRouteFollowsOwnershipAcrossTransferAndKill)$' ./framework/remoteentity/` | 5 PASS，没有 SKIP（`it-regression.log`） |
| `GOWORK=off go test -count=1 ./codegen/internal/entity/ ./codegen/internal/marker/` | ok |
| `GOWORK=off go test -count=1 -timeout 60m -run '^TestModuleTeamGenerated$' ./codegen/internal/roost`（facebook 9 / team 31 / rehome 62 / team+rehome 2，12 条 recipe 变异全部被抓，含新加的共享声明一条） | ok（83s，`module-team-generated.log`） |
| scratchpad 新工程 `roost project new … -template game` + `roost module add team` + `roost module verify` | facebook 9/9、team 31/31，build / vet / id check / generate --check 全 ok（`module-verify-team.txt`） |
| `GOWORK=off go test -count=1 .`（根包） | ok |

没有跑全量 `go test ./...` 与 pretag（按本批共用规则）；`TestModuleMatchmakingGenerated` / `TestModuleAllianceGenerated` 只有 facebook 计数
（9，不变）与本改动相关，没有重跑。

## 七、team 是否也声明共享（只给建议，没有改）

现状：`modules/team/files/game/controllers/team.go.tmpl` 建队是 `teamNextID`（Mongo 计数器发号）→ `teams.Create` →
`ClaimRemoteOwnership` → 若不共享再 `EnterRemoteSharedMode`，在建队请求里、事务之前做完。

- **并发首写**：Team 的号由发号器给出、只有建队的那个请求知道，不存在两个进程同时首写同一个 Team——facebook 的并发问题在 Team 上不发生。
- **两步之间崩溃**：建队请求本身失败（客户端拿不到号），留下一条没人知道号的独占空认领，不卡任何人，只占一条记录（可用删空认领清）。
- **改成声明共享的收益**：建队少一条 `_remote_entity_meta` 上的 `findAndModify`（切共享）；没有独占的中间态；声明直接说明“Team 生来共享、
  不能退回独占”。现有建队代码不用改也能工作（声明后 `ClaimRemoteOwnership` 直接返回共享记录，`!lease.Shared` 分支不再走），
  可以再简化为不显式认领、由第一笔事务认领。
- **风险 / 代价**：要重跑 `TestAllianceE2ESocialTeam` / `SocialTeamInvite` / `SocialMatchmaking` / `SocialRehome` 与 team 模块 31 个场景；
  team 的场景替身实现了 `ClaimRemoteOwnership` / `EnterRemoteSharedMode`，简化时要跟着改。没有发现依赖“Team 先独占”的代码
  （team 不调 `LeaveRemoteSharedMode`；心跳本来就拒绝挂共享实体）。
- **建议**：收益小、不修 bug，可以顺手做（B：加 `ownership=shared`，建队去掉显式认领与切共享，场景与真实依赖各跑一遍）；不做也没有缺陷（A）。
  本轮没改 team。

## 八、尚未确认

- `ownership` 这个键名与 `exclusive|shared` 取值是本轮按“与 snapshots 同为枚举”定的，维护者原话举例是 `shared=true`，尚未确认要哪种写法。
- 共享 kind 不能退出共享是按“最小语义”定的；维护者若要允许（例如运维把一个热点共享实体收回独占），去掉 `LeaveRemoteSharedMode` 里的一个判断即可，
  但退回独占后它又会在别的进程那里 fence，尚未确认要不要。
- 真实依赖下“两 game 并发首投”修前是否每次都红：两条连接同时发，是否真撞在同一窗口取决于时序（见五）；确定性的红在框架层用例。
- 声明是进程级登记：同一个 kind 在一个进程里声明、另一个进程（旧二进制）里没声明时，后者首写会插入独占记录、前者读回被 fence。
  线上未部署、同一工程同一份生成代码，按“不兼容旧进程”处理，没有加跨进程一致性检查。

## 九、要维护者定的点

1. 注解写法：A（本轮）`ownership=shared`（枚举，缺省 `exclusive`）；B `shared=true`（布尔）。改名只动 marker 词表、parse、模板与文档。
2. 共享 kind 能否退出共享：A（本轮，推荐）拒绝；B 允许（与运行期切的共享实体相同）。
3. team 是否也声明共享：A 不改（现状无缺陷）；B 声明并简化建队（见七）。

> 维护者 2026-11-18：注解保持 `ownership=shared`；共享 kind 不能退出共享（保持拒绝）；Team 也声明共享并简化建队（见 team-shared-kind-20261118）。

## 十、建议文字（协调者合入时用；本 agent 未改 CHANGELOG / KNOWN-LIMITS）

### CHANGELOG `[Unreleased]`

```
### 新增
- Remote 共享 kind：`//roost:entity … remote=managed ownership=shared` 声明这个 kind 的实体生来共享。首写认领（写准入与
  ClaimRemoteOwnership）一条插入就写成共享的所有权记录，没有“先独占认领、再切共享”的中间态；两个进程同时第一次写都成功
  （插入输了的一方读回共享记录照常准入）；认领后崩溃留下的是共享空认领，不卡别人。这类实体不能退出共享（LeaveRemoteSharedMode
  返回 ErrRemoteOwnerTransition）；非持久所有权存储（Redis marker）上认领共享 kind 返回 ErrRemoteWriteCapabilityDisabled。
  未声明的 kind 行为不变。生成的登记调 entity.DeclareRemoteSharedKind。

### 变化
- remoteentity：WriteAuthority.ClaimAuthorityState 加 shared 参数（自定义实现要跟着改）。
- facebook：Facebook 实体声明 ownership=shared；Ensure 只把它 GetOrCreate 进内存，不再认领、切共享；删除 facebook.ErrOpening。
  两个 game 同时第一次投给同一个人时两份邀请都进收件箱（修前输的一方 ErrOpening、那份不进收件箱）；第一次投的 game 认领后崩溃
  不再让别的 game 一直 ErrOpening。读守卫保留，注释改为真实理由。场景数不变（9）。

### 升级
- 所有权记录格式不变（共享 kind 只是第一代记录直接写 _owner_shared=true）。只对新生成 / 重新装 facebook 的工程生效；旧数据里
  “认领了还没切共享”的独占 Facebook 记录不会自动变成共享，线上未部署，按“升级需清空”处理。
```

### KNOWN-LIMITS

删去“共享伴生实体（facebook）的建立两步走”一条（已由共享 kind 解决）。“社交容量与归属”一条里的
“认领后没切到共享就崩时，别的 game 得到 `ErrOpening` 直到所有权按 Remote 规则释放，等多久没测”一句删去。

框架固有限制（如需登记，放 Remote 段）：

```
- 共享 kind（ownership=shared）的每次读写都走共享准入（分布式锁 + 一次 GrantWrite），没有独占实体的稳定持有快路径；不能退出共享。
  声明是进程级登记：同一 kind 在不同二进制里声明不一致时，没声明的一方首写会插入独占记录（同一工程同一份生成代码时不出现）。
```
