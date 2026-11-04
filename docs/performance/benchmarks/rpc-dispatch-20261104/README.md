# 总线 RPC 接收端派发前后对照（2026-11-04）

改动：总线 RPC 由“按方法名哈希选固定 worker”改为“按顺序键派发”（[guide/09 接收端并发与顺序](../../../framework/guide/09-services.md)，CHANGELOG `[Unreleased]` 同名条目）。

- 修前：main `ee732878`；修后：同一基线加本改动（工作树，提交前测量）。两边用同一份用例 `TestAllianceE2ERPCConcurrency`（`scripts/testdata/alliance-e2e/perf_test.go.txt`），修前的检出临时放入同一文件。
- macOS（Apple M5，10 核），Go 1.27.3；`scripts/mirror-local.sh` 私有依赖（Mongo 三节点副本集、NATS 三节点 JetStream、Redis 单机）+ 脚本自起 etcd；生成的 game-demo 二进制起真实进程：**一个** alliance 实例（3000，`alliance.max_members` 500），调用方是生成的路由 `BusClient`（带路由缓存）。
- 交替跑四轮：修前 1 → 修后 1 → 修前 2 → 修后 2，每轮约 2 分钟（含生成与编译）。本机有后台负载，四轮前后的 load average（1 分钟）在 5.9～11.7 之间。单样本，延迟只作量级。

复跑：

```sh
ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=<偏移> scripts/mirror-local.sh up
source <私有目录>/roost-dataengine-it/env.sh
ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RUN='^TestAllianceE2ERPCConcurrency$' scripts/test-alliance-e2e.sh
scripts/mirror-local.sh clean
```

## 结果（次/秒 · p50 / p99）

| 调用 | 并发 | 修前 1 | 修后 1 | 修前 2 | 修后 2 |
| --- | --- | --- | --- | --- | --- |
| `Found`（不带键） | 1 | 8.8 · 106 / 139 ms | 8.4 · 101 / 178 ms | 7.9 · 115 / 156 ms | 7.4 · 117 / 165 ms |
| `Found`（不带键） | 16 | 9.3 · 1.72 / 1.99 s | **21.9** · 690 / 864 ms | 9.7 · 1.67 / 1.72 s | **22.2** · 698 / 891 ms |
| `Info` | 1 | 107 · 9.0 / 17.0 ms | 70 · 11.3 / 60.0 ms | 101 · 9.1 / 18.9 ms | 98 · 10.0 / 17.1 ms |
| `Info`，16 个联盟各一个并发 | 16 | 104 · 152 / 198 ms | **685** · 21.2 / 49.2 ms | 96 · 168 / 198 ms | **1040** · 15.9 / 20.6 ms |
| `Info`，同一个联盟 | 16 | 97 · 166 / 209 ms | 78 · 196 / 305 ms | 89 · 166 / 357 ms | 89 · 183 / 212 ms |
| `Join` | 1 | 12.2 · 82 / 93 ms | 10.3 · 95 / 116 ms | 9.3 · 102 / 140 ms | 10.1 · 98 / 130 ms |
| `Join`，16 个联盟各一个并发 | 16 | 11.2 · 1.40 / 1.52 s | **16.1** · 952 ms / 1.38 s | 11.3 · 1.41 / 1.59 s | **18.2** · 847 ms / 1.13 s |
| `Join`，同一个联盟 | 16 | 11.3 · 1.42 / 1.56 s | 8.7 · 1.80 / 2.49 s | 10.3 · 1.57 / 1.69 s | 11.8 · 1.32 / 1.55 s |

全部请求成功（failed=0）。

## 读法

- **不同实体 / 不带键的请求不再互相排队**：`Info` 分散到 16 个联盟从约 100 次/秒到 685～1040 次/秒，p50 从约 160ms 到 16～21ms（接近单并发的 10ms）；`Found` 16 并发从约 9 个/秒到约 22 个/秒。
- **同一实体按到达顺序逐个处理，吞吐不变**：同一个联盟的 `Info` / `Join` 修前修后都约等于单并发吞吐（这是承诺本身：同键逐个进 handler）。
- **`Join` 分散到 16 个联盟只从约 11 次/秒到 16～18 次/秒**：总线已不是瓶颈（同一时刻 16 个 handler 都在跑），剩下的限制在入会事务本身（守卫新建与认领、strict Remote 写的 Mongo 往返等），没有在这次定位，见 KNOWN-LIMITS 对应条目。`Found` 同理受建盟事务限制。
- 单并发 `Info` 修后 1 的 p99 60ms 是单样本抖动（修后 2 回到 17ms），派发路径对单个请求只多一次 goroutine 启动。
