# NC-16～20 修复证据与复跑

基线 `08d18be9608598c042a58b3658aa4735d88f8c7b`，Windows/Go1.27.0/GOWORK=off。当前七Go文件摘要见[source-hashes.csv](source-hashes.csv)，统计见[summary.json](summary.json)。

| 执行 | 证据与结果 |
| --- | --- |
| 修前正式16叶子 | [red.jsonl](red.jsonl)、[退出码](red-exits.json)：10fail/6控制，五根因；旧产品实际未修改时运行 |
| 原入口复跑 | [red-replay.jsonl](red-replay.jsonl)、[退出码](red-replay-exits.json)：同样10fail/6控制；旧产品及旧fake测试overlay，不改工作树 |
| 最终正式回归 | [green.jsonl](green.jsonl)、[退出码](green-exits.json)、[vet](vet.log)：十包race/vet，七测试包252叶子pass/273 pass事件、0fail/skip；四新测试文件共41叶子 |
| 正式生成消费 | [consumer.jsonl](consumer.jsonl)、[退出码](consumer-exits.json)、[生成日志](consumer-generate.log)、[vet](consumer-vet.log)：当前DAO CLI生成独立module，生成ref-hmap DAO实际Set→nil父Patch→Get与已有路径更新，一个消费者pass |
| 中间夹具错误 | [vet文本](interim-fixture-vet.log)、[退出码](interim-fixture-exits.json)：故意重复json tag触发vet，改用redisdao重复tag仍验证同一字段碰撞；不是产品失败，不删样本 |

[red清理](red-cleanup.json)、[red复跑清理](red-replay-cleanup.json)、[green清理](green-cleanup.json)均exited=true。最终专属Redis8.8.0 PID22632、port3200，无持久化；正式Redis七集成已包含本次全包日志，不重复累计。

```powershell
& ./docs/bugfix/evidence/noncore-bugfix-20261004-07/Run-Verify.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -Mode Red -OutputRoot <new-scratch>/red
& ./docs/bugfix/evidence/noncore-bugfix-20261004-07/Run-Verify.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -Mode Green -OutputRoot <new-scratch>/green
```

[Run-Verify](Run-Verify.ps1)建立独占loopback/无持久化Redis，finally只关闭本次process。Red使用[原Ref测试](red-ref-hmap-test.go.txt)、[原分页测试](red-pagination-test.go.txt)和git show基线；Green跑当前正式测试/vet后调用[Run-Consumer](Run-Consumer.ps1)，复制[定义](consumer-definition.go.txt)、[类型](consumer-record.go.txt)、[消费测试](consumer-test.go.txt)，构建codegen/cmd/dao并生成执行。需要现有Go模块缓存，不安装依赖环境、不改变仓库go.mod或go.work、不提交发布。Red脚本exit0只代表证据采集完，必须检查test_exit与行为失败，不能把编译错误当红。独立Run-Consumer需ROOST_REDIS_TEST_ADDR指向自有隔离实例。

未执行真实Mongo、Cluster/HA、长期容量或benchmark。新审查故障见[独立证据](../../../review/evidence/noncore-review-20261004-14/README.md)，不混为本轮五项修复失败。
