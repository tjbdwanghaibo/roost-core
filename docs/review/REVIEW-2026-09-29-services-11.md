# Service 第十一轮：Pipeline 修复与本阶段审查完成

**Service 本阶段 review 完成：10/10 功能域主链、存储契约、组装/运输与具名本机专项均已有记录；RR-20260929-01～34 均已修复并通过各自声明场景，本轮没有新增确认缺陷。**后续工作转向具名设计实施和真实环境验收；不再把已整理的其余 Service 留作泛化“下轮再看”。这是有界审查结论，不是无 bug 或生产全面验证证明。

源码基线 `328fc6ef31fdc91f21035389b2110d15f2e1c4e3` 加[RR-34 本轮修复](../bugfix/RR-20260929-34.md)。按两项 skill 顺序执行；三仓 fetch/快进未新增 main 源码/Wanted，本轮没有 feature 分支变更需要冒认验收。复用隔离树，保存其他 agent 工作，无发版/部署/生产数据迁移。

## 范围与进度

分母是当前 Core `service/` 与 `kit/service/` tracked 生产 Go 路径：**100 路径，82 非生成、18 生成**。本轮逐路径读取内容计算 Git blob，100/100 与第十轮相同，复用已列审查证据；没有冒称本轮逐行重新阅读。另直接阅读共享 Redis 改码、正式回归、cache 四处 pipeline 调用和维护入口。[清单](evidence/service-review-20260929-11/inventory.csv)保存当前 blob/复用方式；18 生成路径用当前 12 次只读一致性检查核验。

| 域 | 本阶段完成范围 | 本輪及最近证据 |
| --- | --- | --- |
| Account | 身份/session、名字、slot/持久建角计划、发布准入/profile、Redis/Mod/RPC | RR-03/11/12/19/22 已修；当前整包回归，run 无后台占用记录需要回收 |
| Directory | Normalize、Reserve/Commit/Cancel/Release、token/代次/条件删除、Redis/Mod | RR-21 原子身份删除已验；当前回归，未改源码复用 |
| Global routing/lease | 路由/迁移、lease epoch/incarnation、Load、Redis/runner | RR-13/17 已验；run 不删除 lease key，过期在访问时判断以保留版本围栏 |
| Activity | 持久 Opening/贡献/聚合、dispatch/ACK/owed、Admin/runner/Redis/RPC | RR-02/08/15、旧 Opening 残余与近期 RR-31 Cluster 已验；本轮 sweep 顺序及只观察、不消耗 attempt 已读 |
| Mail | 意图/ledger/envelope、投递/页读取、领取/取消/删除/墓碑、Redis/runner/Mod | RR-04/16/20/33及旧删除残余已验；当前整包及 Mod 两后端回归，RR-34 wrapper 邻接补修 |
| Match | 队列/候选/分组/Commit/未知恢复、取消、expiry/sweep、Redis/Mod/RPC | RR-25 已验；本轮 configured queues + SweepBatch=200，无队列告警、可恢复错误继续运行 |
| Platform | auth/callback、Order/pending、attempt/backoff/未知证明、Admin/对账、Redis/runner | RR-01/23/24/28/30 已验；本轮坏记录延期、缺失原子清索引、Held/Settled/Expired 分类复查 |
| Rank | 编码/Submit/CAS/去重、Page/Rank/Around/Remove/Reset、Redis/Mod/RPC | RR-05/06/10/26/29 已验；本轮 no-op runner 的期限/重试义务核对，当前整包回归 |
| Session | Enter/claim/request、Attach/Finish/Leave、释放/Admin、OwnerSource/Sweep、Redis/Mod/RPC | RR-09/14/18及旧正常 Finish 残余已验；本轮启动能力断言、SweepBatch=100/30秒/取消边界 |
| Chat | auth/policy/body/频道、Append/Seq/History/Scrollback、去重/保留/Prune、Redis/RPC | RR-07/27 已验；本轮 PruneChannels 缺省禁用、每频道 PruneBatch、取消/失败边界 |

辅助 split 四文件、integration/doc.go、servicemetrics 转发六路径继续纳入范围，未虚设第11服务域。表中完成表示具名主链契约已审；各域故障限制沿[完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)与下列交接保留。

## 本轮补查与设计/性能结论

1. **维护义务各有入口。** Match 的静态 queues、Activity 的 groups、Session 的 rotating OwnerSource、Platform 的 pending index、Chat 的 PruneChannels 是部署接线；没有接线不能把 ticker 存在等同后台回收生效。Account token 过期按读检查，Global lease 保留 epoch/version，Rank 无期限义务；Mail envelope TTL 和 inline eviction 不等于全邮箱背景清理，旧源码 runner 的“gap”注释不能单独证明 RR-04 当前仍未修。
2. **故障不应吞掉义务。** 维护页读取或某个条目失败会记录并保留下次机会；Activity 从持久 Delivering 找恢复任务，DueDispatches 只观察，真正消费 attempt 在游戏侧。Platform malformed 延期避免毒条目持续占队首，missing 清索引需保留已修原子围栏。当前回归包含这些已有行为，本轮未将注释当执行证明。
3. **有界分页仍有容量边界。** 每页 batch 有上限，配置/回调返回的队列、group、owner/频道集合仍依接入方规模、公平轮转与 ctx 配合；复杂度随目标数和每页批量增长，不能称整次 tick 固定 O(1)。Pipeline 新增扫描 O(batch)，不新增 Redis 命令/往返，future/命令结果空间 O(batch)。没有新吞吐/p99 测量，也不把本机测试时长作性能结论。
4. **关闭边界保留协作要求。** run select 收到 ctx 后返回，ticker defer 停止，向存储/外部 callback 传 ctx；不证明一个忽略 ctx 的用户 Releaser/Deliverer/PruneChannels 会被强制中断。真实资源系统需要持久幂等与身份围栏，不能由示例打印日志验收。

