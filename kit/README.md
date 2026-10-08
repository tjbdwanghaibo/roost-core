# roost-kit

> **这里已经是 roost-core 的一部分**（装配层，模块路径 `github.com/tjbdwanghaibo/roost-core/kit/…`）。独立的 roost-kit 仓库已归档；下文里说"本仓库"的地方指的是这个目录。方案见 [三仓合一仓](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md)。

`roost-kit`（仓库目录名 `roost-kit`，Go 模块 `github.com/tjbdwanghaibo/roost-kit`）是 roost 框架的**装配层**：核心实现在 `roost-core`（引擎、契约、领域算法、基础设施客户端），`roost-kit` 负责把它们装成 `app.Mod`——读配置、取依赖、注册 capability、接生命周期与健康 / 日志——并提供通用服务（account / mail / match / chat / session / global）的服务器与客户端接入。三层的分工是：**Core 核心实现，Kit 装配与使用便利，Codegen 代码生成**；Kit 里目前仍持有的领域实现（通用服务的状态机与存储）正按 roost-core `docs/bug/REVIEW-2026-09-16-04.md` 第 7 节的 ARCH-01..04 分批下沉，本 README 的组件表以当前目录为准。

三级阅读路径：完全新手从 [Roost 五分钟快速开始](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/QUICKSTART.md) 开始；熟练开发者阅读 [完整使用说明](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/USER_GUIDE.md) 后按本 README 查具体 Mod；框架维护者阅读 [实现原理](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/INTERNALS.md)、[生产部署](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/DEPLOYMENT.md) 与本 README 的实现章节。

```text
业务服务（游戏服 / world 服 / 自定义服务）
  └─> roost-kit   装配层：把 core 的实现装成 app.Mod，并接入通用服务（本仓库）
        └─> roost-core   Mod 生命周期、app.Registry、稳定接口与 Nest 执行引擎
```

---

## 1. 组件总览

| 组件 | 解决什么问题 | 外部设施 | 何时用 |
| --- | --- | --- | --- |
| `dataengine/` | 统一的 Nest 事务持久化：本地 WAL、Put/Patch/Delete Mongo CAS projection、receipt/effect outbox、聚合 load、schema migration、Saga native step 与 Remote commit | 本地独占磁盘 + MongoDB replica set + NATS JetStream | 新服务与完成迁移的 Entity 服务 |
| `nest/` | 装配实例级 core Nest 引擎，并从 Data Engine 取得唯一 transaction committer | 无 | 所有 Nest 服务 |
| `redis/` | Redis 客户端、pipeline、pub/sub；`EvalDurable`/`EvalBatchDurable` 保留为通用 durable Lua 能力；App 单实例锁的后端 `SingletonStore`。core 的分布式锁（`SetNX`、`AutoExtendLock`）仍在 `roost-core/redis`，Mod 不再以 capability 发布锁工厂 | Redis | 缓存、去重、单实例锁后端 |
| `mongo/` | MongoDB 客户端、collection、session/事务封装。写关注硬编码 majority+journal、事务读关注 snapshot；**启动预检拒绝无逻辑会话的部署（单机 mongod 起不来），`require_replica_set` 可再收紧**；索引冲突重建需全局与单索引双开关 | MongoDB（副本集或分片集） | 一切持久化 |
| `nats/` | NATS 连接、RPC（同步 Call 按调用方期限计时、没有期限时单次 5s，带 jitter 退避，RR-20261006-73 / CallAsync 固定 5s）、JetStream（消费端 Nak 指数退避、Drain 与 Stop 语义分离）、可靠 Bus（inbox 去重 + 死信，**需 redis Mod 且装配顺序在前**）；`nats.rpc.transport=jetstream` 可切 JetStream RPC | NATS/JetStream（Provide 硬依赖 admin registry） | 服务间消息 |
| `etcd/` | 服务注册/发现（租约丢失自动重注册、停机静默注销）、prefix 本地镜像（一致性快照锚点 + CAS 写 + 订阅隔离：慢订阅者单独踢除、handler panic 容器化）。core 的 `IFencedElection` 选主（CreateRevision 栅栏）仍在 `roost-core/etcd`，Mod 不再以 capability 发布选举工厂 | etcd | 多实例部署的发现与配置镜像 |
| `saga/` | 跨事务域长事务：Mongo 状态机 + outbox + lease fencing + 按操作实例的步骤收件箱（每个操作一份状态文档 + 租约 + 接替，同一操作的各次尝试至多一次生效）；通过 Data Engine effect outbox 从 Nest 事务拉起 saga | MongoDB + NATS JetStream | 跨服务多步业务流程 |
| `syncbus/` | 服务间状态同步总线：`SyncBusMod`（只提供 `ISyncBus`，NATS 或 JetStream 二选一；实现在 roost-core `sync/syncbus/driver`）。客户端方向的实体同步在 roost-core `sync/entitysync`（`Manager`：subject 私有订阅者表、按会话组帧、逐帧准入、持久化水位门槛），见下节 | NATS / JetStream | 服务↔服务的同步消息；实体复制见 `entitysync` |
| `manager/` | `ManagerMod`：一个 Service 的内存单例 manager 生命周期的 **Mod 包装**——Mod 名 `mods.ModManager`、capability 登记、Mod 形状的 Stop / StopWithContext。引擎（按 `DependsOn` 稳定拓扑序启动、逆序停止、启动失败只回滚已成功者、启动中收到 shutdown 中止启动、`Start` 后 `Register` 报错）**在 roost-core/manager.Engine**（M-09） | 无 | 场景注册表、路由表、缓存这类进程内单例逻辑 |
| `lock/` | 进程内锁管理器（per-id 可重入互斥，同 id 同实例）——与 `redis.IDistLock`/`IVersionedLock` 是进程内 vs 跨进程的不同层，不参与"分布式锁二选一" | 无 | 进程内互斥 |
| `ops/` | 运维 HTTP：`/healthz`（存活，恒 200）、`/readyz`（ready 位 + 依赖健康：任一 checker fail 才 503，degraded 仍 200 并在 `degraded_dependencies` 里列出，D1）、`/metrics`（Prometheus 文本，**不鉴权**）、`/admin/*`（token 双通道鉴权，关闭时 404 隐藏）。**默认关闭（`ops.enabled`），默认只监听 127.0.0.1**；Start 里同步 bind，端口被占用即启动失败（NC-230）；admin 命令期限 `ops.admin_timeout`（缺省 10s，到期 504 = 结果未知） | HTTP | 探针、指标抓取与运维命令 |
| `configdata/` | 配置快照热更：首次 Load 失败即启动失败；reload 带 rollback 语义；每次 Load / Reload 计 `configdata.reload.total{result=ok|failed}`，发布后被撤回与运维 Rollback 计 `configdata.rollback.total{trigger=apply_failed|operator}`，失败 / 撤回在 core Store 留 Warn 日志。依赖 roost-core `configdata.DefaultRegistry()` 全局注册表（业务表类型须先注册） | 本地文件 | 配置表热更 |
| `statslog/` | 周期统计 JSONL（每行一个 `StatsRecord`，每次 flush 都 fsync）：runtime + 自动富化 nest/entity 统计（装了对应 Mod 才有）、业务 provider 扩展点（panic 被捕获成记录）、窗口增量 + 累计双报。**默认关闭（`stats_log.enabled`）；entity 统计是 O(N) 全量扫描，interval 勿设太小** | 本地文件 | 周期运行时统计 |
| `mods/` | 全部 capability 名称常量（`mods.ModRedis`、`mods.ModDataEngine`…），含对 core `app.*` 常量的再导出；常量 → 注册者 → 实际类型见 §3.3 | 无 | 业务从 Registry 取依赖时使用 |

