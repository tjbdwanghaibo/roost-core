# Service 第八轮可复跑证据

基线 db642494 加第六批修复，Activity/购买文件未改；[结果](RESULTS.json)8 次叶子执行中 3 pass/5 fail，归属两个新 RR。Go 1.27.0、Windows amd64、race、真实单机 Redis8.8.0与三 master Cluster；[覆盖限制](COVERAGE.json)、[源码摘要](SOURCE.json)与[问题/实施交接](../../../bug/REVIEW-2026-09-29-services-08.md)齐全。

准备自己的可丢弃单机/Cluster，不连接生产；Cluster 要先就绪。DemoRoot 为正式 Roost CLI 生成的游戏工程，模块可任意命名，框架 replace 指向本轮 Core checkout；Platform collaborators、purchase 必须与当前模板一致，脚本会核对。此次复用本机 D:/whb_s/.tmp/service-review5-demo，未声称重新生成全工程或联网部署。

```powershell
./docs/review/evidence/service-review-20260929-08/Run-Repro.ps1 -DemoRoot D:/whb_s/.tmp/service-review5-demo -RedisAddress 127.0.0.1:16419 -ClusterAddresses '127.0.0.1:16416,127.0.0.1:16417,127.0.0.1:16418' -OutputDirectory D:/whb_s/.tmp/service-review8-final
```

脚本不提交/推送/启动/关闭 Redis。Activity overlay 注入正式包；purchase probe 注入生成业务包，seed/control 使用原 catalog，第三个新 Go 测试二进制只通过 overlay 将商品 Count=10 改为3。生产/模板文件不变；临时输出保存 probe、overlay、原始 JSONL 与摘要。go test -mod=mod 可更新临时 DemoRoot 的依赖清单，不能把本机消费 go.mod/go.sum 提交框架。

Activity keys 在 fixture cleanup 删除；purchase 使用独立 GUID 前缀保留到人工检查或销毁独占 Redis。每阶段预期 exit 与最终叶子计数都验证，build/skip/race/panic 不能当作确认缺陷。修复后计数会变化，应转为正式行为回归并保留历史材料。

旧无 tag/空/未闭合/空首对后有效 tag 四例实际 CROSSSLOT；tagged Activity 生命周期控制排除连接/Cluster 未就绪。purchase seed/control=10、upgrade=3；真实 HSet 成功后注入 deadline，非真实网络代理/kill；未执行真实 Nest/Mongo 背包消费。本次新容量/HA 性能未跑。
