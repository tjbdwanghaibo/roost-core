# Outbox与历史留项：非压测收口

基线main `88fc852c`，工作分支`codex/outbox-closure`；第一批提交36ce227c、55f2cc20、42eb95e8，后续代码由包含本文的提交承接。维护者最新指示：除压测外全部继续，macOS/Linux，不做部署，不自动发版。普通验证均串行，Go使用`GOMAXPROCS=2`、`-p 1`。

## 当前实现与历史条目

| 条目 | 实现/结论 | 边界 |
| --- | --- | --- |
| E14 | 真实Mongo提交后、发布前SIGKILL；同sid Assembly.Start恢复，真实Redis/NATS只读收敛 | 单机独立进程，非物理掉电 |
| 唯一outbox | 投影只提交并唤醒；finalizer只确认/释放；RecoverOutbox唯一协调者 | 跨进程幂等补发，不宣称分布式恰好一次 |
| RR-42～45 | Layered有界元数据、游标跨失败页、异步有界兴趣广播、真实scope负载配额 | 不降低持久/收敛标准；RR-45只改负载夹具配置 |
| RR-47 / 约60 TPS | 有界并行独立事务，全部Entity保序；新增阶段计时 | 确定性反例红转绿，未重跑性能；不宣称恢复80 TPS |
| A1 | WAL只读诊断、完整副本、哈希与未完成标记 | 不自动跳坏记录，不冒充权威业务修复或生产恢复 |
| A2 | Go/C#单流Sync接收基线；Unity正式适配在缺口/应用失败时关旧流，经业务正常鉴权重连取得Full | Full可能仅是恢复第一包；业务对象应用模型、Unity引擎/IL2CPP未验 |
| A8 / RR-46 | 锁等待与删除准入前可取消，已接受/未知删除仍完成收尾 | 不承诺强杀OnDestroy等业务回调 |
| A6/A7 | 旧实体租约/handBackIdle路线已由静态绑定/App单实例锁取代；通用Remote Transfer保持writeGate+权威CAS契约 | 维护者2026-10-08定案：静态Player跨机迁移以已落库数据为准；WAL只保证落库，不参与迁移；WAL热迁移无需求，不列待办 |
| A14 | 保留kit装配边界，根包依赖守卫加正式消费验证 | 不为目录平铺重复重构 |
| N03/N04 | 复用具名真实资源证据，新增本机三节点etcd leader强杀 | 不称全域穷尽；多主机网络/部署另列 |

## 验证

原始日志持久位置：`roost-core/artifacts/perf/outbox-closure-20261008/`，不提交凭据、大日志或二进制。分支原`artifacts/perf/remote/`已完整复制到该目录的`worktree-remote/`并通过逐文件checksum对照，包含各轮矩阵和中断负载；清理worktree不会删除这些证据。历史失败保留。

