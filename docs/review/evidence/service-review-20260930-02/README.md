# Service 第十三轮：网络故障、HA、积压与热点复跑

基线 Core `d91d8a30d97f2bef8f0b00d449187fb741c2f95b`。[结果摘要](RESULTS.json)列出每个原始 `go test -json` 文件的 SHA-256 与 test pass/fail/skip；原始日志在执行机 `D:/whb_s/.tmp/review13-tools/`，未入库。这里的 `.go.txt` 由 Go overlay 映射为虚拟测试文件，生产源码与正式测试均未修改。源码最终只做过一次 `gofmt` 空白整理；20 秒 Match 探索样本在加入可配置持续时间之前运行，最终 60 秒样本对应本目录源码。

## 独占环境

从 Core 根目录运行[本轮启动脚本](Start-Isolated-Redis.ps1)，会检查端口空闲，启动 standalone `127.0.0.1:16630`、3 主 3 从 Cluster `:16631..16636`，AOF everysec、数据目录 `D:/whb_s/.tmp/service-review13-redis/<port>`。脚本硬编码了本次 Windows/Scoop 路径，复跑机器需调整；绝不可指向已有或共享 Redis。所有实例只监听本机。本轮脚本创建后检查 `cluster_state:ok`、16384 槽、6 节点。

