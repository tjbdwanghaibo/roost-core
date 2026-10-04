# N03 第四批可复跑证据

2026-10-04，Windows/amd64、Go1.27.0；`c4aa1e7d`加本批请求修复工作树对应`49796514`，N03产品未修改。[运行/范围](../../REVIEW-2026-10-04-noncore-06.md) · [NC-08～10未修](../../../bug/REVIEW-2026-10-04-noncore-06.md) · [摘要](summary.json) · [当前源码blob/读取范围](source-manifest.json)。

- bus_review_test.go.txt：4种JS响应、3个拒绝/空消息控制，共7叶子/1失败；无handler实际裸code/reason导致client version0，正常/业务error/panic正确version1。
- nats_driver_review_test.go.txt：Assembly已取消ctx/已进入阻塞callback的预算反例1失败；真实pool16terminal竞争只完成1次、空闲关闭2控制。
- servicerpc_review_test.go.txt：discovery无deadline和实等150ms父预算2失败；保留原deadline、空/无效候选拒绝、指定SID成功及业务error5控制。

[初始review](initial-review.jsonl)16项/3失败；追加等待反例仅重跑ServiceRPC后，[最终review](review.jsonl)合并各包最后一次执行，17项/4失败/13通过、3根因。[退出](exits.json)review=1、race/vet=0，[既有回归事件](regression-events.jsonl)六包93 pass、0fail/skip。失败均为行为断言，不是编译/环境失败；声明的scope与fixture不等于全域功能覆盖。

```powershell
& docs/review/evidence/noncore-review-20261004-06/Run-Review.ps1 -GoExecutable 'C:/path/to/go.exe' -OutputRoot 'D:/scratch/n03-review'
```

脚本以overlay追加临时test路径，不写产品测试；依赖已有包fixture/cachedmodules，GOWORK=off。review预期exit1，既有race/vet必须0。JS使用capture backend，NATS只用正式callback pool，etcd发现为协作替身；原始日志在`D:/whb_s/.tmp/noncore-review-20261004-06`。只释放自己创建的pool/门闩，所有测试正常结束；没有操作生产资源或冒认真实多机验收。

[图谱coverage](graph-coverage.json)为roost-core Tier2，代际09-30；metadata_changed/newprobe not_tracked已以当前源文补证，无记录gap不证明完整。source-manifest中24个Bus/NATS/ServiceRPC源文件与etcd接口全读（N03 25/39），KitNats另1；etcd.driver.Discover和worker.Pool范围另标，不计整文件完成。相邻核心域源码没有修改，未重建/停止共享索引。
