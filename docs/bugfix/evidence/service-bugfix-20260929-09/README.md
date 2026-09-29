# 第九批 Service 修复证据

基线 `328fc6ef31fdc91f21035389b2110d15f2e1c4e3` 加 [SOURCE.json](SOURCE.json) 四份实际执行源内容。原 RR-34 [红测](ORIGINAL-BEFORE.json)/[绿测](ORIGINAL-AFTER.json)来自第十轮原 overlay；其源码、历史结果未改。新正式测试在 redis/driver 下，通常测试无需新依赖；真实后端需要 integration tag 与两个地址环境。

[RESULTS.json](RESULTS.json)保存单元红测、初次 hook fixture 失败、最终重复定向和整体回归统计、失败输出及原日志SHA。大日志/缓存/Redis目录不提交。单元红测3 fail/3 pass；初次 hook 装得过早导致连接初始化失败，standalone2次 fail，预热后同样断言58次叶子执行全过。最终整体验证19测试包/1035pass事件/951pass叶子，3物理网络测试skip，0fail/build-fail；无测试package skip另计。

## 复跑

先启动自己拥有的 standalone 和三 master Cluster，核对 cluster_state=ok/16384 slots；下面地址只是本轮实例。runner 不创建/停止服务器，也不自动启动 Toxiproxy。不要对共享数据执行 flush 或任意 shutdown。

```powershell
$env:GOCACHE='D:/whb_s/.gocache'
./docs/bugfix/evidence/service-bugfix-20260929-09/Run-Verify.ps1 `
 -RedisAddress '127.0.0.1:16449' `
 -ClusterAddresses '127.0.0.1:16446,127.0.0.1:16447,127.0.0.1:16448' `
 -OutputDirectory 'D:/whb_s/.tmp/service-bugfix9-recheck' -AllServices
```

本轮对应命令与原 runner 已实际运行；新汇总runner提供同参数复跑入口，语法和路径核对，不把新增包装脚本称为第二次完整回归。物理网络用例接线见 `redis/driver/lock_toxic_integration_test.go`：ROOST_DATAENGINE_IT=1 加 ROOST_DATAENGINE_IT_TOXIPROXY_URL/ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR；ROOST_IT_TOXIPROXY=1 要求它而非容许跳过。不要通过允许所有skip掩盖环境缺失。

Core/consumer编译 `go test -run '^$' ./...`；`go vet ./redis/driver ./cache ./service/... ./kit/service/... ./kit/mods`。12次RPC只读check复用第九轮 `Run-Review.ps1 -RPCOnly`，必须按原 go:generate 的cwd/参数比较；[检查结果](../../../review/evidence/service-review-20260929-11/RPC-CHECKS.json)。源码API/持久格式不变，未部署。

Cluster同tag错误排序、取消、部分写成功、reuse/discard/旧future、执行后sentinel未知回复均正式回归；hook不是物理网络丢包/replica failover证明。3个Toxiproxy用例未执行，外部资产/资源/渠道、强杀、旧数据迁移、长稳容量仍不计已验。[修复说明](../../RR-20260929-34.md)。
