# Nest 只读请求认领没有所有权记录的 Remote 实体（2026-11-18）

任务来源：KNOWN-LIMITS“Nest 只读请求会认领没有所有权记录的 Remote 实体”，维护者 2026-11-18 定“查清并修框架”。
现象来自 facebook 模块实施（[evidence/social-facebook-entity-20261117](../social-facebook-entity-20261117/README.md) 第二节）：
B 先在 game-1001 查一次空收件箱，这次只读请求把还没建的 Facebook(B) 独占认领，之后 game-1000 想建它时得到 `ErrOpening`。
基线 main `8a413b07`。codebase-memory-mcp 本会话连不上，以下行号都按当前源码读出。

## 一、结论

- **根因**：写准入在“所有权缓存是没有记录”时**先认领、后冷加载**。实体在本进程内存与权威里都不存在时冷加载必然失败，
  认领却已经插进 `_remote_entity_meta`，不撤销；Nest 对带 Remote 目标的每个请求（读写不分）都走这条写准入。
- **性质**：缺陷（细节问题，不是前提问题）。“读也走写准入”“首写认领”都是有意的设计；但认领一个准入马上就要答
  “不存在”的实体没有任何收益，只留下一个没有时限、别的进程解不开的独占所有权。修前文档把它当作空认领的来源之一记了
  下来（`empty_claim_total{left}` 说明、`TestDeleteEmptyClaimAfterRestartConfirmsTheAggregateIsAbsent` 的注释），属于“把副作用
  记成现状”，不是有意的保证。
- **修法**：认领之前确认实体存在——内存里没有实例时先冷加载，答不出实例就不认领、原样返回（Nest 照旧答
  `entity_not_found`），并把所有权缓存置为未知。只在所有权缓存是“没有记录”时走到（冷路径），稳定持有与共享路径不变，
  新建路径（内存里已有实例）不多一次 I/O；不存在的路径少一次认领插入。没有改公开 API 与客户端答复。
- **真实依赖**：去掉 facebook 的读守卫后，`TestAllianceE2ESocialTeamInvite`（B 先在 game-1001 查空收件箱、再由 game-1000 邀请）
  修前红、修后绿；保留读守卫的原样配置修后也绿。

## 二、根因（代码行，基线 8a413b07）

1. `framework/nest/nest_dispatch.go:695-712` `prepareRemoteWriteBatch`：消息有 Remote 声明目标就调
   `PrepareRemoteWriteBatch`，不区分读写（REMOTE_ENTITY“只读请求”：没有只读声明，读排在写门后，是设计）。
2. `framework/remoteentity/batch.go:153` `beginWrite` → `confirmWriteOwnership`。
3. `framework/remoteentity/batch.go:344-345`（基线）：
   ```go
   if _, found := w.cachedOwnership(); !found {
       claimed, err := w.claimOwnership(ctx, authority)   // ClaimAuthorityState：InsertOne 第一代记录
   ```
   这里没有看实体是否存在。
4. 存在与否要到 `batch.go:168` `admitExclusive` → `loadWriteTarget` 才知道：loader 答 `ErrAuthorityEntityNotFound`，准入失败，
   Nest 答 `entity_not_found`（`nest/io_load.go` `entityStateError`）；第 3 步插入的记录留下（空认领，`noteEmptyClaimLeft` 计 left）。
5. 之后别的进程：`ClaimAuthorityState` 撞上已有记录读回 owner 不是自己 → `ErrRemoteFenced`（`mongo_authority.go:145`）；
   `EnterRemoteSharedMode` 只许 owner 切（`ownership.go:117`）。facebook 的 `ensure` 把它报成 `ErrOpening`。

## 三、修法（`framework/remoteentity/batch.go`）

`confirmWriteOwnership` 在认领之前（持久权威下）调新函数 `confirmExistsBeforeClaim`（`batch.go:388`）：

