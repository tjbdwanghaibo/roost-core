# P3：kit 瘦身与 roost-service 并入

批次：P3（收敛方案 §4）
状态：完成（kit `consolidation` 分支 `434c3ee`，独立于 go.work 构建、vet、`-tags integration` vet、全部测试通过；分支 CI 结果见下）
前置：P2 全部批次；core 预发布 tag `v1.14.0-alpha.1`～`alpha.4`

## 1. 瘦身：删除 18 个已下沉实现，Mod 胶水改用 core 导出 API

### 1.1 方法

先用编译器列清单：把每个包除 Mod 胶水外的文件删掉，`go test -gcflags=-e -run '^$'` 列出全部 `undefined:` / `cannot refer to unexported`。八个 Mod 包共需要 core 导出：

| core 包 | 导出（原名 → 新名） |
| --- | --- |
| nats | `natsClient`→`Client`、`newNatsClient`→`NewClient`、`jetStreamClient`→`JetStreamClient`、`newJetStreamClient`→`NewJetStreamClient`、`rpcClient`→`RPCClient`、`newRpcClient`→`NewRPCClient`、`clientOptions`→`ClientOptions`（字段 `IgnoreDiscoveredServers`） |
| redis | `redisClient`→`Client`、`newRedisClient`→`NewRedisClient`（`NewClient` 已存在、返回 IRedis）、`distLockFactory`→`DistLockFactory`、`newDistLockFactory`→`NewDistLockFactory`；新增 `Client.Raw()` |
| mongo | `mongoClient`→`Client`、`newMongoClient`→`NewClient`、`validateDeployment`→`ValidateDeployment` |
| etcd | `etcdClient`→`Client`、`newEtcdClient`→`NewClient`、`discovery`→`Discovery`、`newDiscovery`→`NewDiscovery`、`electionFactory`→`ElectionFactory`、`newElectionFactory`→`NewElectionFactory`；新增 `Client.Raw()`、`Discovery.SetRetryIntervals` |
| remoteentity | `remoteEntityManager`→`Manager`、`newRemoteEntityManager`→`NewManager`、`newRedisMarker`→`NewRedisMarker`、`newRemoteSnapshotL2Store`→`NewSnapshotL2Store`、`newRemoteSyncer`→`NewSyncer`、`newVersionedLockFactory`→`NewVersionedLockFactory`、`remoteInterestReplicaStore`→`InterestReplicaStore`、`remoteSnapshotReplicaStore`→`SnapshotReplicaStore`、`syncTopicRemote*`→`SyncTopic*`；方法 `SetFatalHandler` `FatalError` `ValidateDependencies` `SealDependencies` `RecoverOutbox` `StartFinalizer` `StopFinalizer` `WrapperCount` `SetSyncer`；**新增装配辅助** `Stats()`、`Backend()`、`BindSync(bus)`（`assembly.go`），Mod 不再触碰 `remote`、`backend`、同步器内部字段 |
| dataengine/engine | `newRuntime`→`NewRuntime`、`pipelinedRuntimeConfig`→`PipelinedRuntimeConfig`、集合名常量 `Outbox/Receipt/TransactionCollection`、`Projector.ReplayPass`、**测试缝** `Projector.OverrideAck`（"投影成功、checkpoint 丢失"重启场景的故障注入，生产装配不调用） |
| saga | `JetStreamPublisher.Client()` |
| room | （只需已导出的 `NewJetStreamSyncBus` / `NewNatsSyncBus` / `JetStreamSyncConfig`） |

这些是**最小导出集**：Mod 仍是编排者。方案 M-01 期望的"Core 提供有限的构造 / 注入 / 封存 / 运行 / 关闭接口、Mod 只转配置"在 remoteentity 上做到了一半（`Stats` / `Backend` / `BindSync`），nats / etcd / dataengine 的 Mod 仍直接拿 `Raw()` 和内部构造器。**P3b（Mod 瘦身）**记为后续：把各 Mod 里的编排逻辑提成 core 的 `Assemble*` 函数。