本轮图谱先定位 run/维护/配置：首批133条搜索分两页100+33，runner专项29条无剩余；相关 trace 使用双向 depth1。图谱把部分标准库 time.NewTicker、slog.Error 等误连到仓内同名函数，`Exec` receiver 重名又落到测试 stub；Activity 初次遗漏 global 前缀的 snippet 失败，随后重新搜索确切 qn 成功。按真实 import/receiver/源码复核，不据误连边宣称调用者已穷尽。

## 修复台账：已登记缺陷的原场景状态

| 编号范围 | 已实施/声明场景验证 | 证据入口 |
| --- | --- | --- |
| RR-01～18 | 18/18；另旧正常 Finish ABA 残余补修 | [首批逐项表](../bugfix/SERVICE-BUGFIX-2026-09-29.md) |
| RR-19～22 | 4/4 | [第三批](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md) |
| RR-23/24 | 2/2；另旧 Mail 删除残余补修 | [第四批](../bugfix/SERVICE-BUGFIX-2026-09-29-04.md) |
| RR-25～27 | 3/3；另旧 Activity Opening 残余补修 | [第五批](../bugfix/SERVICE-BUGFIX-2026-09-29-05.md) |
| RR-28～30 | 3/3 | [第六批](../bugfix/SERVICE-BUGFIX-2026-09-29-06.md) |
| RR-31/32 | 2/2 | [第七批](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md) |
| RR-33 | 1/1 | [第八批](../bugfix/SERVICE-BUGFIX-2026-09-29-08.md) |
| RR-34 | 1/1；本轮原3断言和正式两后端定向通过 | [第九批](../bugfix/SERVICE-BUGFIX-2026-09-29-09.md) |

共34个本阶段新编号，旧残余单列不重复编号。不回写历史 FAIL/“未实施”；本节为当前状态。旧修复沿未变源码与各自原证据复用，当前 Service 整包回归补证；RR-32 生成业务行为沿第七批实际生成 consumer 回归复用，本轮 consumer 是全编译，不误称重跑全部消费测试。

## 当前实际验证

原 RR-34 standalone/Cluster 2 反例及复用控制修后3/3通过；正式 driver 定向 integration/race/count=2 **58 次叶子执行、70 pass 事件、0 fail/skip/build-fail**。完整 `go test -tags integration -race -count=1 -timeout=180s -json ./versionstore ./cache ./redis/driver ./kit/mods ./kit/service/... ./service/...`：**19个测试包、1035 pass事件、951 pass叶子、0fail/build-fail**。3个 Toxiproxy test skip；servicemetrics 无测试 package skip=1。

Core `go test -run '^$' ./...`、既有生成 consumer 同命令、相关 vet、按源码 go:generate cwd/参数运行的12次 RPC只读检查全部通过。新未知回复 hook fixture 首轮连接初始化错误保留，再预热同 owner 连接后原断言全部通过；不是改产品代码掩盖失败。[结果/复跑](../bugfix/evidence/service-bugfix-20260929-09/README.md) · [生成检查](evidence/service-review-20260929-11/RPC-CHECKS.json)。本轮专用 Redis身份核对后关闭，用户 MCP不受影响。

Tier2，graph project `roost-core`，root `D:/whb_s/cube-core`，开始 generation `2026-09-29T14:09:57Z`、ready。对100清单+9邻接证据路径及service/kit/service/redis-driver scopes统一 coverage；两新测试主树未纳入，109路径 freshness非clean（metadata_changed/missing），两旧runner有parse_partial且直接读了完整脚本。当前树源内容摘要、100blob对照及实际回归补证，[coverage](evidence/service-review-20260929-11/COVERAGE-BEFORE.json)不当完整性证明。交付后刷新主树索引并保存新 generation，后补文档不改变测试源码。

## 后续是具名实施/真实环境验证

| 事项 | 已审结论 / 先用现有工具 | 验收条件 |
| --- | --- | --- |
| Match 终态与永久 request ledger 容量 | 归档必须与幂等保留契约一起设计，优先现有 versionstore/CAS/有界索引，不能直接按 TTL 遗忘 | 保留期、重放拒绝、长期规模/公平性和真实热点 SLO；[既有方案](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md) |
| 购买 grant fulfilled 与分页 drain | 首份奖励固定已修；永久 ClaimPurchase/HGetAll 仍存在，须先权威消费回执再压缩归档 | 消费后拒绝旧 producer 复活、并发/未知回复/重建，真实资产对账；同一既有方案 |
| Session/支付/活动/邮件外部效果 | 内部状态围栏及恢复流程已有；示例外部 effects不算接入验收 | allocator incarnation+持久幂等，渠道/资产权威回执，延迟/重复/失联/跨进程重启 |
| 存量升级与配置 | 每批兼容/legacy 恢复入口已交接；混版本写者可能重开旧根因 | 核对真实旧数据/owner升级、备份与业务审计，明确维护窗口；本轮不迁移 |
| HA/物理网络/长稳 | 本机三 master无 replica；hook证明错误分类，3 Toxiproxy未跑 | replica failover、网络分区、强杀、真实长稳与容量，独立记录环境/失败标准 |

上述事项不冒充新确认 bug，也不冒充已实施功能；不阻止给出“本阶段Service源码主链审查已完成”，但不能称Service全面生产收敛。其他核心域、既有Wanted/性能台账不因本轮Service完成自动升级。
