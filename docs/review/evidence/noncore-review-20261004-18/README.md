# N04 第五批审查证据

源码基线 `3d3b22c9`，同树[NC-26～29 已修](../../../bugfix/evidence/noncore-bugfix-20261004-09/README.md)。[最终 14 叶子 pass、0 fail/skip](review.jsonl) · [退出 0](exits.json) · [独占 Redis 已退出](cleanup.json) · [版本](redis-version.log)。[本轮结论](../../REVIEW-2026-10-04-noncore-18.md)14项本身没有新RR；追加wanted另确认P2，不等价于Redis全域通过。

```powershell
../../../bugfix/evidence/noncore-bugfix-20261004-09/Run-Verify.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -Mode Review -RepoRoot <repo> -OutputRoot <new-dir>
```

[review18_test.go.txt](review18_test.go.txt)通过 overlay 加入 redis/driver，不留临时正式测试。14 个场景：3 个真实命令执行后丢回复注入、2 个旧 token、5 个自动续租生命周期和4 个订阅退出/消息映射。真实 Redis 执行 Lua；错误注入不冒称真实弱网。全队列先观察 len=cap，再关闭；healthy 在等待原 TTL 的两倍后检查 key 仍有效。NUMSUB 确认订阅就绪后才测试投递。

[首版同样 14 pass](initial-review.jsonl)和[首版清理](initial-cleanup.json)保留；最终探针补强“队列确满”“跨原 TTL”“消息 channel 映射”，未把首版通过代替更强场景。续租丢锁检查按有界观察等待真实事件；ticker 用作状态观察，不靠任意 sleep 制造并发。

[inventory.csv](inventory.csv)在原固定 40 文件基础增加真实事务实现文件，当前 **41** 个 Go 产品候选，累计源文 41/41：39 相同 hash 复用，mongotest 读当前 diff/相关源码，新 transaction.go 全读。Blob/SourceHead 为基线，WorkingSHA256 为修后当前文件；新文件 not_in_baseline，明确两个版本口径。[50路径N04当前hash](source-hashes.csv)含必要消费者与锁测试，不把测试当产品覆盖分母。

[coverage](coverage.json) Tier 2、project roost-core、generation `2026-09-30T11:58:14Z`；图谱 stale、metadata_changed/not_tracked 用当前源码和 diff 补证。search_graph 精确文件已完整分页，watch trace 两方向无截断；generic receiver/heuristic 跨包误边不用作调用证明。无记录 gap 仅是 best-effort 信号，不声称索引已更新或全业务无 bug。

真实 Mongo/Cluster/HA、toxiproxy 弱网、重连、租期时钟、订阅总体关闭、长期容量/性能仍缺证据；17 个消费者 skip 不混成通过。下一轮 RefHMap schema/并发/未知恢复，再正式迁移消费者。[进度](../../PROGRESS.md) · [学习](../../IMPLEMENTATION-REDIS-LOCK-RENEWAL-AND-PUBSUB-LIFETIME.md) · [留项](../../../bug/CARRYOVER.md)。

## 新增wanted

提交前fetch到cfe878fe，新增W-2026-10-04-01→[RR-20261004-01](../../../bug/RR-20261004-01.md)。[wanted_test.go.txt](wanted_test.go.txt) overlay加入remoteentity，实际Lua执行后注入DeadlineExceeded；先验服务端owner/TTL与本地未准入，再立即重取。[wanted.jsonl](wanted.jsonl)4叶子2fail/2控制、[退出1](wanted-exits.json)、[实例退出](wanted-cleanup.json)、[范围coverage](wanted-coverage.json)。这是有意的审查反例，不加入普通包绿色门禁。首次草稿因HGet/TTL接口类型误用未编译，本地记录保留；不计作行为红。

复跑：前述Run-Verify.ps1使用 `-Mode Wanted`，同样传Go/Redis/RedisCli/RepoRoot与新OutputRoot，`-run '^TestWantedAcquireUnknown$' ./remoteentity`。两类丢回复失败，正常取得/执行前错误控制通过；用唯一前缀并删除自身两个键，无生产Remote改码。修前追加5路径hash单列，验收9路径另存；Remote不纳入N04产品分母41，不声称整体审查完成。

## 独立验收上游修复

后续fetch取得3bb901fb/5d386146，上游同一RR已修；本轮保留其产品实现和主记录，追加独立验收。以下代码状态为5d386146产品加本轮Mongo替身修复，**RR-20261004-01现已修、声明场景验收，未发版**；上节未修是发现时点。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 同一4场景真实Redis探针 | 4pass/0fail/skip | [事件](wanted-acceptance.jsonl) · [退出0](wanted-acceptance-exits.json) · [清理](wanted-acceptance-cleanup.json) |
| 7取锁未知 + 6旧释放未知正式回归 | 13pass/0fail/skip，race/vet0 | [事件](wanted-official.jsonl) · [退出](wanted-official-exits.json) · [vet](wanted-official-vet.log) |
| 3条现有真实Redis集成 | 3pass/0fail/skip，race0 | [事件](wanted-integration.jsonl) · [退出](wanted-integration-exits.json) · [清理](wanted-integration-cleanup.json) |

复跑本轮探针：Run-Verify使用 `-Mode Wanted`，传当前RepoRoot是绿色验收；传cfe878fe的独立checkout可重现原两红，不替换共享工作树。fixture从脚本旁证据目录读取。正式13条：`go test -race -count=1 -timeout=120s -run '^TestVersionedLock(Reacquires|LostAcquire|UnsentAcquire|LateAcquire|UnknownRelease|UnlockContextExpiry|LateOldTouch)' ./remoteentity`，另 `go vet ./remoteentity`。

真实3条：Run-Verify使用 `-Mode RemoteIntegration`，只给自己Redis地址设置所需IT变量并恢复，`-tags integration -run '^TestRealVersionedLock(ReacquiresAfterAcquireReplyLost|ReacquiresAcrossUnknownChain|LateAcquireScript)$'`；不启动Mongo/NATS。三种运行实例均关闭。未验authority完整故障/三资源生成消费者/HA/长期性能。

版本口径：[source-hashes.csv](source-hashes.csv)50个N04与必要消费者当前路径；[wanted-red-source-hashes.csv](wanted-red-source-hashes.csv)5个cfe878fe修前证据路径；[wanted-acceptance-source-hashes.csv](wanted-acceptance-source-hashes.csv)9个5d386146当前验收路径，[验收coverage](wanted-acceptance-coverage.json)。40→41产品候选分母未再扩大，Remote只做这次bounded wanted验收。
