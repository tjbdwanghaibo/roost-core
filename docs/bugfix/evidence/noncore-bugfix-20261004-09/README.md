# NC-26～29 修复证据

基线 `3d3b22c9f8398c032c6a2c7a574197dfe1bf59a2`，四个公开 Mongo 替身 P3 已修、未发版。[运行](../../../review/REVIEW-2026-10-04-noncore-17.md) · [汇总](summary.json) · [源码 hash](source-hashes.csv)。

| 执行 | 叶子 pass / fail / skip | 退出 |
| --- | --- | --- |
| [修改前当前代码](red.jsonl) | 6 / 7 / 0 | [1](red-exits.json) |
| [旧产品 overlay 红重放](red-replay.jsonl) | 6 / 7 / 0 | [1](red-replay-exits.json) |
| [最终十包 race](green.jsonl) | 320 / 0 / 0；28 个新增正式叶子 | [test/vet 均 0](green-exits.json) |
| [正式 DAO 生成的独立 module 消费者](consumer.jsonl) | 2 / 0 / 0 | [test/vet 均 0](consumer-exits.json) |
| [首次受影响消费者](initial-consumer-fixture.jsonl) | 672 / 2 / 17 | [test 1、vet 0](initial-consumer-fixture-exits.json) |
| [最终受影响消费者](affected-consumers.jsonl) | 674 / 0 / 17 | [test/vet 均 0](affected-exits.json) |

首次两项是 `stage_effect_race_test` 依赖共享可见性/全库回滚的夹具适配问题，不登记新生产 RR。更新为明确 ErrDuplicateKey 注入并断言并发提交存活，原分类/回滚/幂等承诺保留。调试期间一次测试返回类型拼写错误已更正，不计产品红证据；本地 scratch 保留该编译输出。最终 17 个环境 skip 单列，不当通过。

```powershell
./Run-Verify.ps1 -GoExecutable <go.exe> -Mode Red -RepoRoot <repo> -OutputRoot <new-dir>
./Run-Verify.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -Mode Green -RepoRoot <repo> -OutputRoot <new-dir>
./Run-Affected.ps1 -GoExecutable <go.exe> -RepoRoot <repo> -OutputRoot <new-dir>
```

[Run-Verify](Run-Verify.ps1) Red 从 git 恢复基线 mongotest.go，overlay 删除新 transaction.go 和仅新实现可编译的两个追加测试，运行原 13 个正式场景；真实红是语义断言，不是编译/超时/race。Green `GOWORK=off`，十包 race/vet 后运行[DAO CLI 独立消费者](Run-Consumer.ps1)，不更改仓库模块依赖。[Run-Affected](Run-Affected.ps1)仅验证现有 DataEngine/Kit/Service 消费，不重审或改三大核心产品。

Green/Review 各使用 loopback 临时端口、唯一 WorkingDirectory、自己的 Redis 8.8.0；[green 实例退出](green-cleanup.json)，[Redis 版本](redis-version.log)。只停止自身进程；不需要真实 Mongo。[生成输出](consumer-generate.log)，vet 空输出表示无诊断。本机 MSYS Redis 需沙箱外执行，但不操作现有服务。

本轮事务是集合粒度私有快照，不是 Mongo 服务端替代认证；索引 O(n²) 存量检查与快照复制无 benchmark。真实 Mongo、Drop/namespace、完整索引/路径、未知 commit 与 HA 留项。[接续 Redis 审查](../../../review/evidence/noncore-review-20261004-18/README.md)独立统计，不混入修复通过数。
