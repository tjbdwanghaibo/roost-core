# NC-13～15 准入修复证据

基线`1502f97372139a238e73850ff7337b9454146f9e`，Windows/Go1.27.0/GOWORK=off。产品六文件、两新正式测试及旧期待调整，当前9文件摘要见[source-hashes.csv](source-hashes.csv)。完整统计见[summary.json](summary.json)。

| 文件 | 结果 |
| --- | --- |
| [red.jsonl](red.jsonl)、[red-exits.json](red-exits.json) | 产品修改前正式21叶子9fail/12控制；3根因 |
| [red-replay.jsonl](red-replay.jsonl)、[red-replay-exits.json](red-replay-exits.json) | Run-Verify Red实际复跑同样9fail/12控制 |
| [green.jsonl](green.jsonl)、[green-exits.json](green-exits.json)、[vet.log](vet.log) | 五包race/vet exit0，186 pass事件/170叶子、0fail/7skip；41正式准入叶子通过；KitRedis无测试 |
| [consumer.jsonl](consumer.jsonl)、[consumer-exits.json](consumer-exits.json)、[consumer-generate.log](consumer-generate.log)、[consumer-vet.log](consumer-vet.log) | 正式DAO CLI在独立module生成，两个消费者race/vet通过；归档入口也复跑通过 |
| [consumer-replay.jsonl](consumer-replay.jsonl)、[consumer-replay-exits.json](consumer-replay-exits.json) | Run-Consumer实际复跑：两个消费者通过、test/vet exit0；不叠成四项业务覆盖 |
| [实际Redis补验](../../../review/evidence/noncore-review-20261004-12/README.md) | 原七Redis集成全绿、Raw/Hash/JSON旧/新写六项全绿；新RefHMap失败另计 |

原21包含旧写/较新写、fatal Get/Delete/Set、Layered/ReadThrough三种顺序场景。41最终项追加等版本与策略/读回错误控制，不把它们冒称修前21，也不累加重复覆盖。绿色日志之后仅更正新正式测试头注释，无产品行为变更。原七skip为未设置ROOST_REDIS_TEST_ADDR，后补独占实测另存，不覆盖历史skip。

## 复跑

从模块根设置可用Go/模块缓存与任务临时目录：

```powershell
& ./docs/bugfix/evidence/noncore-bugfix-20261004-06/Run-Verify.ps1 -GoExecutable <go.exe> -Mode Red -OutputRoot <new-scratch>/red
& ./docs/bugfix/evidence/noncore-bugfix-20261004-06/Run-Verify.ps1 -GoExecutable <go.exe> -Mode Green -OutputRoot <new-scratch>/green
& ./docs/bugfix/evidence/noncore-bugfix-20261004-06/Run-Consumer.ps1 -GoExecutable <go.exe> -OutputRoot <new-scratch>/consumer
```

[Run-Verify](Run-Verify.ps1) Red只git show原六产品文件与[原测试](red-original-test.go.txt)overlay，指定原三测试集合；不改工作树/模块。Green跑当前正式集合及受影响包；必须看行为断言，不将build失败/超时当红。Run-Consumer仅在新目录复制[定义](consumer-definition.go.txt)、[业务类型](consumer-record.go.txt)、[测试](consumer-test.go.txt)，构建当前codegen/cmd/dao并生成/执行；本地replace不修改仓库go.mod，无go.work。脚本不会安装环境、提交、推送或发布。

首次为查询CLI帮助另构建了roost.exe，但实际生成用dao.exe；查询不存在redis_test.go后定位parse_test.go，不属于产品失败。二进制/缓存/本机配置未归档。真实Mongo/Cluster/HA、未知网络结果、长期容量与性能未执行；空vet日志表示执行无输出，退出码已保存。
