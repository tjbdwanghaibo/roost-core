# cube-core

`cube-core`（仓库目录名 `roost-core`，Go 模块路径 `github.com/tjbdwanghaibo/cube-core`，当前稳定版本 v1.8.0）是一个通用游戏服务器运行时框架：它把"实体串行调度 + 内存事务 + WAL 持久化 + 状态同步"做成可复用的基础设施，让业务代码只写 handler 和 DAO，不碰锁、WAL 与回滚。

## 三级文档入口

- **完全新手**：从 [五分钟快速开始](docs/QUICKSTART.md) 生成并运行第一个项目。
- **有经验的开发者**：阅读 [开发者完整使用说明](docs/USER_GUIDE.md)，按场景选择 Entity/Nest、Remote Entity、Saga、状态同步或帧同步。
- **框架维护者/生产负责人**：阅读 [实现原理与不变量](docs/INTERNALS.md)、[生产部署手册](docs/DEPLOYMENT.md) 和 [薄弱点与路线图](docs/ROADMAP.md)。完整索引见 [文档中心](docs/README.md)。

本 README 保留能力总览和可独立运行的 core 最小示例。

- [能力总览](#能力总览)
- [设计不变量](#设计不变量)
- [快速启动](#快速启动)
- [核心概念](#核心概念)
- [关键实现细节（进阶）](#关键实现细节进阶)
- [学习路径](#学习路径)
- [与 roost-kit / roost-codegen / roost-skill 的关系](#与-roost-kit--roost-codegen--roost-skill-的关系)
- [开发与验证](#开发与验证)
- [深入文档索引](#深入文档索引)

## 能力总览

| 模块 | 解决什么问题 | 何时用 |
| --- | --- | --- |
| `app` | `Mod`/`Service` 生命周期、类型安全的 capability `Registry`、配置生产门禁、运行期 fail-stop | 装配任何服务的入口 |
| `entity` | 实体 = `EntityBase` + 组件 + DAO 的组合；`EntityManager`/`Getter`；实体锁与 guard 作用域；远程实体元数据 | 定义所有业务对象 |
| `nest` | 按实体 ID 哈希的串行 actor 调度、全局锁序死锁预防、`RollbackTx` 内存事务、WAL commit point、pipelined 提交 | 所有实体状态修改的唯一执行入口 |
| `checkpoint` | `DirtyTracker` 字段级脏掩码、有界 `Journal`、`Flusher` 批量合并刷盘、版本化 tombstone、Redis WAL | 实体状态的后台持久化 |
| `lock`、`worker`、`misc` | 可重入实体锁（parking 语义）、同 key 串行的哈希 worker pool、goroutine-ID 等基础原语 | 框架内部依赖；业务偶尔直接用 `worker.Pool` |
| `entitysync`、`sync`、`syncstream`、`replication` | 订阅协调 + prepare/commit 两阶段同步、有序状态流、Quake3 风格 delta+LOD 房间复制 | 把实体状态推送给客户端或其他服务（状态同步通道） |
| `lockstep` | 帧同步（输入帧）核心：乐观帧锁定 `Sequencer`、帧冗余广播编码、全量帧历史（追帧/回放）、关键帧哈希多数派裁决 | 客户端确定性模拟的实时对战（MOBA/格斗/RTS）；与状态同步互为并列通道，见实现细节第 11 条 |
| `saga` | 租约驱动的多域业务操作状态机 + transactional outbox，Resume 开启新 incarnation | 跨服务、多阶段、需补偿的业务操作 |
| `bus`、`event` | NATS 之上的模块级消息 / 轻量 RPC / **JetStream 持久化 RPC**（`CallReliable`，与轻量 RPC 并存）/ 可靠消费（inbox 去重 + 死信 + `bus.dlq.*` 运维命令）；进程内事件总线（self 同步、他人异步） | 服务间与实体间的异步通信 |
| `taskflow`、`ai` | 实体内行为契约（**不是通信设施**）：动作/任务状态机接口、声明式 `MissionPlan` 步骤图、ActionGroup 分组冻结；AI 策略契约（`CanStopByNext` 抢占仲裁、经 `ActionList` 下达动作）。**core 只有接口，执行器在 cube-kit 的 `taskflow`/`ai` 包** | 实体行为层（怪物/NPC/玩法状态机） |
| `ownerroute`、`replica`、`entity`（remote 部分） | 按 owner sid 分派命令的泛型路由器（本地执行 vs 经 bus 转发）；带订阅-应用回环的副本复制器（空 Data 即删除的线格式）；ownership marker + fence + 路由 epoch（epoch 在 `entity`，不在 ownerroute） | 跨服实体读写与命令路由 |
| `cache`、`mongo`、`redis`、`nats`、`etcd`、`httpclient`、`httpserver` | 三档：`mongo`/`nats` 纯接口（实现全在 kit）；`redis`/`etcd` 接口 + 核心实现（Lua `CompareAndSet`、`WatchCallback`、LocalMirror 契约）；`cache`/`httpclient`/`httpserver` 是完整实现（8 种缓存 store、HMAC 签名客户端、chi 之上的生产 HTTP 引擎——core 对 chi 的依赖是唯一例外） | 缓存选型见实现细节第 13 条；连接装配由 `roost-kit` 的 Mod 提供 |
| `health`、`obs`、`log`、`admin`、`lifecycle`、`security`、`failurelog`、`featureflag`、`hotcode` | 健康检查（degraded 在聚合层等同失败）、指标（counter/gauge/timer 无分位数；histogram 17 桶指数分布带 p50–p99 与 Prometheus `_bucket` 导出）、结构化日志（自动注入 goId/逻辑帧/player + ELog 链式实体日志）、管理命令（含元数据注册表，审批灰度由上层实现）、生命周期钩子 + 泛型 `ManagerGroup` 编排、限流/HMAC 签名/会话令牌、Redis 有界失败记录、布尔开关表、热修补 | 平台能力；**一律用 `app.Lookup` 取实例注册表**（见实现细节第 12 条） |
| `gateway`、`webroute`、`errcode`、`configdata` | 协议无关的请求边界、生成路由运行时、错误码、配置表快照（原子热更/回滚/内容 hash/请求一致性；三条接入通道：手写 TableDef、`cfg` tag 自动注册 `RegisterAutoTable`、外部生成聚合 `RegisterExternalTables`——配置定义可全量生成，见实现细节第 17 条） | 接入层契约 |
| `robot` | 机器人（模拟客户端）框架：统一包协议 transport（TCP/WS 内置，KCP/QUIC 在 kit）、seq 匹配会话、`RegisterCall` 泛型零样板动作、行为树场景（Go 组合子 + 可选 YAML）、三种压测执行器（pool/looping/arrival-rate）+ SLO 阈值裁决与 Markdown 报告 | 模拟客户端逻辑回归、压测（见实现细节第 18 条） |
| `timer`、`clock`、`map`、`query`、`ctx` | 时间任务、逻辑时间、容器与索引、请求上下文 | 通用工具 |

core 只定义抽象与框架语义，不含具体玩法、玩家协议或中间件连接实现——那些分别属于业务仓库与 `roost-kit`。

## 设计不变量

所有模块的失败语义都从四条不变量推导，读代码前先记住它们：

1. **成功不可先于 commit point 被观察。** 任何"已完成"的对外承诺（返回值、outbox、同步分发）都必须晚于对应的持久化 admission。
2. **结果不确定时 fence，而不是猜。** fsync 结果不确定（`ErrCommitIndeterminate`）时不做内存回滚——那会制造与 WAL 已提交历史冲突的第二条历史——而是放弃事务、熔断进程，由新进程 WAL replay 判定权威结果。
3. **删除防复活。** 版本化 tombstone 语义在内存去重、Redis Lua 脚本、WAL 回放三层一致：同版本 delete 胜出，复活必须携带严格更高的版本。
4. **接纳即执行。** 有界队列一旦受理任务（返回 true / nil），任务必然被执行或被显式释放，不存在"接受了但悄悄丢掉"的窗口。

## 快速启动

下面的示例约 130 行，展示最小闭环：**定义一个实体 + DAO → 注册 nest handler → 跑一次成功提交和一次带 undo 回滚的事务请求**。示例已在本仓库上编译运行验证（v1.6.2 起可用）。

```bash
mkdir quickstart && cd quickstart
go mod init quickstart
go get github.com/tjbdwanghaibo/cube-core@v1.8.0
# 将下面代码存为 main.go 后：
go run .
```

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/cube-core/checkpoint"
	"github.com/tjbdwanghaibo/cube-core/entity"
	"github.com/tjbdwanghaibo/cube-core/nest"
)

// ---- 1. DAO：实体的持久化文档，字段级脏掩码由 checkpoint.DirtyTracker 跟踪 ----

const heroGoldField = 1 // 字段位：Gold 对应脏掩码第 1 位

type HeroDao struct {
	id      int64
	tracker checkpoint.DirtyTracker
	Gold    int64
}

func (d *HeroDao) Id() int64            { return d.id }
func (d *HeroDao) SetId(id int64)       { d.id = id }
func (d *HeroDao) DbName() string       { return "game" }
func (d *HeroDao) CollName() string     { return "heroes" }
func (d *HeroDao) Dirty() entity.IDirty { return &d.tracker }
func (d *HeroDao) CleanDirty()          { d.tracker.SelfClean() }

// DirtyTracker 让 RollbackUndo 事务能在回滚时恢复脏掩码快照。
func (d *HeroDao) DirtyTracker() *checkpoint.DirtyTracker { return &d.tracker }

// AddGold 是"可回滚写"的最小样板：先 RecordUndo 登记逆操作，再改内存、标脏。
// 真实项目中这类 setter 由 roost-codegen 生成。
func (d *HeroDao) AddGold(n int64) error {
	old := d.Gold
	if !nest.RecordUndo(d, heroGoldField, func() error { d.Gold = old; return nil }) {
		return errors.New("AddGold must run inside a rollback=undo nest handler")
	}
	d.Gold += n
	d.tracker.MarkPersist(1 << heroGoldField)
	return nil
}

// ---- 2. 实体：嵌入 EntityBase，绑定 DAO ----

const (
	heroKind     entity.EntityKind     = 1
	heroCategory entity.EntityCategory = 1
)

type Hero struct {
	*entity.EntityBase
	Dao *HeroDao
}

func (h *Hero) Base() *entity.EntityBase { return h.EntityBase }

// RangeDao 实现 entity.Guardable：事务与 checkpoint 由此发现实体的 DAO。
func (h *Hero) RangeDao(f func(entity.DaoInterface)) { f(h.Dao) }

// ---- 3. Getter：Nest 通过它按 ID 取实体（生产中由 EntityManager 提供）----

type memGetter struct {
	mu sync.RWMutex
	m  map[int64]entity.IThreadSafeEntity
}

func (g *memGetter) Add(e entity.IThreadSafeEntity) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.m[e.ID()] = e
}

func (g *memGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if e, ok := g.m[id]; ok {
		return e, nil
	}
	return nil, nest.ErrEntityNotFound
}

func (g *memGetter) GetMany(ctx context.Context, ids []int64, cats []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	ret := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		e, err := g.Get(ctx, id, cats[i])
		if err != nil {
			return nil, err
		}
		ret[i] = e
	}
	return ret, nil
}

func main() {
	// 注册实体 Kind -> Category 映射（ID 编码需要）
	entity.MustRegisterEntityKindCategory(heroKind, heroCategory)

	// 创建实体：完整 EntityID 由 uniqueID + kind 编码而成
	id, err := entity.BuildEntityID(1001, heroKind)
	if err != nil {
		panic(err)
	}
	hero := &Hero{
		EntityBase: entity.NewEntityBase(id, heroCategory, false, heroKind),
		Dao:        &HeroDao{id: id, Gold: 100},
	}

	getter := &memGetter{m: map[int64]entity.IThreadSafeEntity{}}
	getter.Add(hero)

	// 注册 handler：Rollback=undo —— handler 出错时按登记的逆操作恢复内存
	nest.MustRegisterHandlerWithMeta(nest.NewHandlerName("hero.add_gold"),
		func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
			h := es[0].(*Hero) // 进入 handler 时实体锁已按全局锁序持有
			if err := h.Dao.AddGold(params[0].(int64)); err != nil {
				return nil, err
			}
			if h.Dao.Gold < 0 {
				return nil, errors.New("gold would go negative") // 触发回滚
			}
			return h.Dao.Gold, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo})

	// 启动引擎：消息按实体 ID 哈希到固定 worker，同一实体串行执行
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(getter),
		nest.NestOptionWithWorkerNumAndMsgCap(2, 1, 64),
	)
	if err := engine.Start(); err != nil {
		panic(err)
	}
	defer engine.Shutdown(context.Background())

	name := nest.NewHandlerName("hero.add_gold")

	// 成功：+50，事务提交，内存生效、脏位保留（等待 checkpoint 刷盘）
	ret, err := engine.Request(context.Background(), name, id, nest.NewParams(int64(50)))
	fmt.Printf("add +50: gold=%v err=%v dirty=%v\n", ret, err, hero.Dao.DirtyTracker().Dirty())
	hero.Dao.CleanDirty() // 示例简化：模拟 checkpoint 已刷盘并清理脏位

	// 失败：-1000 使余额为负，handler 返回错误，事务按 undo 回滚：
	// 内存值恢复为 150，脏掩码恢复为 handler 进入前的快照（干净）
	_, err = engine.Request(context.Background(), name, id, nest.NewParams(int64(-1000)))
	fmt.Printf("add -1000: err=%v\n", err)
	fmt.Printf("after rollback: gold=%d dirty=%v\n", hero.Dao.Gold, hero.Dao.DirtyTracker().Dirty())
}
```

输出：

```text
add +50: gold=150 err=<nil> dirty=true
add -1000: err=gold would go negative
after rollback: gold=150 dirty=false
```

这个示例没有配置 `TransactionCommitter`，事务停留在内存层（`DurabilityMemory`）。生产环境把 handler 声明为 `DurabilityStrict`/`DurabilityPipelined` 并注入 `roost-kit` 的 WAL committer，即可获得崩溃一致性——业务代码一行不改。本地联调多仓库时可用 `go.work` 或 `go mod edit -replace` 指向本地目录。

## 核心概念

### 实体 / 组件 / DAO

实体是**组合**而非继承（`entity/entity_base.go`、`entity/entity.go`）：

```text
Hero（业务实体，具体类型）
 ├── *entity.EntityBase        身份、生命周期（Touch/UnTouch 引用计数 + removed/cleared 位）、
 │                             实体锁、事件总线、同步状态、lastCommitLSN
 ├── entity.ComponentManager   组件容器；组件按注册的依赖关系拓扑排序初始化
 └── entity.DaoManager         DAO 容器；每个 DAO 对应一个持久化 collection 文档
```

- **组件（Component）** 持有对宿主实体的**具体类型指针**（不是 interface），实现行为逻辑；用 `entity.RegisterComponentDependency` 声明初始化顺序。
- **DAO** 是持久化边界：实现 `entity.DaoInterface`，内嵌 `checkpoint.DirtyTracker` 做字段级脏掩码。组件改状态时只改 DAO 字段并标脏，何时刷盘由框架决定。
- 组件/DAO 与实体之间的接线代码（工厂、快照、undo setter）在真实项目中由 `roost-codegen` 生成，`entity/example_gen_test.go` 手写模拟了生成产物的形态，是理解这套约定的最佳入口。
- 实体注册进 `entity.EntityManager` 后通过 `ManagerAccess`（`entity/manager_access.go`）暴露创建/获取/销毁/分组索引，服务持有实例而不是全局单例；ID 由 `entity.IDGen`（Redis/etcd 分配号段）生成。

### 串行调度模型（Nest Actor）

Nest 是所有实体状态修改的唯一入口（`nest/nest.go`、`nest/dispatcher.go`）：

```text
Client.Dispatch/Request(handlerName, entityID, params)
        │  按 entityID 哈希（misc.Hash64）选择固定 worker
        ▼
worker（每 worker 一条 MPSC 队列，有界，满即拒绝 ErrQueueFull）
        │  NestDispatch：取实体 → Touch → 按锁序 Sort → 加实体锁
        ▼
handler(es []entity.IThreadSafeEntity, params []any) (any, error)
        │  事务提交 / 回滚（见下节）
        ▼
解锁 → guard release hook（checkpoint/sync）→ 回包
```

- **同一实体天然串行**：同 ID 恒定哈希到同一 worker，队内 FIFO。不同实体并行。
- **handler 内已持锁**，可安全读写传入实体；多实体请求（`DispatchMulti`）在进入 handler 前一次性按全局锁序拿齐所有锁。
- **handler 内禁止同步跨实体调用**：`Request`/`Dispatch` 在 handler 内会 panic（`ErrSyncInHandler`/`ErrAsyncInHandler`）。跨实体写用 `nest` 的 cast（`nest/cast.go`），它带加载前锁序预检。
- 引擎实例化（`nest.NewEngine`）、单次使用、不可重启；`Fence` 用于不确定故障后的立即熔断。

### 事务与回滚策略

每个 handler 注册时声明 `HandlerMeta{Rollback, Durability}` 两个独立维度（`nest/rollback.go`、`nest/transaction.go`）。**Rollback 管 commit point 之前的失败，Durability 管 commit point 何时被确认。**

Rollback 三档：

| 策略 | 机制 | 适用 |
| --- | --- | --- |
| `RollbackNone` | 无事务开销 | 只读 handler |
| `RollbackState` | handler 前对每个 DAO 抓完整快照（DAO 需实现 `RollbackSnapshotter` 或 `RollbackParticipant`），失败时整体恢复 | 简单、写字段多 |
| `RollbackUndo` | setter 第一次改字段时 `nest.RecordUndo(owner, field, inverse)` 登记逆操作（同 owner+field 自动去重合并，map 键用 `RecordUndoToken`），失败时逆序执行 | 热路径推荐；生成代码默认 |

两种策略都会自动快照并恢复 `DirtyTracker` 的脏掩码与版本，回滚后实体"字节级"回到事务前。

Durability 四档：

| 策略 | commit point | 说明 |
| --- | --- | --- |
| `DurabilityMemory` | 无 WAL | 靠实体 release 后的 checkpoint 异步落盘 |
| `DurabilityAsync` | WAL write 受理，不等本批 fsync | 后台按间隔刷盘 |
| `DurabilityStrict` | **锁内**等待 group commit fsync 完成 | 返回成功即已持久化 |
| `DurabilityPipelined` | 锁内仅"入队拿 LSN"，fsync 锁外等待 | 热点实体锁不被 I/O 拖住，见进阶细节 |

handler 成功后，框架把所有被改实体的 after-image/delta（由 `CommitParticipant.PrepareCommit` 物化）连同 `nest.Emit` 发出的 outbox `Effect` 组成**一条** `CommitRecord` 交给 `TransactionCommitter`——多实体修改与外部消息在 WAL 里永远是一个原子单元。外部副作用（DB、RPC、消息）严禁写在 handler 里，用 `nest.AfterCommit`（提交后、通常锁外执行）或 `nest.Emit`（事务性 outbox，会自动把 memory 事务升为 strict）。

### 持久化管线（checkpoint）

`checkpoint` 是"内存 → 数据库"的 after-image 管道：

```text
DAO setter 标脏（DirtyTracker，原子 Or）
   ▼ 实体 guard 释放时（release hook，持锁内）采集 Snapshot → SaveItem
Journal（有界队列，满则背压/拒绝）
   ▼ Flusher worker 或显式 Flush(ctx)
按 (collection, id) 去重合并 + 版本化 CAS → StorageBackend（Mongo 等，kit 实现）
```

- `DirtyTracker`（`checkpoint/dirty.go`）用 `atomic.Or`/`Swap` 维护 persist/sync 两套 64 位掩码：`TakePersistDirty` 交换出快照后，其后标的脏位落在新掩码里，旧确认永远清不掉新数据（天然免 ABA）。
- WAL 三档（`checkpoint/option.go`）：async 尽力、required WAL 拒绝则拒绝提交、durable fsync 确认才受理。`Checkpoint.Start` 强制先完成 WAL replay 再接受实时 flush。
- 删除是**版本化 tombstone**（`JournalEntry.Deleted`），先 WAL 后 journal，后端 `BulkRemove` 成功才 ACK——防止崩溃窗口让旧快照复活实体（见进阶细节）。

## 关键实现细节（进阶）

每条附源文件指引，建议对照代码阅读。

### 1. 实体锁为什么是 parking 而非自旋 —— `lock/reentrant_mutex.go`

`ReentrantMutex` 用**容量 1 的信号量 channel** 实现：token 在 channel 里 ⇔ 锁空闲，等待者阻塞在 `<-rm.sem` 上停车（park），由 Go runtime 挂起。持有者做慢操作（典型：`DurabilityStrict` 的锁内 WAL fsync，毫秒级）时，等待者不烧 CPU；channel 的接收顺序还提供近似 FIFO 公平性，热点实体锁不会像不公平自旋锁那样饿死个别等待者。可重入基于 goroutine ID（`owner atomic.Int64` 只与调用者自己的 gid 比较，非持有者的陈旧读不可能误中快路径）；`recursion` 只被持有者触碰，sem 交接提供前后持有者之间的 happens-before。

### 2. 锁序如何防死锁 —— `entity/entity_guard.go`、`nest/nest_dispatch.go`

死锁预防是多层体系而不是单个技巧：

- **全局锁序**：`EntityGroupRemote → Player → Alliance → Other`（`GetEntityGroupFunc` 由应用映射 category 到组），Remote 最先因为分布式锁必须先于任何本地 mutex。
- **批内确定性排序**：多实体请求进 handler 前 `SortEntity` 按（组，GUID）排序后依次加锁——任意两个事务对同一批实体的加锁顺序全局一致。
- **cast 预检**：handler 内跨实体 cast 在加载实体前用 `CheckContainAllIDs` 校验目标组不违反已持有的最大组序，违序直接拒绝。
- **已持锁时降级 TryLock**：`lockDispatchEntities` 发现 guard 已持有锁且新目标不满足全序时，改用 `TryLock`，拿不到立即全部回退而不是阻塞等待（阻塞就可能成环）。
- **有界重排队**：锁冲突（`ErrLockTimeout`）的消息由 `requeueTransientDispatch`（`nest/group_transition.go`）延迟重新入队，最多 `entityGroupDispatchRequeueMax` 次，带 `nest.dispatch.requeue.total` 指标。

API 层再补一刀：handler 内同步跨实体调用直接 panic，从根上消除"持锁等待另一个持锁者回包"的环。

### 3. WAL commit point 与 `ErrCommitIndeterminate` 的崩溃一致性哲学 —— `nest/rollback.go`、`nest/transaction.go`

`invokeWithTransaction` 是全部语义的汇聚点。`DurabilityStrict` 下 `tx.durableCommit` 在**实体锁内**调用 `committer.Commit`：

- committer 明确拒绝 → `ErrCommitRejected`，执行内存回滚，对外报错——世界回到事务前。
- committer 报告 `ErrCommitIndeterminate`（fsync 出错，字节可能已到、也可能没到持久介质）→ **不回滚**：`tx.abandon()` 丢弃 undo 与 AfterCommit，内存保持事务后形态，进程应当 `Fence` 停止接流。

为什么不回滚？因为如果 WAL 实际已提交，内存回滚会制造一条与持久历史相反的第二历史，后续事务会在错误状态上继续叠加。唯一诚实的做法是承认"不知道"，把裁决权交给新进程的 WAL replay：replay 到该记录则事务成立，没有则自然消失。这是设计不变量第 2 条的直接实现；`TestIndeterminateCommitDoesNotRollback`（`nest/nest_test.go`）固化了该语义。

### 4. `DurabilityPipelined`：前缀持久化与外化闸门 —— `nest/rollback.go`、`nest/pipelined_completion.go`、`NEST_PIPELINED_COMMIT.md`

Strict 的代价是热点实体的锁持有时长包含 fsync。Pipelined 把两者解耦：

1. **锁内只做 `Enqueue`**（`PipelinedTransactionCommitter`）：同步完成全部可拒绝校验并分配 LSN——这是唯一拒绝点，且背压策略是拒绝而非等待（调用方持着锁，等待会把背压转化为锁占用）；随后给每个实体盖 `lastCommitLSN` 戳（`entity/entity_base.go`）。
2. **提前放锁**，fsync 在锁外由 group-commit 完成。
3. **回包与 `AfterCommit` 等到 ticket 变 durable 后**才执行。

正确性靠两条性质：**前缀持久性**（单 WAL 按 LSN 顺序 fsync，任一记录落盘则所有更小 LSN 已落盘）使进程内的级联脏读无需阻止——T2 若观察过 T1 的状态，T2 的 LSN 必大于 T1，崩溃只截 LSN 后缀，重放不出"有 T2 没 T1"的历史；**外化闸门**负责堵住脏状态离开进程——entitysync 与 checkpoint 对比 `entity.LastCommitLSN()` 和 committer 的 `DurableLSN()` 水位线，未落盘的状态不分发、不落库。committer 不实现该能力时派发返回 `ErrPipelinedCommitterRequired`，绝不静默降级；生产还应用 `NestOptionWithPipelinedAllowlist` 按 handler 灰度。Phase 2 异步完成（`completionPump`）把 durable 等待也移出 worker，per-entity 完成链（锁内 link，链序 = LSN 序）保证同实体完成回调在任何路径（池内、内联降级）都按提交序执行。

### 5. tombstone 的"同版本 delete 胜"语义 —— `checkpoint/flusher.go`、`checkpoint/redis_wal.go`

删除与保存的竞争按版本裁决，且**平局判给删除**、**复活要求严格更高版本**，三层实现同一规则：

- 内存去重（`Flusher.dedup`）：delete 只有在已存在**严格更高**版本的 save 时才让位；反过来 save 想取消同批次的 tombstone 必须版本**严格大于**它。
- Redis WAL Lua 脚本：token 形如 `s|d:version:fence:...`，同版本时先比 fence，再比 `d` 前缀——已有 `d` 记录时同版本 save 直接被拒。版本比较用 `decimal_greater`（先比字符串长度再比字典序）规避 Lua number 53-bit 精度陷阱；ACK 脚本用期望 token 的 CAS 删除，迟到的旧 ack 无法误删更新的记录。
- WAL replay 与后端 `BulkRemove` 沿用同一版本 CAS。

动机：普通生成实体 ID 永不复用，"同版本又出现一个 save"只可能是崩溃重放里的旧快照——判给 delete 就是防复活；真正的重建业务必须显式携带更高版本。

### 6. goroutine-ID 上下文的边界 —— `misc/goid_prod.go`、`entity/entity_guard.go`、`ctx/`

框架把三样东西挂在**当前 goroutine ID** 上：guard 作用域（`guardScopes sync.Map`）、请求上下文（`ctx` 包，事务通过它定位 `CurrentRollbackTx`）、锁可重入性（`ReentrantMutex.owner`）。`misc.GoID()` 非 race 构建用 `modern-go/gls` 高速取 gid，race 构建退化为 `runtime.Stack` 解析。

这是**被接受的架构决策**：它换来了业务代码零显式 context 传递的 handler 签名。其硬性约束是——**handler 内严禁裸 `go func(){...}` 后再访问框架能力**：新 goroutine 的 gid 不同，事务、guard、可重入锁全部静默丢失（读到 nil 或死锁，而不是报错）。需要异步工作时，用 `worker.Pool.Go`（受 `StopWithContext` 追踪）或把工作作为新消息投回 Nest。

该约束有静态检查器兜底：`go run ./cmd/glsvet ./...` 扫描 `go` 语句内对 goroutine 绑定 API（`RecordUndo`/`CurrentRollbackTx`/`fctx.CurrentContext`/`GetEntityGuard` 等）的调用并报错，已接入本仓库 CI；业务仓库同样可以把它加进自己的 CI。默认跳过 `_test.go`（测试常合法地模拟跨 goroutine 行为），`-tests` 可包含。

### 7. 锁内耗时预算：`nest.handler.lock_hold` —— `nest/nest_dispatch.go`

每次 dispatch 记录该 handler 从获得实体锁到释放（pipelined 提前放锁按提前点计）的时长 `nest.handler.lock_hold{handler}`（obs 的 timer 只有 count/sum/max/last，**没有分位数**——看均值用 sum/count，max 是进程生命期高水位永不衰减；需要分位数的时长用 `obs.ObserveHistogram`——17 桶指数直方图，1ms 起逐桶翻倍，`HistogramQuantile` 线性插值取 p50–p99，Prometheus 侧导出累积 `_bucket{le}`）；超过阈值（`NestOptionWithSlowLockThreshold`，默认 100ms，0 关闭告警）另计 `nest.handler.lock_hold.slow.total` 并记日志。这是选择 `DurabilityPipelined` 灰度对象的运营依据——锁内耗时被 fsync 主导的 handler 是最先受益者；灰度扩大到默认档的完整路线见 [NEST_PIPELINED_COMMIT.md](NEST_PIPELINED_COMMIT.md) §12。

### 8. 冷加载合并与缓存降级可见性 —— `entity/manager_access.go`、`cache/ref_hmap.go`

`ManagerAccess.Get` 的冷路径做 single-flight：并发请求同一实体只发一次 `LoadEntity`（错误共享、失败航班立即移除以便重试、等待者可被自身 ctx 取消），消除热实体冷启动惊群。`cache` 的 Redis Lua 写失败会降级为非原子回退——降级保留（可用性优先），但通过 `cache.refhmap.write_degraded_total` 指标与 Warn 日志强制可见：非原子窗口是运维必须知道的事实。

### 9. tick 回调与 handler 注册的作用域 —— `nest/ticker.go`、`nest/nest_dispatch.go`

tick 回调注册表按注册顺序实时生效（引擎启动后注册的回调下一个 tick 即执行，顺序确定）。handler 注册有两级作用域：包级 `MustRegisterHandlerWithMeta`（生产便利入口）与实例级 `(*NestMgr).RegisterHandlerWithMeta`（Start 前有效，实例优先查找）——测试与多引擎进程用实例级，避免共享包级注册表带来的重复注册冲突。

### 10. file journal 的组提交 —— `syncstream/file_journal.go`

生命周期 journal 的 `Record` 保持"返回即持久"，但并发调用会合并为一次 write+fsync（leader-follower 合批，常驻文件句柄），fsync 次数从每条降到每批——观察者频繁进出的场景不再被逐条 fsync 地板限速。

### 11. 输入帧同步与状态同步：三条通道的取舍 —— `lockstep/`、`entitysync/`、kit `sync/`

roost 的同步能力是三条并列通道，按"谁跑模拟"划分：

- **entitysync（core）**：按订阅主体推送字段级 delta + LOD，服务端跑模拟，客户端是显示器。通用状态同步。
- **kit `sync/room_replication`**：entitysync 之上的房间批处理前端——固定 tick（默认 50ms）把房间内全部脏主体合成一个**状态帧**下发。它是状态同步的分支，不是帧同步。
- **lockstep（core）+ kit `lockstep/`**：服务端只裁决**输入帧**——`Sequencer` 乐观帧锁定（到点就切帧，永不等慢客户端；缺席即空输入，迟到折入下一未切帧），模拟由客户端确定性执行（定点数学、注入随机、无墙钟——`roost-skill` 运行时天然满足该契约）。带宽与玩家状态规模无关，回放 = 输入历史，反外挂靠关键帧哈希多数派裁决（`DesyncDetector`）。

lockstep 的丢包策略是**冗余而非重传**：每个广播报文携带最近 N 帧（`RedundantEncoder`，深度 N 可修复连续 N−1 个丢包），走不可靠 datagram 通道（AEAD UDP）；追帧/重连走可靠通道（KCP/QUIC），按 tick 限速分页（kit `Room.StartCatchup`）。对实时帧广播用 ARQ 重传是用错工具——重传回来的帧已经过期。

### 12. 平台注册表：实例优先，包级 default 只是进程内兜底 —— `app/registry.go`

`app.NewRegistry` 预装 6 个平台 capability（health/obs/admin/admin.metadata/lifecycle/runtime.failure），其中**只有 obs 把包级 default 同步指向了实例**。因此平台能力一律通过 `app.Lookup[*T](r, app.ModX)` 取**实例**；包级 `health.Register`/`admin.Register`/`lifecycle.Register` 写的是另一个进程内 default，除 obs 外**不会被 app 装配路径（如 kit 的 ops HTTP）看到**。写 Mod/业务代码时把这条当铁律，能避免"注册了却查不到"的一整类问题。`hotcode.RegisterAdminCommands(reg)` 就按此契约设计：装配期从 `app.Lookup`（`app.ModAdmin`）拿实例传入，热修补命令才会出现在运维端点。

### 13. 缓存分层与选型 —— `cache/`

八种 store 按需组合：`LocalStore`（精确 LRU）、`AtomicLocalStore`（分片 + 读路径零锁竞争：读只取 RLock 不动 LRU 链，淘汰用插入时钟近似；**MaxBytes 按 shard 均分，单条超过 `MaxBytes/shards` 直接 `ErrEntryTooLarge`**）、`GroupedLocalStore`（O(1) 整组失效）、`LayeredStore`（write-through + 回读校验：写 remote 后回读填 L1，回读不存在则删本地——remote 是权威）、`ReadThroughStore`（single-flight 不开 goroutine，领航者在自己调用栈里加载；每 key 等待者上限超限**拒绝**而非排队）、四种 Redis 后端（JSON/Hash/Raw/SortedSet）、`RedisRefHMapStore`（反射把嵌套 struct 铺成多个 Redis hash + `Patch` 单字段更新 + Lua 原子重写，key 用 `{hash tag}` 保证 Cluster 同 slot）。两个通用陷阱：**配了 `Stale` 的 Redis store 每次写都先读一次做版本比较且读写不原子**；RefHMap 的 Lua 失败降级为非原子 DEL+HSET（保可用性，Warn + `cache.refhmap.write_degraded_total` 强制可见——降级窗口内可能读到空值）。

### 14. bus 的四条易踩契约 —— `bus/bus.go`、`bus/jetstream_rpc.go`

① **Bus 是一次性对象**：Stop 会退订包括 RPC 在内的全部订阅，而 Start 只重建基础 subject，所以 Stop 后拒绝重启（Start 失败可重试，成功过才不可重启）。② **`Send(toSid, module)` 是服务类型无关的**——发到 `{prefix}.srv.{sid}`，同 sid 的不同类型进程都会收到；定向到某类型用 `SendByType`。③ 有序性粒度：异步消息按 `ToModule` 哈希到固定 worker（**同 module FIFO、跨 module 并行**），RPC 按 method 哈希。④ **开启 JetStream RPC 后 `HandleRpc` 只注册持久化通道，但 `Call`/`CallTo` 仍走轻量 core-NATS**——要持久化必须显式 `CallReliable`/`CallToReliable`。另注意可靠消费的崩溃语义是**至多一次**：`BeginConsume` 只 SetNX 占位，handler 执行中崩溃后重投会被判重复而静默跳过（直到 inbox TTL 过期）；需要更强语义由 store 实现提供。

### 15. 跨 goroutine 的正确出路：`CaptureSnapshot` —— `ctx/context.go`

第 6 条讲了"handler 内禁止裸 `go func()` 后访问框架能力"，正向出路是：`snap := fctx.CaptureSnapshot()` → 新 goroutine 里 `ctx, release := fctx.NewContext(fctx.WithSnapshot(snap)); defer release()`。三个要点：snapshot **刻意不携带 Now**（每个 goroutine 装载时自己盖逻辑时间戳，时间不该跨 goroutine 冻结）；`Context` 是 `sync.Pool` 复用对象，**release 之后继续持有 `*Context` 是 use-after-free**；snapshot 的 keyValue 是浅拷贝，共享可变对象仍有竞态。`worker.Pool.Go` 的 goroutine 只在 pool 运行期间（Start 后 Stop 前发起）才被 `StopWithContext` 追踪；`worker.Cast` 是**有损**的（自旋超限或已关闭时静默 `OnRelease` 丢弃）——优先 `TryCast`/`TryDispatch`，并注意 `TryDispatch` 失败时任务未被释放（调用方负责）而 `Dispatch` 失败时已代为释放。

### 16. 单所有者组件清单（非并发安全，靠实体锁/单 worker 独占）

`timer.Scheduler`（无锁；Tick 内的增删改延迟到 tick 结束统一执行，闭包 timer 纯内存不持久化、不进快照；宿主用偏移时钟驱动 Tick 时必须 `SetClock` 同源注入，否则新建 timer 的 End 与 tick 时钟差一个偏移）、`fmap.FastMap`（开放寻址 + tombstone，为"已被外层锁保护的热路径"设计）、`misc.KeyMap`、`misc.ObjectPool`、`misc.BucketHolder` 的游标遍历（`RangeWithCursorCnt` 把全量扫描摊平到多个 tick——大规模实体周期巡检的惯用法）。与之相对：`fmap.ShardedSafeMap`（分片锁；`Compute` 持写锁回调不可重入同分片，`Range` 脱锁回调可安全改 map）、`fmap.SmallSafeMap`（唯一带 BSON codec、可直接嵌 DAO 字段的容器）。

### 17. 配置管线：meta 定义一切，映射零手写 —— `configdata/auto.go`、`configdata/external.go`、roost-codegen `cfggen`

配置接入有三条递进的通道，业务按场景选：

1. **手写 `TableDef`**（存量兼容）：Key/Index 函数手写，适合有特殊构建逻辑的表。
2. **`cfg` tag 自动注册**（`RegisterAutoTable`）：struct 上打 `cfg:"key"`/`cfg:"index[=名],skipempty"`/`cfg:"ref=表名[,required]"`，映射全部推导；嵌入（非指针）字段按 encoding/json 语义提升。`ref` 是 Luban 式悬空引用校验——目标表存在性与 key 类型兼容在**表级前置校验**（空表也拦拼写错误），每行成员校验非零值必须存在于目标表主键（零值 = 无引用，`required` 则零值即错）。tag 错误一律注册期 fail-fast；反射只发生在注册与构建期，读取路径零反射。配合 `Store.SetStrictJSON(true)` 可拒绝数据里的未知字段（防字段改名静默归零）。
3. **meta 文件全量生成**（推荐，类似简化版 Luban）：roost-codegen 的 `cfggen` 从一个 YAML schema（表/字段/类型/key/index/ref/bean 嵌套；`objects` 段定义无主键的全局单例配置）生成 struct + 注册 + 类型化访问器（含二级索引的强类型查询函数，如 `MonsterBySceneID(snap, 7)`），**业务只写 meta 文件和一行 `cfg.MustRegisterGeneratedConfigData(reg)`**。schema 错误（未声明的 key、悬空 ref、类型不匹配、meta 拼写错误）全部在生成期 fail-fast。端到端示例：`examples/configgen`；**meta 文件完整参考见 roost-codegen 的 `docs/CFGGEN_META.zh-CN.md`**。

**热更回调契约**（`ReloadListener`）：所有回调 panic 容器化（panic = 失败并整体回退）；`RollbackReload` 与 `BeforeApplyReload` 配对（只有 prepare 成功的监听者按逆序收到回滚）；`Old == nil` 的首次加载失败**不触发**回滚回调——监听者不得把它当"回退默认配置"。外部聚合的内容经 read 字节指纹进入 `Snapshot.Hash`；自带非导出状态的 custom 值需调 `Snapshot.SetFingerprint`，否则 build 报错提示。

接真正的 Luban 时用 `RegisterExternalTables`：Luban 管定义/校验/导出/代码生成（Excel 族源、bean 继承多态、ref/path/range 导出期校验），生成的 `Tables` 聚合作为快照成员装进 roost——原子热更、回滚、hash、`ActiveSnapshot` 请求一致性全部继承，Luban 侧零运行时。**已真实接入**：`examples/lubanreal` 的 `gen/` 与导出数据由官方 luban CLI（v4.11.0，XML schema + JSON 数据源）真实生成并可运行验证，重新生成见其 `gen.sh`。

### 18. 机器人框架：约定优于配置，业务只写 OnResp —— `robot/`、kit `robot/`

`robot` 从 cube 的 robot 服务提炼而来（实现拣入 core，业务协议留在业务侧），针对两个痛点重构：**手写太多**、**基建不全**。分层自下而上：

- **transport**：统一包协议 `[4B body_len][4B msg_id][4B seq]`（小端，seq=0 为服务端推送）。TCP/WebSocket 内置；`RegisterDialer` 是扩展点——kit `robot` 包据此注册 KCP（AES-GCM+FEC，流式复用同一帧协议）与 QUIC（单双向 stream 承载）客户端拨号。
- **session**：seq 匹配的请求/响应 + push 分发，`Call` 自动埋 `robot.session.call{msg,result}` 直方图（result 枚举 ok/timeout/closed/…）。未注册解码器的推送保留原始字节，不算错误。
- **action（消灭手写样板的核心）**：`RegisterCall[Req,Resp](reg, protocols, name, msgID, opts...)` 一行注册一个可在场景里引用的调用动作——请求字段按 json tag/snake_case 自动从场景参数与黑板取值填充，响应按 `GetCode() int32/int64` 约定判错，**业务唯一要写的是 `OnResp` 闭包**（把响应写回黑板/断言）。编码器按方向拆分安装（`EnsureEncoder`/`EnsureDecoder`），请求响应共用 msgID 也不冲突。
- **scenario**：行为树组合子（Sequence/Selector/Parallel/Retry/Timeout/加权 Random——随机数按 robot Seed 确定性），Go 代码是第一公民；YAML spec（`ParseSpec`）是**可选**通道，供无需编译的编排复用已注册动作，解析期全量校验（未知键/一节点多种类/路径定位错误）。
- **runner/loadtest**：借鉴 k6 的三种执行器——pool（闭环跑一遍）、looping（持续 + `Stages` 分段升降 VU）、arrival-rate（开环恒定到达率，测系统而非测机器人）；账目不变量 `Started == Success+Failure+Canceled`（中断的 bot 计 Canceled，不失踪）。`loadtest.Manager` 单活跃 run 状态机 + 环形历史 + 6 条 admin 命令（`robot.loadtest.start/stop/status/…/report`），`Threshold`（error_rate/p50–p99）不满足即整场判 failed（`StopReasonThreshold`），报告直接产出 Markdown 分位数表。10k bot 全生命周期基准 ~2s/60MB（goroutine-per-bot，万级单进程）。
- **lockstep 客户端半场**：core `lockstep.FrameAssembler`（冗余广播去重 + 严格顺序释放 + 缓冲越界报"该追帧了"）；kit `robot.LockstepBot` 在其上补输入提交、关键帧哈希上报（缺省哈希是输入链 FNV 折叠——确定性模拟下输入同则状态同）、每 gap 一次的追帧请求去重。回归基线：3 bot × 600 帧 × 30% 独立丢包，冗余 + 追帧后全帧应用、`DesyncDetector` 零误报。

端到端示例 `examples/robotdemo`：进程内游戏服 + 2 行 `RegisterCall` + YAML 场景 + 200 bot 压测 + 阈值裁决 + Markdown 报告。

### 补充几条常踩的契约

- **`lock.LockManager` 的重验合同**（`lock/lock_manager.go`）：锁实例可能被 `ReleaseLock` 并发释放重建，两个 goroutine 可能各持"同一 ID 的锁"。因此拿锁本身证明不了什么——加锁后必须重验受保护状态（`IsRemoved`/`IsClear`、索引成员资格），状态已消失就退让；释放方必须先在持锁状态下让状态不可达，再释放锁实例。
- **saga 的租约预算静态校验**（`saga/engine.go`）：`NewEngine` 在构造期校验 `(LeaseDuration − StoreTimeout) / Batch` 对 coordinator 与 publisher 的预算，拒绝任何"处理中租约过期 → 双主并发"的配置组合；Resume 递增 `Record.Incarnation` 并折入 `CommandID`，恢复后的命令与故障前的 completion receipt 永不碰撞。saga 还有几条同族设计：coordinator 时钟是唯一权威（远端 `CompletedAt` 被无条件覆盖为本地 UTC）；迟到的重复 completion 查收据表后**作为可 ACK 的成功返回**（防重投风暴）；缺失 definition 版本的记录直接 fence 到 `ManualRequired` 而不猜测；补偿阶段的失败一律进人工（补偿不可补偿）。
- **陈旧持有者必须能被 fence（三处同族契约）**：`redis.IDistLock` 是 best-effort（无 fence token，TTL 过期后旧持有者不自知）——只用于重复执行可容忍的场景；必须阻止陈旧写入时用 `redis.IVersionedLock`（fence 单调、下游 CAS 拒旧）；选主侧 `etcd.IsLeader()` 有固有 stale 窗口（服务端租约已过期、客户端未感知），**领导权敏感写必须携带 `IFencedElection.Fence()` 的 token 并在存储侧比较**。这与 nest WAL 的"结果不确定时 fence"是同一条设计不变量在分布式层的三个化身。
- **configdata 的热更不撕裂请求**（`configdata/configdata.go`、`ctx/context.go`）：`Reload` 成功后新快照写进 goroutine context 的 Config 槽，但**只有新建的 Context 才读到它**——在途 handler 手上仍是旧快照。取配置一律用 `configdata.ActiveSnapshot()`（优先 goroutine 绑定的快照，回退全局）；直接 `configdata.Current()` 会在热更瞬间读到新表，破坏同请求一致性。Rollback 只有一级（连续两次是在两个快照间来回），且回滚后 Version 不再单调——别用 Version 做审计/幂等键。
- **`errcode.ClientError` 是信息隐藏边界**（`errcode/errcode.go`）：任何非 `*IntError` 的错误统一变成 `(1, "server error")`，内部细节永不外泄给客户端；`errors.Is` 按 code 匹配（code 是唯一身份，重复 code 的 `Define` 会静默覆盖——大项目自行做重复检测）。`httpserver.HandleJSON` 则相反：把 handler error 文本直接返回客户端（400）——生产上配 `webroute.WriteResultWithMapper` 包一层。

## 学习路径

按由浅入深顺序，每步"源文件 + 对应测试"配对阅读（测试往往就是最好的用法文档）：

1. **实体模型**：`entity/entity_base.go` → `entity/example_gen_test.go`（模拟生成代码的完整样例）→ `entity/entity.go`（接口与 CreateParam）。
2. **锁与 worker 原语**：`lock/reentrant_mutex.go` + `lock/lock_test.go`；`worker/pool.go` + `worker/worker_test.go`（接纳即执行、同 key 串行）。
3. **Nest 调度主线**：`nest/nest.go`（引擎与选项）→ `nest/client.go`（Dispatch/Request 六个入口）→ `nest/nest_dispatch.go`（`NestDispatch` 到加锁调 handler 的全流程）→ `nest/nest_test.go`。
4. **事务与回滚**：`nest/rollback.go`（`RollbackTx`、`invokeWithTransaction`）+ `nest/transaction.go`（CommitRecord/Committer 契约）→ `nest/nest_test.go` 中 `TestRollback*`、`TestStrictCommit*`、`TestIndeterminateCommitDoesNotRollback` → 文档 `NEST_TRANSACTION_WAL.md`。
5. **Pipelined 提交**：文档 `NEST_PIPELINED_COMMIT.md` 先读正确性论证 → `nest/pipelined_completion.go` → `nest/pipelined_commit_test.go`、`nest/pipelined_async_test.go`。
6. **Guard 与实体管理**：`entity/entity_guard.go`（锁序、guard 作用域、release hook）→ `entity/entity_manager.go`、`entity/manager_access.go` + 对应测试。
7. **checkpoint 持久化**：`checkpoint/dirty.go` → `checkpoint/journal.go` → `checkpoint/flusher.go`（dedup/tombstone）→ `checkpoint/checkpoint.go` → `checkpoint/redis_wal.go` + `checkpoint/redis_wal_test.go`、`checkpoint/checkpoint_test.go`。
8. **跨实体与跨服**：`nest/cast.go` + `nest/cast_test.go`（锁序预检）→ `entity/entity_remote.go`、`entity/remote_manager.go`、`nest/remote_access.go` → 文档 `REMOTE_ENTITY.md` → `ownerroute/`。
9. **状态同步**：`entity/subject_sync.go` + `entitysync/subscription.go`（prepare/commit 两阶段）→ `syncstream/syncstream.go` → `replication/`（delta+LOD）→ 文档 `ENTITY_SYNC.md`。
10. **帧同步（输入帧）**：`lockstep/sequencer.go`（乐观帧锁定）→ `lockstep/wire.go`（冗余广播编码）→ `lockstep/history.go`、`lockstep/desync.go` → `lockstep/lockstep_test.go`（丢包仿真与确定性验证就是用法文档）→ kit `lockstep/room.go`（房间与传输接线）。
11. **编排与装配**：`saga/engine.go` + `SAGA.md`（`saga/engine_test.go` 开头的 `memoryStore` 与 `TestStoreContract*` 是 Store 实现者的必读规格）→ `bus/bus.go` + `bus/bus_lifecycle_test.go`（生命周期与 subject 布局的权威文档）→ `app/app.go`、`app/registry.go` + `app/example_test.go`（shared/service-specific mod 分层的完整装配样例）。
12. **语义即测试的推荐清单**：`worker/worker_test.go`（"接纳即执行"不变量的回归，注释写明了原缺陷）、`webroute/route_test.go`（生成路由运行时的完整用法说明书）、`configdata/configdata_test.go`（reload/DryRun/Rollback/listener 回滚）、`etcd/watch_callback_test.go`（无损背压 vs LocalMirror 订阅隔离的选型依据）、`bus/reliable_test.go`（去重按 consumer、DLQ requeue 语义）。

## 与 roost-kit / roost-codegen / roost-skill 的关系

```text
业务服务（roost-codegen 生成的项目骨架 + 手写玩法）
  ├── roost-core     通用运行时与抽象（本仓库，模块名 cube-core）
  ├── roost-kit      具体基础设施 Mod（模块名 cube-kit）：Redis、Mongo、NATS/JetStream、
  │                  etcd、nest WAL committer（kit/nestwal）、分布式锁、运维 HTTP 等，
  │                  实现 core 定义的接口并注册进 app.Registry
  └── roost-skill    可复用技能编译器与权威战斗运行时：其 combatcomponent 把战斗状态
                     接成 core 实体的 DAO（DirtyTracker + nest.RecordUndo 逆操作），
                     是"第三方库如何正确接入 core 事务体系"的参考实现
```

- **roost-core（本仓库）**：只定义抽象与框架语义。业务代码依赖这里的接口和稳定类型，不直接依赖任何中间件客户端。
- **roost-kit**：每个 Mod 实现 `app.Mod` 生命周期（Init 读配置 → Provide 注册 capability → Start 建连接 → Stop 释放），为 core 的 committer、StorageBackend、Redis WAL 等接口提供生产实现。
- **roost-codegen**：项目生成器 + 代码生成器。`roost project new <name>` 按 `roost.yaml` 生成服务骨架；扫描实体/组件/DAO 定义生成工厂、无副作用回滚快照、setter 级 inverse undo、`PrepareCommit` after-image 等——本 README 手写的样板在真实项目中全部由它产出。
- **roost-skill**：building on core 的领域库（技能/战斗），展示 checkpoint、nest 事务、syncstream 的完整集成方式。

本地联调多仓库时在共同父目录建 `go.work`（不要提交到任何仓库）：

```bash
go work init ./roost-core ./roost-kit <你的业务仓库>
```

## 开发与验证

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

CI 在 Linux 与 Windows 上运行完整测试矩阵，核心包开启 `-race`。修改公开接口时检查：生命周期是否可停止、是否需要 health/metrics、是否泄漏业务语义（core 不得出现 `player`、`alliance` 等玩法词汇）、能否在无具体中间件的测试环境中替换。修复并发/一致性缺陷必须附带能复现原缺陷的回归测试。

## 深入文档索引

可观测性规范与全仓指标清单：[OBSERVABILITY.md](OBSERVABILITY.md)（含 Prometheus 导出接线、告警基线与 [Grafana 总览面板](observability/grafana-roost-overview.json)）。


| 文档 | 内容 |
| --- | --- |
| [RUNTIME_EXECUTION_MODEL.md](RUNTIME_EXECUTION_MODEL.md) | 业务执行模型、锁边界、兼容迁移 |
| [NEST_TRANSACTION_WAL.md](NEST_TRANSACTION_WAL.md) | 内存事务、WAL commit point、indeterminate 语义、幂等要求 |
| [NEST_PIPELINED_COMMIT.md](NEST_PIPELINED_COMMIT.md) | Pipelined 提交：锁外 fsync、外化闸门、灰度与验收 |
| [ENTITY_SYNC.md](ENTITY_SYNC.md) | Entity 同步契约、prepare/commit、订阅协调 |
| [REMOTE_ENTITY.md](REMOTE_ENTITY.md) | 跨服实体写协议、fenced commit、恢复边界 |
| [SAGA.md](SAGA.md) | Saga 状态机、outbox、幂等与补偿 |
| [PRODUCTION_READINESS.md](PRODUCTION_READINESS.md) | 生产部署门禁与检查清单 |

## 许可证

[MIT License](LICENSE)。
