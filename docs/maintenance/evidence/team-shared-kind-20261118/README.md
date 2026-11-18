# Team 声明为 Remote 共享 kind，建队去掉“切共享”（2026-11-18）

任务来源：维护者 2026-11-18 定，[remote-shared-kind-20261118](../remote-shared-kind-20261118/README.md) 第七节的 B：Team 实体也声明
`//roost:entity … remote=managed ownership=shared`，并简化建队。基线 main `835dcb7b`。codebase-memory-mcp 本会话连不上，以下按当前源码读出。

## 一、改了什么

- 实体 `modules/team/files/game/entities/team/entity.go.tmpl`：标记加 `ownership=shared`，注释改为“建队认领的那一条插入就是共享记录、
  不能退出共享”。生成的 `registerTeamEntity` 随之调 `entity.DeclareRemoteSharedKind(EntityKindTeam)`（生成器不改）。
- 建队 `modules/team/files/game/controllers/team.go.tmpl`：认领抽成 `teamClaim(ctx, remote, id)`；去掉 `if !lease.Shared { EnterRemoteSharedMode }`
  分支（声明后认领直接答共享记录，这一步与它的一次 `_remote_entity_meta` `findAndModify` 不再需要）。认领答出**非共享**记录时建队报错
  （`team: claim of <id> is exclusive to sid <sid>: the Team entity must be declared ownership=shared`）：只在本进程的登记没有声明共享时出现
  （标记被删、旧的生成代码），这样的队伍别的 game 写它会被 fence，报错而不是把它建出来。显式认领保留（见“没有做的”）。
  `teamDeps`、`HandleTeamCreate` 的注释同步。
- 场景 `modules/team/files/game/controllers/team_test.go.tmpl`：新增 `TestTeamCreateClaimsSharedInOneStep`（见二）。team 场景 **31 → 32**
  （`./game/controllers` 7 → 8），同步：`modules/team/module.yaml` `pass: 32`、`scripts/test-modules-generated.sh` `team=32`、
  `codegen/internal/roost/module_{team,matchmaking,alliance}_test.go` 的 `"team": 32`、`modules/team/README.md` 验证范围。
- `codegen/internal/roost/module_team_test.go`：两条变异固化进 `recipeMutationMustFail`（见三）。
- 文档：`modules/team/README.md`（开头、Controller、不要改、一致性、验证范围）；`docs/framework/REMOTE_ENTITY.md`“共享 kind”节
  删去“Team 目前就是这样建的”，加“使用者”一条（Facebook、Team）。

冲突检查：team 与依赖 team 的 matchmaking / alliance 模块、game-demo 都不调 `LeaveRemoteSharedMode`（`grep` 全仓：只在 framework 与文档里），
没有“退出共享 / 转独占”的用法，与“共享 kind 不能退出共享”不冲突。alliance 的 `TransferRemoteOwnership` 只转联盟实体，不涉及 Team。

## 二、先红后绿

用例 `TestTeamCreateClaimsSharedInOneStep`（`game/controllers`）：Remote 运行时替身按框架的认领规则作答——本进程声明了共享 kind，
`ClaimRemoteOwnership` 答共享记录，否则答建队进程独占的记录（与 `ClaimAuthorityState(shared)` 的行为一致，框架这一层的真实保证由
`framework/remoteentity` 的 `TestSharedKindOwnershipOperations` 与真实 Mongo 的 `TestRealSharedKindFirstWritesFromTwoProcesses` 证明）。断言：
Team kind 声明了共享、建队认领恰好一次、**没有调切共享**、无错；另一半：认领总答独占记录时建队报错且不调切共享。

红（先做不改行为的抽取 `teamClaim`，原样保留认领 → 切共享两步与不带 `ownership=shared` 的标记；`red.log`）：

```
--- FAIL: TestTeamCreateClaimsSharedInOneStep (0.00s)
    team_test.go:114: team creation went through an exclusive claim: kind declared shared=false claims=1 enter-shared calls=1 err=<nil>
```

绿（加标记、去掉切共享、加非共享报错；`green.log`）：`--- PASS: TestTeamCreateClaimsSharedInOneStep`。

## 三、变异（scratchpad 新工程 `roost project new … -template game` + `roost module add team`，逐条撤一处，跑完恢复并 `cmp` 核对；`mutants-team.log`）

| 编号 | 关键行 | 结果 |
| --- | --- | --- |
| T1 | 实体标记去掉 `ownership=shared` 后 `roost generate` | RED：`kind declared shared=false … err=team: claim of … is exclusive to sid 1000` |
| T2 | 生成的 `team_gen_wire.go` 里 `entity.DeclareRemoteSharedKind(EntityKindTeam)` → `_ = EntityKindTeam` | RED（同上） |
| T3 | `teamClaim` 的 `if !lease.Shared {` → `if false && !lease.Shared {` | RED：`an exclusive claim was accepted for a team` |

