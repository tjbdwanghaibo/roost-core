# P3b：kit Mod 瘦身（`Assemble*` 下沉 core）

批次：P3b（[执行方案](POST_RELEASE_PLAN_2026-09-08.md) §5；P3 §1.1 末记下的后续）
状态：**完成并发版**（2026-09-09：core v1.15.0 → kit v1.14.0 → codegen v1.15.1）
前置：P6 已发版（core v1.14.0 / kit v1.13.0 / codegen v1.15.0）；U-0104～U-0107 已合 core main

## 1. 完成定义与做法

P3 之后 kit 的八个 Mod 仍是编排者：nats / etcd / redis 直接拿 `Raw()` 与内部构造器拼装，dataengine 的 `Start` 在 Mod 里做 WAL → projector → outbox → runtime 的构造与失败回滚，saga 的 `Start` / `Stop` 在 Mod 里做订阅、引擎循环与 drain，remoteentity 的 `Start` 在 Mod 里做 Validate → BindSync → Seal → EnsureRemoteStorage → RecoverOutbox → StartFinalizer 及回滚。

P3b 后 **Mod 只做四件事**：viper → 配置结构；`registry.Lookup` 取依赖、`Register` 提供能力；健康检查注册；把生命周期转交给 core 的装配对象。core 每个包提供 `Assemble(...)` 返回 `*Assembly`，构造顺序、失败回滚、`Raw()` 级探活全部在 core 内部。

原计划分三小批各打一个 alpha；实际因为三批的 core 侧都只新增导出（不改既有 API），**合成一次 core 提交、一个 alpha tag**，kit 一次切换。

## 2. core 新增（全部为新增导出，不改既有符号）

| 包 | 新增 | 吃掉的 Mod 编排 |
| --- | --- | --- |
| `redis/driver` | `Assemble(cfg) (*Assembly, error)`；`Assembly{Client, Locks}`，`Ping(ctx)`、`Close()` | `NewRedisClient` + `NewDistLockFactory(client.Raw())`；同一连接池 |
| `etcd/driver` | `Assemble(cfg)`；`Assembly{Client, Discovery, Election}`，`Ping(ctx)`（首个 endpoint 的 `Status`）、`Start(ctx, *ServiceInfo)`（探活 + 按需注册）、`Close(ctx)`（注销 + 关连接，两个错误都上报） | `NewDiscovery(client.Raw(), …)` + `SetRetryIntervals` + `NewElectionFactory(client.Raw())`；两处 `Raw().Status` |
| `nats/driver` | `Assemble(cfg, extra)`；`Assembly{Client, JetStream, RPC}`，`Connected()`、`Close(ctx)`（停 RPC、限时 drain、超时硬关）；常量 `rpcCallbackWorkers = 4` | `NewClient` + `NewJetStreamClient` + `NewRPCClient(client, policy, 4)`；JetStream 初始化失败时顺手关掉连接（Mod 原来会漏） |
| `dataengine/engine` | `AssemblyConfig` / `AssemblyDeps` / `Assemble(deps, cfg)`；`Assembly{Store}`，`Start(ctx)`、`Runtime()`、`Shutdown(ctx)`；`jetStreamOutboxPublisher` 搬入 | Provide 后半段（MongoStore、远端投影绑定与 applier 断言）与整个 `Start`（EnsureInfrastructure、EnsureStream、WAL、Projector、OutboxStore、Publisher、OutboxWorker、Runtime、链式 Close）及 runtime 的读写锁 |
| `saga` | `AssemblyConfig`（Store / Engine / Prefix / Stream / Completions / Starts）、`Assemble(mongo, js, cfg, definitions...)`；`Assembly{Store, Transport, Engine}`，`Start(ctx)`、`Stop(ctx)`、`Running()`、`ConsumersClosed()`、`RunError()`；`drainSubscriptions` / `subscriptionClosed` 搬入（连同其测试） | Provide 的三件构造 + 定义注册；`Start` 的基础设施、两个 durable 消费者、引擎循环 goroutine；`StopWithContext` 的 drain → cancel → engine.Stop → 等循环退出 |
| `remoteentity` | `AssemblyDeps` / `MongoBackendConfig` / `Assemble(deps, cfg, sid, mongoCfg)`；`Assembly{Manager, LockFactory, AtomicStore}`，`Start(ctx, bus)`、`Stop(ctx)` | Provide 的锁工厂、Manager、L2、FatalHandler、Mongo 提交器 + Backend、AtomicStore 断言、RedisMarker；`Start` 的 Validate → BindSync 并启动两个复制器 → Seal → EnsureRemoteStorage → RecoverOutbox → StartFinalizer 与回滚；`Stop` 的 StopFinalizer + 停复制器 |

room / mongo 本来就薄（只调已导出构造器、不碰 `Raw()`），不动。bus 的装配（`bus.New` / `EnableJetStreamRPC` / `EnableReliable` / `RegisterAdminCommands`）读的是 registry 配置、调的都是导出 API，留在 NatsMod。

