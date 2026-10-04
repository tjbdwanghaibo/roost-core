# NC-30 修复证据与复跑

日期 2026-10-04，源码基线 e7027a65，Go1.27.0/Windows，独占 loopback Redis8.8.0；只关闭本轮实例，无凭据/二进制/数据库文件入 Git。

[summary.json](summary.json) 按叶子去除父 test，和 test pass 事件分列：

- [red](red.jsonl)/[red-replay](red-replay.jsonl)：各2 fail/4控制，[退出1](red-exits.json)/[复跑退出1](red-replay-exits.json)。真实 child_remaining_after_delete=1，服务端数据状态失败，不是 harness 超时。
- [green](green.jsonl)：221叶子 pass/0fail/0skip，243 test pass事件，5测试包；六包命令中 KitRedis 无测试。含12新增正式叶子，[race/vet退出0](green-exits.json)。
- [consumer](consumer.jsonl)：7生成 RestorePersisted 迁移 +4生成 Redis/Cached 冲突场景，11叶子/13事件，[退出0](consumer-exits.json)，[生成日志](consumer-generate.log)。
- [review16](../../../review/evidence/noncore-review-20261004-19/README.md) 单列，不把三个观察夹具当快照/CAS保证。

所有 [red](red-cleanup.json)、[replay](red-replay-cleanup.json)、[green](green-cleanup.json)、[consumer](consumer-cleanup.json) owned Redis exited=true。vet 无诊断，PowerShell 无管道输出时未创建文件，归档以零字节 [vet](vet.log)/[consumer-vet](consumer-vet.log) 表示原始静默；退出码独立保存。

## 复跑入口

[Run-Verify.ps1](Run-Verify.ps1) 接受 GoExecutable/RedisExecutable/RedisCliExecutable/RepoRoot/OutputRoot；OutputRoot 必须全新。GOWORK=off，各 Mode 使用自建 loopback Redis，不执行用户环境脚本或关停其他实例。

```powershell
./Run-Verify.ps1 -GoExecutable <go.exe> -RedisExecutable <redis-server.exe> -RedisCliExecutable <redis-cli.exe> -Mode Green -OutputRoot <new-green-directory>
```

Green：`go test -race -count=1 -timeout=120s -json ./cache ./redis ./redis/driver ./migration ./codegen/internal/dao ./kit/redis`，再 vet 同范围。Red：Git show 恢复 e7027a65 的 ref_hmap.go/ref_hmap_test.go 到 output，Go overlay 加 [兼容旧 API 的反例](red_test.go.txt)，只跑原六场景，预期退出1。原基线没有新错误符号，旧兼容探针使用 ErrConflictingWrite 判别，但真实失败是旧 nil 和孤儿，未把此旧符号当新语义；正式修后回归检查 ErrRefHMapRegistryChanged。Review：overlay 运行16具名新场景。

Consumer：调用 [Run-Consumer.ps1](Run-Consumer.ps1)，正式编译 codegen/cmd/dao，并从 [definition](consumer-definition.go.txt) 生成独立模块（GOWORK=off，replace 只在 scratch 指向源码），以 [类型](consumer-record.go.txt)/[消费测试](consumer-test.go.txt) 执行11场景并 vet。不修改仓库 go.mod/go.work 或生成模板。[生成文件摘要](generated-hashes.csv)。若使用本机离线缓存，需要先在调用 shell 配置自己的 Go cache/proxy；脚本不安装依赖。

[18材料路径摘要](source-hashes.csv) 是修后树，SourceHead 表示审查起点，不是假装它是未修改 Git blob；旧红源码来自固定 Git baseline。未复跑真实 Mongo/Cluster/HA/网络代理/长期容量与性能；上轮17环境skip保持留项。脚本跑红失败与绿色执行失败分开；不自动提交、推送或发版。

提交前整合到c888223f后按更新后的skill跑根包：`GOWORK=off go test -count=1 -timeout=120s -json .`，[事件](root.jsonl)/[退出0](root-exits.json)。相关生产/hash未被上游改动；六个上游新RR未由原绿色门禁关闭，见[最新分流](../../../review/REVIEW-2026-10-04-noncore-19.md#提交前上游审计与下一批待修)。

**最终同步**到d49f02f1，RR-02～06已由另一线修复，上述18路径摘要与221绿色是更早测试阶段，不能视为最新树。合并后相关241叶子、12本项正式、16当前review、11生成消费、根包12、mongotest115叶子通过，分别保留在[merged](merged/README.md)；RR-07仍待修。Review脚本默认使用合并后的miss夹具，原零值观察仅作历史证据。无实际Mongo/etcd/Cluster本机追加验收。