T2、T3 固化进 `codegen/internal/roost/module_team_test.go` 的 `recipeMutationMustFail`（T1 要重新 generate，`recipeMutationMustFail` 只换文本后跑测试，
T2 是它在生成物上的等价位置）。

## 四、验证

| 命令 | 结果 |
| --- | --- |
| scratchpad 新工程 + `roost module add team` + `roost module verify` | facebook 9/9、team 32/32，build / vet / id check / generate --check 全 ok（`module-verify-team.txt`） |
| `GOWORK=off go vet ./codegen/internal/roost/` | 干净 |
| `GOWORK=off go test -count=1 -timeout 60m -v -run '^(TestModuleTeamGenerated\|TestModuleAllianceGenerated\|TestModuleMatchmakingGenerated\|TestDemoTeamModuleIntegration)$' ./codegen/internal/roost/` | 4 PASS，`ok … 313s`；team 32、14 条 recipe 变异全部被抓（含新加的 T2、T3），demo 的 `^TestTeam` 含新用例（`module-generated.log`） |
| 共享隔离环境 env + 私有 etcd 35379、端口基 38300（开跑前脚本 lsof 检查），`ALLIANCE_E2E_EXPECT=4 ALLIANCE_E2E_RUN='^TestAllianceE2ESocial(Team\|TeamInvite\|Matchmaking\|Rehome)$' scripts/test-alliance-e2e.sh` | 4 PASS，passed=4 skipped=0，每条 0 条 ERROR 日志（`e2e-social4.log`）；进程日志里 `team: create completed` 照常出现——建队经过 `teamClaim` 的非共享报错判断，说明真实 Mongo 上 `ClaimRemoteOwnership` 对 Team 答的就是共享记录（没有切共享这一步） |
| `GOWORK=off go test -count=1 .`（根包） | ok（提交证据后跑；提交前因本证据 README 未被跟踪，`TestTrackedMarkdownRelativeLinksResolve` 报 REMOTE_ENTITY.md 的新链接，属预期） |

没有跑全量 `go test ./...` 与 pretag（按本批共用规则）。真实依赖的 4 条是回归：基线上它们同样通过（Team 的号只有建队请求知道，不存在并发首写；
“认领 → 切共享”两步在基线上也能建成共享队伍），不证明修前必红；“生来共享”的确定性证明在框架层用例与本模块的场景（二）。

## 五、没有做的

- **建队不显式认领、由第一笔事务认领**（上一份证据第七节提到的进一步简化）：没做。显式认领让所有权记录在 `[Team, Player]` 事务之前就在、
  共享，事务的准入走稳定的共享路径；改由首写认领要连带核对“内存里有实例时首写认领”与建队事务失败时留下的状态，改动面超出“按新语义最小改动”。
  若要做，单独一轮。

## 六、尚未确认

- 认领答出非共享记录时建队报错，是本轮按“声明缺失时 fail-fast，不建出别的 game 写不了的队伍”加的一个判断（替代原来的切共享分支）；
  维护者若倾向“完全不检查、信任声明”，删掉这一个 `if` 与用例的后半段即可。
- 真实依赖下没有直接读 `_remote_entity_meta` 核对 Team 记录 `_owner_shared=true` 与 `findAndModify` 次数；间接证据是建队成功
  （认领答非共享会报错），直接保证在框架层用例。

## 七、建议文字（协调者合入时用；本 agent 未改 CHANGELOG / KNOWN-LIMITS）

### CHANGELOG `[Unreleased]`

```
### 变化
- team：Team 实体声明 ownership=shared（Remote 共享 kind）。建队认领的那一条插入就是共享的所有权记录，不再 EnterRemoteSharedMode
  （少一次所有权记录的 findAndModify，没有建队进程独占的中间态，建队中途崩溃留下的是不卡人的共享空认领）；认领答出非共享记录时
  建队报错（本进程登记没有声明共享）。Team 不能退出共享。team 场景 31 → 32（新增 TestTeamCreateClaimsSharedInOneStep）。

### 升级
- 只对新生成 / 重新装 team 的工程生效；旧数据里已存在的 Team 所有权记录本来就是共享的（原来建队即切共享），无需迁移。
  线上未部署，按“不兼容旧进程”处理：新旧二进制混跑时旧进程没声明 Team 为共享 kind，不要混跑。
```

### KNOWN-LIMITS

无新增。若 KNOWN-LIMITS 的 Remote 段已按上一份证据登记“共享 kind 的每次读写都走共享准入、不能退出共享”，把 Team 加进使用者即可：

```
- 共享 kind（ownership=shared：facebook 的 Facebook、team 的 Team）的每次读写都走共享准入（分布式锁 + 一次 GrantWrite），没有独占实体的
  稳定持有快路径；不能退出共享。
```