- 本进程内存里已有实例（业务在 Nest 之外 `GetOrCreate` 新建的那份）→ 直接认领，行为与修前逐字相同；
- 否则 `loadWriteTarget(ctx, false, 0)` 冷加载：失败就不认领、原样返回同一个错误（错误链与修前“认领后冷加载失败”相同），
  并 `invalidateMarker()`：下一次准入重读权威，不在旧的“没有记录”缓存上再冷加载、再认领一次被 fence；
- 冷加载读得到实例（权威里有数据却没有记录，正常运行不出现）→ 照旧认领，实例留给随后的对齐。

依据：没有所有权记录的实体在权威里不可能有提交（Remote 提交在同一个 Mongo 事务里校验所有权记录，
`authorityCommitFilter`），内存里也没有就只能答不存在。

没有选的做法：
- “只读请求不认领”：准入时不知道 handler 读不读写（没有只读声明，加声明要同时改 codegen / Nest / Remote 三层，文档已否决）；
  而且读一个**存在**的、没有记录的实体（内存里新建的）认领是现有语义，不必改。
- “以共享模式认领”：共享是运行期所有权状态，没有 kind 级声明，准入不知道该实体要不要共享（见七、A）。
- “认领后失败再撤销”：多一次比较删除，结果未知时照样留下；维护者 2026-11-04 已否决同类做法。

热路径：只在所有权缓存是“没有记录”时多一个判断；稳定独占持有（`heldExclusively`）、共享路径、已有记录的对齐路径都不经过。

## 四、先红（修前代码）

用例（框架层，进程替身：两个 Manager 共用 Mongo 权威替身与 Redis 版本锁替身，正式 `ManagerAccess` loader + 真 Nest 引擎）：
`framework/remoteentity/write_admission_test.go` 的 `TestRequestOnAnEntityThatDoesNotExistClaimsNothing`：
A（sid 1000）对一个不存在、无记录的实体发只读请求与写请求，都应答 `entity_not_found`、不留所有权记录、不计 left；
之后 B（sid 1001）新建、认领、切共享并写；A、B 都能写，读到对方的修改。

`red-framework.log`（修前，断言落在“不认领”本身）：

```
write_admission_test.go:1494: A's read-only request on an entity that does not exist claimed it: ownership record {ID:4611686018429775349 Version:0 Enabled:true Owner:1000 Shared:false Marker:1 Route:1 Fence:0 Token:}
--- FAIL: TestRequestOnAnEntityThatDoesNotExistClaimsNothing (0.01s)
```

`red-framework-consequence.log`（修前，临时把前两条断言改成 Logf 看后果，已改回）：B 认领被拒，即 facebook 的 `ErrOpening`。

```
write_admission_test.go:1498: empty_claim_total{left} = 1; want 0 (nothing was claimed)
write_admission_test.go:1509: B claim after A's requests: remote entity: writer fenced: entity=4611686018429775349 owner=1000 marker=1 route=1 shared=false
```

真实依赖（去掉 facebook 读守卫 `if !found || !lease.Shared` → `if false && (...)`，修前 core）`e2e-red-noguard.log`：

```
social_test.go:188: TEAM_INVITE_INBOX: player 100002 on sid 1001 listed &{Code:0 Reason: Invites:[]}; want exactly team 4611686018427388957 from 100001
--- FAIL: TestAllianceE2ESocialTeamInvite (5.51s)
```

与 facebook 实施时的中间态（`e2e-mutant-read-claims.log`）同一失败。推送照样到了（告知不看记录成没成），收件箱没记上。

## 五、变异（修后代码上逐条撤一处，跑完已恢复，`cmp` 核对与修后文件一致）

| 编号 | 关键行 | 结果 |
| --- | --- | --- |
| M1 | `confirmWriteOwnership` 里 `if authority != nil {` → `if false && ...`（不做存在确认） | RED `TestRequestOnAnEntityThatDoesNotExistClaimsNothing`：只读请求认领了它（`mutant-M1.log`） |
| M2 | `confirmExistsBeforeClaim` 去掉 `w.invalidateMarker()` | RED 同一用例：A 信任一小时缓存，B 建好并切共享后 A 的写在旧“没有记录”上冷加载、再认领，`writer fenced ... owner=1001 shared=true`（`mutant-M2.log`） |