### 1.2 kit 侧结果

- 删除目录：nestwal、mongo/mongotest、versionstore、servicerpc、nettransport、spatial、actionflow、ai、gateway、robot、syncstream、lockstep；各 Mod 包只剩胶水文件 + Mod 级测试。
- 留在 kit 的 Mod 级真实集成测试（dataengine 的 real_fixture / real / failover / toxic、remoteentity 的 Mongo 提交器集成、nats 的 JetStream RPC 故障）全部改用 core 导出缝编译通过（`go vet -tags integration ./...` 干净）；随代码搬到 core 的测试助手（`assertWALReplayIDs` / `assertWALReplayCount`）在 kit 侧复制了一份。
- 新增 `dependency_boundary_test.go`：kit 禁 import codegen、cube-*、旧 roost-skill / roost-service 路径。
- go.mod：`roost-core v1.14.0-alpha.4`（`go mod tidy` 不走 go.work，所以 P3 必须有可解析的 core 预发布 tag；alpha 递增四次是因为导出集是靠编译器逐轮发现的——下次同类工作先做完整清单再打一个 tag）。sumdb 对新 tag 短暂 404，用 `GOPROXY=direct GONOSUMDB=github.com/tjbdwanghaibo/roost-core` 拉取。
- 留 kit 的包：mods、manager、ops、nest、lock、configdata、statslog（后两者 Mod 与实现同文件，映射表已改 keep）。

## 2. roost-service 并入 kit/service

- `git subtree add --prefix=service <roost-service> main`（整仓历史）；删 `.github`、`scripts`、`go.mod`、`go.sum`、`.gitignore`；`CHANGELOG.md` → `kit/docs/history/SERVICE_CHANGELOG.md`；`README.md` 留 `service/`。
- `servicemods` 的常量与助手（`All` `Duration` `KeyPrefix` `Redis` `RequiredDuration` `Secret`）并入 `kit/mods`（文件前缀 `service_`），与 kit/mods 原有导出零冲突；包删除。
- import 改写 97 个文件：`roost-service/<pkg>` → `roost-kit/service/<pkg>`、`servicemods` → `mods`、`roost-kit/versionstore` / `servicerpc` → `roost-core/...`、`roost-kit/redis` 按文件区分（只用 `NewClient` / `IRedis` 的改 core，用 `NewRedisMod` 的留 kit）；21 个文件原本同时 import `kitmods` 与 `servicemods`，合并后去重为一个 `mods`。
- 服务的 CI Redis 作业移植为 kit `ci.yml` 的 `service-redis` job（只跑 `./service/...`，`-p 1`，拒绝 REDIS_ADDR 跳过，`-race` 三个共享 Redis 的包）；守卫测试 `ci_test.go` 挪到 kit 仓根 `ci_service_redis_test.go`（`package kit_test`）。actionlint 通过。
- `examples/split` 属于根模块，随包一起编译通过。

## 3. 门禁（本机 macOS，GOWORK=off）

`go build ./...` 0；`go vet ./...` 0；`go vet -tags integration ./...` 0；`go test -count=1 ./...` 全绿（含 service 12 包、边界测试、CI 守卫）。分支 CI（linux-quality / windows / integration / service-redis）结果以 GitHub 为准，写在下一次记录。

## 4. 未验证项 / 风险

- 故障矩阵五切片（`scripts/integration/dataengine-env.sh test`）在新路径上尚未重跑——P5 前必须做（需要本机 docker 或 CI integration job）。
- Mod 编排逻辑仍在 kit（见 1.1 末），P3b 待做。
- `_impl` 后缀文件名、`engine` 包名对业务工程的影响由 P4 升级器与 golden 用例覆盖。

## 5. 下一步唯一动作

P4：codegen 模板改新路径、清单 schema 2、`roost upgrade --consolidate`、`framework-compat` 指向 `consolidation`。
