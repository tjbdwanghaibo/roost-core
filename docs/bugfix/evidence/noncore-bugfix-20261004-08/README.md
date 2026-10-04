# NC-21～25修复证据

起点`ce90e90d43cfaf7b71d3487a84ea2f3d32f97d0b`，GOWORK=off、Go1.27.0，真实Redis8.8.0 MSYS独占loopback实例，结束均自动关闭；没有真实Mongo/Cluster/HA/性能对照。源码对照[source-hashes.csv](source-hashes.csv)，[运行](../../../review/REVIEW-2026-10-04-noncore-15.md)。

| 运行 | 叶子 pass/fail/skip | 说明 |
| --- | --- | --- |
| [修前](red.jsonl) / [旧源码重放](red-replay.jsonl) | 两次均6/6/0 | 原12叶子5根因，先红后改码；正常控制不删 |
| [最终绿](green.jsonl) | 292/0/0 | 十包race，7测试包317 pass事件，3包无测试；40新增正式叶子通过 |
| [生成消费](consumer.jsonl) | 2/0/0 | 真正DAO CLI→独立module→生成ref-hmap DAO→真实Redis；Patch及未知回复保持v3，race/vet |
| [受影响消费者](affected-consumers.jsonl) | 674/0/17 | DataEngine/Kit与Service既有测试，14测试包728 pass事件；17环境skip具名保留 |

确切统计来自[summary.json](summary.json)，不把父测试事件当叶子。原始退出：[red](red-exits.json)、[replay](red-replay-exits.json)、[green](green-exits.json)、[consumer](consumer-exits.json)、[affected](affected-exits.json)；green/vet均0，red为1且真正断言失败。空[vet.log](vet.log)、[consumer-vet.log](consumer-vet.log)、[affected-vet.log](affected-vet.log)需结合exit，不凭空日志宣布成功。Redis释放：[red](red-cleanup.json)、[replay](red-replay-cleanup.json)、[green](green-cleanup.json)。

追加边界初版把Set写前Get的Pipeline误算降级，四断言失败；[初版事件](initial-counter-fixture.jsonl)/[退出](initial-counter-fixture-exits.json)保留，纠正为Eval后计数再通过。不是新产品缺陷或抹掉失败。第一次沙箱Redis命名对象权限失败未运行测试；本机升权后独占Redis完成实测。[redis-version](redis-version.log)。

## 复跑

显式指定当前Go与Redis executable，每次选新OutputRoot；脚本不包含个人绝对路径默认值，不写凭据/发布/生产迁移。

```powershell
./Run-Verify.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -Mode Red -RepoRoot <repo> -OutputRoot <new-red-dir>
./Run-Verify.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -Mode Green -RepoRoot <repo> -OutputRoot <new-green-dir>
```

[Run-Verify](Run-Verify.ps1) Red以git show恢复ce90产品源码overlay，选择TestRefHMapUnknownWrite/TestMongoIdentityPromises；Green跑十包race/vet并调用[Run-Consumer](Run-Consumer.ps1)。consumer定义/记录/test三份源码在本目录，生成日志[consumer-generate](consumer-generate.log)。无Go工具链下载或仓库go.mod改动。受影响消费者命令：

```text
go test -race -count=1 -timeout=180s -json ./dataengine/engine ./kit/dataengine ./kit/service/...
go vet ./dataengine/engine ./kit/dataengine ./kit/service/...
```

[17具名skip](skipped-tests.csv)主要缺ROOST_REDIS_TEST_ADDR/ROOST_REVIEW_CLUSTER，不能替代真实Mongo/Cluster等集成；正式Sync/Remote三进程shell矩阵未执行，不由当前Lua/替身修复外推。生产仅cache/ref_hmap.go改变行为，其他四项是测试替身。
