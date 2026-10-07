# v1.23.0 发版交接（2026-10-06 暂停，2026-10-07 全部完成）

## 0. 当前状态（2026-10-07）：发版前工作全部完成，下一步是打 tag

- **代码冻结点 `5e72ca4d`**，之后只有文档提交。第 2 节的 WIP 与第 3 节第 1～5 步全部完成。
- **发版双文档已合成**：[v1.23.0-GUIDE](../../release/v1.23.0-GUIDE.md)（说明总文档：总览、47 行破坏性变化与升级清单、业务 / 运维改动、维护者决定索引、主题导航、外部验证、闭环状态）与 [v1.23.0-IMPLEMENTATION](../../release/v1.23.0-IMPLEMENTATION.md)（实现总文档：review 顺序与复跑、全局守卫、按包索引、四个反复出问题模块的方向判断、按风险排的检查点）；三份分册在 [`docs/release/v1.23.0/`](../../release/v1.23.0/)，共 191 条。
- **CHANGELOG** 已改为 `## [v1.23.0] - 2026-10-07`，顶部是“破坏性变化与升级清单”精简版；新开空的 `## [Unreleased]`。
- **共享文件小改已做**：App 锁方案 §3.6（与 §7.2）`app/app.go:125` → `:146`（`5e72ca4d` 上 `loadServiceConfig` 写 `server_type` 的行，原规格写的 `:130` 是 `e6828e4f` 的行号）；D-L3 §3.2 `gift_saga.go.tmpl:353` → `:369`、`playerowner.go.tmpl:119` / `:125` → `:124` / `:130`；DECISIONS-PENDING 文首“当前总状态”与第十三轮“不留 WANTED”“原下个大版本项”两行改为已闭环 / 已实施；skill 作者文档两处“未发版”标题改为 v1.23.0。
- **WANTED 未决 0，仍未闭环 0，待维护者决定 0。** 外部验证 E01～E28 不阻塞发版。
- **下一步：打 v1.23.0 tag**，按第 3 节第 6 步做（发版提交改 `codegen/ci/framework-release.yaml`、`codegen/internal/roost/manifest.go:83` 的 Core 下限与 `.github/workflows/framework-compat.yml:50` 的 minimum 行到 v1.23.0；pretag、故障矩阵、tag、对着 tag 生成 game-demo、回填版本号）。之后第 7 步写框架整体双文档。

### 汇总时发现的分册之间的差异（2026-10-07 已全部处理，`1b5c5d61`）

汇总者没有改三份分册的正文；维护者裁决后 5 处都已由 `1b5c5d61`（只改文档）改正，每条末尾写了处理结果：

1. **REM-13 与 CFG-12 的去重没有落到分册 2**：分册 3（CFG-12 说明与实现）写“REM-13 改为引用本条”，规格“汇总者待办”也定为“留 CFG-12，REM-13 改为引用”；但分册 2 的 REM-13 仍是完整条目（含自己的测试与检查点），没有提到 CFG-12。总文档按“同一项、两个编号都保留”处理。**已处理（`1b5c5d61`）**：分册 2 的 REM-13 说明 / 实现 / `_summary` 改为索引条目（编号、一句话结论、以 CFG-12 为准），锚点保留；REM-13 独有的两点（旧 core 读新配置的依据、模板 L2 TTL / 兴趣表容量与 `DefaultConfig` 不同）与三条检查点并入 CFG-12 实现；GUIDE 改为“REM-13 为索引，以 CFG-12 为准”。
2. **分册 2 `_summary` 写“仍未闭环 = 1”**（T-226 现象列的旧日志文本）：该项在分册 2 定稿后由 `34a3585d` 按源码改正，现在三份分册口径都应为 0；分册 2 的 `_summary` 没有回写。**已处理（`1b5c5d61`）**：改为 0 并注明 T-226 已由 `34a3585d` 改正；GUIDE §7 同步。
3. **分册 1 `_summary` §7 第 6、7 条写 RR-20261006-28 / -17 的修复记录“不改，供汇总者决定是否加后注”**：`a3aadb9d` 已在两份记录末尾追加了按 `5e72ca4d` 的更正段，`_summary` 未回写。**已处理（`1b5c5d61`）**：两条改为“RR-28 / RR-17 修复记录已由 `a3aadb9d` 追加更正”。
4. **RR-20261005-01 回归去向两边都写为主**：分册 3 的边界表写 NONCORE-20“本条为主（回归对照在本条）”，分册 1 的 OWN-4 / OWN-5 也把回归去向、`TestAGroupFitsOneLiveQuery` 写成本条内容；内容一致，只是主条目归属重复。**已处理（`1b5c5d61`）**：主条目定为分册 3 的 NONCORE-20；OWN-4 / OWN-5 压成一句话加链接，只保留组上限守卫 `TestAGroupFitsOneLiveQuery`（OWN 的组上限语义），删去重复的对照表；分册 1 `_summary` 交叉节写明反方向去重。
5. **兼容破坏标注口径不一**（小）：APP-9 标“是（契约）”，对应的 NONCORE-50 标“契约补充”；NONCORE-23 的一句话仍是“迟到成功告警”，SAGA-7 已写明本版正向迟到成功改为补偿（NONCORE-23 是以 SAGA-7 为准的索引条目）。**已处理（`1b5c5d61`）**：APP-9 两处与 NONCORE-50 三处统一为“是（契约收紧）”；NONCORE-23 的一句话与三处总表列改为与 SAGA-7 / SAGA-16 一致（本版正向迟到成功改为补偿这一步）。

