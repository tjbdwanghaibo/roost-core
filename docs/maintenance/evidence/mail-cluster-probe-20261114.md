# 邮件 Cluster 测量入口修复（2026-11-14）

M-13 准备阶段确认 `service/mail/redis_integration_test.go` 的 `integrationClient` 始终调用 `DefaultConfig(REDIS_ADDR)`。`DefaultConfig` 只填写单机 `Addr`，驱动仅在 `ClusterAddrs` 非空时创建 Cluster 客户端。因而交接中的逗号种子列表被当作一个 TCP 地址，不能形成 Cluster 测量。这是测试配置装配缺口，不是生产 Redis 驱动或邮件存储缺陷。

修复只涉及 integration 标签下的 helper：单地址保持单机；逗号列表拆成 Cluster 种子并去除首尾空白。原有 `redistest.Cleanup` 已支持相同列表，继续只回收该用例的唯一前缀。修正使用示例里的包路径。没有新增生产选项、修改负载主体或放宽任何断言。

## 红绿证据

使用全新私有 mirror offset 18000，六个 Redis Cluster 节点为 `127.0.0.1:35400` 至 `35405`，单机为 `127.0.0.1:34379`。macOS arm64、Go 1.27.3，`GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0`。本轮同时有生成器功能测试，以下是配置正确性的单次功能验证，不是容量数据。

| 运行 | 输入 / 实现 | 退出码与结果 |
| --- | --- | --- |
| 修前 | 原始二进制 + Cluster 列表 | 1；四个子项都报 `too many colons in address` |
| 修后 | 修正二进制 + Cluster 列表 | 0；publish、revoke、snapshot-hinted、snapshot-reload 全部执行 |
| 单机回归 | 修正二进制 + 单地址 | 0；同四个子项全部执行 |
| 移除修复对照 | 再运行保留的修前二进制 + Cluster 列表 | 1；相同地址解析失败 |
| 恢复 | 修正二进制 + Cluster 列表 | 0；四个子项再次执行 |

每次使用 `-test.run '^$' -test.bench '^BenchmarkBroadcastLedgerAtCapacity$' -test.benchtime 1x -test.count 1 -test.timeout 2m`。移除修复对照复用原二进制，没有声称重新编辑、编译了一份变异。原二进制来自 `357263ba`；父任务已确认其邮件源码与修复前 `48ed0113` 无差异。

- 原始二进制 SHA-256：`2a9fa377ea722d837a1b9b1a9c2e371c66c909597161392ed84f77d0d2c15852`。
- 修正二进制 SHA-256：`4fd9c48e384beb134351e172487b0cac06d7213a42a8bcd2603df6c68fcbcaa8`。
- 原始材料：`/private/tmp/v147-mail-cluster-check-20261114/` 的 `before.log`、`after-cluster.log`、`after-single.log`、`reverted.log`、`restored.log`、`result.txt`、`deps.log`。协调脚本为 `/private/tmp/v147-mail-cluster-check.sh`。
- 私有依赖已 `down`，目录保留；benchmark 按原逻辑清理本例唯一前缀。没有清理共享环境、缓存或 worktree。

正式 M-13 使用修正后的同一个测试二进制，分别指向单机和 Cluster，按 2000x、3 次的原计划测量。这里的 1x 结果不替代正式数据。

## 复跑

先启动私有 `scripts/mirror-local.sh` 环境并 source 其 `env.sh`，在本检出执行：

```bash
GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 \
  REDIS_ADDR="$ROOST_MIRROR_LOCAL_REDIS_CLUSTER" \
  go test -tags integration ./service/mail -run '^$' \
  -bench '^BenchmarkBroadcastLedgerAtCapacity$' -benchtime 1x -count 1 -timeout 2m
```

再以 `ROOST_DATAENGINE_IT_REDIS_ADDR` 替换 `REDIS_ADDR` 验单机，结束只停自己创建的环境。源代码与复现入口均保留在仓库，临时日志不代替下一轮实跑。
