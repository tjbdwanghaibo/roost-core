# M-07：mail 领域实现下沉 roost-core（ARCH-01 第二批）

**来源**：`docs/bug/REVIEW-2026-09-16-04.md` §7 ARCH-01；形状沿用 M-06（match）。
**类型**：架构迁移，不占 U 编号；不改行为、不改 Redis key / JSON 字段 / 错误码（550301 段）/ RPC 方法名。

## 拆分

| 半 | 内容 | 位置 |
| --- | --- | --- |
| 领域（本批已迁） | `types.go`（`Envelope` / `Entry` / `Mailbox` / `Claim` / `SettledClaim` / `SendRequest` / `Page` / `Summary`、errcode 段与 `Error()` / `Code()`）、`mailbox.go`（邮箱状态机：已读 / 删除 / 三段式领取、淘汰规则）、`store.go`（`EnvelopeStore` / `MailboxStore` / `SendLedger` 接口）、`service.go`（`Service`、`Config`、`Deliverer`、`New`）、`redis_store.go`（`RedisConfig` / `RedisStores` / `NewRedisStores` / `NewRedisEnvelopes`） | `roost-core/service/mail` |
| 装配与传输（留 kit） | `mail.go`（`Mail` RPC 接口，`//roost:rpc`）、`mail_rpc_gen.go`（wire、handler 表、BusClient、ClientMod）、`mail_mod.go`（`Mod`：读配置、取 Redis、注册 capability）、`server_run.go` | `roost-kit/service/mail` |

## 本批改动（roost-core）

- 新增 `service/mail/`：五个领域文件原样复制，`servicemetrics` import 改指 `roost-core/servicemetrics`；
  `types.go` 里两处"belongs to roost-kit"的注释改成事实（`versionstore.ErrConflict` 属 versionstore；`servicerpc.Error` 约定在 kit 传输层）。
- 随迁的领域测试：`mail_test.go`（harness：`newHarness` / `clock` / `tokens`）、`fake_envelopes_test.go`、`errcode_test.go`、`redis_store_test.go`、
  `atomic_refusal_` / `claim_identity_` / `entry_guards_` / `get_consistency_` / `send_race_` / `settled_claim_retention_` / `validate_promises_test.go`，
  以及 `//go:build integration` 的 `redis_integration_test.go`（`REDIS_ADDR` 未设即 skip；core CI 的 `go vet -tags integration ./...` 覆盖编译）。
- 留在 kit 的测试：`mail_mod_test.go`、`rpc_test.go`、`wiring_promises_test.go`（断言 Mod / Server / 生成传输）。
- `dependency_boundary_test` 通过；`service/mail` `-race` 绿。

## kit 半（已于 2026-09-16 随 core v1.15.4 发版后完成，kit v1.14.5；原计划如下，均已按此做）

1. 删掉五个领域文件与随迁的测试，加 `alias.go`：类型别名（`Audience`、`Status`、`Envelope`、`Item`、`Entry`、`Mailbox`、`Claim`、`SettledClaim`、
   `SendRequest`、`SentRecord`、`Page`、`Summary`、`Config`、`Service`、`Deliverer`、`DelivererFunc`、`EnvelopeStore`、`MailboxStore`、`SendLedger`、
   `RedisConfig`、`RedisStores`），常量（`Code*` 十五个、`Audience*`、`Status*`、`Default*`、`Max*`），变量（`Err*` 十四个，同一指针），
   函数包装（`New`、`NewRedisStores`、`NewRedisEnvelopes`、`Error`、`Code`）。`Mail` 接口的方法签名引用的都是别名类型，不用改。
2. kit 保留的三个测试用到 `newHarness` / `h.service` / `h.clock`：在 kit 放一个精简的 `harness_test.go`（`clock`、`tokens`、`newFakeEnvelopes` 的最小版或直接用
   `versionstore.NewMemoryStore` + 一个内存 `EnvelopeStore`），不要把领域测试整套留在 kit 重复跑。
3. 验收：kit 全套；`go list -deps ./... | grep roost-kit` 在 core 为空；codegen `-template game-demo` 生成工程编译（demo 的 `level_up_mail.go`
   只用 `mail.SendRequest` / `mail.Item` 别名）。

## 不做

- 不改 `Mail` 方法名、subject、errcode、Redis key；不在 core 放 `app.Mod`。
- 生成文件的 wire / handler / BusClient 与 ClientMod / Server 的拆分归 ARCH-04。

## 实际做法与计划的差异

- `NewRedisEnvelopes` 未在 kit 别名：其参数类型 `envelopeClient` 未导出，kit 内也没有调用方（`mail_mod.go` 只用 `NewRedisStores`）。
- kit 的 `fake_envelopes_test.go` 留下作为 Mod / 传输测试的替身，其中 `Envelope.clone()` 改为本地 `cloneEnvelope`（同样只深拷两个切片）。
- `TestRedisStoresRefuseInvalidConfigAndEmptyIDs` 原在 kit 的 `wiring_promises_test.go`，断言的是 store，一并搬进 core（`redis_guards_promises_test.go`）。