以下能力**已在 roost-core**（收敛已合回 main，Kit 不再持有实现，只在 Mod 里装配）：`nestwal`（Data Engine 的 WAL）、`remoteentity`、`syncstream` / `sync/syncbus` / `sync/frame`、`sync/lockstep`、`gateway`、`spatial`、`ai` / `actionflow`、`versionstore`、`servicerpc`、`mongo/mongotest`、`robot`（kit 侧只剩 KCP/QUIC 拨号已随 nettransport 归 core）。用法见各自的 core 包注释与 [roost-core README](https://github.com/tjbdwanghaibo/roost-core/blob/main/README.md)。

---

## 2. 快速启动

### 2.1 独立体验 nestwal：写入 → 崩溃 → 恢复重放

下面的程序不需要任何外部设施，直接演示 WAL 的核心承诺：**Append 返回即持久化；崩溃产生的 torn tail 会在重开时被截断；重放从 ack fence 开始且不重复**。

新建一个空目录，写入 `go.mod`。core 与 kit 均使用已经发布的版本，不需要本地 `replace`：

```go
module nestwal-demo

go 1.25.0

require (
	github.com/tjbdwanghaibo/roost-core v1.10.0
	github.com/tjbdwanghaibo/roost-kit v1.10.0
)
```

框架贡献者需要同时联调 core/kit 工作树时使用 `go work`；不要把本地 `replace` 提交进可发布的 `go.mod`。

`main.go`：

```go
// nestwal 独立演示：写入 → 模拟崩溃（torn tail）→ 恢复重放 → ack 检查点。
// 运行：go run .
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

func record(id byte, entityID int64, payload string) corenest.CommitRecord {
	var txID corenest.TransactionID
	txID[15] = id
	return corenest.CommitRecord{
		ID:         txID,
		Handler:    "demo.add_gold",
		CreatedAt:  time.Now().UnixNano(),
		Durability: corenest.DurabilityStrict,
		Mutations: []corenest.EntityMutation{{
			EntityID: entityID, Resource: "player", Codec: "json",
			Data: []byte(payload),
		}},
		Effects: []corenest.Effect{{
			ID: fmt.Sprintf("demo-effect-%d", id), Topic: "demo.gold_changed",
			Payload: []byte(payload),
		}},
	}
}

func main() {
	dir, err := os.MkdirTemp("", "nestwal-demo-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()

	opts := nestwal.DefaultOptions(dir)
	opts.OnFatal = func(err error) { log.Fatalf("WAL fatal, fence the process: %v", err) }

	// ---- 第一次“进程生命周期”：写入三笔事务后模拟断电 ----
	wal, err := nestwal.Open(opts)
	if err != nil {
		log.Fatal(err)
	}
	for i := byte(1); i <= 3; i++ {
		fence, err := wal.Append(ctx, record(i, int64(i), fmt.Sprintf(`{"gold":%d}`, int(i)*100)))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("append tx#%d -> fence{segment:%d offset:%d}\n", i, fence.Segment, fence.Offset)
	}
	if err := wal.Close(ctx); err != nil {
		log.Fatal(err)
	}
	// 模拟崩溃：向活动段尾部追加半个 frame（断电时常见的 torn write）。
	segment := filepath.Join(dir, "segment-00000000000000000001.wal")
	f, err := os.OpenFile(segment, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := f.Write([]byte{0x52, 0x53, 0x57}); err != nil { // 不完整的 magic
		log.Fatal(err)
	}
	_ = f.Close()

	// ---- 第二次“进程生命周期”：重开时自动截断 torn tail，从 ack fence 重放 ----
	wal, err = nestwal.Open(opts) // Open 内部 scanFramesEnd 截断损坏尾部
	if err != nil {
		log.Fatal(err)
	}
	defer wal.Close(ctx)

	applied := map[int64]string{}
	var last corenest.CommitFence
	replay := func(fence corenest.CommitFence, rec corenest.CommitRecord) error {
		for _, m := range rec.Mutations {
			applied[m.EntityID] = string(m.Data) // 幂等落库（演示用内存表）
		}
		for _, e := range rec.Effects {
			fmt.Printf("replay tx=%s publish effect %s\n", rec.ID, e.ID)
		}
		last = fence
		return nil
	}
	if err := wal.Replay(ctx, replay); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recovered entities: %v\n", applied)

	// 落库+发布成功后推进 ack 检查点（双 slot + generation，fsync 持久化）。
	if err := wal.Ack(ctx, last); err != nil {
		log.Fatal(err)
	}

	// 再次 Replay：扫描从 ack fence 开始，已确认前缀不再出现。
	replayed := 0
	if err := wal.Replay(ctx, func(corenest.CommitFence, corenest.CommitRecord) error {
		replayed++
		return nil
	}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("records after ack: %d (expect 0)\n", replayed)

	// ---- Pipelined 提交：Enqueue 拿 ticket，group commit 后水位线先行发布 ----
	ticket, err := wal.Enqueue(ctx, record(9, 9, `{"gold":900}`))
	if err != nil {
		log.Fatal(err)
	}
	<-ticket.Done() // 组提交 fsync 完成即被唤醒
	if err := ticket.Err(); err != nil {
		log.Fatal(err) // 只可能是 ErrCommitIndeterminate
	}
	fmt.Printf("ticket lsn=%d durable, watermark DurableLSN=%d\n", ticket.LSN(), wal.DurableLSN())
}
```

预期输出（已实际编译运行验证）：

```text
append tx#1 -> fence{segment:1 offset:206}
append tx#2 -> fence{segment:1 offset:412}
append tx#3 -> fence{segment:1 offset:618}
replay tx=00000000000000000000000000000001 publish effect demo-effect-1
replay tx=00000000000000000000000000000002 publish effect demo-effect-2
replay tx=00000000000000000000000000000003 publish effect demo-effect-3
recovered entities: map[1:{"gold":100} 2:{"gold":200} 3:{"gold":300}]
records after ack: 0 (expect 0)
ticket lsn=1 durable, watermark DurableLSN=1
```

> 生产环境不要直接使用裸 `WAL`：应用应装配 `dataengine.Mod`，由统一引擎负责 WAL、Mongo projection、outbox 与 ack。

### 2.2 作为 committer 接入 roost-core Nest（生产装配）

生产装配走 Mod 体系，Data Engine 是唯一持久化引擎；下列骨架中的
`entity.Getter/ManagerAccess` 由业务提供。

```go
import (
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/bus"
	kitdata "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	kitnest "github.com/tjbdwanghaibo/roost-kit/nest"
	"github.com/tjbdwanghaibo/roost-kit/mongo"
	"github.com/tjbdwanghaibo/roost-kit/nats"
	"github.com/tjbdwanghaibo/roost-kit/ops"
)

dataMod := kitdata.NewMod(kitdata.WithEntityAccess(access))

application := app.New("game", "v1.0.0").
	Mods(
		ops.NewOpsMod(), // health/ready/metrics HTTP
		mongo.NewMongoMod(),
		nats.NewNatsMod(bus.JSONCodec{}),
		dataMod,
		kitnest.NewMod(getter), // 自动读取 dataMod 的 lazy committer；recovery 后才 Start
	).
	RegisterServer("game", service)

if err := application.Execute(); err != nil {
	panic(err)
}
```

对应的最小配置（各键在各 Mod 的 `Init` 中读取）：

```yaml
sid: 1
persistence:
  engine: dataengine
ops:
  enabled: true        # ops 默认关闭：不写这行，/healthz 等端点不会监听任何端口
mongo:
  uri: "mongodb://127.0.0.1:27017"   # 必须是副本集或分片集（事务前提），单机 mongod 会被启动预检拒绝
nats:
  url: "nats://127.0.0.1:4222"

dataengine:
  wal:
    dir: "data/wal/dataengine/1" # 缺省为 data/wal/dataengine/<sid>
    group_commit_interval: 10ms
  projection:
    batch_records: 256             # 每次重放最多记录数
    batch_bytes: 4194304           # 本地单/多 DAO 批量事务逻辑字节上限，缺省 4 MiB
    checkpoint_records: 256        # 安全成功前缀的确认阈值；1 恢复逐单元确认
    checkpoint_interval: 20ms      # 投影单元之间检查；不是固定等待窗口
    max_unacked_records: 0         # 0 不限；按业务容量配置未确认事务准入上限
    warn_unacked_records: 0        # 0 无独立预警，有上限时达到上限仍预警
  outbox:
    max_pending: 1000000
    max_oldest_age: 30m
nest:
  pipelined:
    allowlist: ["player.consume", "player.reward"]  # 允许 DurabilityPipelined 的 handler
    async: false                                    # Phase 2：异步完成
```

handler 侧通过 `HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict}`（或
`DurabilityPipelined`）声明持久化级别；成功回包时事务已进入 Data Engine WAL。Mongo
projection 完成后推进 WAL ACK，effect 由独立 outbox worker 投递，因此 NATS 故障不会
阻塞 Entity 落库。旧 Checkpoint 数据导入说明见 roost-core
`docs/DATA_ENGINE_MIGRATION.md`；运行时不存在回切到第二写引擎的路径。

`dataengine.projection.batch_records` 限制一次 WAL 重放读取的记录数，也限制普通批投影
segment 的记录数；`dataengine.projection.batch_bytes` 只限制普通批投影 segment 的保守逻辑
字节数。本地多 DAO 和单 DAO 可以共享 Mongo 批量事务，每笔业务保持独立事务身份。
Remote、receipt、effect、migration 固定逐笔处理，超过字节上限的单条记录也单独执行。
顺序始终保持 WAL 先后；冲突先回滚整个批次，再逐笔判断，只确认成功前缀。

有持久事务 marker 的路径可按数量/时间合并 checkpoint，在批次末尾、失败、held 或取消时
确认已成功前缀。单 DAO 的单记录快路只保存最后事务 ID，因此仍立即确认；未知 Store 也保留
逐单元确认。合并不改变 WAL fsync 或 Nest durable ticket，ACK 从不越过失败记录。
`CheckpointInterval` 不主动等待凑批，检查发生在存储调用结束后；单次阻塞由 context 控制。

准入上限覆盖 Projector.Commit / Enqueue / CommitSystem，在写入 WAL 前原子预留。
达到上限返回 `engine.ErrProjectionBackpressure`，Nest 回滚当前事务，调用者可稍后重试；
不会触发 fatal。ACK 成功后归还容量；健康检查到达预警阈值返回 Degraded，并输出
`projection_backlog_warning` / `admission_rejected`。生产 Runtime 在 ready 前先恢复旧 WAL，
该限制约束 ready 后的在途业务事务，不替代 WAL 的磁盘/年龄保护。默认不启用限流，示例中的
0 不是推荐生产值，应结合实测落库速度与允许积压延迟设定。负值或预警高于非零上限会拒绝启动。

---

## 3. 核心概念

### 3.1 Mod 装配与配置体系

所有组件实现 `roost-core/app.Mod` 四阶段生命周期：

```text
Init(cfg)   读取 viper 配置、构造参数（不启 goroutine、不访问远端）
Provide(r)  构造对象并向 app.Registry 注册 capability
Start()     建连、注册 health、启动后台任务（失败必须向上传递）
Stop()      停后台任务、flush、关连接（保证停服收敛）
```

- **依赖声明**：硬要求使用 `DependsOn()`，缺失立即失败；可选集成使用 `OptionalDependsOn()`，依赖存在时自动排到当前 Mod 之前、缺失时忽略。框架统一做拓扑排序和环检测。NATS→Redis（reliable 开启时使用）与 Nest→Remote Entity 已使用可选依赖契约，业务不再依靠 `Mods(...)` 书写顺序碰运气。
- **capability 查询**：业务永远通过 `app.Lookup[接口类型](registry, mods.ModXxx)` 取依赖，只依赖 `roost-core` 接口，不触碰 Mod 私有实现。名称常量集中在 `mods/name.go`；泛型参数容易写错的常量见 §3.3 的对照表。
- **配置**：每个 Mod 在 `Init` 中读取自己的配置命名空间（`dataengine.*`、`nest.pipelined.*`…），零配置时使用可运行的默认值。`persistence.engine` 省略或设为 `dataengine`；其他值会在 Init 阶段被拒绝。

### 3.2 配置命名空间与跨键不变量

各 Mod 的配置命名空间（键的完整清单以各 `Init` 为准）：`redis.*`（含 `cluster_addrs`，逗号分隔串或 YAML 列表即切 Cluster，RR-20261005-NC-190）、`mongo.*`（含 `require_replica_set`、`mongo.index.allow_recreate`）、`nats.*` + `nats.rpc.*`（JetStream RPC）+ `nats.reliable.*`（可靠 bus）、`etcd.*`（含 `advertise_addr` 的 server_type 感知回退）、`dataengine.*`（WAL、projection、outbox、effect stream）、`saga.*`、`nest.*` + `nest.pipelined.*`（`allowlist`/`async`/`async_workers`/`async_queue_capacity`）、`sync.*`、`remote_entity.*`、`ops.*`、`stats_log.*`、`config_data.dir`。

**跨键不变量（违反即启动失败或语义破坏）**：

| 不变量 | 后果 |
| --- | --- |
| `saga.completion_receipt_ttl > saga.stream_max_age` | Init 期拒绝（收据先于流过期会破坏去重） |
| saga 消费者 `ProcessTimeout < AckWait`（jetstream/step/start 三处） | 启动拒绝（否则处理未完 ack 先过期 → 重投风暴） |
| saga 步骤 `Timeout < saga.DefaultStepAckWait`（30s；`saga.step_defaults.timeout`、`saga.steps.*.*.timeout`、定义里写死的 `Timeout`） | App 启动前 / Init 期拒绝并点名键（否则 handler 还在跑时命令被重投给另一个消费者，RR-20261006-46） |
| mongo 部署必须有逻辑会话（副本集/分片集） | 启动预检失败；`require_replica_set=true` 额外拒绝 mongos 之外的无副本集名部署 |
| ops `admin_enabled` 必须配非 `dev-` token（或显式 `allow_dev_token`） | Init 期拒绝 |

### 3.3 capability 常量 → 注册者 → 实际类型

`app.Lookup[T]` 的泛型参数必须匹配注册的实际类型，最容易写错的几个：

| 常量（字符串值） | 注册者 | 实际类型 |
| --- | --- | --- |
| `ModNest`（`nest`） | nest Mod | `*corenest.NestMgr` |
| `ModDataEngine`（`dataengine`） | dataengine Mod | `*dataengine.Mod`（同时提供 lazy Nest committer；`WaitEntityProjection(ctx, entityID)` 是跨进程交出实体所有权前的本进程投影屏障，慢路径调用，RR-20260926-31） |
| `ModRedisVLock`（`redis.versioned_lock`） | **remote_entity Mod**（不是 redis Mod） | `fredis.IVersionedLockFactory` |
| `ModRemoteEntityAtomicStore`（`remote_entity.atomic_store`） | remote_entity Mod | `AtomicCommitStore`（Data Engine projection 消费） |
| `ModEntityRuntime`（`entity.runtime`） | nest Mod 顺带注册（已存在则不覆盖） | entity getter（statslog 消费） |
| `ModSaga`（`saga`） | saga Mod | `*coresaga.Engine` |
| `ModConfigData`（`config_data`） | configdata Mod | `*fconfigdata.Store` |
| `ModManager`（`manager`） | manager Mod | `*manager.ManagerMod`（包装 roost-core/manager.Engine） |
| `ModSyncBus`（`syncbus`） | syncbus Mod | `fsyncbus.ISyncBus` |

其余（`ModRedis`/`ModMongo`/`ModNats`/`ModNatsJetStream`/`ModNatsRpc`/`ModBus`/`ModEtcd`/`ModEtcdDiscov`/`ModLock`/`ModOps`/`ModStatsLog`/`ModRemoteEntity`）与直觉一致，注册者即同名 Mod。`ModRedisLock`（`redis.lock`）与 `ModEtcdElection`（`etcd.election`）已删除：“同一服务类型 + sid 只跑一个进程”由 App 单实例锁（`app.Singleton`，见 USER_GUIDE §2 单实例锁）提供；键级去重 / 选主直接用 core 的 `redis/driver.Assemble().Locks`、`etcd/driver.Assemble().Election` 自建。

### 3.4 停机语义

| Mod | StopWithContext | 预算 |
| --- | --- | --- |
| dataengine | ✅（声明了 `app.ModStopperWithContext` 与 `app.ModStopBudgetProvider`） | `dataengine.shutdown_timeout`（默认 30s）经 `StopBudget` 声明给 App，在 `shutdown.total_timeout` 内优先分配；先收敛 projection，再停止 outbox claim |
| ops / etcd | ✅ | 默认 5s |
| nats | ✅ | bus → rpc → Drain，超时强制 Close |
| statslog | ✅ | 有界：卡住的 provider 不会挂死停服（文件留给后台关闭） |
| saga / nest | ✅ | App 传入统一 `shutdown.total_timeout`；直接调用兼容 `Stop()` 才使用 background context |

App 的分配规则（RR-20260926-42，RR-20260926-51 修订）：Service.Shutdown 之后，Mod 逆序停止，每停一个按剩余时间重新规划。
实现了 `app.ModStopBudgetProvider`（`StopBudget() time.Duration`，<=0 视为未声明）的 Mod 先按声明值分配，上限为
“剩余时间 − 每个未声明 Mod 的固定保底（3s）”；未声明的 Mod 均分其余（每个不低于 3s）。只有声明值之和超过上限时才按比例
缩放声明值，此时未声明的 Mod 恰好拿保底，声明的 Mod 记 Warn `mod stop budget scaled down`；保底是固定下限、不参与缩放，
所以一个很大的声明值不会把先停的 Mod 压到接近零而中断关闭链。剩余时间连每个 Mod 一份保底都给不起时全部均分并记 Warn
（`cannot cover the per-mod floor`）。声明预算之和永远不超过剩余总时长。服务专属 Mod 先于共享 Mod 停止，规划时会为之后的
共享 Mod 保留它们声明的预算。没有 Mod 声明预算时与原先“剩余 / 剩余 Mod 数”相同；没有总截止时间的兼容路径（启动失败回滚）下，
声明的 Mod 用声明值，其余 5s。某个 Mod 超时后仍按既有语义停止后续关闭、保留其余 Mod 的资源。

生成配置的 `shutdown.total_timeout` 按服务计算（RR-20260926-66）：声明预算之和（`dataengine.shutdown_timeout: 30s`；托管 player TCP 接入的服务另有生成的
TCP Mod 声明的 `player_access.tcp.shutdown_timeout: 10s`，RR-20260927-05）+ 3s × 其余 Mod 数
+ 5s（Service.Shutdown 在同一时限内、先于 Mod 运行），Mod 数取该服务进程实际注册的全部 Mod（含共享 Mod）；部署的终止宽限期
（k8s `terminationGracePeriodSeconds`、compose `stop_grace_period`、systemd `TimeoutStopSec`、`deploy/dev/run.sh`）为
`max(公式值, 该服务配置里实际生效的 total_timeout) + 5s`——配置手改、旧格式或缺键（App 兜底 30s）时宽限期跟随配置，永远不低于配置 total + 5s。
game-demo 缺省 Mod 集的 game 服务 25 个 Mod：114s / 119s（`-mods configdata,mongo,nats,dataengine,nest` 时 23 个：108s / 113s），dataengine 拿满 30s、player TCP 拿满 10s、其余每个 Mod ≥ 3s、不告警
（RR-20260927-05 之前 TCP Mod 不声明、只得 3s 保底，两组值为 107s / 112s 与 101s / 106s）；框架服务（6 个 Mod、无声明）23s / 28s。
自定义时保持 `total_timeout ≥ 声明预算之和 + 3s × 其他 Mod 数`（`config.<svc>.yaml`、`.prod.example.yaml`、k8s secret 示例逐份判定，任一份不足时 `roost project doctor` WARN 并指明文件，建议值按该份配置的 `dataengine.shutdown_timeout` 计算，0 或负的时长按运行时的 30s 兜底判定；示例解析失败或时长非法时 WARN 只指明文件与键）；改大 total 后执行 `roost project sync`，
部署模板的宽限期随之放大（模板宽限期低于配置 total + 5s 时 doctor FAIL）。
RR-66 之前生成、仍为 `total_timeout: 60s` 的工程：Mod 多的服务（game-demo game）每次停机告警且个别 Mod 低于保底，应改为生成值；
重新 sync 后部署模板的宽限期不低于 65s（跟随配置里的 60s），不会被静默缩短。
RR-51 之前生成、仍为 `total_timeout: 30s` 的工程，dataengine 在 5 / 7 / 10 个 Mod 时得 21s / 18s / 12s 并告警。

### 3.5 Durability 管线全景（nest 事务 → 磁盘 → 数据库）

一笔 Nest 事务从 handler 返回到最终落库经过如下阶段（Strict 与 Pipelined 只在前两步不同）：

```text
handler 成功返回
  │  持实体锁
  ├─ Strict:    Committer.Commit → WAL.Append —— 在锁内等待本批 fsync 完成
  ├─ Pipelined: Committer.Enqueue → WAL.Enqueue —— 锁内只做“可拒绝检查+拿 LSN+入队”，
  │             立即解锁；返回 CommitTicket
  ▼
group commit（nestwal/wal.go writerLoop）
  批量聚合（BatchDelay/BatchMaxRecords/BatchMaxBytes）→ 一次 write + fsync
  ▼
durable 水位线发布（resolveDurableLocked）
  DurableLSN 先推进，随后才唤醒 ticket 等待者
  ▼
外化闸门（externalization gates）
  回包 / AfterCommit 等动作 gate 在 durableLSN >= tx.LSN
  ▼
Data Engine projector（dataengine/projector.go）
  从 ack fence 顺序 Replay → MongoStore 版本 CAS/事务投影
  · mutation + receipt + effect staging 原子落 Mongo
  · NATS publisher 不在 WAL ACK 路径
  ▼
ack 检查点推进（nestwal/checkpoint.go）
  双 slot + generation + fsync；已确认前缀此后不再重放，旧 segment 可回收
```

关键点：**WAL 是本地 commit point，Mongo projection 是 ACK 前置条件，JetStream
publish 不是**。Mongo 事务先把 mutation、receipt 和 effect outbox 原子 staged；独立
publisher 再按 EffectID 至少一次投递。NATS 停机只增加 outbox backlog，不阻塞 WAL ACK。

设计文档：`roost-core` 仓库的 `NEST_TRANSACTION_WAL.md` 与 `NEST_PIPELINED_COMMIT.md`（中文，含正确性论证）。

---

## 4. 关键实现细节

每条都给出源文件指引，读代码时可对照。自 2026-09 起大部分实现已迁入 roost-core，路径以 `roost-core/` / `roost-kit/` 前缀标明所在仓（ARCH-04）；kit 里留下的是 Mod 装配、配置读取与 capability 登记。

### nestwal（roost-core）：WAL 与提交

- **双 CRC frame**（`roost-core/nestwal/wal.go` `encodeFrame`/`scanFramesFrom`）：20 字节 frame 头含 payload CRC32 和头自身 CRC32。头 CRC 保证长度字段可信（否则一个损坏的 length 会让扫描越界误判），payload CRC 保证内容可信。
- **torn-tail 截断**（`roost-core/nestwal/wal.go` `openActive` → `scanFramesEnd`）：重开时以 `allowTornTail=true` 扫到最后一个完整 frame，`Truncate` 掉尾部残缺字节——断电时最后一笔未 fsync 的写入被安全丢弃，且只可能丢“未向调用方确认”的后缀。
- **group commit**（`roost-core/nestwal/wal.go` `writerLoop`/`collectBatch`/`processBatch`）：单写线程聚批，一次 `write` + 一次 `fsync` 摊薄毫秒级 fsync 成本；`GroupCommitInterval` 定时器兜底刷新异步写。
- **Enqueue ticket 与唯一拒绝点**（`roost-core/nestwal/wal.go` `Enqueue`）：Pipelined 的 `Enqueue` 在实体锁内被调用，因此把**一切可拒绝检查**（编码、大小、容量预约 `reservedBytes`、terminal 状态、队列准入）同步做完；`enqueueMu` 让 LSN 分配与入队原子，保证 LSN 序 = 物理日志序。入队成功后调用方即可解锁——之后唯一可能的失败是 ticket 上的 `ErrCommitIndeterminate`。`processBatch` 对已预约请求**永不拒绝**（调用方已凭准入放弃了回滚权）。
- **为什么 watermark 必须先于唤醒发布**（`roost-core/nestwal/wal.go` `resolveDurableLocked`）：ticket resolve 是一个承诺——“`DurableLSN` 已覆盖你的记录”。外化闸门在 `Done()` 触发后立刻读水位线，若先 `close(done)` 再推水位线，等待者会读到旧水位线而再次阻塞甚至误判未持久化。因此先 `durableLSN.Store(upto)`，再逐个 `close(ticket.done)`。
- **ack fence 是合法扫描起点**（`roost-core/nestwal/wal.go` `Replay`）：ack fence 记录的是某个 frame 的**结束偏移**，天然落在 frame 边界上，所以重放可以跳过已确认的 segment 和 fence 所在 segment 的已确认前缀（`scanFramesFrom(file, segment, ack.Offset, ...)`），不必每轮从头读全量并重新校验 CRC。
- **ack 检查点：双 slot + generation + 目录 fsync**（`roost-core/nestwal/checkpoint.go`）：`ack-0.chk`/`ack-1.chk` 按 `generation & 1` 交替写，写入走 tmp 文件 → fsync → rename → `syncDirectory`。加载时取 CRC 合法者中 generation 最大的一个：任何时刻允许一个 slot 是 torn 的，另一个 slot 在替换者持久化前不会被碰。ack 丢失最多导致重复重放（幂等吸收），绝不会跳过记录。
- **fsync 不确定 ⇒ 熔断而非重试**（`roost-core/nestwal/wal.go` `Options.OnFatal` 注释、`setTerminal`；`roost-kit/dataengine/mod.go` `onFatal`）：fsync 报错后，内核可能已经丢弃了 dirty page 却清掉了错误标记，重试的 fsync 会“成功”但数据并没有落盘——写入结果从此不可知。所以任何物理写/fsync 失败都被包装为 `corenest.ErrCommitIndeterminate` 并置 terminal：拒绝一切后续写入、以该错误 resolve 所有 pending ticket，并经 `OnFatal` 熔断进程（Data Engine Mod 会 `NestMgr.Fence` + `app.RuntimeFailure.Fail`）。重启后由重放从最后一个 ack 恢复出唯一可信的历史。
- **单写者锁与容量健康**：`writer.lock` 文件锁防止双进程写同一目录（`roost-core/nestwal/lock_unix.go`）；`MaxDiskBytes`/`MaxUnackedAge` 超限时 `Healthy()` 报错，接入 health 后表现为实例不健康而不是静默膨胀。
- **落库与 outbox**（`roost-core/dataengine/engine/projector.go`、`roost-core/dataengine/engine/mongo_store.go`、`roost-core/dataengine/engine/`（outbox publisher））：重放循环在事务仍被 Entity 锁持有时让路（`TransactionReleased` 唤醒）；普通 mutation、Remote commit、receipt 和 effect staging 按需要进入同一个 Mongo session transaction。WAL ACK 在 projection 后推进，outbox publisher 独立 claim/lease/retry，JetStream MsgID 去重只是热路径优化。

### Data Engine（引擎在 roost-core/dataengine/engine，Mod 在 roost-kit/dataengine）：唯一数据引擎

原 Checkpoint 的聚合冷加载、schema migration 和字段级 patch 已由 Data Engine 的
`EntityRepository`、`MigrationRunner`、`Tracker`/`MutationParticipant` 接管。所有业务
修改都进入 Nest transaction；低隔离调用也通过 detached transaction 进入相同 WAL。
Mongo projection 以 version CAS 保证幂等，Saga receipt/effect staging 与业务 mutation
按需要位于同一 Mongo transaction。旧 Checkpoint 包和 Redis 快照 WAL 不再是运行时路径。

`EntityManager.Destroy(deleteFromDB=true)` 由 Data Engine Runtime 注册的唯一 delete
admitter 接管。本地与 Remote Entity 都先把删除写入当前/隔离的 strict transaction；
Remote 路径使用显式 delete intent，并继续经过 ownership marker、lock fence、route epoch。
只有 admission 成功才完成内存移除；rollback 保持实体存活，结果不确定则触发 fail-stop。

### 分布式锁与选主（实现均在 roost-core：redis / remoteentity / etcd；kit 只装配 Mod）

**进程 / sid 级单例不在这里选**：同一服务类型 + sid 只跑一个进程用 App 单实例锁（`app.Singleton`，配置 `singleton.*`），kit 不再以 capability 发布 `redis.lock` / `etcd.election`。下面的原语留给键级互斥与选主。

**先做二选一**（两套锁并存是刻意的分层，不是重复实现）：

| 需求 | 用哪个 | 原因 |
| --- | --- | --- |
| 可容忍偶发双执行的互斥（缓存预热、可去重任务、优化性串行化） | `redis.IDistLock`（可套 `AutoExtendLock`） | 轻量；但**无栅栏**——TTL 过期后旧持有者不自知，存在双执行窗口 |
| 正确性互斥（实体所有权、存储必须能拒绝旧持有者的写） | `remoteentity` 的 `versionedLock` / `etcd.IFencedElection` | fence 计数器独立于 TTL 永不回退，下游按 fence 单调性 CAS 拒旧 |

判据一句话：**如果"锁过期后旧持有者又写了一笔"会造成数据损坏，就必须用带 fence 的那套**；`roost-core/redis/lock.go` 的包注释里写有同样的契约边界。

- **普通 Redis 锁状态机与 `AutoExtendLock` 的 TTL 预算重试**（`roost-core/redis/lock.go`）：每次 acquisition 都生成新 owner token；`SetNX`/释放响应丢失后进入 `uncertain`，在值保护释放完成前拒绝重获，避免旧命令作用于新一代。非法 TTL、空 client/key、重复 Acquire 均 fail-closed。watchdog 每次续期调用都被限时（`extendCtx` 超时 = 续期间隔），瞬时错误不会立即判丢；只有续期间隔超过 TTL 才置 `Err()`，服务器明确答复“不再持有”则立即停止。它仍**不带栅栏**：需要防旧持有者脏写时用 `IVersionedLock`。
- **`versionedLock` 的 fence 与 TTL 分离**（`roost-core/remoteentity/versioned_lock_lua.go`）：fence 来自独立的 `key:fence` 计数器（`INCR`），**永不过期、不共享锁 hash 的 TTL**——若 fence 随锁一起过期，计数器归零后新持有者会拿到更小的 fence，栅栏失效。锁本体是带 TTL 的 hash（owner/version），下游写路径按 fence 单调性做 CAS 拒绝旧持有者。
- **幂等 unlock**（`roost-core/remoteentity/versioned_lock_lua.go` `versionedUnlockLua`）：unlock 的应答可能丢失，重试时若发现 `version == 本次要写的新版本`，证明先前那次 unlock 已生效，返回 2（成功）而非 NotOwned——依赖“unlock 版本按 key 单调唯一”的接口契约。
- **versioned lock 的代际与幂等释放**（`roost-core/remoteentity/versioned_lock.go`）：每次 acquisition 使用新 owner token；每次 `UnlockWithRetry` 使用固定 operation ID，Redis 保存 `last_unlock` 收据。响应丢失后的同一次重试可判成功，但“业务版本恰好相等”不再被误当作释放证明，旧代命令也不能匹配新代 owner。`AutoAsyncTouch` 每次把剩余 PTTL 加 `AsyncTouchExtend`，但封顶 `2*TTL`。
- **选主 fence**（`roost-core/etcd/election.go`）：`Fence()` 返回本候选者 campaign key 的 **CreateRevision**，它随 prefix 的每次领导权更替单调递增。`IsLeader()` 存在固有 stale 窗口（lease 已在服务端过期、客户端未感知），所以领导权敏感写必须携带 fence token 并在存储侧拒绝旧 token。实现上 fence 先于 `isLeader` 标志发布：观察到 `IsLeader==true` 的调用方一定能读到本任期的 token。

### sync/nettransport（roost-core，原 kit replication）：帧复制网络层

- **`AsyncTransport` 是心脏**（`roost-core/sync/nettransport/channel.go`）：每个 session 一个有界 reliable 队列 + 一个 worker，`SendReliable` 入队即返回（拷贝载荷，调用方可复用缓冲），worker 按序排空；队列满报 `ErrReliableBackpressure`，entitysync 把它当作该会话的失败并关会话。**只有这一条 lane**（M-18，2026-09-23）：老 Replicator 用的 latest-only datagram lane 已删——只带变化字段的帧经不起被下一帧替换（RR-20260920-02），而 lockstep 从来直接走裸 `DatagramSender`。
- **下游失败是会话终态**：一次 `SendReliable` 下游出错，该会话队列作废（`ReliableAbandoned` 计数）、`ErrorHandler` 拿到原因、worker 退出并调用下游的 `RemoveSession`，id 随即可复用。**`Close(ctx)` 是有界优雅 drain，`RemoveSession` 是立即取消丢队列**——房间下线要 drain 必须用 Close。`ErrorHandler` 必须迅速返回（panic 被吞并计数，但阻塞会卡住该 session lane）。
- **三个 transport 的能力矩阵**：

  | 维度 | UDPTransport | KCPTransport | QUICTransport |
  | --- | --- | --- | --- |
  | 可靠通道 | **无**（显式返回 `ErrProtocolConfig`，用 `CompositeTransport` 拼） | KCP 流 + 4B 长度前缀 | 每 session 一条惰性单向流 + 4B 长度前缀 |
  | 不可靠通道 | 原生 UDP + AEAD | kcp-go OOB（**构造强制 FEC + BlockCrypt**） | QUIC DATAGRAM（**必须双向协商**） |
  | 加密 | 自带 per-session AES-GCM | kcp `BlockCrypt`（强制） | TLS 1.3（ALPN `roost-nettransport-v1`） |
  | 地址迁移 | 手工（仅 AEAD 验证通过后） | 无 | QUIC 原生连接迁移 |
  | 流控旋钮 | 仅包上限（默认 1232 = IPv6 最小 MTU 减头） | 窗口/nodelay/fastresend/限速/DSCP 全套 | 委托 quic-go |

  选型：只要一条不可靠下行 → UDP（最轻）；同一条 UDP 链路上跑不可靠 + 可靠（lockstep 实时帧 + 追帧）→ KCP（旋钮多、CPU 轻）或 QUIC（443 穿透 + 连接迁移）；异构组合 → `CompositeTransport`。**状态帧**由 `entitysync.Manager` 编码，经 `AsyncTransport` 的可靠队列逐帧准入；客户端接入和就绪由业务层配合 `OpenHeldSession / ReadySession` 完成。**lockstep 输入帧**直接走协议传输的 `DatagramSender`，追帧使用可靠通道。

- **AEAD UDP**（`roost-core/sync/nettransport/udp_crypto.go`、`roost-core/sync/nettransport/udp_transport.go`）：AES-GCM per-session；`SendSalt`/`ReceiveSalt` 每个方向独立且构造时强制不相等（同 key 双向复用同一 nonce 空间会灾难性破坏 GCM）；nonce = salt(4B) + 单调 sequence(8B)，序列号耗尽即拒发；接收端 64 包位图防重放窗口；**地址迁移只在 AEAD 验证通过之后**（`Open` 成功且 `isCurrentRoute`）才生效，未认证的包改不了路由。UDP `Serve` 单飞、handler 返回 error 会终止整个接收循环（业务 handler 必须自行吞掉可恢复错误）。

### sync/entitysync（roost-core）：实体状态到客户端会话（ARCH-10 / M-13，2026-09-22）

一个机制：`entity.SubjectSyncState` 是内容（版本、脏掩码、packer、CommitLSN），`entitysync.Manager` 一个进程一个，拥有全部 subject 与会话；
subject 自己持有订阅者表 `session → {profile, kind, baseVersion}`；每 tick 给每个会话**一帧**（statesync 帧，`Epoch/Tick` 是会话时钟）；
room / AOI / 直接绑定只是"谁订谁"的政策，调 `Subscribe / Unsubscribe`。

- **传输只有两种失败**（`entitysync.Transport`）：`ErrRetryLater` = 整体不可用，tick 作废、脏位保留、无人受罚；其他 = 该会话关闭（`SessionLost` 通知政策）。
  这就是慢消费者策略——不再有 Evict / FailBatch 两套。`AsyncTransport` 适配 nettransport 的可靠通道。
- **持久化水位**（`ManagerConfig.DurableWatermark`）：按 `CommitLSN` 挡**整个 subject**（快照与 delta 一起），水位过了一起走。
- **一次锁内捕获**（`PrepareViews`）：给在线者的 delta 与给新订阅者的快照在同一把实体锁内、同一版本上产生；每个不同 profile 只 pack 一次。
- **编码在副本上**：每帧准入后采纳对应的会话时钟与 ObjectRef 表，部分交付后的重试以全量恢复基线；旧交付不覆盖重置后的会话。
- **没有反向索引**：`session → subjects` 归政策（AOI 的 `observer.visible`）；会话关闭时遍历 subject 删条目。
- **held / ready**（`OpenHeldSession / ReadySession`）：会话可订阅不出帧，Ready 后首帧是新 epoch 的 FrameFull；demo 用 `scene_ready` 消息触发，消掉"快照抢在客户端解码器之前"的竞态。
- **组织方式在 `sync/entitysync/policy`**：`Interest`（spatial AOI + 关系源聚合，直接驱动 Manager，被拒重试）、`Group`（成员全互见）、`Direct`（显式绑定）。各实例通过独立 `SubscriptionSource` 订阅，不碰帧与传输。Group/Interest 关闭只释放本来源，实体真正销毁仍由应用调用 `Manager.Unregister`。
- **会话被 Manager 丢掉而观察者仍在**（`SessionLost` 但玩家还有活连接，例如重连窗口里旧连接写失败）：重开会话后调用 `Interest.Resubscribe(observer)`，
  该观察者持有的全部 pair 在下一次 `Apply` 重说，客户端收到新 lifetime 的全量快照；其他观察者对它的订阅不受影响（RR-20260926-40）。
  是否离场由应用按“该次 Join 的代际仍是当前且确实没有活跃连接”判定，demo scene 是参考实现。
- **离开后立即重新加入**（subject 仍在退役、观察者还欠 ObjectRemove）：用 `Manager.RegisterAfterRetirement(state, done)` 把登记排到退役完成，
  done 里（交给自己的 goroutine）让政策 `Apply` 重说退役期间被拒绝的订阅；会话打开遇 `entitysync.SessionOpenRetryable` 的错误按有界退避重试（RR-20260926-55）。

**`SyncBusMod` 只提供 `ISyncBus`（服务间消息面）**；NATS vs JetStream 的持久性不同但 handler 契约一致（`roost-core/sync/syncbus/driver/nats.go`、`jetstream.go`）：
纯 NATS 至多一次、无确认、故意不实现 `PublishConfirmed`；JetStream 有 durable 与发布确认。
两种传输的 `Subscribe` 都返回 `*syncbus.Subscription`：`Unsubscribe(ctx)` 返回 nil 之后这个订阅没有在途回调、也不会再有新回调，超时返回 ctx 错误、可重试；handler 收到投递 ctx，在 handler 里退订自己要传它（A3 ②，[方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md)）。
配置段为 `syncbus:`（键：transport / prefix / stream / storage / ack_wait / max_deliver / stream_max_age / duplicates / replicas / max_bytes / setup_timeout / publish_timeout）；A4 起不再读取旧 `room:` / `sync:`；syncbus 段按统一声明校验；`transport` 只接受 nats / jetstream（js），写错直接 Init 失败而不是退回普通 NATS（RR-20260926-12）。启动日志 `syncbus mod: started` 的 `transport` 是实际生效的那个。
JetStream 流名：显式 `stream` 优先；未写时由 `prefix` 派生（`driver.JetStreamSyncStream`）——生成配置的 `roost.sync` 与未配 prefix 时的缺省 `roost.room`
仍为 `ROOST_SYNC`（已部署的流与 durable 游标不变），`zz3640.sync` → `ZZ3640_SYNC`，含 `_` / `-` / 大写等的 prefix 追加摘要避免相撞。
共用一个 NATS 的部署用不同的非兼容 prefix 即各有各的流；**例外是兼容映射**：`roost.room`（含未写 prefix）与 `roost.sync` 都映射到 `ROOST_SYNC`，
一个部署用 `roost.room`、另一个用 `roost.sync` 时仍会争同一个流（后启动者改写其 subjects），这类部署要给至少一方显式写不同的 `stream`，
或改用非兼容 prefix（OPEN-ITEMS A11）。启动日志同时输出 `prefix` 与实际 `stream`。写了非默认 prefix、没写 stream 的旧部署升级会换流，
要沿用旧流与游标请显式写 `stream: ROOST_SYNC`（RR-20260926-56）。
要在测试或工具里算出本 Mod 实际使用的流名，调用 `kitsyncbus.JetStreamStreamFromConfig(cfg)`（与 Init 同一规则，含上面的兼容映射）；
只按 `driver.JetStreamSyncStream(prefix)` 推会漏掉 `roost.room` → `ROOST_SYNC`（RR-20260927-35）。

### syncstream（roost-core）：observer 维度的包流

与 entitysync 的分工：**syncstream 内置 Publisher 跑在 ISyncBus 上；skillsync 复用其流语义，宿主可适配到客户端。entitysync 独立跑在 entitysync.Transport 上**。

- **发布确认是构造期硬校验**：`RequireConfirmation=true` 且 bus 未实现 `ConfirmedSyncPublisher` → 构造失败（JetStream 实现该能力，纯 NATS 故意不实现）。
- **压缩阈值触发**（默认走 gzip BestSpeed，编码器池化，>1MiB 的 buffer 不归还池）；**校验和算在压缩前的 JSON 上**，每个分片带同一 checksum 供重组后整体校验。
- **分片非原子**：逐片发布，中途失败会留下已发布的前若干片——接收端靠有界重组 + `AssemblyTTL`（默认 30s）在下一次多片帧到达时清扫过期残片，没有独立清扫定时器。重组 key 含 encoding 与 checksum，不同次发布不互相污染。边界默认：MaxChunks 256、MaxAssemblyBytes 8MiB、MaxDecodedBytes 同（防解压炸弹）。
- **`BufferedPublisher` 的两个入口语义分离**：`Publish` = 同步 + 重试；`TryEnqueue` = 有界异步，满则 `ErrBackpressure`。**队列准入 ≠ broker 确认**。
- `roost-core/syncstream/observability_impl.go` 导出 5 个 `roost_sync_*` gauge + `Health()`。

### remoteentity：跨服实体（Mod 在 roost-kit/remoteentity，所有权实现在 roost-core/remoteentity）

- **Mod 注册四个 capability**：`ModRemoteEntity`、`ModRemoteEntityAtomicStore`（nestwal 消费）、**`ModRedisVLock`（全应用的 versioned lock 工厂出自这里，不是 redis Mod）**、`ModRemoteMirror`（`Manager.SnapshotClient()` 的只读能力，与只读服务同一个名字）。
- **只读服务用 `RemoteMirrorMod`**（Mirror 第 5 步）：只建 `SnapshotClient`（共享 L2 + 同步总线 + 只读 Mongo loader），不要求原子 backend、锁或 finalizer，注册表里只有 `ModRemoteMirror`；业务用 `//roost:mirror` DTO 生成的 `New<DTO>Reader(MirrorSource(registry))` 读。与 `RemoteEntityMod` 同进程会撞能力名、启动即失败。停机预算 `remote_entity.mirror.shutdown_timeout`（手写 Mod，不计入生成的 `shutdown.total_timeout`）。[说明](../docs/framework/README.md)硬前置：sid 非 0（所有权 fencing 的前提）。`Start` 序列：绑 sync 双 replicator（snapshot + interest 两个 topic）→ 封存依赖 → 建存储 → **启动期重放未发布的已提交事务（outbox 恢复）** → 启动 finalizer；任一步失败回滚已启动的 replicator。health 在容量耗尽（interest 键/活跃事务达上限）时也报 fail。
- **所有权状态机**（`roost-core/remoteentity/marker.go`）：一个 Redis hash + 5 段 Lua CAS，租约编码 `mode:owner:marker:route`，mode ∈ {local, shared}；enter/leave shared 与 transfer 都递增 marker（transfer 还递增 route）。**关键契约：ownership 缺失永远不被解释为本地租约**——Redis 数据丢失不会被误读成"我拥有它"。
- **Redis 键与部署隔离**（RR-20260927-17 / RR-20260930-19）：Mod 写到 Redis 的只有两类键，缺省都不带部署前缀，共用一个 Redis db 的部署要逐项配置：`remote_entity.snapshot_l2_key_prefix`（缺省空 = `remote_entity:snapshot:*` 不变；非空 → `<prefix>:remote_entity:snapshot:*`，不能带 hash tag）与 `remote_entity.lock_key`（缺省 `e` → `lock:e:<id>` / `:fence`；各部署配不同值，Cluster 下必须带非空闭合 hash tag，如 `{roost:game-a}`；不自动改写、改锁身份要整体重启）。`remote_entity:marks` 只有自行 `SetOwnershipStore(NewRedisMarker…)` 的非 authority 兼容装配才写，Mod 的所有权存储是 Mongo 权威，所以 Mod 没有它的配置项；兼容装配用 core 的 `NewRedisMarkerWithKeyPrefix(redis, prefix)`（→ `<prefix>:remote_entity:marks`，空值不变）。
- **两级快照缓存**：进程内 L1 + Redis L2；L2 的 CAS 顺序是 **(marker, route, version) 三元组 + checksum**——延迟的发布者不能覆盖更新的所有权 epoch 或状态版本；同 epoch 同 version 但 checksum 不同返回分歧信号。
- **独立资源预算**：`remote_entity.max_concurrent_writes` 默认 128，控制 Prepare 到实际释放的 Remote 写批次数；每批可能包含多个 Entity/DAO，结果不确定转后台后仍占额度。显式 0 沿用 `async_finalize_capacity`，有效值不会超过收尾容量。满额立即返回 `entity.ErrRemoteOverloaded`，不阻塞慢 worker 等额度。**定容规则**：写许可覆盖 Prepare → 投影确认 → finalizer 释放的整段生命周期，依赖（Mongo 投影 / 确认）停顿期间在途事务不会释放许可，所以按 `max_concurrent_writes ≥ 目标写 TPS × 可容忍的依赖停顿秒数 + 基线在途（p50 延迟 × TPS，约 15）` 设定——默认 128 在 80 TPS 下只容忍约 1.6s 停顿；本机同机三副本 Mongo 长跑中出现 1.2～2.0s 停顿时 128 会被占满（[B30 复测](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/REVIEW-2026-09-29-b30.md)），80 TPS 按 256（容忍约 3s）验收。`Stats` 的 `WritesInFlight / WriteLimit / WriteRejected` 及 health 可观测压力；满写额度为 degraded，已有 fatal/capacity fail 优先。
- **与 Nest、WAL 配合**：慢池可为其他慢 I/O 保留较大并发，但 Remote 写预算需按存储能力独立设定；已有 `dataengine.projection.max_unacked_records` 在 WAL 准入处原子限制所有未确认事务。两层额度不同，不自动互相推导，也不把采样值当原子预留；例如 Remote 128、WAL 512 是可测试起点，不保证给本地事务预留 384。过载是明确拒绝，框架不自行重试写请求。预算按进程生效，多进程部署须分配各自的额度。

- **MongoCommitter 的幂等契约**：事务按 `RemoteTransactionID` + 批 digest 判重——同 id 不同 commits 拒绝（"transaction id reused"），同 id 同 digest 直接返回已存储的 receipts。实体 CAS 元数据、DAO 文档、不可变快照、幂等状态在**一个 Mongo 事务**里提交。

- **Outbox发布并发**：`remote_entity.outbox_publish_workers`默认8（0取8），最大64；唯一RecoverOutbox协调者内部按完整事务全部Entity保序、无关事务并行。与慢池、写许可、投影worker数量独立，设1用于串行诊断；不能提前确认Applied或归还写额度来提高吞吐。阶段计时见[指标清单](../OBSERVABILITY.md)，本批尚未重跑性能负载。

### saga（引擎在 `saga/`，Mod 在 `kit/saga/`）：步骤至多一次生效与 nest 启动通道

- **Mongo store 的 lease fencing**（`roost-core/saga/mongo_store.go`）：任务领取用 `FindOneAndUpdate` 带 `version` + `lease_until <= now` 的 CAS，`$inc lease_token` 使过期实例的后续提交被 owner+token 过滤。状态落库走 `Apply(ctx, ApplyRequest)`：无 outbox/收据时是不开事务的快路径（一次 CAS ReplaceOne）；事务路径在**一个 Mongo 事务**里同落 saga 状态、outbox 命令、completion 收据与 `CloseOperation`（driver 可能重跑事务回调，outcome 在回调开头重置）。重复完成通过收据 digest 幂等判定。
- **`MongoCommandInbox` 按操作实例协调**（`roost-core/saga/command_consumer.go`、`step_operation_inbox.go`，saga 方向 ②，2026-10-06）：先在 Reserve 事务里拿租约（每个操作一份状态文档 `<收件箱集合>_operations`，与原生 `DataEngineStepInbox` 同一份判定，2026-10-06 起取代按尝试的 claim）：同一操作实例已有成功或本生拒绝 → 回放、**不重跑 handler**；另一次尝试持有有效租约 → nak 等待；租约过期 → 接替。再在执行事务里跑 handler、对状态文档做条件写（租约封顶到命令截止、未被接替）、写收据（`_id = command.ID`，格式不变）；条件不满足整笔回滚。handler 失败后交还租约，重投立即重试。命令过 `DeadlineAt` 后不开始新业务，只补发已提交的完成（自己的，或同一操作实例已生效的成功）。业务写在这笔事务里时按 `IdempotencyKey` 的业务幂等只是纵深防御；事务外的调用（发邮件）仍要幂等。
- **`DataEngineStepInbox` 的显式 reservation fence**：`SubscribeDataEngineStep` 只在同步 handler context 中附加 reservation；业务必须用 `ReservationFromContext(ctx)` 取出后作为 Nest 参数显式传递，并在 Nest handler 内调用 `inbox.Bind(command, reservation)`。Mongo projection 在同一事务中对操作状态文档（`_dataengine_step_operations`，每个操作实例一份）做条件写（owner、lease token、command digest、`pending` 和 `lease_until > now` 都匹配才写），与过期接管串行化；旧 worker 晚到时写 skipped marker 并 ACK，不应用业务/Remote mutation 或 effect。被跳过的记录已改过内存：准入到投影结果确定之间同实体写入以可重试的 `dataengine.ErrFencedEntityPending` 拒绝，跳过后常驻实体在快池驱逐、从 Mongo 重载，Sync 订阅者收到全量（RR-20260926-30，见 SAGA.md）。异步任务不得依赖该 context。**同一操作实例（saga + 步骤 + 方向）的各次尝试最多一次生效**（U-0280）：操作状态文档上的租约封顶到命令截止，截止后才投影的记录被跳过；新尝试先看同一操作实例的其他尝试——有成功或本生拒绝就回放那次的 completion、旧尝试仍持有效租约就 nak 等待、租约过期就接替（`$inc lease_token`）；协调器放弃之后才到的正向成功由协调器补偿那一步（saga 方向 ④，终态会被重开），补偿方向的只告警（`saga.completion.late_after_abandon_total`）。契约见 SAGA.md「原生步骤执行契约」。
- **步骤预算是配置**（`kit/saga/step_budgets.go`）：`saga.step_defaults.{timeout,max_attempts,backoff_min,backoff_max}` 给默认值，`saga.steps.<type>.<step>.<字段>` 按步骤覆盖；写错类型 / 步骤 / 字段时 `Init` 失败。`StepBudgetsFromConfig` 也给生成工程的测试用。
- **`StepHandler` 契约**：只能通过传入的事务 ctx 改 Mongo；网络调用等不可逆副作用必须走事务性 outbox（driver 允许重跑事务回调）。
- **nest-effect 启动通道**（`roost-core/saga/nest_start_consumer.go`）：业务在 Nest 事务里 `EmitStart` 一条 start effect，nestwal 重放投递到 `ROOST_EFFECTS` 流，saga 协调者的 durable 消费者解码后 `StartSaga`——"事务里声明一句 effect 就拉起 saga"。所有协调者副本必须共用同一个 Durable。envelope 带 wire version（未知版本拒收）与 8MiB 上限；subject 校验拒绝通配符注入。

### 基础设施细节（redis / mongo / nats / etcd / actionflow / gateway 的实现在 roost-core；ops / statslog / configdata 的 Mod 在 kit）

- **redis `EvalDurable`/`EvalBatchDurable`**（`roost-core/redis/driver/client.go`）：用 `client.Conn()` 钉死一条物理连接，pipeline 把 Lua 脚本与 `WAITAOF` 一起发——`WAITAOF` 观察到的复制偏移必然覆盖前面的脚本；批量版把 fsync 成本摊到整批。**Cluster 直接在 IO 之前拒绝**（无 key 命令的路由无法保证同分片）。`goredis.Nil` 统一映射为 `fredis.ErrNil`；pipeline 的 future 只能读一次，pipeline 对象可复用。`Eval` / `EvalSha` / `EvalBatchDurable` 的脚本**不经 go-redis 自动重放**：回复丢失（EOF、读超时）时原样返回错误、由调用方按结果未知处理；普通读写命令仍按 `MaxRetries` 重试（RR-20261005-NC-100）。
- **mongo 的持久性前提是硬编码的**（`roost-core/mongo/driver/client.go`）：写关注 majority+journal、读关注 majority、事务读关注 snapshot，业务无法通过配置放松。启动预检跑 `hello`：无逻辑会话（不支持事务）直接启动失败；`require_replica_set=true` 额外拒绝非副本集/非 mongos。索引冲突迁移是双开关（全局 `mongo.index.allow_recreate` **且** 单索引 `ConflictPolicy`），默认绝不 drop 生产索引。易踩坑：`InsertOne`/`BulkWrite` 返回的 ID 是 `fmt.Sprintf("%v")` 字符串化（ObjectID 会变成 `ObjectID("...")` 形态）；`WithTransaction` 的回调可能被 driver 重试多次，非幂等回调即错误。
- **nats 的 Drain、Stop 与失败终态**（`roost-core/nats/driver/jetstream.go`）：`Stop` 立刻 cancel handler ctx；`Drain` 先排空缓冲消息、等订阅关闭**之后**才 cancel。Saga 停机先等待两个 durable consumer 的 `Closed()`，再 cancel runCtx；超时会强制 Stop 并返回 deadline。消费错误默认 Nak 指数退避；显式 permanent 错误或到达 MaxDeliver 时 `Term`，同时记录结构化错误和 `nats.jetstream.terminal.total`。callback panic 被适配边界捕获并计数。AckPolicy 强制 explicit。
- **etcd 本地镜像的一致性**（`roost-core/etcd/driver/local_mirror.go`）：构造期先做带 Revision 的一致性前缀快照，watch 从 `Revision+1` 起——不漏事件；watch 断开后必先重新快照才重建 watch，且有 revision 回退检测（拒绝被回滚的集群）。订阅隔离：慢订阅者（队列满）单独以 `ErrMirrorSubscriberSlow` 踢除、handler panic 容器化为错误终止该订阅——都不影响 mirror 与其他订阅者。读路径返回深拷贝并附带"当前是否可信"的状态错误；CAS 写 `PublishIfRevision(0)` 表示"要求键不存在"。服务注册在 keepalive 丢失后指数退避自动重注册，`Deregister` 先标记停机再注销（正常停机不打 lease-lost 告警）。**选主的反直觉不变量**：选出后 campaign ctx 被取消不丢领导权（见 `election_test.go`）。
- **actionflow（原 taskflow）的无锁契约**（`roost-core/actionflow/action_runner.go`）：所有调用（含 Tick）必须由持实体锁的一方串行化——包内刻意无锁。回调（action 的 Start / Tick / Cancel 与 OnQueued / OnTransition / OnEnded 钩子）里对 runner 的变更（Start、Enqueue、End、EndAll、ClearQueue、ClearMission、Recover、Tick）进延后队列，回调返回后由最外层调用按发起顺序执行；回调里的 Start / Enqueue 立即返回已分配的 ID（`Deferring()` 为真表示被延后），拿到 ID 的动作一定收到结束；延后队列有界（`ErrDeferredQueueFull`），回调互相触发由执行预算截停（`ErrDeferredRunaway`）（B7）；钩子与 action 回调全部 panic-safe。`ActionContext` 走 `sync.Pool`，**action 内不得保留 ctx 指针**。`Start` 抢占当前 action、`Enqueue` 只入队；队列中某项启动失败不阻塞后续。`MissionRunner` 同样把回调（任务的 Start / Tick / OnActionEnd / End 与各钩子）里的 StartMission / CancelMission / EndCurMission / Tick / OnActionEnd 延后到最外层调用返回前按序执行（回调里返回 nil、执行时的错误经 OnError，`MaxDeferredCommands` / `MaxDeferredSteps` 有界，维护者第五轮决定）；任务替换先问 `CanReplaceBy`；start 失败做完整清理。**EndAll 不是清场**：它期间回调发起的动作（如任务按 OnFail 推进的下一步）会在它返回后启动；要什么都不再运行，先 `EndCurMission` 再 `EndAll`。`PlanMission` 会复制 steps 并回填默认跳转，入参 plan 不被修改。
- **ops 的安全面**（`roost-kit/ops/ops_mod.go`）：`/healthz` 恒 200（liveness）、`/readyz` = ready 位 && 没有依赖 fail（readiness，503 语义；degraded 算就绪，响应体 `degraded` / `degraded_dependencies` 标出，D1）——K8s 探针要分开配；每个 checker 并发调用、期限 1.5s（`health.DefaultCheckTimeout`），卡住的报 fail 不拖住 `/readyz`（第十二轮）。**`/metrics` 不鉴权**，默认绑 127.0.0.1 是唯一防护，改 0.0.0.0 等于公开指标。admin 鉴权双通道（`X-Admin-Token` 或 `Authorization: Bearer <token>`；第十二轮起 Authorization 必须带 `Bearer ` scheme，裸 token 401），关闭时端点返回 404 隐藏存在性；`dev-` 前缀 token 未显式允许即启动失败；执行命令自动补 `Source = ops:<service>:<sid>` 供审计。
- **statslog 的窗口语义**（`roost-kit/statslog/statslog.go`）：nest 处理量双报（本窗口增量 + 累计），计数器回退（进程重启）时返回当前值不产生负数；provider panic 被捕获成 `{"error":"panic:..."}` 记录而不是打断统计线程；同名 provider 覆盖后旧反注册闭包失效（ABA 防护）；`StopWithContext` 有界——卡住的 provider 不会挂死停服。**写失败不再静默**（RR-20260928-04）：`Start` 先打开一次文件，打开 / 写入失败计入无标签计数器 `stats_log.write_failures`，开始失败或错误变化时 WARN 一次、恢复时 INFO；不因此让 `Start` 失败（同一份数据每次采集都已发布为 gauge 与 `/statsz`）。相对的 `stats_log.dir` 按工作目录解析，只读根文件系统的部署要给它挂可写目录（生成的 compose / k8s / systemd 已做）。**统计文件不轮转**（REMAINING §3 N30）：文件以 `O_APPEND` 打开、进程不重开，部署侧配 logrotate 必须 `copytruncate`，示例见 `docs/DEPLOYMENT.md` §4 / §5 与生成的 `deploy/shell|docker/README.md`。
- **configdata**（`roost-kit/configdata/configdata.go`）：依赖 roost-core `configdata.DefaultRegistry()` 全局注册表（业务表类型须在 Mod 装配前注册，Mod 无自定义 registry 注入点）；指标来自 Store 的 `OnReloadOutcome`（每次 Load / Reload / Rollback 恰好一次，含 build 阶段的失败）：`configdata.reload.total{result=ok|failed}`、`configdata.rollback.total{trigger=apply_failed|operator}`、gauge `configdata.version`（当前在用的一代）；标签只有这些低基数取值，`reason` 进日志不进标签（N07 C-O6）；反注册按逆序执行。
- **gateway 中间件的三条硬契约**（`roost-core/gateway/middleware.go`）：`RateLimit` 的鉴权兜底不可绕过——**limiter 为 nil 时仍检查 principal**（关掉限流不能扩大认证面；曾有此回归，见 `roost-core/gateway/middleware_test.go` 注释），限流 key 是玩家 × 消息号；`Timeout` 只收紧不放松（调用方已有更严 deadline 时透传）；`Recover` 统一返回 `ErrEndpointPanic` 不泄漏 panic 细节（明细走 report 回调）。链式顺序：鉴权在 `RateLimit` 之前。

### 并发定位一览（组件均在 roost-core）

| 组件 | 定位 |
| --- | --- |
| `lockstep.Room`、`actionflow.ActionRunner/MissionRunner` | **单所有者无锁**，由实体串行 handler 驱动 |
| `ai.Controller` | 外部（实体锁）串行化，自身不加锁；`Blackboard` 自带锁 |
| `policy.AOI`（原 `spatial.InterestManager`） | 非并发安全（场景私有） |
| `policy.AOICluster`（原 `spatial.InterestCluster`） | 单锁并发安全（多房间 handler 并行 tick） |
| `nettransport.AsyncTransport` | 每 session 一个 worker，`SendReliable` 在 session 锁下入队 |
| `entitysync.Manager` | 注册表读写锁 + 每 subject 一把锁 + pending 集合锁；tick 由 `flushMu` 串行，会话状态只在 tick 内改 |

### 玩法与实时组件（均在 roost-core）

- **spatial 的增量兴趣管理**（`roost-core/spatial/interest.go`、`roost-core/spatial/interest_cluster.go`）：`InterestManager` 在 BlockIndex 之上做九宫格订阅——observer 订阅其离开半径覆盖的块，实体移动只重评估受影响邻域；进出用**双半径滞回**（EnterRadius < LeaveRadius，边界震荡零事件）；距离带直接映射 entitysync 的 SyncProfile LOD；MaxVisible 是防广播风暴闸门（近似 top-N 语义）。`InterestCluster` 把多房间拼成一个共享坐标平面：贴边 observer 被**镜像**进邻房（接缝无视野盲区），Flush 输出**净变化**（每 (observer,subject) 维护房间→距离带表，对外只发与上次发射状态的差异）——因此跨界迁移是 make-before-break 且**下游订阅零闪断**。并发定位：Manager 非并发安全（场景私有）、Cluster 单锁并发安全（多房间 handler 并行 tick）；基准 4 房 × 1000 subjects 全移动 + 100 observers ≈ 0.34ms/tick。
- **ai 的树到执行流闭环**（`roost-core/ai/behavior_strategy.go`、`roost-core/ai/nodes.go`、`roost-core/ai/tree.go` / `tree_parser.go`（原 wire.go））：`BehaviorStrategy` 把行为树装进 Controller（完成的树自动 Reset、动作完成事件缓冲到下一 tick 的上下文）；`TaskflowAction` 叶子发起 taskflow 动作并等待 `OnActionEnd`，被高优先级分支打断时经 `OnInterrupt` 收尾——"树决策、taskflow 执行"成为标准写法。计时节点（cooldown/time_limit）只读注入的 tick 时钟、随机节点只用注入掷点——权威侧决策可复现。`ParseTree` 严格装配 JSON 树（未知字段/节点/元数违规当场拒绝，诊断带 `$.root.children[0]` 式 path），复合节点内建、condition/action 叶子经 `Registry` 注册；配合 `Controller.SetStrategy` 的事务性替换，坏 JSON 永远不会顶掉在跑的策略。两条接线约定（N10 O-T3 / O-T4，维护者第十二轮决定保持）：策略在第一次 Tick 里发起动作，不在 `Init` 里发起（替换时 `EndActions` 在新策略 `Init` 成功后才跑，会把 Init 里发起的动作一并结束）；`Controller.Shutdown` 不调 `EndActions`，要结束在途动作就在策略的 `Stop` 里做，或在 Shutdown 之前自己 `EndCurMission` / `EndAllAction`。
- **gateway 的定位声明**（包 doc）：中间件集合（限流/鉴权守卫），**不是网关服务器**。
- **lockstep 的双通道分工**（`roost-core/sync/lockstep/room.go`）：`Room.Tick` 切帧 → 记历史 → `RedundantEncoder` 封包（携带最近 N 帧）→ 对每个附着 session 走 **datagram** 通道（AEAD UDP）——丢包由后继报文的冗余修复，永不重传（重传回来的实时帧已过期）；`StartCatchup` 的重连追帧走 **可靠** 通道（KCP/QUIC），每 tick 最多 `CatchupBatchFrames` 帧分页限速，追上帧头后自动切回实时广播（追帧期间不发实时包，避免双份下行）。可靠通道选型：KCP 与 QUIC 均有适配，延迟和 CPU 应按业务网络实测，不作固定百分比承诺——两者都已在 `sync/nettransport/` 落地，一个 `ReliableSender` 接口互换。迟到输入折入下一帧并计 `lockstep.input.late.total`（针对当前帧的显式输入会覆盖折入的过期输入），非法输入计 `lockstep.input.rejected.total{reason}`；掉线座位不移出比赛（乐观帧锁定天然把缺席当空输入），重连 = `Attach` 换 session（同 session 重复 Attach 幂等且保留追帧游标；session 被其他座位/观战者占用则拒绝）+ `StartCatchup`（追帧连续失败超预算自动放弃并在 Tick 错误中显式说明，历史被 Trim 出缺口时同样放弃）。**构造期预算校验**：`冗余深度 × 座位数 × MaxInputBytes` 超出 datagram 包上限（默认 1232）直接拒绝配置——单个满载客户端永远打不黑整房间下行。哈希裁决按"同意组 ≥ quorum"出结论（默认 quorum = 座位过半，串谋少数抢先上报无法误伤诚实玩家），`OnDesync` 只在离群**集合**变化时回调；`ReportHash` 校验座位与帧号上界，Trim 过的帧墓碑化。观战者走 `AttachSpectator`/`SpectatorCatchup`（只收不发、不占座位）。一个 Room 只服务一局，结束调 `Close()`。

---

## 5. 学习路径

路径按 2026-09 的仓归属标注：`roost-core/…` 在 roost-core 仓，其余是本仓文件（ARCH-04）。

按下面的顺序读源码与测试，每步都可 `go test ./<pkg>/` 验证认知：

1. **框架心智模型**：`roost-core` 仓库 `README.md` + `RUNTIME_EXECUTION_MODEL.md`，然后读本仓库 `mods/name.go` 和任意一个小 Mod（如 `lock/lock_mod.go`）理解四阶段生命周期。
2. **WAL 设计文档**：`roost-core/NEST_TRANSACTION_WAL.md` → `NEST_PIPELINED_COMMIT.md`（中文，含 Strict/Pipelined 语义对比与正确性论证）。
3. **nestwal 主线**（本仓库核心，建议精读）：
   - `roost-core/nestwal/wal.go`（帧格式、group commit、Enqueue/ticket、terminal 熔断）→ `roost-core/nestwal/checkpoint.go`（双 slot ack）→ `roost-core/dataengine/engine/projector.go`（事务 projection 与 ack）。
   - 测试按价值排序：**`roost-core/nestwal/crash_test.go`**（真实子进程 `SIGKILL` 验证崩溃后 durable 前缀完整）、`roost-core/nestwal/pipelined_test.go`（ticket 语义）、`roost-core/nestwal/wal_test.go`（torn tail、ack、rotate）、`dataengine/fatal_fence_test.go`（熔断到 Nest/RuntimeFailure 的传导）、`roost-core/nestwal/` 的 backlog 集成测试（100k backlog 恢复）。
4. **Data Engine 主线**：`roost-core/dataengine/engine/projector.go` → `roost-core/dataengine/engine/mongo_store.go` → `roost-core/dataengine/engine/entity_repository.go` → `roost-core/dataengine/migration.go` → `roost-core/dataengine/engine/outbox_worker.go`。
5. **锁与选主**：`roost-core/redis/lock.go` + `roost-core/redis/driver/lock_test.go` → `roost-core/remoteentity/versioned_lock.go`、`roost-core/remoteentity/versioned_lock_lua.go` + `roost-core/remoteentity/versioned_lock_unlock_test.go` → `roost-core/etcd/election.go` + `roost-core/etcd/driver/election_test.go`（含"campaign ctx 取消不丢领导权"这条反直觉不变量）。
6. **横向扩展**：`roost-core/saga/mongo_store.go`（lease CAS）+ **`roost-core/saga/command_consumer_test.go`**（重投只执行一次 = 步骤至多一次生效的规格）→ `roost-core/remoteentity/transaction_manager.go`（跨服事务追踪）→ `roost-core/sync/nettransport/udp_crypto.go`、`roost-core/sync/nettransport/udp_transport.go` → `roost-core/sync/entitysync/flush.go`（状态帧）→ `roost-core/sync/lockstep/room.go` + `roost-core/sync/lockstep/room_test.go`（输入帧：追帧限速、断线重连、裁决回调的用例即文档）→ `spatial/`、`ai/`、`roost-core/actionflow/action_runner_test.go`（抢占/队列/重入检测）。
7. **语义即测试的推荐清单**：`roost-core/etcd/driver/local_mirror_test.go`（原子快照、慢订阅隔离、panic 隔离、CAS）、`roost-core/nats/driver/jetstream_test.go`（Drain 与 Stop 的语义差）、`roost-core/gateway/middleware_test.go`（带回归原因注释的鉴权兜底）、`ops/ops_mod_test.go`（5 个测试 = 该包完整规格）、`roost-core/mongo/driver/collection_test.go`（"默认绝不 drop 生产索引"）。

---

## 6. 版本与仓库关系

- **与 roost-core 的版本对应**：研发 source-head 由 `go.work` 选择本地 Core；正式发布则以本仓库 `go.mod` 为准。发布 Kit 前必须先发布它依赖的 Core tag；发布版 `go.mod` 不允许本地 `replace` 或 pseudo-version。
- **仓库分工**：

  | 仓库 | 模块路径 | 角色 |
  | --- | --- | --- |
  | roost-core | `github.com/tjbdwanghaibo/roost-core` | 契约 + 实现：Nest、Data Engine、Saga、Remote Entity、客户端、同步、技能系统（`skill/`，原 roost-skill） |
  | roost-kit（本仓库） | `github.com/tjbdwanghaibo/roost-kit` | 装配层：配置解析、Mod、生命周期、运维入口；`service/` 通用游戏服务（原 roost-service） |
  | roost-codegen | `github.com/tjbdwanghaibo/roost-codegen` | DAO/Sender 等代码生成 |

- **本地联调**：在共同父目录建 workspace，勿把 `go.work` 提交进任何仓库：

  ```bash
  cd /path/to/workspace
  go work init ./roost-core ./roost-kit ./roost-codegen
  ```

  需要业务工程时再执行 `go work use ./your-service`。发布/standalone 验证必须使用
  `GOWORK=off`；workspace 绿灯不代表正式 tag 已形成依赖闭包。

- **开发验证**：

  ```bash
  go build ./... && go test ./...
  # 单独跑核心持久化链路：
  go test ./nestwal/ ./dataengine/
  ```

新增 Mod 时请同时提供：配置读取、Registry capability、health 检查、确定的 Stop 行为，以及不依赖真实外部服务的测试替身。

本仓库以 [MIT License](LICENSE) 发布。

正式实体同步可使用 `kit/nest.NewModWithEntitySync`，通过 `sync.entity.mode` 在周期与变化触发之间选择；默认周期。`Init` 后从 `EntitySync()` 安装业务 Interest 与会话，启动及停机排空由 Mod 管理。详见[双模式接入](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)。
