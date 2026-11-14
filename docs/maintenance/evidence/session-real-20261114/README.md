# B5：合作 Roguelike / 社交经营仅 WAL 的崩溃恢复

基线 `c91b6d93`（包含 P9/B9），分支 `tmp/v147-session-real`，2026-11-14，macOS arm64 / Go 1.27.3。`GOWORK=off GOFLAGS=-p=2`，私有 mirror-local offset 37500；本批只连接 Mongo 副本集。没有容量测量，也没有重置共享依赖。

两个正式脚本实际运行，自定义 module 分别为 `example.com/custom-coop-accept`、`example.com/custom-social-accept`。默认场景数保持合作 10 / 经营 9；真实档各一条测试、两个崩溃子场景，均启用 race，零 SKIP。默认 build / vet / ID 检查 / generate --check 均通过，恢复夹具也在默认档编译与 vet。完整输出：[合作](coop-generated-green.log)、[经营](social-generated-green.log)。

| 真实测试 | 仅 WAL 崩溃点 | 已确认记录 / fsync | 恢复后核对 |
| --- | --- | --- | --- |
| TestCoopRoguelikeRealCrashRecovery/partial_vote | 第一关完成、第二关只有一人投票 | 7 / 7 | 完整 DAO 与最后回执不变，半票、路线种子保留；继续三关，两人各领 10 金币 |
| TestCoopRoguelikeRealCrashRecovery/first_reward | 三关完成、第一人已领奖，第二人尚未领取 | 11 / 11 | 已领奖者仍为 10、另一人为 0；重试不重付，第二人独立领取，最终 10 / 10 |
| TestSocialSimRealCrashRecovery/growing | 主人种植、访客帮助一次，尚未成熟 | 6 / 6 | ReadyAt 不重算，离线两小时加载不自动发收益；主人 harvest 后 5 / 0，访客不能领 |
| TestSocialSimRealCrashRecovery/harvested | 收获已确认、主人得到 5 金币 | 7 / 7 | 收获数 1、Planted=false、余额 5 / 0；同序号重复不增加，新序号重复收获拒绝 |

每个窗口都另起第三个 OS 进程冷加载最终 Room 和两名 Player 的完整持久字节，再重试各人的最后命令并核对终态；合作还固定核对三关后的种子 `8129822322734983565`。回执只覆盖各人最后一条成功命令，不宣称永久幂等账本。

## 崩溃屏障

共同装配在 `scripts/testdata/genre-common/persistence/runtime_test.go.txt`，业务专属 seed / recover / verify 在各玩法 `persistence/recovery_test.go.txt`。只共享相同 Player/Coins、GameRoom/State 的 Runtime / Nest 接线与进程屏障，不复制两套引擎夹具。

子进程使用正式生成 Sender、DAO setter、strict WAL 和真实 Mongo Store；Projector 的正式 `ManualReplay` 选项仅用于 fixture，让后台投影保持关闭。所有业务调用成功返回后要求 Appended=Admitted、Syncs>0、Acknowledged=0、TerminalError 为空，并查 `GameRoom` / `Player` 两个真实业务集合均为 0。完整持久快照由 FD 3 发回父进程；父进程再次查空集合，再 SIGKILL，检查 OS 退出信号，杀后仍查空集合。新 Runtime 从原目录 WAL 重放后才冷加载比较，没有用固定 sleep 猜崩溃窗口。

正常恢复继续执行玩法并 Flush/Shutdown 后，第三进程才接管原目录。本次不修改业务状态机、持久级别或新建实体规则；仅新增验收。

## 复跑

```sh
export GOWORK=off GOFLAGS=-p=2
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/session-private ROOST_MIRROR_LOCAL_OFFSET=37500
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
ROOST_COOP_ROGUELIKE_REAL=1 ROOST_GENRE_MODULE=example.com/custom-coop-accept bash scripts/test-coop-roguelike-generated.sh
ROOST_SOCIAL_SIM_REAL=1 ROOST_GENRE_MODULE=example.com/custom-social-accept bash scripts/test-social-sim-generated.sh
bash scripts/mirror-local.sh down
```

先查端口空闲并按本机情况选 offset。`ROOST_GENRE_OUTPUT` 可保留完整生成工程与 `test.log`、`real.log`；缺省成功后删除，失败保留工程证据。测试的子进程、临时 WAL 和唯一物理库由本次测试清理；不碰其他工程。

## 反向验证

仅修改一次性生成工程的恢复调用，把父进程的原 `root` 改为 `filepath.Join(root, "lost-wal")`；子进程仍在原目录提交，Mongo 仍为 0。两玩法各两个子场景实际都在 `WAL recovery lost acknowledged...` 断言失败（[合作红](coop-lost-wal-red.log)、[经营红](social-lost-wal-red.log)）。随后还原生成文件，重新运行同一真实测试，两个子场景全部通过（[合作绿](coop-restored-green.log)、[经营绿](social-restored-green.log)）。复跑见 [本次变异命令](lost-wal-mutation.sh.txt)，需替换其中一次性生成工程与证据路径。此变异不删除真实日志、不改生产实现，第三进程与业务断言也未放宽。

## 范围与 KNOWN-LIMITS 回写文本

新真实档直接走 Sender → Nest → 生成 DAO → 文件 WAL → Mongo，**不包含 TCP/PB、真实账号登录、独立 Gate、NATS 或跨服联合链**。脚本先跑的默认 TCP Robot 用替身鉴权/提交器，是另一个验证范围；不能把两者通过合称为网络与存储联合验收。社交经营的成熟在显式 harvest 时按持久 ReadyAt 判断，没有新增自动定时器。没有 Linux、跨机、断电、回复未知、客户端引擎或容量证据。

供主线程统一回写，本文没有修改 KNOWN-LIMITS：

> **合作 Roguelike / 异步社交经营真实恢复**（2026-11-14）：已加 `ROOST_COOP_ROGUELIKE_REAL=1` / `ROOST_SOCIAL_SIM_REAL=1`；私有 Mongo、正式 Nest/生成 DAO/文件 WAL 下各两个仅 WAL 已确认、尚未投影的 SIGKILL 窗口。合作覆盖第二关半票和第一人已领奖，恢复保留路线种子/投票/回执并最终各得 10 金币；经营覆盖帮助后生长和已收获，期限不重算、主人只领一次 5 金币、访客不能领。第三进程冷加载终态并重试不变，丢 WAL 变异四个窗口均红、还原后绿。默认合作10/经营9条不变。真实档直接走 Sender，不包含 TCP/PB、真实账号、Gate、NATS 联合链；默认 TCP 仍用替身提交器。跨机/断电/回复未知/容量未验。

索引使用父线程 Verify / Tier 2 项目 `Users-whb-roost-worktrees-v147`、generation `2026-11-14T02:55:37Z`。`.go.txt` 夹具未跟踪到结构图，新文件不在旧代际；已按 coverage 对当前源码补读，Projector ManualReplay 用精确 snippet / outbound trace 验证。未把图谱无结果或 scope 无记录缺口当作完整性结论。

定向根包注册 / pretag / gofmt 守卫通过，shell 语法通过，文档锚点检查 368 文件、0 坏链。未跑整仓慢套件。私有依赖在结束后停机，生成工程与变异临时 WAL 均清理；工作树和报告保留。