## 六、验证

| 命令 | 结果 |
| --- | --- |
| `GOWORK=off go test -race -count=1 ./framework/remoteentity/` | ok（19.8s） |
| `GOWORK=off go test -race -count=1 ./framework/nest/ ./framework/dataengine/engine/ ./gameplay/movement/ ./wiring/dataengine/ ./wiring/nest/ ./wiring/remoteentity/`（全部引用 remoteentity 的测试包） | 全 ok |
| `GOWORK=off go vet ./framework/remoteentity/` | 干净 |
| `GOWORK=off go test -count=1 .`（根包） | ok |
| 共享隔离环境：`go test -tags integration -run 'TestRealDeleteEmptyClaimIsGuardedByTheOwnershipGeneration\|TestRealListOwnedRemoteEntitiesFiltersByOwnerAndKind' ./framework/remoteentity/`（真实 Mongo） | 2 PASS（-v 核对没有 SKIP） |
| `scripts/test-alliance-e2e.sh`，`ALLIANCE_E2E_RUN='^TestAllianceE2ESocialTeamInvite$'`，私有 etcd 34779、端口基 36700，去掉读守卫 + 修前 core | FAIL（上面的红） |
| 同上，去掉读守卫 + 修后 core | PASS，passed=1 skipped=0（`e2e-green-noguard.log`） |
| 同上，保留读守卫（原样）+ 修后 core | PASS，passed=1 skipped=0（`e2e-green-guard.log`） |

改了一个已有用例的前置：`empty_claim_test.go` 的 `TestDeleteEmptyClaimAfterRestartConfirmsTheAggregateIsAbsent` 原来用“在不存在的
实体上准入失败”制造空认领（断言修前行为），现在改用 `rejectFirstWrite`（内存里新建的空实例被认领、事务被拒）制造同样的空认领，
删空认领那一半的断言不变。没有放宽断言、删除或跳过用例。没有跑全量 `go test ./...` 与 pretag（按本批共用规则）。

## 七、Remote 多写语义核对（维护者 2026-11-18 追加）

### 1. Remote 实体“先读后还能再写吗”

**(a) 实体已是共享模式，任一 game 先读，之后本进程与别的 game 都能写。** 读与写走同一个共享准入（`batch.go` `lockSharedWrite`，
`:411`）：取 Redis 分布式锁 + `GrantWrite`（`_remote_entity_meta` 一次 `findAndModify`，`_grant_fence` 加一）、按许可里的权威版本
对齐本地实例，读完 Close 即放锁。读消耗一个 fence 号，但 fence 单调递增、下一个写者拿到更新的 fence，读不留下任何占用，
不影响别人的写许可与校验。代价：共享实体上每次读也是一次 Redis 锁 + 一次 Mongo 写命令。
用例：`TestReadsBeforeWritesOnSharedAndExclusiveEntities/shared`（A 读三次 → A 写 → B 写 → A 读到 `a=A1;b=B2@3`，所有权仍共享），修前修后都绿。

**(b) 实体没有所有权记录时先读。**
- 修前：读的进程把它独占认领（空认领）。读的进程自己之后能写（所有权是本 sid；业务先 `GetOrCreate` 进内存，写准入走对齐
  路径）。别的 game 不能写也不能建：认领被 fence（`mongo_authority.go:145`），切共享只许 owner（`ownership.go:117`），facebook 报
  `ErrOpening`。**卡多久：没有时限**。所有权记录没有 TTL，进程重启同 sid 仍是 owner；只有三种方式放开——读的那个进程自己
  `EnterRemoteSharedMode`（facebook：那个 game 上有人邀请同一个玩家，走到它的 `Ensure`）、`TransferRemoteOwnership`、或在那个
  进程上运维 `remote_entity.empty_claims delete=true`（记录 `_ver` 为 0，可删）。用例：`red-framework-consequence.log`。
