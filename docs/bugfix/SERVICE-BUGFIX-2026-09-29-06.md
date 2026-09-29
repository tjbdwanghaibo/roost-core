# Service 第六批修复：pending 退休与 Cluster 配置

2026-09-29，基线 `db642494f7dbe594539244dbdba356490d466a51`，隔离树 `D:/whb_s/.tmp/review-service-20260928`。最新三仓 fetch/快进均无变化。用户要求 bugfix 后继续 review；本批修复上轮 RR-28/29/30，三个原触发均已修复、实际验证。继续 review 的新问题单独留在[第八轮](../bug/REVIEW-2026-09-29-services-08.md)。未发版/部署/迁移。

| 问题 | 根因与最终行为 | 正式回归 |
| --- | --- | --- |
| [RR-28](RR-20260929-28.md) P1 | 缺失读取不能授权迟到无条件 ZREM。新增 versionstore.RedisStore.IndexRemoveIfAbsent，EXISTS 值 key 与 ZREM 固定 index 同一 Lua；Platform RetirePending 接线 | [Platform 屏障与重建](../../kit/service/platform/bugfix_pending_retirement_test.go)、[单机/Cluster 原语](../../versionstore/bugfix_index_retirement_test.go) |
| [RR-29](RR-20260929-29.md) P2 | Rank Mod 在 Cluster 模式调用共享 ValidateClusterKeyPrefix，拒绝无有效首个 tag 的前缀 | [配置拒绝/真实 Cluster 生命周期](../../kit/service/rank/bugfix_cluster_prefix_test.go) |
| [RR-30](RR-20260929-30.md) P2 | Platform 原左括号检查改为共享验证，空首对不能被后续 tag 挽救 | [配置拒绝/真实回调](../../kit/service/platform/bugfix_pending_retirement_test.go)、[首 tag 规则](../../kit/mods/service_servicemods_test.go) |

## 为什么这样修

保留已有 IndexRemove/IndexRemoveIn 的无条件语义，避免改变其他调用方的维护协议；新原语只服务固定索引的缺失清理，不修改通用 Store 接口或增加未使用的 per-owner API。EXISTS 保护任何已有 Redis 值，包括空字节、损坏 envelope；无法解码不代表可删除。返回 true 表示确实移除了 member，false 表示已有值或没有 member，transport error 保留未知结果；重新调用仍检查当前权威状态。

创建先完成，清理看到 EXISTS 就保留新索引；清理先完成，后续 Create 的原子值/索引写恢复入口。新的脚本仍需同槽，不能拆成 Go Get→ZREM。旧 IndexRemove 的注释同步明确这一边界，修正 Platform 注释中“当前 Delete 分两步”的过时说法。

共享验证留在现有 kit/mods 包，仅由当前需多 key 原子性的 Rank/Platform 主动使用，不给所有单 key 服务强制 tag。规则与 Redis 一致：第一个 `{` 后的第一个 `}`，内容必须非空；单机原前缀继续使用。没有自动补 `{}`、改变键编码或把 CAS 拆成两写。

## 兼容与恢复

- RPC、订单/榜 JSON、键格式与通用 Store 接口不变；新增 RedisStore 方法是增量 API。直接 Redis 构造器的窄接口没有拓扑信息，调用者必须提供有效共同 tag，Rank 配置注释已说明。
- 原有效 Cluster 前缀与所有单机前缀保持行为。过去被错误接受的无 tag/空 tag/未闭合 tag/空首对后有 tag 配置现在启动失败，错误点名配置键。修配置前应按旧 key 空间做审查与迁移计划，不能直接加 tag 后误认为旧数据已经迁移。
- 一个公共 tag 将相关 keys 集中到一个 slot；本批保证当前事务条件，不实现按 board/order 分片。多 owner 混用旧 Platform 版本仍可执行不安全退休，全部 retry owner 升级后才兑现新保护；回退会重开原缺陷。
- 已经丢失 pending member 的历史 paid 记录不会被本次修复自动找到。保留订单，不自动删除或重发资产；按已知订单 ID/渠道账单核对持久 delivery/attempt 证明，再恢复索引或调用既有 AttemptDelivery。未实现自动全库对账。

## 实际执行

Go 1.27.0 Windows amd64；独占 Redis 8.8.0 单机 16419，三 master 16416/16417/16418、16384 slots、cluster_state=ok，无 replica/持久化。测试前重新执行上轮原 overlay：15 叶子中 9 pass / 6 fail / 0 skip/build-fail；[修前摘要](evidence/service-bugfix-20260929-06/BEFORE.json)。修后保持原 overlay 不变重跑：**15/15 pass**。

新增正式定向 race：**43 叶子/51 测试及子测试事件通过，4 包，0 skip/fail/build-fail**。覆盖迟到创建、同 store 对象重建、ghost-only、正常 paid、终态 defer、首 tag 表、真实 Cluster Rank Submit/replay/Page/Remove/Reset 与 Platform callback/terminal，以及两后端的清理→创建/创建→清理、空/损坏值、执行前失败/执行后丢回复再有迟到创建。对象重建不是强杀进程。

完整 `go test -race -count=1 -timeout=180s -json ./versionstore ./kit/mods ./kit/service/... ./service/...`：**17 测试包/826 测试及子测试事件/764 叶子通过**；servicemetrics 无测试包 1 个 package skip，test skip=0，失败/编译失败=0。全仓 `go test -run '^$' ./...` 与四个改动包 vet 通过。正式生成消费工程的 Platform/purchase/handler/game 按本地 replace 编译/测试；临时消费模块补充实际测试 import 的 go.sum，不修改框架 go.mod/toolchain。

[实际计数与日志 hash](evidence/service-bugfix-20260929-06/RESULTS.json) · [复跑入口](evidence/service-bugfix-20260929-06/README.md)。生产文件、正式测试与 review 依赖的 blob/SHA256 在[源码摘要](../review/evidence/service-review-20260929-08/SOURCE.json)。证据以 baseline+工作树内容摘要标识，不把测试时 HEAD 当作已经包含未提交修复。

未验证实际支付渠道/资产、replica/failover、断网/强杀、历史索引迁移与长稳容量。原第五批 4/4 验收沿未变实现保留，新第八轮问题不会改写其历史。

实施提交 `5f81e5c80ef3c8b7f517ab827157e3fc5fbc31d6` 已推送并核验 origin/main，主树干净同步；codebase-memory full索引成功、HEAD对齐、generation11:18:53Z。记录metadata_changed/partial限制，未停止共享MCP；测试独占实例核对目录后关闭，未发版。后续文档补记不改变上述生产源码/测试结果。