---

## 附：2026-10-06 暂停时的交接原文


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
| `wip/drill`（`976086d6`） | 真实进程演练四项：① 当前代码上重跑 game-demo 6b 演练（单实例锁、fail-stop、静态绑定）；② Init 中途失败收尾；③ 停机 hook 卡住时 SIGTERM 时序；④ kit Mongo / NATS Mod 真实依赖 Close 路径 | 已写 `kit/mongo`、`kit/nats` 的真实 Close 用例草稿；改了 `nats/driver/client.go`、`jetstream.go` 和 close 契约测试（**有代码改动，可能是发现了缺陷，需先判明是否 RR**）；演练脚本存在 `docs/bugfix/evidence/real-process-drills/wip/`（drill1 / drill1b 输出只留尾部） | **已完成**（分支 `drill2` 合入 main，远端 `wip/drill` 已删）：WIP 的 nats/driver 改动确是缺陷 → RR-20261006-24（扩到 RPC CallAsync）；①～③ 符合预期，③ 发现 hook 超时错误不点名 → RR-20261006-25；④ Mongo 符合、NATS 另发现 RR-20261006-26。[记录](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md)。分册 1 的 APP-1 / 7 / 8 / 9 / 12、OWN-2 / 3 与分册 2 的 DRV-5 需按此更新 |
| `wip/sktest`（`5b4d49f5`） | SKILL-1 三条分支（Interrupt 停进程出错、toggle release 回调出错、charge enter 失败后宿主侧残留）与 SKILL-3 reset 里 process 条目的用例 | 只写了 `skill/runtime_cast_terminal_branches_promises_test.go` 草稿 | **已完成**（分支 `sktest2` 合入 main）：Interrupt / toggle 两条一次通过、变异证明能红；charge 那条红 → RR-20261006-21，核对时又发现 RR-20261006-23；SKILL-3 process 条目红 → RR-20261006-22。“补测”节写在 NC-110 / NC-114 修复记录末尾。分册 3 的 SKILL-1 / SKILL-3 条目需按此更新 |
| `tail`（`2c01e06d`，已合入 main） | 收尾小项：① `activity.groups_file` 改为必填（缺失时协调器 Init 拒绝；删不核对分支）；② `slow_reroute.total` 删除补触发改道的用例；③ B10 两条管线分工写进 B10 §2.4、USER_GUIDE 配置节、codegen 文档（对照表），DECISIONS 第十三轮 B10 行改已实施 | **已完成**：① 见 [RR-20261006-17 修复记录“后续”](../../bugfix/RR-20261006-17.md#后续groups_file-改为必填)；② 见 [RR-20261006-18 修复记录“补测”](../../bugfix/RR-20261006-18.md)（变异可见红）；③ DECISIONS 第十三轮 B10 行已改 | 发版分册需按新代码更新：OWN-5（组文件必填、`OpenActivity` 核对已做）、OPS-2（O3 序列删除已接、`slow_reroute` 补测）、CFG-7（两种标签方言合一不再“列为后续”，维护者选 A） |

## 3. 之后的步骤（恢复后按序；2026-10-07：第 1～5 步已完成，下一步是第 6 步）

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
