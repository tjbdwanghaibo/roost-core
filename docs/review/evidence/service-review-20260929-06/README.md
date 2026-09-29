# Service 第六轮证据入口

源码 4b0837d70b6d62d84b7b3ebf9ab5e5b2b6a819f0。本轮直接维护正式行为回归，不生成第二套 overlay。

- [测试前后摘要与源文件 hash](../../../bugfix/evidence/service-bugfix-20260929-05/RESULTS.json)：修前旧 overlay 12 叶子失败；16 包/830 事件完整绿测；最终两模式各 27 叶子/32 事件全绿。
- [100 路径清单](inventory.csv)：6 个生产路径本轮修改/审查，94 个 blob 不变复用前轮；这是范围核算，不是行覆盖率。
- [最终图谱 coverage](COVERAGE.json)：08:12:07Z full generation、11 改动及回归路径 metadata_changed、两个 scope 无记录 gap；实际源码补证。

准备自己的可丢弃 Redis，本轮实际端口 16396，不要连接生产：
```powershell
$env:ROOST_REVIEW_REDIS='127.0.0.1:16396'
$env:ROOST_BUGFIX5_BACKEND='memory'
go test -race -count=1 -run '^TestBugfix5' ./service/match ./kit/service/rank ./kit/service/chat ./kit/service/global/activity
$env:ROOST_BUGFIX5_BACKEND='redis'
go test -race -count=1 -run '^TestBugfix5' ./service/match ./kit/service/rank ./kit/service/chat ./kit/service/global/activity
```
完整套件命令、生成消费者及实际验证顺序见[第五批 bugfix](../../../bugfix/SERVICE-BUGFIX-2026-09-29-05.md)。本轮不提交原始大日志/临时工程/缓存，不用本机对象重建代替真实多进程强杀证据。
