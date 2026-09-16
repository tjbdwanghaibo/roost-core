# M-10：servicerpc 生成的传输拆成 transport / assembly 两半（ARCH-04 生成器部分）

**来源**：`docs/bug/REVIEW-2026-09-16-04.md` §7 ARCH-04（"codegen 不再生成旧职责下的接口引用"）；M-06 记录里"kit 半 第 5 步"。
**类型**：生成器重构，不占 U 编号；生成物的符号集、subject、错误码、capability 名不变，只是分到两个文件。

## 问题

`servicerpc` 以前把一个 `//roost:rpc` 接口的全部传输生成进一个 `<iface>_rpc_gen.go`：wire 类型、handler 表、`BusClient`、capability 包装、
`OwnerCapabilities`、`Server`、`ClientMod`。后三者要 `roost-kit/mods`（Mod 名 `ModBus` / `ModNats`、`mods.Capability`、`RegisterAll`），
于是整份传输都拖着 kit 依赖——RPC 接口（`Mail` / `Session` / `Matchmaker`）搬进 core 领域包（M-06～M-08）时生成文件跟不过去，
core 的 `dependency_boundary_test` 会红。

## 改动（roost-codegen `internal/servicerpc`）

- `template.go`：`transportTemplate` 只剩传输半（常量、`rpcStatus`、wire 类型、`RegisterHandlers`、`BusClient`、`capability` / `Capability()`、
  `CapabilityName` / `LocalCapabilityName`），import 去掉 `log/slog`、`viper`、`roost-kit/mods`；新增 `assemblyTemplate`
  （`OwnerCapabilities`、`Server`、`ClientMod`），两份模板共用 `templateFuncs`。两份文件头互相指名对方（`FileBase` 视图字段）。
- `gen.go`：`type File{Name, Content}`；`Generate(service) ([]File, error)` 依次返回 `TransportFileName(iface)`（`<iface>_rpc_gen.go`）与
  `AssemblyFileName(iface)`（`<iface>_rpc_assembly_gen.go`）；渲染 + `go/format` 抽成 `render(tmpl, half, …)`。
- `run.go`：逐文件比对 / 写入 / `-check`；老仓库第一次跑会报 `…_rpc_assembly_gen.go (missing)`。
- 测试：`split_promises_test.go`（传输半的 import 声明里没有 `roost-kit`、装配半 import `roost-kit/mods`、两半各自的声明集互不重叠）；
  `golden_test.go` 改用 `generateJoined` / `parseGenerated`（emittedNames 的两条对照测试对两个文件的声明取并集）；golden 拆成
  `shop_rpc_gen.go.txt` + `shop_rpc_assembly_gen.go.txt`。codegen 全套 `GOWORK=off go test ./...` 绿。

## 实际接起来（roost-kit）

用新生成器重生成 kit 全部 `//roost:rpc` 接口（mail / session / match 等），每个包多出一个 `*_rpc_assembly_gen.go`；
`GOWORK=off go build ./... && go vet ./... && go test ./...` 绿。kit 没有 codegen 的 `-check` CI 门（`go:generate` 走 go.work 的本地 codegen）。

## 之后

- M-06～M-08 的 kit 半做完后，下一步是把 `Mail` / `Session` / `Matchmaker` 接口连同传输半搬进 `roost-core/service/<x>`，kit 只保留装配半
  （`OwnerCapabilities` / `Server` / `ClientMod` 引用别名类型即可）。这需要 kit 与 core 各一次发版。
- 不做：不改 `Server` / `ClientMod` 的行为；不把 `mods` 的 Mod 名常量搬进 core。