- RR-46修前memory/durable各失败，RR-47修前无关事务被阻塞，原文见各bug记录。
- `go test -p 1 ./lock ./entity ./nestwal ./sync/frame`通过；目标Remote/Kit通过。
- `go test -p 1 -race ./lock ./entity ./nestwal ./sync/frame ./remoteentity ./kit/remoteentity -count=3`通过：1.927/6.715/13.481/1.611/94.154/2.210s。之后新增worker上限/取消、legacy锁与持久checkpoint测试已在最终普通及race回归覆盖。
- `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json`通过全部14组输出；直接编译Unity正式适配源码，用真实TCP验证Sync缺口与应用异常关闭。首次遗漏golden参数导致启动失败，保留`client-unity.log`；正确命令结果为`client-unity-r2.log`。
- `go test -p 1 -tags=integration ./etcd/driver -run '^TestRealEtcdThreeNodeLeaderLossPreservesWatchDiscoveryAndElection$' -count=1 -v -timeout=60s`通过11.805s。三个本机独立etcd进程，杀实际Raft leader，Watch恢复、发现跨原TTL存活、选主fence递增。首次测试漏传CreatedNotify而等待超时，修正测试用法后通过，未改etcd生产代码。
- 最终矩阵首轮18/21通过，3项因私有toxiproxy退出而连接拒绝（lease-process、durable-process、broker-network）；在同一持续执行会话内启动依赖后，仅这3项原命令补跑全部通过（10.796/5.149/21.327s），无skip。21项均已有通过证据，首次失败不删除。
- 最新E14精确强杀/同sid恢复：race三轮通过2.775s。Mirror核心真实资源4项通过30.080s，覆盖同sid重启、Mongo选举初始化、单机墓碑WAIT对照与Cluster定位主节点；正式生成Mirror八场景也全部通过118.481s（测试主体116.63s），日志`mirror-generated-final.log`。
- macOS最终build/vet通过；首轮全仓发现新增配置未同步生成器快照，`TestKitConfigSchemasMatchKitDeclarations`与doctor业务声明计数失败。按正式go generate修复产物，保留原`all-final.log`，不改测试期望。
- WAL CLI实跑通过：真实Append+Ack+Close后执行`walinspect -dir ... -snapshot ...`，报告complete、1条记录、无损坏；逐文件核对原件/副本字节、大小与SHA-256，无INCOMPLETE。日志`wal-cli.log`。
- 配置声明快照已正式重新生成（仅新增outbox_publish_workers一条声明），再次生成字节不变。macOS最终全仓build/vet/test全部通过（`build-final-r2.log`、`vet-final-r2.log`、`all-final-r2.log`，131个有测试包通过，根包已执行，生成器包243.637s）；最终六包race通过（1.626/3.225/5.341/1.699/33.628/1.878s，`race-final.log`）。全仓`go generate ./...`前后diff/新文件列表无变化；独立`project new -template game-demo`生成458文件，replace到本次检出后build/vet/test通过（19个有测试包，日志`generated-demo-*.log`）。Linux/arm64（Go1.27.1、LinuxKit 7.0.12）全仓build/vet通过；六个受影响包race通过（1.200/2.803/4.668/1.019/31.801/1.201s），根包通过50.015s，三项配置/生成声明测试通过91.577s。Linux WAL CLI实际生成完整副本并成功报告1条记录、无损坏。日志在linux/；Linux本轮未重跑所有未改动包的普通测试，不冒称Linux全仓test。容器限制2 CPU/4GB、禁外网，只用缓存镜像；Go/C#源码与工作树checksum一致。

## 暂停与排除

只有性能验证仍按维护者要求暂停：新的1h Remote、Sync AOI两模式负载及性能profile。旧240秒完成14423笔、错误4520、约60.1 TPS的失败样本完整保留，不能拼接为1h通过，也不能当作当前有界并行版结论。[数据](OUTBOX-PARTIAL-LOAD-2026-10-08.md)与[分析](OUTBOX-TPS-ANALYSIS-2026-10-08.md)。

Windows、真实部署、跨主机网络与物理断电不在本轮验收范围。新增SDK保证协议基线与失败边界，不提供所有游戏业务模型、C++或真实引擎验收。未打新tag、未部署。

## 收口状态与复跑入口

本轮约定的非压测实现与具名验收已完成。复跑入口均随仓库保留：`go generate ./...`、`go build ./...`、`go vet ./...`、`go test ./...`、六目标包`-race`、C#命令、etcd具名integration测试、`scripts/test-remote-matrix.sh`、`scripts/mirror-local.sh test`及`test-core`、`cmd/walinspect`。真实资源必须使用自己的私有根目录/端口；启动、故障用例和停止放在同一持续执行会话，验收后down并保留证据。不能把这些功能入口改成启动负载的理由。

实现提交`95c89dde`已快进合入并推送main，包含前序36ce227c/55f2cc20/42eb95e8；未发新版本。

CBM已按main刷新：项目`Users-whb-roost-roost-core`，generation `2026-10-08T05:39:49Z`，25613 nodes / 246778 edges。50个变更Go/C#路径中48个`metadata_match`且无记录缺口；两个`codegen/internal/entity/testdata/remoteflow`文件按目录规则排除，已直接读取变更并通过正式生成消费验证。全库另有5个既有demo模板的部分解析提示，未伪称图谱完整；后续仍按coverage和当前源码补证。原始输出`cbm-index.json`、`cbm-coverage.json`随本轮证据保留。
