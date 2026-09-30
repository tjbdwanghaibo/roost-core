# Service 第十二轮本机证据与复跑

基线 Core `88c6528675b527a7c30a662ed2e370d964cb13f4`。本目录只有测试覆写源、环境脚本和[结果摘要](RESULTS.json)；原始 `go test -json` 日志在执行机 `D:/whb_s/.tmp/service-review12-evidence/`，未入库，摘要保留各文件 SHA-256、通过/失败/跳过数。覆写源扩展名为 `.go.txt`，生产源码及正式测试没有改动。

## 隔离环境

在 Windows PowerShell、Go 和 Redis 8.8.0 可用时，先检查端口 `16530..16536` 均空闲，再执行 `./Start-Isolated-Redis.ps1`。脚本创建一个 standalone 和 3 主 3 从 Cluster，所有实例仅监听本机，数据目录为 `D:/whb_s/.tmp/service-review12-redis/<port>`，启用 AOF `appendfsync everysec`。脚本的 `D:/whb_s` 与 `C:/Users/tjbdw/scoop/shims` 路径是本次实际执行路径；其他机器需先改成自己的路径。请勿把脚本对准已有实例或共享 Redis。

在 Core 根目录创建一个临时 Go overlay JSON，将以下虚拟 `*_test.go` 绝对路径映射到同名 `.go.txt` 的绝对路径；映射文件本身可放在临时目录。三个 Session 文件同时映射到 `service/session/`，Match 文件映射到 `service/match/`：

| 虚拟文件 | 本目录源文件 |
| --- | --- |
| `service/session/review12_overlay_test.go` | [session_crash_test.go.txt](session_crash_test.go.txt) |
| `service/session/review12_backlog_overlay_test.go` | [session_backlog_test.go.txt](session_backlog_test.go.txt) |
| `service/session/review12_failover_overlay_test.go` | [session_failover_test.go.txt](session_failover_test.go.txt) |
| `service/match/review12_overlay_test.go` | [match_capacity_test.go.txt](match_capacity_test.go.txt) |

Overlay JSON 格式为 `{"Replace":{"<虚拟绝对路径>":"<源绝对路径>"}}`。设 `$env:ROOST_REVIEW_REDIS='127.0.0.1:16530'` 与 `$env:ROOST_REVIEW_CLUSTER='127.0.0.1:16531,127.0.0.1:16532,127.0.0.1:16533,127.0.0.1:16534,127.0.0.1:16535,127.0.0.1:16536'`。`-json` 输出重定向到独立文件后可用 `ConvertFrom-Json` 按 `Action/Test` 统计；不能只看退出码，因为 skip 也退出 0。

```powershell
go test -race -count=2 -timeout=180s -overlay $sessionOverlay -json -run '^TestReview12SessionCrashAfterExternalEffect$' ./service/session > session-crash.jsonl
go test -count=1 -timeout=180s -overlay $sessionOverlay -json -run '^TestReview12SessionRotatingBacklogWithOneFailure$' ./service/session > session-backlog.jsonl
go test -count=1 -timeout=180s -overlay $matchOverlay -json -run '^TestReview12MatchHistoricalCost$' ./service/match > match-capacity.jsonl
go test -count=1 -timeout=180s -overlay $matchOverlay -json -run '^TestReview12MatchHotHistorySoak$' ./service/match > match-soak.jsonl
```

HA 分成两次 `go test` 进程，中间只强杀本轮独占 Cluster 中负责测试 key 的 master。以下前缀、run ID 与本轮通过日志一致。`Failover-Owned-Master.ps1` 先验目录、主从关系和 `WAIT 1`，再定位当前 master 的 Redis 进程并强杀，轮询目标 replica 成为 master、16384 槽可用。执行前核实集群只属于本轮测试；脚本会终止一个 Redis 进程。

```powershell
$env:ROOST_REVIEW12_HA_PREFIX='{review12-ha-b}'
$env:ROOST_REVIEW12_HA_RUN='review12-ha-b-run'
$env:ROOST_REVIEW12_HA_PHASE='seed'
go test -count=1 -timeout=180s -overlay $sessionOverlay -json -run '^TestReview12SessionAcrossReplicaFailover$' ./service/session > session-ha-b-seed.jsonl
./Failover-Owned-Master.ps1 -Prefix $env:ROOST_REVIEW12_HA_PREFIX -RunID $env:ROOST_REVIEW12_HA_RUN
$env:ROOST_REVIEW12_HA_PHASE='verify'
go test -count=1 -timeout=180s -overlay $sessionOverlay -json -run '^TestReview12SessionAcrossReplicaFailover$' ./service/session > session-ha-b-verify.jsonl
```

复跑时应使用**新的空目录/端口或唯一前缀**。首次 `{review12-ha}` fixture 的 TTL 仅 1 分钟，运行延迟跨过 deadline；两份旧 verify 日志分别错断言 `StateOpen` 与无 `ErrRunExpired`，属于测试预期错误。最终源已改为 5 分钟 TTL，并允许真正过期时按 `ErrRunExpired` 判断；`{review12-ha-b}` 在时限内 seed、failover、verify 通过。不能把旧失败计作生产 bug，也不能从最终通过推断无时间边界问题。

已有 Toxiproxy 用例的命令是 `go test -tags integration -count=1 -json -run '^TestToxicRedis' ./redis/driver`；本机缺 Toxiproxy 接线和必要环境变量，**3 test skip / 0 pass**。这些不算物理丢回复/延迟通过。完整网络故障验收仍需 Toxiproxy 代理、API、`ROOST_DATAENGINE_IT=1`、`ROOST_DATAENGINE_IT_TOXIPROXY_URL`、`ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR`，并核对代理只接独占测试 Redis。源码提示的 `scripts/integration/dataengine-env.sh` 在本基线缺失。

本机测试结束后按端口逐一用 `CONFIG GET dir` 核对预期独占目录，只对匹配的仍存活实例执行 `redis-cli -p <port> SHUTDOWN NOSAVE`；已经被强杀的端口应关闭。再检查七个端口均不再监听。不要按端口盲关共享实例，亦不要清理其他测试目录。
