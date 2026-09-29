# Service 第四轮：外部未知结果、删除与提交恢复

2026-09-29；roost-review，文档审查，未修改源码/正式测试/生成模板。当前只有一个模块，Core 根目录含 service、kit/service、codegen，旧三仓叙述不作为当前目录事实。

## 基线与图谱

Git origin 为 `tjbdwanghaibo/roost-core`，main；更新前/后均为 `bdbb61bc4340c5c6261f746f882039e6cc4f3928`。fetch 成功，origin/main 同 SHA，无需快进。主工作树初始干净；复用干净 detached 工作树 `D:/whb_s/.tmp/review-service-20260928` 审同一提交、保存文档，保留其他 agent 的工作。

图谱项目 roost-core，根 D:/whb_s/cube-core，Verify。初始 coverage generation `2026-09-29T03:13:02Z`，新增 create_role.go not_tracked；full refresh 提交一次，RPC 300s 超时，但随后查询到新 metadata generation **2026-09-29T05:34:58Z**、recorded_at 05:35:00Z、complete/full。不把超时说成刷新调用成功，也不重复启动/停止共享 MCP。

search_graph 的相关窄查询均到 has_more=false；Platform AttemptDelivery、Mail evict、Session releasePending 完成两方向 depth=1 trace，未截断。首个 Platform trace 曾因索引占用超时，随后重试成功。snippet 读取 AttemptDelivery/recordFailure/evict/releasePending。Match receiver Commit 与 HandleCallback 部分未被图谱提取，interface trace 无法证明实际调用；从当前源码补证。spatial.Add、policy.Join、log.Errorf 的启发式错边没有用作依赖结论。

候选路径统一 coverage 检查，四个服务 scopes 无记录 gap，但多数 metadata_changed，故逐个关键实现回读源码；ledger 两个 partial 范围 249/486 直接读过。初始几个旧布局路径 missing 后定位到 server_run.go、versionstore.go、memory_store.go；kit/service/mail 仅 alias，Mail 实现在 service/mail。[完整 coverage 响应归档](evidence/service-review-20260929-04/COVERAGE.json)。没有全 service 图谱穷尽结论。

## 本轮有界范围与结果

| 域 | 新增主链/不变量 | 动态证据 | 下一入口 |
| --- | --- | --- | --- |
| Platform | 外部错误 → pending → retry/admin；回调错误输出所有权；Mod/index/生成 collaborator | 超时退款、二次发货两后端反例；Memory 输出别名反例、Redis 对照；写前拒绝对照 | typed delivery result、权威收据、实际 game purchase 消费及重启 |
| Mail | Delete → retention → Deliver/Reserve；Delete 与迟到 Commit | 两后端删除复活；两后端 Commit 前后不一致观察 | 删除证明保留/容量、业务在途 Delete 定案、真实奖品收据 |
| Match | queue CAS → 写前/写后错误 → Ticket.MatchID → Match | 两后端各 2 场景通过；直接 Commit retry conflict 但可读回原 match | 正式 matchmaker/房间幂等分配、崩溃与长期历史容量 |
| Session | finish/resolve → releasePending → Release → markReleased → releaseClaim；生成 releaser | 阅读资源所有权和模板，本轮已有包回归；没有新增真实 allocator 场景 | 实际 allocator 的并发幂等、迟到释放对新资源代次保护 |

新增 **RR-20260929-23 P1、RR-20260929-24 P3**；旧 **RR-20260910-02 P2** 追加未领取直接删除变体，不重复计 RR。[问题与实施建议](../bug/REVIEW-2026-09-29-services-04.md)。Mail 在途 Delete 只列观察。旧 10/10 服务域主链有记录的范围进度保持，不等于每行/每故障窗口审完。

## 修复核对与执行证据

读上一轮 SERVICE-BUGFIX-2026-09-29-03 和 bug/bugfix 索引，核对 Account 持久计划/准入/Profile 克隆、Directory 原子 DeleteIf、Mail Token+Attempts 的当前源码。现有原触发、控制、并发和重建回归重新通过；没有把作者的测试复跑称为独立重写全部 4 个问题的验收，也不外推 HA/所有交错。旧空 slot/name conflict 保留对账边界，无新同类 bug 登记。

执行于 Go 1.27、GOCACHE D:/whb_s/.gocache，隔离 Redis `127.0.0.1:16394`、独立前缀、禁 RDB/AOF。完整相关现有回归：

```powershell
$env:REDIS_ADDR='127.0.0.1:16394'
$env:ROOST_REVIEW_REDIS=$env:REDIS_ADDR
$env:ROOST_REDIS_TEST_ADDR=$env:REDIS_ADDR
go test -race -count 1 -p 1 -tags integration -json ./versionstore ./service/... ./kit/service/...
```

退出 0：**16 包、794 test/subtest pass 事件、0 fail/skip**。servicemetrics alias 包无测试。Redis 补跑 Account/Mail 第三轮 TestReview3、重建和并发：2 包、15 test/subtest pass，0 fail/skip，退出 0。原始大日志在忽略的 .tmp；可移交的小型结果和复跑脚本在 [evidence](evidence/service-review-20260929-04/README.md)。

本轮新增 8 个叶子 × Memory/Redis = **16/16 个已声明场景执行**：7 个安全断言失败、9 个对照/观察通过。Go JSON 含 Match 父测试，Memory 为 5 pass/4 fail、Redis 为 6 pass/3 fail，共 18 test/subtest 事件；不能把事件数当独立场景数。两次最终 Run-Repro.ps1 均实际运行，Go 退出 1 为保存的行为反例，不是编译/环境失败；0 skip。新增 `.go.txt` 在 docs 内，由 overlay 注入，不留下正式测试文件。

已有包回归全部通过、新反例仍失败，说明旧测试并未包含这些新不变量。未改生成物，因此不重新执行无关生成/全仓编译；不是宣称它们本轮通过。

## 学习、限制与续跑

新增[机制文档](IMPLEMENTATION-SERVICE-EXTERNAL-OUTCOME-AND-DISPOSAL.md)：外部结果分类、删除/领取证明、错误回包所有权、Match 不确定结果恢复，以及资源端幂等和性能成本。建议均未实施。

没有真实 Broker/资产/退款、多进程 kill、Redis HA/断网、长期容量/公平性和 p99 测试。Session 模板无实际分配器，未做 double-free 结论；生成 purchase 只读未跑。图谱 clean scopes 不是完整证明。

下一轮从 Platform 订单和实际 purchase grant 消费交接、Mail 删除墓碑/容量、Match → 房间资源恢复继续，不回头重复已查主链。若用户说没有修复，跳过旧验收直接推进这些新范围；若调用 bugfix，优先 RR-23，再输出所有权和删除残余。进度以 [PROGRESS](PROGRESS.md) 新停点为准。收尾只提交 docs，推送授权沿用既有流程。
