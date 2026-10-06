# 非核心 Review 单元状态与接手说明

**刷新：2026-10-06（收尾第 1 批，v1.23.0 发版前），基线 main `d6a677e0`。** 仓库单模块 `github.com/tjbdwanghaibo/roost-core`。本文原是 10-05 第 27 轮写的“后续 Review 清单”，各单元的逐轮停点已经过时；这次按当前源码、review 记录、[维护者决定表](DECISIONS-PENDING-2026-10-05.md)与 git 历史整体重写。旧版逐轮停点在 git 历史里（`git log -p -- docs/review/REMAINING-REVIEW-HANDOFF-2026-10-05.md`）。

**当前结论**：15 个非核心单元（N01～N15）的本机有界场景都已给出结论，确认的缺陷都已先红后绿修复；需要维护者拍板的方向已在第二～十二轮全部决定并实施。剩下的只有两类：

1. **外部环境验证**：Linux 内核网络、跨主机分区、多节点 HA、长时间容量、多机 Redis Cluster、Windows、真实部署与客户端。统一列在 [外部验证清单](EXTERNAL-VERIFICATION-2026-10-06.md)，下文各单元只写编号。
2. **保持现状的观察**：review 记录判为“不修 / 设计边界 / 需要性能或运维证据再说”的观察。它们没有确认缺陷，维护者也没有要求改；每条的理由在原记录里，下文按单元列出。

完成总结见 [NONCORE-REVIEW-COMPLETION-2026-10-06](NONCORE-REVIEW-COMPLETION-2026-10-06.md)。

## 读法

- **版本**：v1.20.0 = `999dc672`、v1.20.1 = `be7407ab`、v1.20.2 = `c85d4565`、v1.21.0 = `4881f2b7`、v1.22.0 = `9bf690fb`。标“未发版”的提交在 v1.22.0 之后，随 v1.23.0 发布。bug 索引里各行写的“未发版”是修复当时的状态，以这里的版本为准（都用 `git merge-base --is-ancestor` 核过）。
- **编号**：NC-xx 是 `RR-2026100x-NC-xx` 的简写（10-03 / 10-04 / 10-05 登记），索引见 [docs/bug/README.md](../bug/README.md)。
- **决定**：“第 N 轮”指 DECISIONS-PENDING 里“维护者决定（第 N 轮）”那张表。
- 这不是“全仓无 bug”的证明，也不是逐文件审完的计数；它说明的是每个单元在有界场景矩阵内有结论、确认问题已收敛、受限场景已具名交接。

## 单元状态总表

| 单元 | 范围 | 本机 review | 修复（编号） | 待做 |
| --- | --- | --- | --- | --- |
| N01 | app / lifecycle / manager / health / admin | 完成 | NC-01（10-03）、NC-01～04（10-04）、NC-170、NC-230～234、RR-20261006-07 | 外部 E08 / E13 / E21 / E22 |
| N02 | httpclient / httpserver / security / gateway / webroute | 完成（HTTP/2 未单跑） | NC-02～04（10-03）、NC-05～07（10-04）、NC-80～83 | 外部 E05 |
| N03 | bus / nats / servicerpc / etcd | 完成 | NC-08～12、RR-20261004-06～08、NC-90～93、NC-172 / 173 | 外部 E06 / E07 |
| N04 | redis / mongo / cache / migration | 完成 | NC-13～32、RR-20261004-02～05 / 09、NC-52、NC-100～102、RR-20261006-08 | 外部 E08 / E10 / E11 |
| N05 | remoteentity / ownerroute（含 Mirror） | 完成 | NC-33～36、NC-130 / 131、RR-20260913-01 残余、NC-174、RR-20261006-01 | 外部 E01 / E02 / E06 / E10 / E13～E16 |
| N06 | service / saga / servicemetrics（S1～S6） | 完成 | NC-37～42、NC-50～52、U-0280 / 0281、RR-20261005-01、NC-250、RR-20261006-05 / 06 | 外部 E09 / E11～E13 |
| N07 | configdata / attribute / event / errcode | 完成 | NC-60～65、NC-73 | 外部：五进程热更与客户端帧（E04 附带） |
| N08 | codegen | 完成（macOS） | RR-20261004-12 / 13、cb11be90、NC-70～75、收尾第 2 批 | 外部 E21～E26 |
| N09 | skill（含 cmd） | 完成 | NC-110～117、NC-150～154、NC-210～216、NC-220～224、NC-280～283、RR-20261006-02～04 | 外部：skillsync 真实传输端到端（E04 附带） |
| N10 | ai / actionflow / featureflag / hotcode | 完成 | NC-120～123、NC-240～247 | 外部 E27 |
| N11 | spatial / timer / clock / index | 完成 | NC-140～147、NC-270 | 外部：三进程拒绝路径（E19 附带） |
| N12 | metrics / log / failurelog / robot | 完成 | NC-160～165、NC-261～266、RR-20261006-09 | 外部 E05 / E08 |
| N13 | container / safemap / goroutine / misc / internal | 完成 | NC-180～185、NC-267～269 | 无 |
| N14 | Kit 跨域装配 | 完成 | NC-190～194、NC-233 / 234 | 外部 E08（`cluster_addrs` 起服） |
| N15 | scripts / cmd 与非 Go 资产 | 完成 | NC-200～208 | 外部：修后 heal 矩阵实跑、Linux pid 认领（E26 附带） |

