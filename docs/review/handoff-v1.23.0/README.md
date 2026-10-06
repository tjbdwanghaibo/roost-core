# v1.23.0 发版前暂停交接（2026-10-06）

维护者：“已经到了限制token了，以上未完成工作存档，一会再做”。本文是恢复入口：先读本文，再读同目录的三份规格。

- [release-dual-docs-spec.md](release-dual-docs-spec.md)：v1.23.0 发版双文档的规格，末尾“汇总者待办”是全部未合入的文档修正与归属。
- [framework-docs-spec.md](framework-docs-spec.md)：框架整体双文档规格（13 个分区，发版后以 v1.23.0 tag 源码为准写）。
- [agent-impl-common.md](agent-impl-common.md)：派给实施 agent 的共同要求（独立 worktree、先红后绿、显式 git add、push 前 rebase、推 main 被拦就推同名分支等）。

## 1. main 当前状态（`41bdb9e5` 之后的本提交）

- 最近一次发版是 v1.22.0；之后全部改动都在 main，**未发版**。
- 代码“冻结点”原为 `e6828e4f`；之后又合入了以下代码改动，发版双文档需要再按新的冻结提交核对一次行号：
  - `055a15d6`：RR-20261006-17（OpenActivity 按组核对 expected）、-18（派发器销毁时删除序列）、-19（bus method 标签上界 257）。
  - `41bdb9e5`：RR-20261006-20（TaskPool 统计一致快照，NONCORE-46 维护者选 A）。此提交已含两份 RR 记录、两个 README、CHANGELOG、交接 §7；**DECISIONS-PENDING 第十三轮 NONCORE-46 行尚写“实施中”，需改为已实施（`41bdb9e5`）**。
- 维护者第十三轮决定都在 `docs/review/DECISIONS-PENDING-2026-10-05.md` 末尾，包括：
  - 交给 review 前不留 WANTED；
  - Windows 不保证正确；
  - A1 字段写检查用 `//roost:cache`；
  - saga 收件箱改为单状态文档；
  - 不考虑旧进程（不做兼容）；
  - NONCORE-46 选 A；
  - B10 两条管线选 A。

## 2. 暂停时未完成的工作（WIP 分支，均未验证，不得直接合入）

