# 联盟起服宣战补发扫描（`ScanWars`）随联盟数的代价（2026-11-06）

问题（[联盟服务方案 §2.5B“宣战”](../../../maintenance/FEATURE-2026-11-04-alliance-service.md#25b-联盟不用-effect2026-11-05-晚)）：跨实例宣战在宣战方留下待确认标记 `WarAcks`，宣战方 owner 重启后由 `ScanWars` 列出本实例拥有的联盟、逐个读标记、补问敌方。原文档写的代价是“每次起服每个联盟一次 Nest 读”，没有测过。

## 方法

`TestAllianceE2EWarScanStartup`（`scripts/testdata/alliance-e2e/warscan_test.go.txt`，`ALLIANCE_E2E_PERF=1`）：生成的 game-demo 二进制起 alliance 两个实例。3001 先起、建敌方联盟；3000 起来后 3001 退出建盟队列组，此后的联盟都在 3000 上。每一档 N：

1. 补建到 3000 上有 N 个联盟；
2. 停 3001，3000 上取 N 的 1% 个联盟向 3001 上的敌方宣战（对方不可达，留下标记；一个敌方至多 8 场战争，按总数备足敌方）；
3. 停 3000（运行中的重试随进程丢失），拉起 3001，再拉起 3000——只剩起服扫描能补发；
4. 量：拉起到 `/readyz` 的时间、预加载窗口（日志）、扫描本身（新日志 `alliance: war scan done` 的 `took`）、拉起到全部敌方一侧记上（`Info` 读敌方 `Wars`）的时间、3000 的常驻内存峰值（每 100 ms 采 `ps`）、扫描期间 3000 的 Remote 写准入次数（`remote_entity.remote.prepare_total{batch="single"}`）、整台私有 Mongo 的 `serverStatus` 差值，以及所有权列表查询的执行计划（`explain`）。

环境：macOS（Apple M5，10 核，32 GB），`scripts/mirror-local.sh` 私有依赖（Mongo 三节点副本集，WiredTiger 缓存 0.5 GB）。机器同时有别的 agent 的 128 / 256 并发压测，swap 已用约 14～16 GB；每档记 1 分钟 load average。单样本，时间只作量级。

修前 = `31003f19`（逐个读），修后 = `f85cb8d7`（每页 8 个并发读）。

## 结果

| N（待确认标记） | 检出 | load（起 / 止） | 起服到就绪 | 预加载 | 扫描 | 拉起到补发全部完成 | 常驻内存峰值 | 扫描期间写准入 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 000（10） | 修前 | 33.6 / 29.0 | 1.8 s | 1.6 s | 13.0 s | 14.9 s | 86 MiB | — |
| 1 000（10） | 修前 | 27.5 / 29.3 | 2.4 s | 2.2 s | 11.9 s | 14.3 s | 84 MiB | — |
| 1 000（10） | 修前 | 59.0 / 55.5 | 2.3 s | 1.8 s | 19.0 s | 21.4 s | 87 MiB | — |
| 10 000（100） | 修前 | 45.6 / 18.3 | 12.6 s | 12.2 s | 2 分 50 s | 3 分 2 s | 169 MiB | — |
| 1 000（10） | 修后 | 38.5 / 37.2 | 3.7 s | 3.2 s | 2.7 s | 6.4 s | 88 MiB | 975 |
| 10 000（100） | 修后 | 21.8 / 27.2 | 15.4 s | 15.0 s | 26.5 s | 42.0 s | 200 MiB | 10 089 |
| 50 000（500） | 修后 | 8.0 / 17.7 | 53.3 s | 53.1 s | 2 分 4 s | 2 分 59 s | 494 MiB | 50 500 |

修前 5 万没有跑（按 1 万的每个约 17 ms 外推约 14 分钟，**未实测**）。“扫描期间写准入”是扫描开始前后 3000 的计数差（修前那一轮没有加这个计数；扫描之外只有 k 次确认后删标记的写）。补发全部完成：每一档都在扫描结束后 0.1～1 s 内全部补上，没有遗漏。

### Mongo

- **起服不被扫描挡住**：扫描在服务的后续工作里（`followUps`，后台 goroutine），Mod `Start` 布置完就返回。起服到就绪基本等于预加载（1 万约 12～15 s，5 万约 53 s，约 1.1～1.5 ms / 个，与 [2026-11-04 预加载短测](../alliance-20261104/README.md#预加载与续期testalliancee2eshortperf) 一致）。
- **每读一个联盟就是一次 Mongo 写**：`warAcks` 是 owned Remote 实体上的一次 Nest 事务，写准入（`remoteentity.beginWrite`）对独占实体每次都 `GrantWrite`——对 `_remote_entity_meta` 一次 `findAndModify`（递增 `_grant_fence`），handler 不写也一样。扫描期间的写准入次数正好是 N 加上确认后删标记的 k 次（975 是因为那一档的计数开始时扫描已读了一部分）。修前逐个读，每次一个往返（本机高负载下约 12～19 ms），所以扫描时间随 N 线性、且是全部往返的和。
- **列表查询是线性的**：`ListOwnedRemoteEntities` 的执行计划是 `LIMIT>FETCH>IXSCAN{_id}`（按 `_id` 顺序扫、在 FETCH 里按 owner / kind 过滤，不用 `{_owner_sid: 1}` 索引）；联盟号小、守卫号（玩家号）大，所以前面每页约 1 000～1 063 个键 / 文档，只有最后一页会一路扫过后面全部守卫的元数据（1 万档最后一页 11 012 个）。一次扫描 N / 1000 页，键数约 N + 守卫数。
- `serverStatus` 差值（整台 Mongo，含 3001、outbox 轮询等底噪，只作量级）：5 万档扫描期间（约 2 分钟）command 16.1 万、query 7.6 万、getmore 8.0 万、扫描键 35.8 万、扫描文档 26.4 万；起服到就绪（预加载）command 19.2 万、扫描文档 35.1 万。

## 结论与改动

1. 起服本身不受扫描影响，原来的担心（阻塞起服）不成立；扫描的问题是**补发延迟随 N 线性**：最后一个标记要等全部 N 次写许可往返之后才补问（修前 1 万约 3 分钟，5 万外推约 14 分钟）。
2. 已改（`f85cb8d7`，不改语义）：每页 8 个并发读（`scanConcurrency`）。1 万从 2 分 50 s 到 26.5 s，5 万 2 分 4 s；写许可次数不变（仍是 N 次 Mongo `findAndModify`，只是 8 个一起发），常驻内存不受影响（峰值由预加载决定）。上限取 8：起服时这些写与实例刚开始服务的请求共用 Mongo，不放大成更大的突发。
3. 剩下的线性来自框架：**Remote 托管实体上的只读 handler 也取写许可**。见 [KNOWN-LIMITS](../../../maintenance/KNOWN-LIMITS.md) 对应条（需要维护者决定）。按 DAO 字段只列带标记的联盟走不通：Remote 的 DAO 文档在 Mongo 里是不透明的 `data` 字节，`war_acks` 不能被查询或建索引；`ListOwnedRemoteEntities` 只能按所有权记录（owner、kind）过滤。

## 复跑

```sh
ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=<偏移> scripts/mirror-local.sh up
set -a; source <私有目录>/roost-dataengine-it/env.sh; set +a
ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RUN='^TestAllianceE2EWarScanStartup$' ALLIANCE_E2E_WARSCAN_COUNTS=1000,10000 scripts/test-alliance-e2e.sh
scripts/mirror-local.sh clean
```

`ALLIANCE_E2E_WARSCAN_PENDING` 是待确认标记的千分比（缺省 10）。1 000 + 10 000 两档约 4～7 分钟，单独 5 万一档约 15 分钟（其中建盟约 11 分钟）。证据（PERF 行，已去掉连接串）在维护者本机的证据目录 `alliancegaps-20261106/warscan/`，未提交；进程日志含连接串，未保留。
