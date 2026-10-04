# NC-11/12 红绿与生命周期证据

基线 `3560a19b5c2b2fbeb936bf1bb6a9c1fa0be1f956`，Windows/Go1.27.0，GOWORK=off。只关闭测试独占的 client/listener/server；没有真实etcd集群。

| 文件 | 实际内容 |
| --- | --- |
| [red.jsonl](red.jsonl)、[red-exit.json](red-exit.json) | 原8叶子/独立项，2行为失败、6控制通过；新产品修改前执行 |
| [red-replay.jsonl](red-replay.jsonl)、[red-replay-exits.json](red-replay-exits.json) | 归档Run-Verify的Red入口实际复跑：2行为失败、6控制通过，不改工作树 |
| [green-formal.jsonl](green-formal.jsonl) | 第一次中间验证：etcd控制通过，driver因旧guards夹具factory签名未同步而编译失败；不是行为红测 |
| [intermediate-deadline.jsonl](intermediate-deadline.jsonl) | 中间实现能取消Grant，但未保留DeadlineExceeded，补修后关闭 |
| [regression-final.jsonl](regression-final.jsonl)、[final-exits.json](final-exits.json)、[vet-final.log](vet-final.log) | 最终两测试包race/三包vet均0；61 test pass事件、59叶子/独立项、0fail/skip，KitEtcd无测试 |
| [formal-results.json](formal-results.json) | 最终15个新增正式生命周期叶子/独立项全部通过 |
| [original-overlay-green.jsonl](original-overlay-green.jsonl) | 原审查8项通过；15正式项包括对应8项，不能相加作为覆盖 |
| [summary.json](summary.json)、[source-hashes.csv](source-hashes.csv) | 父测试不重复计叶子的统计、最终7个改动Go文件SHA256 |

## 复跑

从仓库根，设置自己的可用Go、模块缓存及临时目录，不改go.mod：

```powershell
& ./docs/bugfix/evidence/noncore-bugfix-20261004-05/Run-Verify.ps1 -GoExecutable <go.exe> -Mode Red -OutputRoot <new-scratch>/red
& ./docs/bugfix/evidence/noncore-bugfix-20261004-05/Run-Verify.ps1 -GoExecutable <go.exe> -Mode Green -OutputRoot <new-scratch>/green
```

[Run-Verify.ps1](Run-Verify.ps1) 的Red使用git show读取原SHA及overlay，不改工作树；[red-driver-test.go.txt](red-driver-test.go.txt)和[red-etcd-test.go.txt](red-etcd-test.go.txt)保存修前正式测试。预期2个行为失败，必须确认Grant/watcher入场，不能把build失败或测试超时当复现。Green执行当前正式包及vet。脚本不会安装环境、更新模块、提交、推送或发布。

原overlay还可按[上轮入口](../../../review/evidence/noncore-review-20261004-08/README.md)复跑。本次原overlay在最终caller原因补修前通过；最终正式对应反例覆盖该补修，未把原overlay日志冒称最后改动后的重跑。

真实Grant仅本机gRPC LeaseServer；已成功的session/Revoke所有权控制使用可观察context的fixture。正常session.Close的60秒TTL级等待、服务端未知Grant/失败清理、集群恢复与容量没有验证，不以这些日志关闭外部矩阵。