## 3. kit 侧

- 八个 Mod 文件改为持有 `asm *<pkg>.Assembly`；`Init` 原样；`Provide` = Lookup + `Assemble` + `RegisterAll` + 健康注册；`Start` / `Stop` 转交。构造器签名（`NewNatsMod(codec)`、`NewEtcdMod()`、`NewRedisMod()`、`dataengine.NewMod(opts...)`、`saga.NewMod(defs...)`、`NewRemoteEntityMod(sid, opts...)`）与 codegen catalog 里的写法不变。
- dataengine Mod 保留 `modConfig` 与私有字段名（`cfg.projector` / `cfg.wal`），Mod 级配置测试不改。
- **新护栏** `assembly_boundary_test.go`（kit 根，`package kit_test`）：解析根模块全部非测试 Go 文件，任何无参 `.Raw()` 调用即失败；附带探测器自测。
- kit `saga/mod_test.go`（只测 `drainSubscriptions`）随函数搬到 core `saga/assembly_test.go`。
- `scripts/perf/dataengine.sh` 从 kit 搬到 core 并改包路径（它指向的 `./nestwal` 在 kit 已不存在）。

### 3.1 行为差异（全部是错误可见性方向，无功能变化）

| 处 | 之前 | 现在 |
| --- | --- | --- |
| etcd Mod 停止 | `client.Close()` 的错误被丢 | `Assembly.Close` 把注销与关闭错误 `errors.Join` 后返回 |
| nats Mod Provide | JetStream 初始化失败时连接留到 Stop 才关 | `Assemble` 失败即关连接 |
| redis Mod Provide | `NewRedisClient` 不校验地址（Init 有默认值，实际不会为空） | `Assemble` 与 `NewClient` 同样校验 |
| 错误文案前缀 | `"xxx mod: …"` | 由 core 拒绝的部分改为 `"xxx: …"`；Mod 自己的查找错误文案不变 |

## 4. 门禁（本机 macOS，2026-09-08 晚）

| 检查 | 结果 |
| --- | --- |
| core `GOWORK=off go build ./... && go vet ./...` | 0 |
| core `GOWORK=off go test -count=1 ./...` | 全绿 |
| core `-race`：redis/driver、etcd/driver、nats/driver、dataengine/engine、saga、remoteentity | 全绿 |
| kit（go.work 指本地 core）`go build ./... && go vet ./... && go vet -tags integration ./... && go test -count=1 ./...` | 全绿（含新护栏 `TestKitModsDoNotReachForRawDriverHandles`、`TestEveryModDependencyNamesAKitMod`、dataengine Mod 级 Provide → Start → Stop 测试） |
| codegen `scripts/source-head-check.sh full <core> <kit>` | full OK（game 模板 + access player + transport tcp + skill + saga 的 planet 工程对本地两仓源码 build / vet / test） |
| Mod 级真实集成（`dataengine-env.sh test` 五切片） | 本机 docker daemon 未运行，未在本机跑；kit CI `integration`（隔离 Mongo 副本集 + NATS 集群，dataengine real / failover / toxic、saga、remoteentity、nats JetStream RPC toxic）与 `service-redis`（12 个服务，-race）作业**全绿**（run 34241871800；首跑 integration 在安装 nats-server 时下载 500，重跑绿） |

## 5. CI 与发版

- core main `f928bb2`：`ci` 全绿（linux-quality 含 -race、vet integration、windows、release-hygiene）。
- kit main `17b6ea6`（go.mod → core `v1.15.0-alpha.1`）：`ci` 六个作业全绿（released-core ubuntu / windows、local-core-source、integration、service-redis、release-hygiene）。
- codegen：本地 `source-head-check.sh full` 对两仓工作树通过；CI 的 `framework-compat` source-head lane 会在下次 push / 每日 03:17 用 main 源码跑。

**正式发版（2026-09-09，顺序同 P6）**：core `v1.15.0` → `16dbc96`（tag CI 绿）；kit go.mod → core v1.15.0（`af9204e`，main CI 六作业绿）→ `v1.14.0`（tag CI 绿）；codegen 清单 core v1.15.0 / kit v1.14.0、source-head 默认 pin 同步（`a4c9e93`，ci / upgrade-compat / security / framework-compat 六 lane 全绿）→ `v1.15.1`（framework-release 34292627212 全绿，GitHub Release 8 个资产）。proxy 的 kit `@v/list` 在发版后约半小时内尚未列出 v1.14.0，但精确版本可解析，release-smoke / framework-compat 均未受影响。

原计划步骤：core `scripts/pretag.sh v1.15.0` → tag（含 U-0107 的 B-14 修复与本批 Assemble*）→ kit go.mod 升 v1.15.0、`pretag.sh v1.14.0` → tag → codegen `ci/framework-release.yaml` 清单改 core v1.15.0 / kit v1.14.0、`scripts/source-head-check.sh` 默认 pin 同步、`pretag.sh v1.15.1` → tag → 看 `framework-release` 与 `framework-compat` 六条 lane。
