# N03 三项修复：修前 / 修后 / 正式消费

2026-10-04，Windows/amd64、Go1.27.0，基线`e62729ac`，GOWORK=off。NC-08～10已修、声明场景验证，未发版。[运行](../../../review/REVIEW-2026-10-04-noncore-07.md)。

- [formal-red.jsonl](formal-red.jsonl)：上轮反例转正式测试，17叶子4fail/13pass，3根因，均行为失败。
- [formal-green.jsonl](formal-green.jsonl)：最终28正式叶子全绿，含真实Bus.CallReliable方法消费、full queue fallback与停止责任、共同deadline控制。
- [original-green.jsonl](original-green.jsonl)：上轮原文overlay17项全绿，没有改旧附件。
- [regression-events.jsonl](regression-events.jsonl)：六测试包race/126 test pass事件，0fail/skip。最终六包运行后只新增Bus消费者/失败cause测试，单独重跑Bus及vet；事件按最后每包合并，没有将两次Bus相加。
- [summary.json](summary.json)、[source-hashes.json](source-hashes.json)、[graph-coverage.json](graph-coverage.json)：结果、八个变化文件的实际指纹、Tier2图谱补证。

实际执行：`go test -race -count=1 -timeout=60s -json -run '^TestRPCBudget' ./bus ./nats/driver ./servicerpc`（修前）；修后`go test -race -count=1 -timeout=120s -json ./bus ./nats ./nats/driver ./servicerpc ./kit/nats ./worker`及同包vet；随后仅Bus重跑；原overlay用`-overlay <scratch>/original-overlay.json -run '^TestReview6'`。完整临时日志在`D:/whb_s/.tmp/noncore-bugfix-review-20261004-04`。

```powershell
& docs/bugfix/evidence/noncore-bugfix-20261004-04/Run-Verify.ps1 -GoExecutable 'C:/path/to/go.exe' -OutputRoot 'D:/scratch/rpc-fix'
```

脚本是上述命令的便携复跑入口，本轮执行的是等价分步命令。Bus正式消费者用capture传输、NATS用真实callback pool、discovery用协作替身；没有真实broker/etcd、生成服务进程、HA、长期容量或benchmark。正常测试中的panic日志属于被保护的callback/handler控制，不是未处理panic。
