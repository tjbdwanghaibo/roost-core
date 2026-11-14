# B-7 社交模块真实验收（2026-11-14）

基线 `c91b6d93`，工作分支 `tmp/v147-social`。这是一轮本机正确性验收，不是吞吐、延迟容量或跨机长稳测量。私有环境 `/private/tmp/v147-social-deps-34000`，mirror offset 34000；服务端口 36100..36400，etcd 39579/39580。只管理本环境和用例创建的进程、资源与 toxiproxy 代理。

## 真实缺陷与修复

首轮在正式 TCP 上先得到 `MatchEnqueue{Sequence:1}` 的 `queued`，随后 `TeamCreate` 仍返回成功，game 日志依次记录 queued 与 team create。原始失败摘录见 [queue-team-red.log](queue-team-red.log)，完整现场在本工作树 `artifacts/social-dev1/`。

`25d24cb9` 的组件拆分删去了原 Player 初始化中的 `OnTeamJoining("matchmaking")`，新 `game/components/matchmaking` 没搬入这项登记。文档仍承诺“匹配状态未清理不能建队 / 入队”；原模块测试只检查“有队伍不能单排”，漏掉反向顺序。图谱只能定位模板 Module，`OnTeamJoining` 无函数记录；对模板范围 coverage 的部分解析缺口补读源码，并经 git 删除差异确认根因。

修复仅在 matchmaking 组件初始化时向 team 登记检查：`Phase != ""` 返回 `matchmaking_busy`。`Team.Create` / `Team.Accept` 都经过共同 `checkJoin`，检查在 Team / Player 同一笔事务中执行。取消完成但未 `MatchClear`、成局但未清理、历史消失得到 unknown 都继续拒绝；合法 Clear 后可接受邀请。已有两个模块场景补上这些断言，顶层场景数保持 17。未改变 Remote 共享 Team 归 game、跨 sid 分池、Room 归属或任何 Q4 策略。

## 三个真实场景

| 场景 | 实际断言 | 明确边界 |
| --- | --- | --- |
| `TestAllianceE2ESocialTeam` | sid 1000 建队、sid 1001 接受；toxiproxy 下行 timeout=0 扣住接受答复，另一进程查到已提交后断开；80±20ms 下行延迟下重登重试，双方仍两成员 / revision=2；队长退出后新队长 / revision=3；有队伍拒绝换服；跨 sid 整队排队拒绝且双方无残留意图 | 跨 game 邀请推送不保证，接受使用已知 TeamID；未验证独立 Gate、容量、长稳；#154 归属决定保留 |
| `TestAllianceE2ESocialMatchmaking` | 单排 queued 拒绝换服和建队；`SIGKILL` game 后同 WAL 重启，同 Key / Sequence 恢复；同序号重试再加一个同 sid 玩家，只得到同一 RoomID，反复查询不变 | 与 [P9 Room / Turn 纵切](../turn-real-20261114/README.md)互补；不验证跨 sid 成局，也没有改变本服匹配契约 |
| `TestAllianceE2ESocialRehome` | 联盟成员 sid 1001→1000，分别在 handed-off、committed 文件栅栏核对 PID / Player / TransferID 后 `SIGKILL`；同 WAL 重启；account 目标归属、源入口明确拒绝、目标成员展示、捐献资金 +25 且道具扣一次；重登查询原 Saga 回执不再加钱，联盟人数不重复 | handed-off 由一次源登录（明确 moving）触发恢复；committed 后源入口拒绝不代表旧档已删除，也不代表旧驻留定时器和所有权威写已隔离 |

文件栅栏仅注入临时生成工程，沿用 `demo-drill/rehome-instrument.py`；本入口不使用该工具输出的 core overlay，不暂停或替换真实 projector / WAL / account。普通其他联盟场景没有 arm 文件，故障点不生效。

## 命令与口径

```bash
export GOWORK=off GOFLAGS=-p=2
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/v147-social-deps-34000
export ROOST_MIRROR_LOCAL_OFFSET=34000
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
ALLIANCE_E2E_ETCD_PORT=39579 ALLIANCE_E2E_PORT_BASE=36100 \
  ALLIANCE_E2E_LOGS="$PWD/artifacts/social-run" bash scripts/test-social-real.sh
# 可加 SOCIAL_REAL_MODULE_MATRIX=1，在故障注入前跑原始生成工程的全部模块 verify。
# 缺省联盟全集含上述三个场景：bash scripts/test-alliance-e2e.sh
# 关键变异：SOCIAL_REAL_MUTATION=drop-team-reference（只改临时生成工程）。
bash scripts/mirror-local.sh down
```

同一 shell 内完成 up / 脚本 / down，避免启动工具生命周期结束提前回收依赖后台进程。模块矩阵在注入前执行，包含场景数、零意外 SKIP、build / vet / ID 与生成漂移检查；真实结果按实际 PASS / FAIL 逐条记录，不能只引用脚本退出码。

## 已执行结果