Toxiproxy `v2.12.0` 按[官方发布模块](https://github.com/Shopify/toxiproxy/releases/tag/v2.12.0)在隔离临时目录执行 `go install github.com/Shopify/toxiproxy/v2/cmd/server@v2.12.0` 构建，`GOBIN=D:/whb_s/.tmp/review13-tools`，实际产物为 `server.exe`。用 `Start-Process -WindowStyle Hidden -PassThru` 启动 `server.exe -host 127.0.0.1 -port 18475`，保存 PID；先检查 API `:18475` 与代理 `:26380` 端口空闲，再向 API `POST /proxies` 提交 `{"name":"redis","listen":"127.0.0.1:26380","upstream":"127.0.0.1:16630","enabled":true}`。Toxiproxy API 拒绝浏览器式 User-Agent；PowerShell `Invoke-RestMethod` 需 `-UserAgent 'roost-review/1'`，Go `http.Client` 默认 UA 可用。`redis-cli -p 26380 PING` 必须为 `PONG`。

本轮默认 Go 构建缓存路径被沙箱拒绝，首次测试 `build-fail`、未进入测试；将 `GOCACHE` 和 `GOTMPDIR` 设在 `D:/whb_s/.tmp/review13-tools/` 后全部测试正常编译。不要把该环境失败计入框架行为错误。下列命令均在 Core 根目录、PowerShell 环境执行，`$sessionOverlay`/`$matchOverlay` 是用户创建的临时 JSON 文件路径：

```powershell
$env:ROOST_REVIEW_REDIS='127.0.0.1:16630'
$env:ROOST_REVIEW_CLUSTER='127.0.0.1:16631,127.0.0.1:16632,127.0.0.1:16633,127.0.0.1:16634,127.0.0.1:16635,127.0.0.1:16636'
$env:ROOST_REVIEW13_TOXI_API='http://127.0.0.1:18475'
$env:ROOST_REVIEW13_TOXI_REDIS='127.0.0.1:26380'
$env:ROOST_DATAENGINE_IT='1'
$env:ROOST_IT_TOXIPROXY='1'
$env:ROOST_DATAENGINE_IT_TOXIPROXY_URL=$env:ROOST_REVIEW13_TOXI_API
$env:ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR=$env:ROOST_REVIEW13_TOXI_REDIS
go test -tags integration -race -count=2 -timeout=180s -json -run '^TestToxicRedis' ./redis/driver > toxiproxy-tests.jsonl
```

按 `{"Replace":{"<Core绝对路径>/service/session/review13_toxic_overlay_test.go":"<本目录绝对路径>/session_toxic_test.go.txt"}}` 建立 Session overlay；Match overlay 的虚拟路径为 `service/match/review13_match_toxic_overlay_test.go`，对应本目录 `match_toxic_test.go.txt`。其他 `.go.txt` 依下表同理映射。**共享同一个代理的毒性测试按顺序运行**，每个测试自行 `/reset`，不可并行。

| 虚拟路径（Core 根目录内） | 源文件 | 执行命令中的 `-run` |
| --- | --- | --- |
| `service/session/review13_toxic_overlay_test.go` | [session_toxic_test.go.txt](session_toxic_test.go.txt) | `^TestReview13SessionLostAttachReplyReconciles$` |
| `service/match/review13_match_toxic_overlay_test.go` | [match_toxic_test.go.txt](match_toxic_test.go.txt) | `^TestReview13MatchLostEnqueueReplyReplaysOneTicket$` |
| `service/session/review13_ha_overlay_test.go` | [session_multiowner_ha_test.go.txt](session_multiowner_ha_test.go.txt) | `^TestReview13SessionSixOwnersAcrossNoWaitFailover$` |
| `service/session/review13_backlog_overlay_test.go` | [session_durable_backlog_test.go.txt](session_durable_backlog_test.go.txt) | `^TestReview13SessionBacklogCursorAcrossProcessRestart$` |
| `service/match/review13_hot_overlay_test.go` | [match_concurrent_test.go.txt](match_concurrent_test.go.txt) | `^TestReview13MatchFourClientHotSoak$` |

Toxic Session/Match 分别用 `go test -count=1 -timeout=90s -overlay $sessionOverlay -json -run '<对应正则>' ./service/session` 和 `... $matchOverlay ... ./service/match`。它们的测试侧 RedisClient 包装器只在 CAS `Eval` 前调用 Toxiproxy API：第一轮 Redis 读已完成，真正写入走真实代理，回复被下行 timeout 吞掉，再由直连 Redis 核对已提交状态和业务重放。

HA 与 backlog 必须分阶段、分 Go 进程执行：

```powershell
$env:ROOST_REVIEW13_HA_PHASE='seed'
go test -count=1 -timeout=120s -overlay $sessionHAOverlay -json -run '^TestReview13SessionSixOwnersAcrossNoWaitFailover$' ./service/session > ha-seed.jsonl
./Failover-No-Wait-Owned-Master.ps1
$env:ROOST_REVIEW13_HA_PHASE='verify'
go test -count=1 -timeout=120s -overlay $sessionHAOverlay -json -run '^TestReview13SessionSixOwnersAcrossNoWaitFailover$' ./service/session > ha-verify.jsonl

$env:ROOST_REVIEW13_BACKLOG_PREFIX='{review13-backlog-unique}'
foreach ($phase in @('seed','first','resume')) {
  $env:ROOST_REVIEW13_BACKLOG_PHASE=$phase
  go test -count=1 -timeout=120s -overlay $sessionBacklogOverlay -json -run '^TestReview13SessionBacklogCursorAcrossProcessRestart$' ./service/session > "backlog-$phase.jsonl"
}

$env:ROOST_REVIEW13_SOAK_SECONDS='60'
go test -count=1 -timeout=180s -overlay $matchHotOverlay -json -run '^TestReview13MatchFourClientHotSoak$' ./service/match > match-hot-60s.jsonl
```

[无 WAIT 故障脚本](Failover-No-Wait-Owned-Master.ps1)在终止 master 前核对 slot、主从、两端独占数据目录、Redis 进程路径及命令行、集群健康；本轮 owner 1 slot 2596，master `:16631`、replica `:16636`，强杀 PID 15444 后约六个 500ms 轮询恢复 16384 槽。脚本**没有执行 WAIT**，但 seed 到强杀间有命令与进程启动时间，不能声称写入一定未复制，也不能用通过结果证明无 WAIT 写的持久性。复跑须用全新独占实例/前缀，不能在旧数据上复跑固定 run ID。

结束时先查 Toxiproxy PID 对应的可执行路径/`-port 18475`，API proxy 的 upstream 必须是本轮 `:16630`，再停止该 PID；逐个 Redis 端口用 `CONFIG GET dir` 比对 `service-review13-redis/<port>`，只对匹配且仍存活的实例 `SHUTDOWN NOSAVE`。本轮最终检查 `16630..16636`、`18475`、`26380` 全部不监听。原始日志未入库，使用[结果摘要](RESULTS.json)中的 hash 辨认执行证据；不能只看 `go test` 退出码，必须分开统计 test pass/fail/skip。