## 各单元明细

### N01 app / lifecycle / manager / health / admin

- **review**：完成。记录：[noncore-02](REVIEW-2026-10-04-noncore-02.md)、[noncore-03](REVIEW-2026-10-04-noncore-03.md)、[n01s4](REVIEW-2026-10-05-n01s4.md)（singleton 全部返回路径、OnFail、Live、健康迁移）、[n01b](REVIEW-2026-10-06-n01b.md)（Group 停止预算、Ops bind / 权限 / 关闭 / 命令期限、Health 映射）。
- **修复**：
  - RR-20261003-NC-01（单 Mod 绕过依赖校验）`3529a569`、RR-20261004-NC-01～04（Manager 停止 panic、重复 Start、Admin schema 泄漏、Ops 取消后遗忘 server）`483350ca`，v1.19.0。
  - NC-170（Engine 停止超时后重试假成功）`c99a687d`，v1.20.2。
  - NC-230～234（Ops 同步 bind、停机 hook 受预算、停机期失败进返回值、Redis Mod 重复 Stop、remote_entity 失败不记 stopped）与 `ops.admin_timeout` `2c1c7be7`，v1.21.0。
  - RR-20261006-07（`app.run` 退出原因进文件日志）`611d5d72`，未发版。
- **观察与处置**：
  - n01s4 O1 → NC-232；O2（Degraded 让 `/readyz` 503）→ 第五轮 D1“Degraded 算就绪”，`f6828f17`，v1.21.0；O4（活着的 game 服 > 64）→ 第三轮 C4 活动组文件，`277e1252`，v1.20.2；O5（旧 `:lease:*` 键无 TTL）→ 第十二轮写迁移说明，本批写进 [DEPLOYMENT §7.1](../DEPLOYMENT.md#71-升级后的手工清理)；O7 → NC-193 之后无残余；O8 → 维持；O9 → NC-190。
  - n01b O-P1（Ops 不带 `Bearer ` 也通过）→ 第十二轮收紧，`7b73aabc`，未发版；O-H1（checker 无期限）→ 第十二轮每个 1.5s，`7b73aabc`，未发版。
  - 保持现状：O3（`Dispatcher.OnInit` 清围栏，属核心线，记录写不改）、O6（AbortMigration 计 Accepted 的口径）、O-P2（admin 审计身份由客户端自报，设计边界）、O-G1（`ManagerGroup` 无生产调用方，按 C3 / C8“零调用方 API 保留”）。
- **外部**：E08（Redis Cluster 两客户端与进程演练）、E13、E21、E22。

### N02 httpclient / httpserver / security / gateway / webroute

- **review**：完成。记录：[noncore-04](REVIEW-2026-10-04-noncore-04.md)、[noncore-05](REVIEW-2026-10-04-noncore-05.md)、[noncore-n02](REVIEW-2026-10-05-noncore-n02.md)。HTTP/2 路径没有单独跑。
- **修复**：RR-20261003-NC-02～04 `3529a569`、RR-20261004-NC-05～07 `49796514`，v1.19.0；NC-80 / 81 / 83 `c8d72122`、NC-82 `eef7822e`，v1.20.1。NC-83 点名的同形停机候选已逐个处理：NC-170 / 172 / 173 / 174（`c99a687d`，v1.20.2）、bus JetStream RPC（NC-90）、nest（NC-171）。
- **观察与处置**：O1（Ops 命令期限）→ `ops.admin_timeout`，v1.21.0；O2（TCP 派生上限报错不点名字段）→ 收尾 A9，`fcc78ad0`，未发版；“Dispatch handler 不配合 ctx”的单独用例 → 收尾 A15，`fcc78ad0`。保持现状：O3（票据可重用，归 S1 account 语义）、O4（demo SessionID）、O5（httpclient `ReadAll` 不限大小）、O6（gateway / RateLimiter 仓内无装配方）。
- **外部**：E05。

### N03 bus / nats / servicerpc / etcd

- **review**：完成，真实 JetStream 与 etcd 上跑过。记录：[noncore-06](REVIEW-2026-10-04-noncore-06.md)～[08](REVIEW-2026-10-04-noncore-08.md)、[noncore-n03](REVIEW-2026-10-05-noncore-n03.md)。
- **修复**：NC-08～10 `3560a19b`、NC-11 / 12 `1502f973`，v1.19.0；RR-20261004-06 `c57b247b`、-07 `e5aea173`，v1.19.1；RR-20261004-08 `e0591f79`，v1.19.2；etcd 租约已过期时注销视为达成 `b67d5945`，v1.20.0；NC-90 `ae742984`、NC-91 `64179ad5`、NC-92 `25646001`、NC-93 `89a102db`，v1.20.1；NC-172 / 173 `c99a687d`，v1.20.2。
- **观察与处置**：O5（Close 第二次返回 context canceled）→ NC-173 的 A3 复核补修，`50f2ac2a`，v1.20.2；方向“Bus 回调入口统一准入”→ A3 ①，`50f2ac2a`；“排空下沉到 ISyncBus 退订”→ A3 ② 留下个大版本；“etcd 选举去留”→ 第二轮 B5 保留；**bus SETNX 去重 → 第十二轮保持，本批把契约写进 `bus/reliable.go` 的 `ReliableStore` 注释**。保持现状：O1～O4、O6～O11（AckWait 重投、至少一次、NAK 到 MaxDeliver、发布超时语义、LocalMirror Synced、WatchService 不重建、轻量 RPC 无期限、停止后 CallReliable 等满、durable 累积、Consume 无 ErrHandler），记录判为语义说明，不立 RR。
- **外部**：E06、E07。

### N04 redis / mongo / cache / migration

- **review**：完成。记录：[noncore-10](REVIEW-2026-10-04-noncore-10.md)～[20](REVIEW-2026-10-04-noncore-20.md)、[noncore-21](REVIEW-2026-10-05-noncore-21.md) / [22](REVIEW-2026-10-05-noncore-22.md)、[n04-revn04](REVIEW-2026-10-05-n04-revn04.md)。
- **修复**：NC-13～15 `08d18be9`、NC-16～20 `ce90e90d`、NC-21～25 `3d3b22c9`、NC-26～29 `25ef4c1e`，v1.19.0；RR-20261004-02～05 `3bce736c` / `5c1647c6` / `fa885409` / `e7c566ac` 与 NC-30 `7fbdc735`，v1.19.1；RR-20261004-09 `0bd8a9f3`，v1.19.2；NC-31 `c3aa0edd`、NC-32 `b2232db5`，v1.20.0；NC-52（versionstore 退避后用旧值）`be7bcc18`、NC-100～102 `81659082`（NC-101 复审 `edbe290b`、`f608503b`），v1.20.1；RR-20261006-08（mongotest `$in` 具名切片）`611d5d72`，未发版。
- **观察与处置**：观察 3（驱动默认重试）→ 第二轮 A2，`cf5721c9`，v1.20.2；观察 4（DistLock 经 Eval）→ A2 后不经驱动重放；观察 6（用例 `/reset` 共享 toxiproxy）→ A5 与 NC-208 补修（`d43aa3ba`，v1.20.2）；**驱动 Close 契约 → 第十二轮写进契约表，本批按实测写进 [redis/driver](../../redis/driver/README.md) 与 [mongo/driver](../../mongo/driver/README.md) README §5，不一致处登记 [WANTED W-2026-10-06-02](../bug/WANTED.md)**。保持现状：观察 1、2、5、7（记录判为语义说明）。
- **外部**：E08、E10、E11。

### N05 remoteentity / ownerroute（含 Mirror）

- **review**：完成，ownerroute 与赠礼静态路由无缺陷。记录：[noncore-23](REVIEW-2026-10-05-noncore-23.md) / [24](REVIEW-2026-10-05-noncore-24.md)、[n05-revn05](REVIEW-2026-10-05-n05-revn05.md)，Mirror 各步记录（[第 1～3 步](../feature/MIRROR-STEPS-1-3-2026-10-06.md)、[第 4 步与 O4](../feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)、[第 5 步](../feature/MIRROR-STEP-5-2026-10-06.md)、[第 6 步本机替代](../feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)、[第 6 步观察](../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)）。
- **修复**：NC-33 / 34 `be4eb0fa`、NC-35 / 36（含 RR-20260913-08 残余）`7949da08`，v1.20.0；NC-130 `6f06f0da`、NC-131 `c3475150`、RR-20260913-01 跨节点 L2 墓碑残余 `366058a7`、NC-174 `c99a687d`，v1.20.2；RR-20261006-01（删除 Remote 实体时确认报身份不符、不发布墓碑）`4ca757aa`，v1.22.0。
- **观察与处置**：水位方向 → 第三轮 B2（a），`f376bba0` / `7d49e54d`，v1.20.2；O4 兴趣容量 → 第九轮按 consumer 计，`23e17d81`，v1.21.0；O5（JetStream 重放历史）→ B2 发布时刻 + 第 4 步 DeliverNew；O6（无最大陈旧时间）→ `cached_max_staleness`；Mirror 第 1～5 步 `8495c5c4` / `23e17d81` / `a6985cf3`，v1.21.0；第 6 步本机替代 `b15e70c8`，v1.22.0；O-M6-1 / O-M6-3 `db67b8ee`、O-M6-6 `d483238e`、O-M6-5 `7b73aabc`，未发版；O-M6-2、O-M6-4 等其余 Mirror 观察 → 第十二轮保持；**L2 落后于权威 → 第十二轮保持，本批把上界写进 [B2 §7](../feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md) 与 USER_GUIDE**（`snapshot_l2_ttl` + `cached_max_staleness`，缺省约 5m30s）。保持现状：O1（ownerroute 无回执）、O2（赠礼转交先刷新驻留，归 U-0280 线）、O3（兴趣无主动撤销）。
- **外部**：E01、E02、E06、E10、E13、E14、E15、E16。

### N06 service / saga / servicemetrics

- **review**：S1～S6 本机都有结论。记录：[noncore-25](REVIEW-2026-10-05-noncore-25.md)～[27](REVIEW-2026-10-05-noncore-27.md)、[n01s4](REVIEW-2026-10-05-n01s4.md)（S4 global / App 替代）、[n06-revn06](REVIEW-2026-10-05-n06-revn06.md)、[n06s5](REVIEW-2026-10-06-n06s5.md)（saga 六项）。
- **修复**：NC-37 / 38 `47fca740`、NC-39 / 40 `12726715`、NC-41 / 42 与 RR-20261001-09 残余 `10c73e0c`、NC-50～52 与 RR-20261001-06 残余 `be7bcc18`、U-0280 / U-0281（`054fdd66` 等）、RR-20261005-01 `46c4dfba`，v1.20.1；B1 completion 代际 `3fabe34d`，v1.20.2；B9 窗口条目入口 / 建角判定表 `bd6df5e5`、换名释放 `b18d5613`、O37 `f6043e44`、saga 方向 ①② `a95cf4dc` / `9669d181`、NC-250 `31b48bc0`、发版前审查补漏 `42419890` / `5a3c4a60`，v1.21.0；RR-20261006-05（global `Bind` 重试误报冲突）、-06（saga 步骤预算大小写）`611d5d72`，未发版。
- **观察与处置**：revn06 观察 1（默认无指标）→ C6，`491aaf3b`，v1.20.2；观察 2（CAS 冲突率口径）→ 第十二轮，`7b73aabc`，未发版；观察 3（activity 预约身份）→ 第十二轮保持；观察 4 → B9；O-S5-1 / O-S5-4 → `9669d181`；O-S5-2 → 第十二轮启动校验，`7b73aabc`，未发版；O-S5-3 → 写进 SAGA.md 运维要点；O-S5-5 保持；O-S5-6 → RR-20261006-08；O-S5-7 与方向 ③④ → 第六轮暂不做；Mongo 步骤延迟 → 第十二轮维护者选 A（接受现状，`ff08c941` 只有分析）；C5（停机中仍算 Live）→ 写进契约，`bd6df5e5`。保持现状：观察 5（chat 热点频道容量，需要容量证据）。
- **外部**：E09、E11、E12、E13。

### N07 configdata / attribute / event / errcode

- **review**：完成。记录：[noncore-n07](REVIEW-2026-10-05-noncore-n07.md)、[noncore-n07b](REVIEW-2026-10-05-noncore-n07b.md)。
- **修复**：NC-60～63 `3d4fe9f3`、NC-64 / 65 `197f7bb9`、NC-73（errcode id 扫描 AST 化）`80802200`，v1.20.1；B10 / C2 `b12216ed`，v1.21.0；configdata 键大小写敏感 `c474a6ef`，v1.22.0（功能项）。
- **观察与处置**：C-O1、C-O9 → 第四轮 C2 写进契约；C-O5～C-O7 → C2 可见性（日志 + 指标），`b12216ed`；E-O1～E-O4（event 零接线）→ 第二轮 C3 保留、不再深审；属性容器回滚 / 重建 → A1 回滚统一走 DAO，`5407f127`，v1.20.2；C-O8（热更不重算在线玩家的 Gear）→ A1 方案明确保持（Gear 由业务事务 `RefreshGear` 重算，下次换装或加载生效，[A1 方案](../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)“热更新”条）。保持现状：C-O2、C-O3、A-O1、R-O1、R-O2。
- **外部**：五进程全链路热更、客户端复制帧、Luban 真实工具链（随 E04 / E20 的真实部署一起做）。

### N08 codegen

- **review**：完成（macOS）。记录：[n08-codegen](REVIEW-2026-10-05-n08-codegen.md)；09-30 的 codegen-01～06 是历史。
- **修复**：RR-20261004-12 `a646193c`、-13 `7ffa7199`，v1.20.0；cb11be90、NC-70～73 `80802200`、NC-74 / 75 `45d4bc1c`，v1.20.1；B6 信号统一接管与 C9 联网用例门 `f556049c`，v1.21.0；收尾第 2 批 A8 / A9 / A11 / A15 / A17 `fcc78ad0`、cfggen globals 规则 `229a5aa0`，未发版。
- **观察与处置**：O1 → C9；O2、O3、O6 → B6；NC-75 运行时 required → 第四轮 B10 选 A；O5（文档里旧 `roost-codegen` 模块路径）→ 本批改为 `github.com/tjbdwanghaibo/roost-core/codegen/...`（codegen README 与 `codegen/docs`，历史记录不动）。保持现状：O4（错误信息用 Go 字段名）。
- **外部**：E21～E26。

### N09 skill（含 cmd）

- **review**：完成。记录：[batch1](REVIEW-2026-10-05-n09-batch1.md)～[batch4](REVIEW-2026-10-05-n09-batch4.md)、[batch5](REVIEW-2026-10-06-n09-batch5.md)、[batch6](REVIEW-2026-10-06-n09-batch6.md)，以及第七轮、第十二轮实施记录（[求值上下文表](../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)、[第十二轮 skill 与 cfggen](../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)）。`runtime_select` / `runtime_input` / `trace` / `inspect` 没有单独逐行记录，只在 batch4 / 5 对照 Runtime 时覆盖。
- **修复**：NC-110～113 `855c2a38`、NC-114～117 `f37a94e3`，v1.20.1；NC-150～154 `bfd353c0`、NC-210～216 `7cf86f98`、B3 lower fail-fast `023eb276`，v1.20.2；NC-220～224 `5c04726f`、NC-280～283 `4ed038d9`，v1.21.0；RR-20261006-02～04（null 默认值状态 set、checkpoint 拒绝 `phase_timeout`、文件 outbox 遗留 tmp）`b8fbcee0`，未发版。
- **观察与处置**：O1 → 第四轮 B4，Runtime 不进事务、写文档，`62cec54e`；O2（buff 不进伤害）→ 第十二轮“组件给投影入口”，`CombatComponent.ProjectAttributes`，`229a5aa0`，未发版（本批按源码核对了组件注释）；O6 → RR-20261006-04；O7、O22、O29 → 第十二轮，`229a5aa0`；O15～O17、O27、O28 → 第十二轮保持并写作者文档；O19 → NC-151 的控制用例；O20 → RR-20261006-03；O33 → 第七轮编译期拒绝，O34～O36 保持并写文档，O37 → account 判定表，均 `f6043e44`；NC-151 / NC-213 方向 A → B3 ④ 保持方向 B，NC-151 `timeout_ticks` 第十二轮保持 warning；NC-224 方向 B → 第五轮不做；O3～O5、O8～O14、O18、O21、O23～O26、O30～O32 → 第十二轮“其余保持”。B3 ③（Host 取值能力表）→ 下个大版本。
- **外部**：skillsync 经真实传输（kit syncstream / NATS）的端到端；Windows 文件 outbox 替换（只读过源码）。

### N10 ai / actionflow / featureflag / hotcode

- **review**：完成。记录：[noncore-n10](REVIEW-2026-10-05-noncore-n10.md)、[noncore-n10b](REVIEW-2026-10-06-noncore-n10b.md)。
- **修复**：NC-120～123 `556d156d`，v1.20.1；B7 延后队列 `a9b7075b`，v1.20.2；NC-240～247（含 O-A4 → NC-243、O-H1～O-H3 → NC-245～247，NC-244 由真实 .so 发现）`44964553`、MissionRunner 延后队列 `f6828f17`，v1.21.0。
- **观察与处置**：O-A1（EndAll 不清场）→ 第五轮保持、接线说明写清；O-A2 / O-A3 → B7 后消失；O-T6 → 注释改正；**O-T3（Init 里发起的动作会被 EndActions 结束）/ O-T4（Shutdown 不调 EndActions）→ 第十二轮保持并写文档，本批写进 `ai/strategy.go` 的 `Strategy` / `StoppableStrategy`、`Controller.Shutdown` 注释与 kit/README 的 ai 段**。保持现状：O-F1（`gm.flag.list` 的 version 与 flags 可能不同代，只影响运维展示）、O-H4（实例级 handler 不能热补丁）、O-N1（只改了注释）、O-N2、O-A5、O-A6。
- **外部**：E27（Linux / Windows 真实插件加载）。

### N11 spatial / timer / clock / index

- **review**：完成。记录：[n11](REVIEW-2026-10-05-n11.md)、[revleft §2](REVIEW-2026-10-06-revleft.md)。
- **修复**：NC-140～147 `23a10f42`，v1.20.1；NC-270（`PathFindSystem.Stop` 无锁清空 terrain）`36220f34`，v1.21.0。
- **观察与处置**：O1 → 补注释；O4 → 入口穷举，NC-270；O6 → 第六轮 D-L1（期限、priority、登记顺序），`5abae51e`；O7 → D-L2（未注册类型删除时 Warn + 计数），`5abae51e`；O8 → NC-193 之后无残余；O9 → D-L3 业务 / 系统双时钟（`b9fc5342`、`fa472ee7`，v1.21.0）与“业务时间只许前进”（`3e77beb9`，v1.22.0）；组件内存回滚 → A1；index 去留 → C3 / C8 保留。保持现状：O2、O3、O5、O10。
- **外部**：真实 WAL / Mongo 三进程链路里 `ArmActivity` / `TickWorldTimers` 的提交被拒（随 E19 的三进程恢复链路一起做）。

### N12 metrics / log / failurelog / robot

- **review**：完成。记录：[n12-revn12](REVIEW-2026-10-05-n12-revn12.md)、[revleft §3](REVIEW-2026-10-06-revleft.md)、[第十二轮 kit 批](../feature/DECISIONS-R12-KIT-2026-10-06.md)。
- **修复**：NC-160 `f750ce43`、NC-161 `5fea59ce`、NC-162 `efeede0f`、NC-163 `92547035`、NC-164 `2a9e0c2c`、NC-165 `e798a759`，v1.20.2；NC-261～266（O8 / O6 / O4 / O10 / O11）`36220f34`，v1.21.0；RR-20261006-09（robot Stage 序号只增不回收，O9）`7b73aabc`，未发版。
- **观察与处置**：O1（默认无 Reporter）→ C6，`491aaf3b`；O2 / O3（序列删除）→ 第十二轮 `metrics.DeleteSeries`，loadtest 挤出历史时删 `run` 序列，`7b73aabc`；O5 → A2；O7 → RR-20261006-07。刻意没做：nest 派发器 gauge（核心线）、`bus_rpc_pending{method}` 删除（受 2048 上限约束）。
- **外部**：E05、E08；磁盘写满 / 只读文件系统上的日志行为（随 E19）。

### N13 container / safemap / goroutine / misc / internal

- **review**：完成。记录：[n13](REVIEW-2026-10-05-n13.md)、[revleft §4](REVIEW-2026-10-06-revleft.md)（O1～O11 全部有结论）。
- **修复**：NC-180～185（`7e4ed438`、`1d600b9b`、`4c26b4b5`、`20400337`，复审补修 `815c3661`）、C7 遍历回调仓库级契约 `cd43a5ac`，v1.20.2；NC-267～269（O5 / O4 / O9）`36220f34`，v1.21.0。
- **观察与处置**：O7 → C7；O10 → 第六轮 D-L4 保持；O11 → README 包名已改；零调用方 API → C8 保留。保持现状：O1（Get 未命中走写锁）、O8（SafeFunc 不记栈）——需要性能或运维证据再定；O2、O3、O6，及 O9 里 `totalTasks` 的瞬时不一致（没有确定性红测试）。
- **外部**：无。

### N14 Kit 跨域装配

- **review**：完成。记录：[n14](REVIEW-2026-10-05-n14.md)（A1～A19）、[n01b](REVIEW-2026-10-06-n01b.md)（O3 / O4）。
- **修复**：NC-190 `f9367785`、NC-191 `e1a6b01d`、NC-193 `d6550a16`、NC-194 `48b3311a`、NC-192（随第二轮 C1 方案 1）`3e3350d5`，v1.20.2；NC-233 / 234 `2c1c7be7`，v1.21.0。
- **观察与处置**：O1（宽松配置读取）→ 第二轮 A4 严格读取，`3e3350d5`（kit/redis 三个整数读取 `5df60765`），v1.20.2；A4 的配置 schema（①）留作后续重构；O2 → RR-20261006-06；O3 / O4 → NC-233 / 234；O5 → 死要求随 NC-192 删除。
- **外部**：E08（真实 Redis Cluster 下 `cluster_addrs` 列表写法起服）。

### N15 scripts / cmd 与非 Go 资产

- **review**：完成。记录：[n15](REVIEW-2026-10-05-n15.md)、[收尾第 2 批](../bugfix/CLOSING-BATCH-2-2026-10-06.md)。
- **修复**：NC-200～208 `6c1538be`、NC-208 的 kit/dataengine 补修（`TestToxicNATS*` 自建代理）`d43aa3ba`、NC-203 复核补修（A5，全局命令运行期间持锁）`3e3350d5`，v1.20.2。
- **观察与处置**：O1（source-head-check 吞 add 失败、与 CI 序列漂移）→ 收尾 A11，`fcc78ad0`，未发版；O5 → `d43aa3ba`；方向“隔离环境独占还是共享”→ 第二轮 A5 选共享。保持现状：O2、O3、O4（已写进 C01-RUNBOOK）、O6。
- **外部**：修后 heal / 矩阵在真实共享隔离环境上实跑（会注入故障）、Linux 上 NC-202 的 pid 认领（随 E26）。

## 与核心工作线的边界

Nest、Sync、DataEngine、Remote 本体由核心工作线 review（[核心优化交接](../CORE-OPTIMIZATION-HANDOFF.md)），本线只沿集成需要读取。接口处的待办：

- K1 Nest / Entity：快池禁阻塞、冷加载 / 慢续行、组迁移等由核心线负责；本线新增的 [WANTED W-2026-10-06-01](../bug/WANTED.md)（`releaseDispatchLocks` 无 Guard 作用域分支）等 review 判断。
- K2 DataEngine / K3 Remote 权限 / K4 Sync：本线修复只跑受影响回归，不冒认核心线的独立验收。

## 接手

1. 先 `git fetch`，读 AGENTS.md、[roost-coding](../agent-skills/roost-coding/SKILL.md)、核心优化交接与本文。
2. 新的 review 从 [WANTED](../bug/WANTED.md) 的未分流条目（W-2026-10-06-01、W-2026-10-06-02）和新增功能的增量开始；本文各单元的“保持现状”观察只在出现新证据（真实触发路径、性能或运维数据）时重开，不要重新登记。
3. 外部环境到位时按 [外部验证清单](EXTERNAL-VERIFICATION-2026-10-06.md) 逐项做，在该清单的状态列回填。
