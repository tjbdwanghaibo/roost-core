# 维护手册

如果这是第一次维护Roost，先读[入门手册](../GETTING-STARTED.md)。本篇按日常修改、升级、故障、发布、文档、测试与可读性规则组织；先找到你的场景，再执行对应步骤。

当前版本v1.29.0（[发布说明](../release/v1.29.0-NOTES.md)：wiring/web P1 四项、`roost config check` 退出码与跨服务一致性、`roost.yaml` 端口块与各服务不同的 `etcd.advertise_addr`、卡牌 RPG 接入、Nest 生成器找不到锁目标时报错；生成物最低 core 升到 v1.29.0）；上一版 v1.28.0（[发布说明](../release/v1.28.0-NOTES.md)：Sync 慢消费者与自身视图、嵌入 TCP 出站队列、Saga 原生步骤、按实体定时器、DAO 显式持久键、match 跨分区成局等，含破坏性变化；生成物最低 core 升到 v1.28.0）；v1.27.0（[发布说明](../release/v1.27.0-NOTES.md)：`game/playeraccess` 改名、Remote WAL 级 Rejected 精简）；此前补丁 v1.26.1（[补丁说明](../release/v1.26.1-NOTES.md)：安全 toolchain go1.27.2、go.work 加载修复、game-demo 模板修复；生成物最低 core 仍为 v1.26.0）；功能版本 v1.26.0（[发布说明](../release/v1.26.0-NOTES.md)：三池与 Await、Sync 优化、旧兼容移除与全仓整理）；最终代码上的重档复测见[发布验收](../release/v1.26.0-VALIDATION.md)。之后未发版的变化见 [CHANGELOG `[Unreleased]`](../../CHANGELOG.md)。支持保证范围为 macOS/Linux。本地 Windows 编译/检查的结果仅作当前验证证据，不扩大支持平台。

## 1. 版本与兼容

单模块发布，wiring、codegen、demo 随 core 同一 tag。新业务固定 core 与工具版本，重新生成并编译工程。当前生成物最低运行依赖是 core v1.29.0（生成的 `roost.yaml` 带 v1.29.0 起必填的 `services.<name>.port_block`，生成的 Makefile 按钉住的 core 跑 `cmd/roost`，v1.28.0 的生成器读不了这个字段；生成物另用到 `framework/nest/entitytimer`、`wiring/selfsync`、`dataengine.NewStoredKeySet`、saga `Engine.GetByBusinessKey` 等 v1.28.0 起的 API）。文档补丁不改变最低版本和业务 API。

