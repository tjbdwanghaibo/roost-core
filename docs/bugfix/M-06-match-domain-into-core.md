# M-06:match 领域实现下沉 roost-core（ARCH-01 第一批）+ `servicemetrics` 契约进 core

**来源**:`docs/bug/REVIEW-2026-09-16-04.md` §7 ARCH-01(与 RR-20260916-05 / U-0217 同批,先把签名与行为对齐再迁)。
**类型**:架构迁移,不占 U 编号;不改行为、不改持久化 key / JSON 字段 / 错误码 / RPC 方法名。

## 目标形态

**Core 核心实现,Kit 装配与使用便利,Codegen 代码生成。** match 拆成两半:

| 半 | 内容 | 位置 |
| --- | --- | --- |
| 领域(本批已迁) | `Queue` / `Subject` / `Ticket` / `Match` / `TicketState`、errcode 段(550101–550199)与 `Error()`、`Store` 接口、`queueState` 状态机与 `NewStore`、`NewRedisStore`、`Grouping` / `FirstComeGrouping` / `ScoreWindowGrouping`、`Config`,加新增的 `NewMemoryStore` | `roost-core/service/match` |
| 计数契约(本批已迁) | `servicemetrics.Reporter` / `Sink` / `Wrap` / `Recorder` | `roost-core/servicemetrics` |
| 装配与传输(留 kit,待下一步) | `Mod`(读配置、取 Redis、注册 capability)、`Server` 与 sweep 循环、`Matchmaker` RPC 接口与生成的 `matchmaker_rpc_gen.go`(wire 类型、handler 表、BusClient、ClientMod——依赖 kit `mods`) | `roost-kit/service/match` |

## 本批改动(roost-core)

- 新增 `servicemetrics/`:原 kit `service/servicemetrics` 三个文件原样复制(含测试)。
- 新增 `service/match/`:原 kit 的 `types.go` / `store.go` / `queue_store.go` / `grouping.go` / `redis_store.go` 原样复制,
  `queue_store.go` 的 servicemetrics import 改指 core;新增 `NewMemoryStore(cfg)`(`queueState` 未导出,包外无法自己建
  内存 versionstore,kit 的 sweep 测试与单进程工具用它);随迁的领域测试:`match_test.go`、`commit_promises_test.go`、
  `enqueue_request_owner_promises_test.go`、`read_guards_promises_test.go`、`sweep_failure_promises_test.go`、`errcode_test.go`。
- `dependency_boundary_test` 通过:core 不引用 kit。`service/match`、`servicemetrics` `-race` 绿。

## 下一步(kit,**必须等 core v1.15.3 发版后**)

kit 的 `go.mod` 钉 core v1.15.2,CI 不用 source-head;kit 一旦 import `roost-core/service/match` 就在 v1.15.3 发布前编译不过。
发版后在 kit 做:

1. `service/servicemetrics` 改为别名包:`type Reporter = servicemetrics.Reporter`、`type Sink = …`、`type Recorder = …`,
   `func Wrap(r Reporter) Sink { return servicemetrics.Wrap(r) }`、`func NewRecorder() *Recorder`。十个 kit 服务包与 codegen
   生成的 collaborators(`import "roost-kit/service/servicemetrics"`)不用改。
2. `service/match` 删掉五个领域文件与六个领域测试,加 `alias.go`:类型别名(`Queue`、`Subject`、`SubjectKind`、`Ticket`、`TicketState`、
   `Match`、`Store`、`Config`、`Grouping`、`FirstComeGrouping`、`ScoreWindowGrouping`),常量(`Code*` 十个、`Ticket*` 四个、
   `DefaultTicketTTL`、`MaxGroupSize`、`MaxPageSize`、`MaxPayloadBytes`、`MaxQueueLength`),变量(`Err*` 九个,同一指针,
   `errors.Is` 不变),函数包装(`NewStore`、`NewRedisStore`、`NewMemoryStore`、`Error`、`Code`)。
3. `sweep_run_test.go` 的 `newStore` 改用 `match.NewMemoryStore`;`grouping_contract_promises_test.go` 的 `Config` 断言移到 core
   (已随 `match_test.go` 迁的那部分不含它,补一条)。
4. 验收:kit 全套;`go list -deps ./... | grep roost-kit` 在 core 为空;codegen `-template game-demo` 生成工程编译(demo 只用
   `svcmatch.` 别名,不用改)。
5. 之后再决定 `Matchmaker` RPC 接口与生成文件的归属:生成器要能把 wire / handler / BusClient(纯 core 依赖)与 ClientMod / Server
   (kit `mods` 依赖)拆成两个文件——这是 ARCH-04 的生成器部分。

## 不做

- 不改 `Matchmaker` 方法名、subject、errcode、Redis key。
- 不在 core 里放 `app.Mod`。
- session / mail 的领域下沉按同样形状另开 M 编号。
