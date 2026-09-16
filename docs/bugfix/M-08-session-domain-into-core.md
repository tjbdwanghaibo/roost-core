# M-08：session 领域实现下沉 roost-core（ARCH-01 第三批）

**来源**：`docs/bug/REVIEW-2026-09-16-04.md` §7 ARCH-01；形状沿用 M-06 / M-07。
**类型**：架构迁移，不占 U 编号；不改行为、不改 Redis key / JSON 字段 / 错误码 / RPC 方法名。

## 拆分

| 半 | 内容 | 位置 |
| --- | --- | --- |
| 领域（本批已迁） | `types.go`（`Run` / `State` / `Resource` / `Claim` / `LedgerEntry` / `EnterRequest`、errcode 段与 `Error()` / `Code()`）、`service.go`（`Service`、`Config`、`Releaser`、`New`：幂等 Enter、每 owner 一个活 run、资源恰好释放一次、截止时间）、`redis_store.go`（`RedisConfig` / `RedisStores` / `NewRedisStores`）、`admin.go`（`Admin` 操作面：强制释放资源，**不上总线**） | `roost-core/service/session` |
| 装配与传输（留 kit） | `session_rpc.go`（`Session` RPC 接口，`//roost:rpc`）、`session_rpc_gen.go`（wire、handler 表、BusClient、ClientMod、`Capability` 包装器、`OwnerCapabilities`）、`session_mod.go`（`Mod`）、`server_run.go` | `roost-kit/service/session` |

## 本批改动（roost-core）

- 新增 `service/session/`：四个领域文件原样复制，`servicemetrics` import 改指 `roost-core/servicemetrics`；`types.go` 两处过时注释同 M-07。
- 随迁的领域测试：`session_test.go`（harness：`newHarness` / `clock` / `enterReq` / `newReleaser` / `barrierLedger`）、`admin_test.go`、`errcode_test.go`、
  `ledger_failure_test.go`、`enter_collision_cleanup_` / `enter_ledger_collision_` / `guards_` / `validate_promises_test.go`。
- `admin_test.go` 里的 `TestTheOperatorSurfaceIsNotOnTheSessionInterface`（断言生成的 `Capability(&Service{})` 不满足 `Admin`）**留在 kit**：
  它断言的是传输层包装器，领域包里没有 `Capability` 这个符号；core 副本删掉这一条并在文件尾注明。
- 留在 kit 的测试：`session_mod_test.go`。
- `dependency_boundary_test` 通过；`service/session` `-race` 绿。

## kit 半（等 core v1.15.4 发版后做）

1. 删掉四个领域文件与随迁测试（`admin_test.go` 只保留 `TestTheOperatorSurfaceIsNotOnTheSessionInterface`），加 `alias.go`：类型别名
   （`State`、`Resource`、`Run`、`Claim`、`LedgerEntry`、`EnterRequest`、`Config`、`Service`、`Releaser`、`ReleaserFunc`、`RunStore`、`ClaimStore`、
   `RequestLedger`、`Admin`、`RedisConfig`、`RedisStores`），常量（`Code*` 十三个、`State*` 五个、`DefaultTTL`、`Max*` 四个），变量（`Err*` 十二个），
   函数包装（`New`、`NewRedisStores`、`Error`、`Code`）。`Session` 接口签名引用别名类型，不用改。
2. `session_mod_test.go` 用到 `newHarness` / `newReleaser`：kit 放精简 `harness_test.go`。
3. 验收同 M-07。

## 不做

- 不改 `Session` 方法名、subject、errcode、Redis key；`Admin` 继续不上总线（那条断言留在 kit 是刻意的）。
