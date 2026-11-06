# v1.41～v1.42 审查收尾与 Linux 回归（2026-11-07）

审查基线：[v1.40.1](https://github.com/tjbdwanghaibo/roost-core/commit/b3e9ea69) → [main / v1.42.0](https://github.com/tjbdwanghaibo/roost-core/commit/a6235227695a49f3364a6c639bf6cfe4678d9d98)。收尾时重新 fetch，main 未变化。变更共 542 个文件，见 [完整变更清单](evidence/feature-review-20261107/changed-files.txt)。沿用 [前一轮报告](FEATURE-REVIEW-20261106.md)，本次补齐主要功能组的交叉审查、根模块与生成工程回归、客户端和联盟真实进程验证。

**新增两项 P2：联盟迁移可遗失宣战补偿的执行责任；best_effort 单 worker 提前报告停止完成。前轮三项已确认缺陷未修复。** 本轮交付审查报告与可复现探针，不修改运行时，不发 PR/issue，仍推固定审查分支。

“本轮审查收尾”指下表所列范围完成，不代表 542 个文件逐行穷尽证明，也不代表无 bug 或已通过生产上线验收。索引工具不可用，以 Git diff、精确源码调用链和 Linux 实测补证。

## RV-20261107-01 / P2：迁移检查与 fencing 之间可提交新的宣战

位置：[TransferOwnership](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/demo/internal/rpc/alliance/transfer.go.tmpl#L99)、[旧 owner 的补发](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/demo/internal/rpc/alliance/war.go.tmpl#L102)、[启动扫描](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/demo/internal/rpc/alliance/mod.go.tmpl#L76)。这是正式 game-demo 联盟服务的并发问题，不是 Remote 所有权转移原子性失效。

可达时序：

1. TransferOwnership 通过 Nest 读取 WarAcks，看到没有待确认宣战。
2. 在调用 TransferRemoteOwnership、关闭旧 owner 写入准入之前，DeclareWar 成功提交宣战及 WarAcks，并安排 follow-up。
3. 转移成功，报告 Moved；此前的检查已经失效。
4. 旧 owner 的 acknowledgeWar 发现 owner 改变，直接退出，认为新 owner 会接续。
5. 新 owner 已经运行，ScanWars 仅在模块启动调用，加载转入实体时没有接续此项任务。敌方联盟没有相应宣战，等待记录仍在，缺少自动补发。

后果：SLG 扩缩容／迁移期间，宣战双方状态可长期不一致，直到后续请求重试触发补发、人工扫描或新 owner 重启恢复。持久事实未丢，但自动执行责任丢失。迁移注释明确承诺 WarAcks 非空的联盟留在 Pending，当前检查不能兑现并发情况下的承诺。

[探针](evidence/feature-review-20261107/review_transfer_war_test.go.txt) 在正式生成工程的现有 cluster 夹具上，使用真实 Service/Nest/Sender；通过 ownership adapter 在转移入口插入一次合法宣战，确定性地构造上述时序。使用的是夹具的 ownership/transport，不能描述为真实 Mongo 并发复现。运行三轮均失败，Moved 非空、Pending 为空、敌方无对应战争；日志同时显示旧 owner 已停止补发。见 [输出](evidence/feature-review-20261107/transfer-war.log)。

复跑：用本基线生成 `game-demo`、模块名 `example.com/planet`，go.mod replace 指向对应 core checkout，将探针保存为 `internal/rpc/alliance/review_transfer_war_test.go`：

```bash
go test -mod=mod -race ./internal/rpc/alliance -run '^TestReviewWarCommittedBetweenCheckAndTransfer$' -count=3 -v
```

建议将宣战准入与迁移检查纳入同一迁移状态协议，或让新 owner 在取得/加载实体时可靠接管持久 follow-up。不要仅增加一次检查，也不要持有 Nest 实体锁跨所有权网络 I/O。修复验收需同时覆盖宣战在检查之前、检查之后但 fencing 之前，以及新 owner 已启动的迁入恢复。

状态（2026-11-07）：已修复，未发版。新 owner 的宣战扫描在起服后周期运行、接手运行中迁入的联盟；回归 `TestWarDeclaredDuringTransferIsAskedByTheNewOwner` 覆盖上述三种时序，见 [KNOWN-LIMITS](KNOWN-LIMITS.md) 开头与 CHANGELOG `[Unreleased]`。以上原结论保留。

## RV-20261107-02 / P2：best_effort 单 worker 的 Stop 不等待在跑 handler

位置：[deliver 单 worker 分支](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/wiring/dataengine/effects_best_effort.go#L209)、[Stop](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/wiring/dataengine/effects_best_effort.go#L297)。

workers <= 1 时直接在订阅 callback 中运行 process，没有计入 tasks。Stop 退订并取消 context 后，只等待 tasks 就关闭 Closed；Unsubscribe 并不等待已进入的 callback 返回。因此 handler 仍在工作，Closed 却已通知上层“停止完成”。使用者据此释放数据库、Nest 或其他依赖，可能与尚在执行的 handler 重叠。取消 context 不等于 handler 已完成。

[探针](evidence/feature-review-20261107/review_best_effort_stop_test.go.txt) 让 handler 进入后阻塞，调用 Stop，检查 Closed 是否提前关闭；随后释放 handler 并清理。workers=1 三轮失败，workers=2 对照三轮通过，见 [输出](evidence/feature-review-20261107/effect-stop.log)。探针复用本包 NATS mock；错误来自真实订阅包装层的生命周期计数。默认 workers=16，此缺陷在配置单 worker 时触发。

将探针放到目标 checkout 的 `wiring/dataengine/review_best_effort_stop_test.go`，执行：

```bash
go test -race ./wiring/dataengine -run '^TestReviewBestEffortStopWaitsForRunningHandler$' -count=3 -v
```

建议对单 worker 与并行 worker 统一跟踪已接纳 callback/handler 的存活期，先关闭准入，再等全部已接纳任务完成。仅增加一个 WaitGroup.Add 仍需检查它与 Wait 的准入竞态。正常使用原生 DrainContext 的 Drain 路径会等待 callback，本探针只确认 Stop，不泛化为所有关闭路径都失败。

## 之前问题的状态

| 项目 | 本轮状态 |
| --- | --- |
| RV-20261106-01：失败创建 best_effort 事件逃逸（P2） | 未修复；基线未变，保留前轮红色与 overlay 定位对照 |
| RV-20261106-03：旧 Leave 删除重新入场的新实例（P2） | 未修复；前轮三次复现有效 |
| RV-20261106-04：BaseState 重复 MsgPack 标签（P3） | 未修复；直接编码该结构丢 X/VX；不扩大为 demo DAO 映射也丢字段 |
| RV-20261106-02：cellworld 边界抖动 | 本轮完整包通过；此前一次失败根因未定，不关闭 |
| Lockstep Linux 深缓冲快照链路 | 本轮完整包通过；此前同基线失败仍未解释，不以一次成功关闭 |

## 审查覆盖

下面是跨本轮与前轮的主要调用链覆盖，不是每个文件均人工逐行审计的声明。业务策略与框架保证分开判断：Remote 独占写依赖单 owner/fail-stop 契约；不同实体的 effect 到达顺序不是全局 WAL 顺序；best_effort 本身允许丢消息；cellworld 仍为原型。这些既定边界未误报为新 bug。

| 功能组 | 人工复核重点 | 验证／结果 |
| --- | --- | --- |
| Remote / Entity / DataEngine | exclusive 准入、所有权转移、提交 CAS、ack 对应、批量 claim/finalize、删除持锁 | 根模块 race；未新增确认 Remote 原子性缺陷 |
| Nest / effects / outbox | 创建回滚、副作用提交、消费并发、停止屏障、持久投影冲突键 | 根模块 race + 两轮专门探针；两项 best_effort 缺陷 |
| App / wiring / ingress | 启动失败清理、关闭准入、RPC/Saga 入口退场 | 根模块 race；未新增确认缺陷 |
| Bus / NATS / 路由 RPC | RPC withdraw、JetStream 延后结算、队列 worker、consumer 重试、drain | 根模块 race + 联盟真实进程路径 |
| 联盟 demo | 创盟/加入/退出、guard 与预留恢复、迁移、宣战补发、启动预加载 | 生成工程 race + 真实进程 + 迁移时序探针；一项新缺陷 |
| 生成器 | entity owner 扫描与解析、Sender/RPC 校验、服务接线、自定义 module | 根模块生成器测试 + 独立正式 game-demo 全量 race |
| 同步 / cellworld | 代际交接、Late Leave、Mirror、interest LOD/self/reLayer、状态编码 | 根模块 race；两项既有确认缺陷与一项未闭环抖动 |
| Gate / 客户端 / 机器人 | 跨 Gate auth generation、连接/重连、C#/C++/Go 鉴权变化 | .NET 正式脚本、C++ native smoke、生成 Player TCP |

## Linux 验证与限制

Go 1.27.2、GOMAXPROCS=4。未跑压测／容量套件，保留原有压测文件。根模块基础回归通过 overlay 排除之前专门复现缺陷的未跟踪探针（替换为同包空文件），不排除仓库已跟踪测试；新 Stop 探针在基础回归之后加入。

| 验证 | 实际结果 | 证据 |
| --- | --- | --- |
| 根模块 `go test -race -p 2 -overlay=… ./... -count=1` | 162 个有测试包通过，23 个包无测试；根包因 review 临时探针 gofmt 失败，命令退出 1 | [原始日志](evidence/feature-review-20261107/root-race.log) |
| 格式化临时探针后 `go test -race . -count=1` | 根包通过；合计 163 个有测试包在两次命令中通过，不能说第一次全仓命令退出 0 | [复跑日志](evidence/feature-review-20261107/root-gates-repeat.log) |
| 正式 `game-demo` 自定义 module 生成与全量 race | 631 文件生成成功；32 个有测试包通过、50 个包无测试；退出 0 | [生成](evidence/feature-review-20261107/generate.log)、[race](evidence/feature-review-20261107/demo-race.log) |
| `ROOST_REQUIRE_DOTNET=1 bash scripts/test-client-dotnet.sh` | .NET 10.0.402；首次沙箱 NuGet-Migrations mutex 受限；获执行权限后正式脚本通过，含跨 Gate C# 和生成 Player TCP | [首次](evidence/feature-review-20261107/dotnet.log)、[成功重跑](evidence/feature-review-20261107/dotnet-retry.log) |
| C++ native smoke | 无 cmake，改为 g++17 直接编译正式源文件与 smoke.cpp，再运行 tests/run.py；Sync/auth golden、TCP/PB/心跳/有界请求/重连通过 | [环境限制](evidence/feature-review-20261107/cpp.log)、[直接编译后测试](evidence/feature-review-20261107/native-direct.log) |
| 联盟真实进程默认非压测七项 | 6 项通过；RobotOfflineJoin 启动 game-1001 时 35112 端口占用，未到业务断言，整体退出 1；另行补跑见下 | [首次日志](evidence/feature-review-20261107/alliance-real.log) |

补跑将 ALLIANCE_E2E_PORT_BASE 设为 25100，并用 ALLIANCE_E2E_RUN 只选择 RobotOfflineJoin；原样业务断言通过，退出 0，全部进程退出时 0 ERROR，见 [补跑日志](evidence/feature-review-20261107/alliance-repeat.log)。因此七个功能用例在两次执行中全部通过，但首次整条命令的退出 1 保留；端口占用来源未确定，不据此认定框架业务缺陷。

真实依赖使用本机隔离 Mongo replica set、NATS 集群、Redis、toxiproxy 和脚本自起 etcd；测试结束关闭本轮依赖。联盟默认套件没有启用 ALLIANCE_E2E_PERF。同机进程故障验证不等于多机器网络分区验收。

基础全仓没有打开全部外部依赖门变量，非 verbose 日志也不能统计所有 Skip，故不声明零 Skip。C++ smoke 不是完整 CMake/CTest 套件，.NET 的 Unity adapter 测试不等于真实 Unity Player。生产容量、长稳、目标 Unity/IL2CPP 构建、跨机器故障以及所有历史真实恢复矩阵未在本轮全部重跑。

当前应优先修复五项已确认缺陷，并解释 Lockstep/cellworld 的间歇失败，再形成发布验收结论；这次审查结果不足以宣布“框架没有 bug，可以无条件上线”。
