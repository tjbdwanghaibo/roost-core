# Service 第七轮证据

源码 `1625fee0bf0884792acf8520fd5bb26183dd0b64`；[问题](../../../bug/REVIEW-2026-09-29-services-07.md)、[运行记录](../../REVIEW-2026-09-29-services-07.md)。[RESULTS.json](RESULTS.json) 保存 15 叶子结果、失败/保留状态输出和原日志 hash；[BASELINE.json](BASELINE.json) 保存四包 268 测试事件绿测摘要；[COVERAGE.json](COVERAGE.json) 是 MCP best-effort 覆盖信号；[SOURCE.json](SOURCE.json) 固定关键源码 Git blob 与复现材料 hash。

## 复跑

需要自己独享、可丢弃的 Redis 单机和**已就绪**三主 Redis Cluster。只连接测试实例。无需 replica；此处只验证原子清理及同槽准入，不测 HA。

本轮实例均 Redis 8.8.0，单机 16409、cluster 16406/16407/16408，每节点配置 bind 127.0.0.1、protected-mode yes、save ""、appendonly no。Cluster 节点另设 cluster-enabled yes、独立 dir/nodes.conf、cluster-announce-ip 127.0.0.1、各自 port/bus-port。测试实例就绪后：

```powershell
# 仅用于自己新建、空的三个测试节点；不要对已有/生产集群执行。
redis-cli --cluster create 127.0.0.1:16406 127.0.0.1:16407 127.0.0.1:16408 --cluster-replicas 0 --cluster-yes
redis-cli -p 16406 cluster info
# 确认 cluster_state:ok、16384 slots、cluster_size:3，再复跑。
./Run-Repro.ps1 -RepositoryRoot D:/whb_s/cube-core `
  -RedisAddress 127.0.0.1:16409 `
  -ClusterAddresses '127.0.0.1:16406,127.0.0.1:16407,127.0.0.1:16408' `
  -OutputDirectory D:/whb_s/.tmp/service-review7-rerun
```

`Run-Repro.ps1` 使用 platform/rank/match 的 `.go.txt` overlay，保留 go test 预期 exit=1 的红证据；校验 9 pass / 6 fail / 0 skip/build-fail。未来修复后它会报告计数变化，不能据脚本失败继续声称旧 bug 未修。单机测试 helper 若依赖不可达会 skip，但 runner 拒绝把 skip 当通过；Cluster 失败必须包含实际 CROSSSLOT，连接失败不是反例。

既有无 overlay 门禁：

```powershell
$env:ROOST_REVIEW_REDIS='127.0.0.1:16409'
$env:ROOST_REDIS_TEST_ADDR='127.0.0.1:16409'
$env:REDIS_ADDR='127.0.0.1:16409'
go test -race -count=1 -p=1 -timeout=180s -tags integration ./kit/service/platform ./kit/service/rank ./service/match ./versionstore
```

原始日志在本机 `.tmp/service-review7-final/repro.jsonl` 和 `.tmp/service-review7-baseline.jsonl`；共享材料保存摘要/hash，不依赖本机大日志才能复现。修前初稿把 Match Sweep limit 写成非法 256，最终夹具已改 MaxPageSize=200；这两个初稿失败不计入产品缺陷。

## 场景与限制

- Platform：两种对象重建场景都经过实际后台、签名 callback、真实 Redis Orders；只在清理之前用 channel 暂停。一次外部失败明确证明未应用，未模拟真实资金。ghost 初态是异常/历史索引，不声称来自当前原子 DeleteIf。
- Rank/Platform Cluster：实际 service Mod.Init/Provide + production driver。无效前缀失败，有效 tag 控制成功，排除环境未就绪；没有完整 app/RPC 网络验收。
- Rank：Remove/Reset 与在途 Add 的两个屏障控制，真实 Lua 重读而非手写 Redis 替身。
- Match：两后端 64 次终态保留观察，JSON 23084 bytes；不是压测/行覆盖率。

15 叶子只有声明场景的分母；上轮 100 生产路径清单仍是范围核算。对象重建不是 kill，三 master 没有复制容灾，268 绿测不证明生产全链已无 bug。