- 修后：读答 `entity_not_found`，不认领、不留记录；之后谁先建谁认领（facebook 的 `Ensure` 认领后立即切共享），建好后两边都能读写。
  用例：`TestRequestOnAnEntityThatDoesNotExistClaimsNothing`。
- 修后仍会认领的情形：没有记录、但**本进程内存里已有实例**（业务 `GetOrCreate` 了还没认领）时，读也会首写认领成本进程独占。
  facebook 的 `Ensure` 是 `GetOrCreate` → 认领 → 切共享连着做，内存里的实例紧接着就被本进程认领并切共享，这个情形只剩
  “`Ensure` 在 `GetOrCreate` 之后、认领之前失败或崩溃”的窗口，而且被认领的是本进程，之后本进程再 `Ensure` 会补切共享。

**(c) 实体由某进程独占时别的进程读**：`confirmWriteOwnership` 发现 owner 是别人（`batch.go:370`）→ `ErrRemoteFenced`（带 owner），
不认领、不改所有权，owner 照常读写。读要经路由 RPC 改投 owner（`RPCRouteCheck` / `Redirect`）或读快照 / Mirror。
用例：`TestReadsBeforeWritesOnSharedAndExclusiveEntities/exclusive`，修前修后都绿。

### 2. facebook 是否符合“多写 + 写后通知 + 上线取最新”

- **多写**：建好之后是共享模式，任何 game 都能经 Nest strict 写收件箱——符合。
- **`Ensure` 在多 game 并发首写时不完全可靠（尚未确认真实发生率，按代码推断，未用真实依赖复现）**：两个 game 同时第一次投给 B，
  都看到“没有记录”，都 `GetOrCreate`，只有一个认领成功；另一个得到 `ErrOpening`，`Deliver` 不重试、只记 Debug，这份邀请的
  **收件箱记录丢了**（告知照样广播，B 在线时仍收到推送；B 离线则上线查不到它，只能重发邀请）。窗口是赢的一方“认领 → 切共享”
  两条 Mongo 命令之间，很短。赢的一方在两者之间崩溃：所有别的 game 一直 `ErrOpening`，直到那个进程再确保同一个玩家、或运维删空认领
  （同 1(b) 修前的“没有时限”）。这一点与本次框架修复无关，修后依旧。
- **上线取最新**：`TeamInvites` → `facebook.Invites` → Nest 读共享实体，走 1(a) 的共享准入：取锁后按许可里的权威版本对齐、本地
  落后就卸载重载，读到取锁时已提交的最新收件箱（替身用例 shared 子用例、真实 e2e 的离线 C 重登后查到）。注意“上线取一次”要客户端
  登录后调 `TeamInvites`，team / facebook 不在进入游戏时主动推收件箱。
- **通知丢了能否兜住**：`Deliver` 先记录、后告知；记录成功时告知丢了靠 `TeamInvites` 补查兜住。记录失败（上一条的 `ErrOpening`、
  写失败）时兜不住——推送可能到了，收件箱没有。
- **框架修好后读守卫还要不要**：正确性上不再需要（去掉守卫 e2e 已绿）。保留它仍有两个作用：还没打开的收件箱只读一次所有权就答空
  （不经 Nest：省掉写准入里的一次元数据读 + 一次聚合读）；别的 game 确保到一半（独占、未切共享）时答空，而不经 Nest 得到
  `ErrRemoteFenced`、`TeamInvites` 答错误码。代价：已打开（共享）的收件箱每次查询多一次 `_remote_entity_meta` 点读
  （`GetRemoteOwnership` 对共享实体读权威）。**建议保留**，只把它的注释从“不这样会被认领”改成上面两条理由（本轮没改模块，
  见“要维护者定的点”）。