Nest 执行池命名为 short/long/io，不保留 fast/slow 旧名；配置、API、生成 Sender 和监控查询需一起迁移，迁移表见 [Nest 三池说明](../framework/NEST-AWAIT.md#池命名收敛2026-10-13)。

当前没有旧 WAL/schema/interest/客户端包的透明兼容。WAL只支持codec7；DAO只接受当前schema；Remote interest要求非零代际及完整身份；客户端RS v2区分PB、Sync、Lockstep；Remote 快照 checksum 只读 8 字节形态，WAL 中“Remote 写 + lease fence”历史记录不再投影（投影器 fatal、WAL 原样保留）。服务数据同样不兼容旧形态：account 的未转义旧账号 ID、无建角计划的 slot 与无 `CreationID` 的角色，activity 无计划的开窗条目与超限窗口，rank 的逗号分隔请求账本，mail 无窗口的已结算 claim，均被明确拒绝而不读取或升级（清单见 CHANGELOG `[v1.26.0]`“移除旧格式兼容”与[发布说明](../release/v1.26.0-NOTES.md#2-升级要求速查)）。生成器同样不兼容旧工程：roost.yaml 的 `versions.kit/skill/service/codegen`、旧 mod/feature 名与已删除的 `event` feature、`//cube:` 标记、cfggen `objects`、tablegen v1 manifest 均被拒绝并提示手工改法，三仓合一迁移工具已删除；SyncBusMod 缺省前缀改为 `roost.sync`、不再把 `roost.room` 映射到 `ROOST_SYNC`，未写前缀的部署须先停旧再起新（JetStream 全新部署或显式配置旧名，见 CHANGELOG `[v1.26.0]`“移除旧兼容（生成器）/（wiring / syncbus）”）。不要把单模块版本号与这些各自的线/存储格式号混为一谈。

## 2. 升级与回退

1. 记录当前 tag、配置版本、业务 schema、WAL 目录和依赖资源；备份数据库与需要保留的 WAL。不要复制正在被修改的 WAL 来冒充一致快照。
2. 停止新业务准入，等已接纳操作排空。停机超时继续保留底层依赖，不能把 context 取消当成真实完成。
3. 跨旧格式升级时，由能读旧 WAL 的旧程序先完成所需数据落库。新程序使用新 WAL 目录，schema 不同的数据由业务明确离线转换（[schema 变更操作手册](../framework/SCHEMA-MIGRATION.md)）；框架不自动清库。
4. 重新生成消费者，执行 build/vet/test、配置检查及声明场景冒烟，再启动新版本。静态 Player 跨机迁移只依赖已落库数据，不搬本地 WAL 热状态（[按 sid 搬机的参考流程](../framework/PLAYER-MIGRATION.md)）。
5. 回退前检查新进程是否写入旧程序不认识的数据/格式。无法读取时恢复匹配备份并按业务方案处理，不能只把二进制降级。

线上尚未部署（2026-10-06 起）：改存储格式或协议时不做旧进程、旧数据兼容，格式版本加 1、旧版本明确拒绝，并在 CHANGELOG 写“升级需清空 / 先停旧再起新”。维护者宣布上线后改为必须兼容。

## 3. 日常修改与验证

先同步最新代码并确认工作树状态，在独立 worktree 实施。业务修改遵循三大块局部聚集，写明拥有者、锁、准入点、结果未知、资源释放和中文契约注释；不要为方便接入复制另一套持久/并发状态机。

修复先留下能在修前失败的回归，后记录根因、变化、红绿结果及限制。必要文档直接维护到对应设计/实现篇及本目录，不再累积按轮次的交接流水账。源码历史注释里的 RR 和旧 docs 路径，可用 [文档清单](DOCUMENTS.md) 的固定提交追溯。

~~~sh
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -count=1 .
# 改动包的定向测试；跨包行为修改再跑全仓
GOWORK=off go test ./...
# 并发相关改动在支持 race 的环境运行受影响包
GOWORK=off go test -race ./framework/nest ./framework/sync/entitysync
# 改生成器或模板：重生成，确认无漂移，再编译真实消费工程
GOWORK=off go generate ./...
git diff --exit-code
~~~

改协议（`client/wire`、生成 PB）、Sync 帧（`framework/sync/frame`、entitysync）、Lockstep 编码或 `client/dotnet` 时，另跑 C# 客户端 SDK 的验证：

~~~sh
./scripts/test-client-dotnet.sh   # 金样 + SDK 自测 + 生成工程真实 TCP；缺 dotnet 打印 SKIP 并以 0 退出（不是通过）
~~~

它需要 .NET 10 SDK；没有时如实记“C# 未验证”。pretag 只跑金样与自测（生成工程那一条随 `go test ./...` 在 dotnet 可用时运行），CI 的 `client-dotnet` job 装 .NET 10 并以 `ROOST_REQUIRE_DOTNET=1` 跑全部。

根包 `go test -count=1 .` 是一组仓库级门禁，其中 `TestTrackedGoFilesAreGofmtClean` 要求所有跟踪（及未忽略的新增）`.go` 文件 gofmt 无差异，失败时列出文件；提交前对改过的文件跑 `gofmt -w`。

真实资源测试需要独立 Mongo/Redis/NATS/etcd 和唯一命名空间。不要打印/提交 env 凭据，不用共享业务库进行故障注入。测试文件数不是测试通过数，跳过、未运行、仅编译均分别记录。

## 4. 故障处理

| 现象 | 先确认 | 正确处理边界 |
| --- | --- | --- |
| Nest fence/提交未知 | 首个失败原因、WAL/存储结论、进程 incarnation | 停止安全写入并走恢复；不盲重发扣款/发奖 |
| strict 成功但 Mongo 尚旧 | 投影队列、held、版本冲突、outbox | strict仅承诺WAL fsync；需要落库时用投影等待 |
| WAL 格式拒绝 | 程序版本和codec、目录备份 | 用匹配旧程序完成落库；不删坏记录/推进checkpoint |
| 启动投影恢复 `fatal projection version conflict ... expected=0 next=1` | 冲突的 `_id` 在 Mongo 是否为墓碑（`_deleted: true`）：是则为“删档后同 ID 重建”记录（D10 修复前写下，或直接 Create 复用了删过档的 ID） | 不删记录、不跳过、不放宽版本比较；按 [KNOWN-LIMITS“删档后同 ID 重建：违反约束或修复前写下的冲突记录”](KNOWN-LIMITS.md) 人工处置。修复后的 GetOrCreate 拒绝删过档的 ID（客户端码 `entity_deleted`）；直接 Create 必须用 id-gen 新 ID |
| Entity schema 不匹配 | 全部DAO descriptor及实际数据版本 | 校验发生在水合前；明确离线迁移计划，不自动“补默认值” |
| Sync 不发/延迟 | held/ready、订阅来源、profile、冻结预算、水位、背压 | setter只标脏；不绕过提交水位来降低延迟 |
| 客户端增量缺口 | epoch/base/对象存在、重连代际 | 恢复全量；不继续应用不可信增量 |
| 玩家 TCP 断线伴随 `player_tcp_outbound_overflow_total` / `player_tcp_write_error_total` 增长 | 客户端是否停读或链路过慢、单帧大小、`write_timeout` | 嵌入 TCP 出站队列满或写超时即关闭该连接，属预期的慢连接处理，重连恢复全量；不要为留住慢客户端放大队列或 `write_timeout`（[GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)） |
| `player_tcp_outbound_budget_exhausted_total` 增长（嵌入 TCP），Sync 延迟上升、`player.sync.push_refused.total{outcome="retried"}` 增长 | 是否很多连接同时停读（占着预算直到 `write_timeout`）、健康流量是否超过发送能力、`player_access.tcp.outbound.resident_bytes` 是否过小 | 全进程出站驻留预算耗尽：推送被拒、不断开连接，Sync 整轮重试；按内存余量调大预算，或排查停读连接来源（[GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站)） |
| Remote 已提交但快照旧 | outbox进度、确认、L2水位和读取策略 | 区分数据权威与快照水位；不把旧L1写回覆盖 |
| 总线停止/重订失败 | consumer关闭、在途回调、ErrSubscriptionBusy | 等真实排空；有限MaxDeliver不保证永不丢 |
| 停机超时 | Service与各Mod预算、仍在途操作 | 保留依赖并继续等待，禁止先释放连接/锁 |
| readiness Degraded | 哪个checker降级、是否仍满足准入 | Degraded仍可ready；Fail才阻断 |
| 启动报 `mod saga start: saga: subscribe Nest starts: ... stream not found` | 运行进程 `--check-config` 或 `roost project doctor` 的 `config-check:<service>`：`saga.start_effect_stream` / `start_effect_prefix` 是否显式写成与 `dataengine.effects` 不同；目标 NATS 上是否真有该流 | 删掉 saga 的这两个键（不写时取 `dataengine.effects`）；不要为让订阅成功而手工建一条没人发布的流 |
| activity目标长期失联 | 持久投递状态、静态sid及人工管理操作 | 观察重试不自动耗尽，按业务决定收口 |

## 5. 发版

保持已有tag不可变。更新 release manifest 与两篇发布说明；新增API时才提高生成器最低版本和compat minimum lane。干净worktree执行 scripts/pretag.sh，包含生成、build/vet/tidy/test与tag存在性检查。运行时变化重验相关真实资源矩阵与消费工程；文档补丁清楚记录沿用哪个运行时验收，不能声称重新跑过。

提交和推送后创建 annotated tag、推送tag；不等待GitHub CI。随后下载真实tag验证模块解析与消费者，记录实际结果。发布不等于部署，不能顺便改线上资源。性能只能来自同环境同参数实际测量。

## 6. 维护文档的规则

接口/配置/格式改变时，同时更新设计篇、实现篇、源码定位、升级要求与已知限制。当前文档不保留相互矛盾的“旧段落+末尾更正”；过程讨论进Git历史，未解决问题与当前验证边界留在KNOWN-LIMITS：它只写当前仍有效的限制，已修复问题的经过归CHANGELOG、docs/release与Git历史，不留在KNOWN-LIMITS。历史证据链接固定commit，当前规范用仓内相对链接。

根目录只保留 README、CHANGELOG、AGENTS 和许可证等仓库入口。详细契约归 `docs/framework/`，性能报告与随仓样本归 `docs/performance/`，发布过程与功能验收归 `docs/release/`；本目录保留维护规则、已知限制、外部验收与文档清单。各包 README 和工具手册保留在对应代码附近。

WAL目录使用单写持久卷，不同实例不得共享同一写目录。Mongo原子提交要求事务能力及相同database scope；receipt保留窗口必须覆盖对应的重放窗口。详细契约见[Nest/WAL](../framework/NEST_TRANSACTION_WAL.md)与[DataEngine](../framework/guide/03-dataengine.md)。

## 7. 测试与生成代码维护

### 测试组织

- 测试按当前业务契约和功能主题放进包内现有文件，不按每次修复新建文件；文件名不带 RR、日期或 Bugfix，单文件不超过约 1200 行。不同测试包、构建标签与平台特定文件不跨合并；生成产物、fixture、独立示例模块及 benchmark 原位保留。
- 测试名与头注释写当前承诺；需要追溯时最多一行“历史：见 <提交>”，过程留在 Git 与 CHANGELOG。

### 保留与删除

- **保留**：顺序、并发、回滚、持久恢复、协议往返、状态机交错、结果未知、fail-stop 的行为回归；结构守卫；生成产物与正式生成工程的测试；benchmark、example、fuzz。拿不准的一律保留。
- **可删除**：断言已被同包另一用例完整覆盖的重复用例（删前写明覆盖它的保留用例，独有断言先并入）；只测测试替身、不经产品代码的用例；只断言返回值形状而不触及承诺的用例；一次性诊断、已删除实现的文字扫描、限制内部函数组织方式的结构断言。
- 历史名称不等于过时功能：生成器对已退役写法的拒绝、失效产物清理测试仍是现行契约。不能通过删除测试或放宽断言掩盖生成错误或产品缺陷。

### 批量整理测试时的验证方法

- 每包整理前后对全部非 import 声明做语法摘要对比，除列明的删除外必须一致；
- 逐函数语句覆盖率对比，差异只允许来自计时相关后台循环的跑间波动，并逐条说明；
- 受影响包 `go vet` 与 `-race`，`-tags integration` 的 `go vet`，根包门禁，全仓 `go test ./...`，`go generate ./...` 无漂移；
- integration 标签的真实依赖用例没有运行时只记“编译与 vet”，不计作外部资源验收。

整理不等于证明所有测试互不重复。最近两次整理的数字与过程见 CHANGELOG `[v1.26.0]` 的“测试整理与门禁”与[整理时的维护手册（固定提交）](https://github.com/tjbdwanghaibo/roost-core/blob/ea776b4a977f29661932896b11aea80dd00ea3a0/docs/maintenance/README.md#7-测试与生成代码维护)。

### 编写行为测试的边界

- on_change 等交付测试必须走正式提交边界：经 Guard 取得实体锁、`BeginSyncMutation` 建提交门、锁内修改并 Admit 冻结、确认后解锁、最后 Release 开放交付。裸 `MarkDirty` 加 `CaptureSync` 不等于 Nest handler 提交，会让 Manager 抢先 Flush 而偶发失败；冻包失败要检查 `LastError`，不能忽略错误或靠重试掩盖。用 `synctest` 时，同一虚拟时刻的先后顺序用 `synctest.Wait` 明确写出，不改生产调度顺序。
- 需要区分“未配置”和“显式零值”的 Mod 用 `app.LoadConfigWithPresence`：它复用声明解码器、在读取声明键时返回来源存在性，失败时不返回部分结果。不由 Mod 再读 Viper，不复制单键直读或增加守卫白名单。
- 写真实 Redis 的用例用 `infra/storage/redis/redistest` 取唯一前缀（`redistest.Prefix`）或为自己生成的唯一前缀注册清理（`redistest.Cleanup`），在建存储 / Mod 之前调用，用例结束时删掉键名含该前缀的键（单机与 Cluster 都按主节点 SCAN，不 FLUSH）；不要只删自己列举的几个键名。CI `service-redis` 末尾要求 Redis 为空，漏清理会在那里变红；固定键名（前缀不含本次运行唯一的部分）即使清理了也不允许，那一步发现不了，复核方法见[外部验证](EXTERNAL-VERIFICATION.md#服务层-redis-组指向共享隔离环境的-redis2026-10-15)。
- 服务层 Redis 组（CI `service-redis` job 的包与变量组）**允许**指向共享隔离环境的 Redis（2026-10-15 核对：无固定键名，跑完键名清单不变）。条件：只设 CI 的五个 Redis 门变量，unset 其余 `ROOST_DATAENGINE_IT_*` / `ROOST_IT_*`（否则 `wiring/dataengine` 的真实依赖与故障用例会跟着在共享环境上跑）；确认 `remote-acceptance.lock` 不存在；前后只读核对 `DBSIZE` / `--scan` 键名清单，不 FLUSH。共享 Redis 不为空时，判据是“本次运行前后键名清单不变”，不是“跑完为空”。
- 生成工程里需要真实依赖的用例（`codegen/internal/entity/testdata` 下复制进生成工程运行的目录、`demo/` 的 `*_test.go.tmpl`、把用例写进新生成工程的仓库内测试）`go test ./...` 看不到；新增时让 `dataengine-env.sh test` 调用的生成工程脚本（`scripts/test-{dataengine,remote,demo}-generated.sh`）以 `-run` 选中它，或在根包 `integration_coverage_test.go` 的表里写明专门入口，否则 `TestGeneratedProjectRealDependencyTestsHaveAnEntry` 变红。
- 先红后绿：每个修复先有一条在修前代码上确实变红、且红在承诺本身上的回归，修前失败文本原样记录；不在“恰好能过的规模”上宣布验收。

### 生成代码

生成代码同样面向人阅读：局部变量表达职责，必要中文注释说明校验和执行边界；从模板修改，不手改产物。保留统一格式化、当前生成物编译及运行回归。改生成模板后：

1. `GOWORK=off go generate ./...`，`git status --porcelain` 必须干净；
2. 有意改动的 golden 用 `-update` 刷新并逐个看 diff；
3. 重新生成 game-demo 跑 `go build ./... && go vet ./... && go test ./...`（命令见 [demo README](../../demo/README.md#改这里的东西之后)）。

修改 Wiring 的配置声明时，同步运行 `go generate ./codegen/internal/roost`，更新生成器持有的 `kitconfig_gen.go` 快照，再运行 `go test ./codegen/internal/roost -run '^(TestKitConfigSchemasMatchKitDeclarations|TestDoctorReadsBusinessDeclarationsFromTheProcess)$' -count=1`。遗漏快照会让配置检查与运行时声明不一致，doctor 还会把新增框架字段误计为业务字段；不能只测 Wiring 包或修改业务字段数量断言来掩盖漂移。

### 本地证据目录

`artifacts/perf/` 是忽略的本地证据目录，其中的 `.go` 源码备份会被 Go 包枚举（`.gitignore` 不控制 Go 构建范围），让 `go build ./...` / `go test ./...` 报错。保存源码备份时放进带独立 `go.mod` 的目录或以下划线开头的目录；构建、pretag 在干净 worktree 里跑。删除 worktree 前先把 `artifacts/perf/*`、pprof、日志复制到持久位置。

## 8. 可读性整理的现行要求

这些要求适用于以后的代码修改与 review，不只是一次整理活动。

1. **注释写当前约束**：中文注释写职责、所有权、锁与准入边界、失败语义和原因；不写过程编号（RR、NC、O、B、M、U、W 等）、轮次、日期与“以前 / 修复前”叙述。过程留在 Git 与 CHANGELOG，确需追溯时一行“历史：见 <提交>”。错误前缀与日志文本用当前包名与现行术语（例如 `actionflow:`、`wiring mod`、short/long/io），按文本匹配的使用方在 CHANGELOG 里列出。
2. **命名表达职责与状态**：变量和字段名说明它是什么（如 `retainedCapacity` 区别于当前切片 `cap`），零值或 nil 的含义写在声明处（如“nil 表示没有待处理窗口”）。改名不留旧别名；导出名改动按 API 变化记入 CHANGELOG。
3. **按步骤拆分长函数**：拆成按准备、校验、执行、收尾命名的局部步骤，不新增包、接口或第二套执行路径；锁边界、调用顺序、错误与资源释放时机保持不变。拆分本身不写结构断言，复用既有行为回归。
4. **热路径改函数体须 benchstat**：Nest 调度、Sync Flush / AOI / Relation、DataEngine 提交与投影、编解码等热路径的函数体改动（含为可读性做的拆分与改写），在同机同配置下前后交替多轮跑相关 benchmark（`-count` 至少 10，长基准后台跑并加 `-timeout 0`），用 `benchstat` 对比 ns/op、B/op、allocs/op；样本放 `docs/performance/benchmarks/<label>/`。有退化写明代价与取舍，不宣称未测量的吞吐变化。只改注释和命名不需要。
5. **生成产物字节不变**：只为可读性改生成器或模板（局部命名、注释、拆分）时，生成物必须字节不变：`go generate ./...` 后 `git diff --exit-code` 干净，各生成器 golden 测试不加 `-update` 即通过，重新生成 game-demo 与改动前的生成树 `diff -r` 一致。若改动有意让生成物只差注释或 help 文本，在提交说明里写明“生成代码只差注释 / 文本”并一并提交重新生成的产物与快照。
6. **验收**：每批改动跑受影响包 `go vet`、`go test`，并发相关加 `-race`，再跑根包 `go test -count=1 .`（含 gofmt 与文档链接门禁）；跨包改动推送前跑全仓 `go test ./...`。文档补丁不混入行为重构。
