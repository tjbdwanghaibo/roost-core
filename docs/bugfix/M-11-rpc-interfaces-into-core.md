# M-11：`Mail` / `Session` / `Matchmaker` RPC 接口连同传输半进 core 领域包（ARCH-01 / ARCH-04 收尾）

**来源**：`docs/bug/REVIEW-2026-09-16-04.md` §7 ARCH-01 / ARCH-04；M-06～M-08 记录里都写着"下一步"。
**类型**：架构迁移，不占 U 编号；subject、方法名、wire JSON 字段、capability 名、错误码全部不变。

## 目标形态

| 半 | 内容 | 位置 |
| --- | --- | --- |
| 接口 + 传输半 | `//roost:rpc` 接口（`Mail` / `Session` / `Matchmaker`）与 `<iface>_rpc_gen.go`（常量、wire 类型、`RegisterHandlers`、`BusClient`、`Capability` 包装、`CapabilityName` / `LocalCapabilityName`）；只依赖 roost-core | `roost-core/service/{mail,session,match}` |
| 装配半 | `<iface>_rpc_assembly_gen.go`（`OwnerCapabilities` / `Server` / `ClientMod`，依赖 kit `mods`）、Mod、server run、别名 | `roost-kit/service/{mail,session,match}` |

## 生成器（roost-codegen，d6e11f1）

- `servicerpc` 新增 `-emit transport|assembly|all`（默认 all）与 `-out <dir>`；`GenerateWith(service, Options{Half, Regenerate})`。
- 文件头的"Regenerate with"记录实际命令（`-dir` 按给定值、`-emit`、`-out`），所以从别的包的接口生成的装配半自己说明来源。
- `-dir` 接受 import path：不存在于磁盘且形如 `github.com/...` 时用 `go list -f {{.Dir}}` 解析（在 `-out` 目录的模块上下文里），kit 的 go:generate 因而不用写模块缓存的绝对路径。
- 测试 `halves_promises_test.go`：两半可分开生成到不同目录、头部命令、`-check` 对另一目录生效、未知 half 拒绝。

## core 半（本批）

- `service/mail/mail.go`、`service/session/session_rpc.go`、`service/match/match_rpc.go`：原 kit 接口文件原样搬入，`go:generate` 加 `-emit transport`；三个 `*_rpc_gen.go` 用本地 codegen 生成。
- 测试：kit `service/mail/rpc_test.go` 的传输部分（`Methods` 与接口逐一对照、`RegisterHandlers` 只发布声明的方法、本地 / 总线两种实现行为一致、错误码穿过两种传输、身份参数逐方法核对、handler 原样传递身份）连同 `newTransport` / `spyMail` / `fakeBus` / `invoke` 进 core；session 的 `TestTheOperatorSurfaceIsNotOnTheSessionInterface`（生成的 `Capability` 包装器不满足 `Admin`）进 core。
- `dependency_boundary_test` 通过（传输半只 import core）；`service/*` `-race` 绿。

## kit 半（已于 2026-09-16 随 core v1.15.5 发版后完成，kit v1.14.6；原计划如下，均已按此做）

1. 删 `mail.go` / `session_rpc.go` / `match_rpc.go` 与三个 `*_rpc_gen.go`；`alias.go` 追加：接口别名（`Mail` / `Session` / `Matchmaker`）、`BusClient`、`NewBusClient`、`RegisterHandlers`、`Capability`、`ServiceType`、`CapabilityName`、`LocalCapabilityName`、`DefaultCallTimeout`、`Method*` 常量与 `Methods`。
2. 装配半改为从 core 的接口生成：`//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir github.com/tjbdwanghaibo/roost-core/service/mail -emit assembly -out .`（放在 `alias.go`）。
3. kit `rpc_test.go` 只留 Mod / Server 形状的断言（两个 Mod 同名、ClientMod 发布接口而非具体类型、没有 bus 时报错、Server 拒绝 client capability / 缺 capability）。
4. 验收：kit 全套；codegen 生成工程编译（生成物只引用 kit 别名：`mail.Mail`、`svcmatch.Matchmaker`、`svcmatch.CapabilityName`）。

## 不做

- 其余六个 kit RPC 服务（account / chat / global / activity / platform / rank）的领域不在 core，接口与两半仍整体在 kit。
- 不改任何 subject / 方法名 / capability 名。

## 更正（2026-09-17）

- core 三份 `*_rpc_gen.go` 首次是用 `-dir /Users/whb/roost/roost-core/service/<x>` 生成的，文件头记录了绝对路径，按包内 directive（`-dir . -emit transport`）跑 `-check` 会报 STALE（审查 09-16 第五轮观察项）。已从各包目录 `go generate` 重生成，正文逐字节相同，只有命令头变化。教训：生成命令要从接口包目录、按 directive 原样跑。