- **`Ensure` 的“确保共享”能否简化**：现有框架下不能省（共享只能由 owner 在认领之后切）。要省掉需要框架提供“共享 kind 首写直接以
  共享认领”，属于公开语义变化，列为选项 A。

### 3. 真实依赖

见四、六：去掉读守卫后 `TestAllianceE2ESocialTeamInvite`（用例第 173-176 行就是“B 先在 game-1001 查空收件箱、再由 game-1000 邀请”）
修前红、修后绿。

## 八、限制与遗留

框架固有（可留 KNOWN-LIMITS）：
- 共享 Remote 实体上的读与写同价：每次都取分布式锁 + 一次 `GrantWrite`（`findAndModify`）。
- 没有所有权记录、但本进程内存里有实例的实体，读也会首写认领成本进程独占（读写同一准入，没有只读声明）。

遗留（尚未确认）：
- facebook `Ensure` 的并发首投丢收件箱记录、确保到一半崩溃后别的 game 无限期 `ErrOpening`：按代码推断，没有真实依赖复现，
  也不在本轮修复范围。
- facebook 模块里读守卫的注释（`facebook.go.tmpl` `invites` 与 `facebook_test.go.tmpl` 第 307 行附近）仍写着“Remote 写准入会把没有
  记录的实体认领成本进程独占”，框架修后已不成立；本轮没改模块文件。

## 九、要维护者定的点

1. **共享伴生实体的建立方式**（影响 facebook `Ensure` 与并发首投）：
   - **A（推荐）**：框架加 kind 级“共享”声明（实体注解，例如 `remote=managed shared`），首写认领直接插入共享记录（一条 InsertOne，
     原子），写准入直接走共享路径；facebook 的 `Ensure` 简化为 `GetOrCreate`（或去掉），`ErrOpening` 与“确保到一半”的中间态消失。
     代价：新增 kind 声明与 codegen 注解、`ClaimAuthorityState` 加共享参数、REMOTE_ENTITY 契约与真实依赖验证；公开语义变化。
   - B：框架不变，facebook `Deliver` 对 `ErrOpening` 有界重试（如 3 次、50ms 退避），覆盖并发首投；崩溃在中间的仍要那个进程再确保
     或运维删空认领。代价小，不消除崩溃窗口。
   - C：保持现状。
2. **读守卫**：A 保留守卫、改注释（推荐，见七.2）；B 去掉守卫，`invites` 把 `ErrRemoteFenced` 当空。

## 十、建议文字（协调者合入时用；本 agent 未改 CHANGELOG / KNOWN-LIMITS）

### CHANGELOG `[Unreleased]`

```
### 修复
- remoteentity：Nest 请求（含只读请求）声明一个没有所有权记录、在本进程内存与权威里都不存在的 Remote 实体时，写准入不再先认领它；
  请求照旧答 entity_not_found，不留空认领。修前一次只读请求就会把它独占认领给本进程，别的进程之后认领不了、切不了共享且没有时限
  （facebook 收件箱在真实依赖下踩到）。remote_entity.empty_claim_total{left} 不再计这种情形。
```

### KNOWN-LIMITS

删去“Nest 只读请求会认领没有所有权记录的 Remote 实体（是否改框架尚未确认）”一条，改为：

```
- 共享伴生实体（facebook）的建立：Ensure 是“认领 → 切共享”两步，两个 game 并发首投时输的一方得到 ErrOpening、这份邀请不进收件箱
  （告知照样广播）；确保到一半崩溃时别的 game 一直 ErrOpening，直到那个进程再确保或运维删空认领。按代码推断，尚未用真实依赖复现；
  框架加“共享 kind 首写直接共享认领”或 facebook 重试待维护者定（evidence/nest-readonly-claim-20261118 九.1）。
```

框架固有限制（如需登记）：

```
- 没有所有权记录、但本进程内存里有实例的 Remote 实体，只读请求也会首写认领成本进程独占（读写同一准入，没有只读声明）；
  内存与权威里都不存在时不认领（2026-11-18）。
```