| WIP 分支 | 原任务 | 已做到哪 | 恢复时要做 |
| --- | --- | --- | --- |
| `wip/rv2`（`14a011d8`） | 发版分册 2（SAGA / DRV / DAO / REM）按冻结提交重核：SAGA 条目按单状态文档重写，补 RR-10/11、L1 写入守卫、interest Ack、迁槽 / NATS 实测、A1 `//roost:cache`、O-M6-5、durable 名；修待办里分册 2 的源文档不一致 | 已改 release 分册三文件和多份源文档（CHANGELOG、TROUBLESHOOTING、USER_GUIDE、NC-250、U-0280、B1、B2、SAGA-DIRECTION、DECISIONS），停在写 guide 的 REM-14 条目 | **已完成**（分支 `rv2b`，rebase 到 `37338490` 后合入 main，`wip/rv2` 已删）：39 条（新增 SAGA-14 / SAGA-15 / DAO-4 / REM-14），行号以 `37338490` 为准，未验证项只剩外部 E 编号，WANTED = 0 |
| `wip/drill`（`976086d6`） | 真实进程演练四项：① 当前代码上重跑 game-demo 6b 演练（单实例锁、fail-stop、静态绑定）；② Init 中途失败收尾；③ 停机 hook 卡住时 SIGTERM 时序；④ kit Mongo / NATS Mod 真实依赖 Close 路径 | 已写 `kit/mongo`、`kit/nats` 的真实 Close 用例草稿；改了 `nats/driver/client.go`、`jetstream.go` 和 close 契约测试（**有代码改动，可能是发现了缺陷，需先判明是否 RR**）；演练脚本存在 `docs/bugfix/evidence/real-process-drills/wip/`（drill1 / drill1b 输出只留尾部） | 先读 nats/driver 的 diff，判明是缺陷就按 RR 流程先红后绿（编号从 RR-20261006-21 起）；完成 ①～④；记录写 `docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md` |
| `wip/sktest`（`5b4d49f5`） | SKILL-1 三条分支（Interrupt 停进程出错、toggle release 回调出错、charge enter 失败后宿主侧残留）与 SKILL-3 reset 里 process 条目的用例 | 只写了 `skill/runtime_cast_terminal_branches_promises_test.go` 草稿 | **已完成**（分支 `sktest2` 合入 main）：Interrupt / toggle 两条一次通过、变异证明能红；charge 那条红 → RR-20261006-21，核对时又发现 RR-20261006-23；SKILL-3 process 条目红 → RR-20261006-22。“补测”节写在 NC-110 / NC-114 修复记录末尾。分册 3 的 SKILL-1 / SKILL-3 条目需按此更新 |
| `tail`（`2c01e06d`，已合入 main） | 收尾小项：① `activity.groups_file` 改为必填（缺失时协调器 Init 拒绝；删不核对分支）；② `slow_reroute.total` 删除补触发改道的用例；③ B10 两条管线分工写进 B10 §2.4、USER_GUIDE 配置节、codegen 文档（对照表），DECISIONS 第十三轮 B10 行改已实施 | **已完成**：① 见 [RR-20261006-17 修复记录“后续”](../../bugfix/RR-20261006-17.md#后续groups_file-改为必填)；② 见 [RR-20261006-18 修复记录“补测”](../../bugfix/RR-20261006-18.md)（变异可见红）；③ DECISIONS 第十三轮 B10 行已改 | 发版分册需按新代码更新：OWN-5（组文件必填、`OpenActivity` 核对已做）、OPS-2（O3 序列删除已接、`slow_reroute` 补测）、CFG-7（两种标签方言合一不再“列为后续”，维护者选 A） |

## 3. 之后的步骤（恢复后按序）

1. 完成第 2 节四项，全部合入 main。
2. **重新冻结代码**。
3. 三份发版分册按新冻结提交再核对一次 `path:line`（`docs/release/v1.23.0/`）。分册 1 需更新 APP-1/7/8/9、OWN-2/3/5、OPS-2；分册 3 需补 NONCORE-46（RR-20 已修）。确认“仍未闭环”为 0。
4. 合成总文档 `docs/release/v1.23.0-GUIDE.md` 与 `v1.23.0-IMPLEMENTATION.md`。内容包括：版本时间线、主题地图、行为变化与兼容破坏总表、业务 / 运维改动清单、全局门禁清单、按包索引、WANTED 未决 = 0、外部验证指向 `docs/review/EXTERNAL-VERIFICATION-2026-10-06.md`。
5. 共享文件小改：
   - App 锁方案 §3.6 `app/app.go:125`→`:130`；
   - D-L3 §3.2 `gift_saga.go.tmpl:353`→`:360`；
   - DECISIONS 第十三轮“不留 WANTED”行状态改为已闭环（列提交）。
6. 发版：
   - 改 `codegen/ci/framework-release.yaml` `release: v1.23.0`；
   - 判断生成器 Core 下限是否要提高（生成代码用了新 API 才提高）；
   - 在干净 worktree 跑 `scripts/pretag.sh v1.23.0`，再跑 `scripts/test-remote-matrix.sh`（21 格，独占，约 4 分钟）；
   - 打 tag 并 push；
   - 对着 tag 生成 game-demo（`GOPROXY=direct`）做 build / vet；
   - 回填 bug / bugfix README、交接、bugfix SKILL 的版本号。
7. 发版后写框架整体双文档（`framework-docs-spec.md`）。

## 4. 环境注意

- 共享隔离环境 `~/.roost-it/roost-dataengine-it/env.sh` 含凭据：只 source，不打印。
- 不写共享的 game / saga / remote_entity 库和 `ROOST_*` 流。
- 私有依赖用 `scripts/mirror-local.sh`（私有目录 + 端口偏移，用完 clean）。
- 本次暂停时已停掉全部私有依赖进程和演练进程；scratchpad worktree 已移除，WIP 都在上表分支。