- [完整模块矩阵](module-matrix.log)：46 个模块 / 联动条目，594 / 594 场景，0 SKIP；包括 alliance 55 + enter 2 + rehome 2、team 35 + rehome 2、matchmaking 17 + rehome 2。全部模块使用 `-race`，build / vet / id check / generate --check 通过。
- [修后定向真实场景](social-green.log)：Team 5.71s、Matchmaking 18.88s、Rehome 36.37s，3 / 3；两个 rehome 栅栏实际命中、PID 核对后均被 SIGKILL，所有用例退出时记录 0 ERROR 进程日志。耗时是本轮观察值，不能当性能指标。
- [关键变异](team-reference-mutant-red.log)：临时删除 `Team.Accept` 的 Player `SetTeamID`；Team 权威已是两名成员 / revision=2，而该玩家查询回 `TeamID=0`，以 `TEAM_AUTHORITY` 失败退出 1。证明断言同时核对权威与 Player 引用，不只看接受响应码。
- [恢复后联盟缺省全集](alliance-full-green.log)：11 / 11、0 SKIP，测试阶段 301.199s；新增三场景再次通过（Team 5.17s、Matchmaking 18.88s、Rehome 36.94s）。既有 SIGSTOP / SIGKILL / Redis outage 的错误日志属于其故障现场，不宣称整个全集零 ERROR；新增 social 用例收尾均记录 0 ERROR。实际脚本有非空集合 / SKIP / 缺省计数断言，输入散列见 [input-sha256.txt](input-sha256.txt)。
- 根包准确入口登记测试 `TestGeneratedProjectRealDependencyTestsHaveAnEntry`、`TestRealProcessDependencyEntriesRemainReachable` 通过；shell 语法、gofmt、`git diff --check` 通过。文档检查 367 个文件、0 broken。

真实首轮的另一项失败是测试把恢复触发登录误当成普通成功登录：handed-off 重启后的源 `EnterGame` 明确返回 `rehome_moving`。夹具现在要求这个确定拒绝码，再等 account 提交；不会吞任意登录错误。进入目标服前又要求源入口返回 `player_elsewhere`。这项调整没有修改正式换服流程。

完整原始日志保留在工作树 `artifacts/social-dev1`、`social-dev2`、`social-mutant`、`social-full`。结束后已停止并清理本私有依赖环境，业务 / etcd / 依赖端口及所属进程复核无残留；工作树保留供集成。未操作共享依赖，也未推送或发版。

## 保留限制

Q4 未关闭。committed→删源档窗口仍可能保留旧源文档；旧内存 / 定时器权威写的全路径隔离没有在本轮验证，也不在本次修复里改变。account 和玩家入口唯一归属的证据不能替代这项决定。来源与更完整的六点演练见 [P1 换服报告](../rehome-20261114/README.md)。

这轮没有独立 Gate 下的跨 game Team 邀请分发验收、弱网长稳、跨机压测或高并发承诺。匹配历史过期后的 unknown 仍要求业务核对；没有自动解开屏障。

## 供 KNOWN-LIMITS 汇总的精确文本

- **Team**：2026-11-14 `scripts/test-social-real.sh` 在私有 Mongo / Redis / NATS 上验收两个 game sid 共享 Team，经玩家 TCP 建队 / 邀请 / 接受；toxiproxy 丢接受回包、80±20ms 下行延迟下重登重试，双方成员表与 revision 一致，重复接受不重复入队，队长退出正确转移；跨 sid 整队匹配拒绝且无残留意图。未验独立 Gate / 跨 game 邀请推送、跨机与长稳容量；Team 仍为 game 服务的 Remote 共享实体，#154 的维护者归属决定保留。
- **Matchmaking**：2026-11-14 真实 Redis / Mongo / NATS 联合场景通过：queued 拒绝换服 / 建队，同 WAL 重启 game 后按原 Key / Sequence 继续，重试和反复查询保持同一 RoomID；P9 另覆盖同 sid Room / Turn 与独立 Gate 流程。修复组件拆分漏搬共同入队检查，queued / cancelled 未 Clear / matched 未清理 / unknown 都保留 TeamCreate / TeamAccept 屏障，合法 Clear 后解除。仍按 sid 与人数分池、不跨服；历史丢失的 unknown 需业务核对，不自动解锁，容量未测。
- **Alliance**：2026-11-14 新增联盟成员 sid 1001→1000 的 handed-off / committed 两个源 game SIGKILL 场景；同 WAL 重启，前者经一次源登录触发恢复，核对 account 目标归属、源入口明确拒绝、目标服成员展示 / 捐献回执与联盟资金只增加一次。完整模块矩阵 46 个模块 / 联动项 594 / 594、零 SKIP（alliance 55 + enter 2 + rehome 2）。Q4 未关闭：account 已提交到删源档之间仍可能留下旧源文档；证据只覆盖 account / 玩家入口唯一归属，没有证明旧内存 / 定时器和所有权威写都被隔离，不承诺源登录自动删除已提交的旧档。
