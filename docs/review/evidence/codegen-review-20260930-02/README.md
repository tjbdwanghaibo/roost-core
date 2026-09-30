# Codegen 修复前后与第二轮隔离审查证据

基线 Core `f6566d4e0a9026484d66d78f53036bbd9bc5f3fc`；Windows/PowerShell；修复源码在隔离工作树，生成夹具位于 `D:/whb_s/.tmp/review-codegen-20260930/`，均不提交。原始第一轮 CLI 结果见 [上轮证据](../codegen-review-20260930-01/README.md)。

## 固定回归与 CLI

正式测试先加入包内再在未修源码运行：

```text
go test ./codegen/internal/servicerpc ./codegen/internal/protocol -run TestRunRetires -count=1
FAIL servicerpc: -check accepted retired transport: <nil>
FAIL protocol: retired output remains .../protocol/proto/protocol.proto: <nil>
```

修后定向 `TestRunRetires|RunPreserves|RunDoesNotRetire|SyncCommitsProtocolRetirement|GenerateCheckDetectsMarkerlessProtocolDrift` 在 `protocol/servicerpc/roost` 三包均通过；相同范围 `go test -race ... -count=1` 三包均通过。`roost` 的 `TestSyncCommitsProtocolRetirement` 实际用 `NewProject`、`Add protocol`、`Generate`、删定义、`SyncProject`，确认 `result.Removed` 四个默认产物均有；`TestGenerateCheckDetectsMarkerlessProtocolDrift` 单独改 proto/manifest 并确认 `-check` 逐一失败。

本机重复上轮保留的两个 CLI 夹具：

```text
servicerpc -dir <rpc-retire> -check
  STALE: sample_rpc_assembly_gen.go (orphan)
  STALE: sample_rpc_gen.go (orphan)
  RPC_CHECK_EXIT=1
servicerpc -dir <rpc-retire>
  removed orphan: sample_rpc_assembly_gen.go
  removed orphan: sample_rpc_gen.go
  GENERATE_EXIT=0 REMAINING=0

protocol -def <protocol-retire/protocol/def> ... -bind '' -handlers '' -robot-protocol ''
  removed orphan: protocol.proto / protocol.pb.go / msgid_gen.go / protocol_manifest.json
  PROTOCOL_EXIT=0 REMAINING=0
```

`go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings` 在最终源码上通过，Codegen 所有包均为 `ok` 或无测试文件；`go vet ./codegen/internal/protocol ./codegen/internal/servicerpc ./codegen/internal/roost` 通过。现有 12 处 service/kit RPC `go:generate` 指令按各自目录和参数执行 `-check`，12/12 返回 0，既有产物全部 `up to date`，没有误报孤儿文件。原样不跳过全包测试时 `roost` 包唯一已知环境失败为本机缺 `sh`（上轮已记录）。本轮没有安装或模拟 shellcheck，也未替代 Linux CI。

## 新 review 夹具

| 当前 CLI / 输入变化 | 首次输出 | 第二次输出与磁盘结果 |
| --- | --- | --- |
| `go run ./codegen/cmd/entity -dir <entity-retire>`；把唯一 `//roost:entity` 改为普通注释 | `generated: player_gen_wire.go`，exit 0 | `no entity markers found`，exit 0；`OLD_WIRE_EXISTS=True`，文件第16行仍有 `//roost:register phase=entity`。 |
| `go run ./codegen/cmd/nest -dir <nest-retire>`；把唯一 `//roost:nest` 改为普通注释 | wrapper、sender、guard test、syncsender 四份生成，exit 0 | `all files up to date`，exit 0；`OLD_COUNT=3` 是三份 `*_nest_gen.go`，另有 `sender/handler_nest_gen_test.go`，合计旧四份留存。 |

这两项只是新问题的隔离 CLI 复现；正式项目下的 registry 聚合与最终业务行为尚未动态验证。本轮未修改 Entity/Nest 源码。
