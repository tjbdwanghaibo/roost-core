# Codegen 第一轮隔离复现与回归证据

运行环境：2026-09-30 本机 Windows/PowerShell，Core `1b7a2fc5aa4bfd9ae921ae8c80efd2392d08a165`。目录位于 `D:/whb_s/.tmp/review-codegen-20260930/`，不在提交中；均调用 Core 当前 `go run ./codegen/cmd/...`，没有改生产源码。

## RPC 标记删除

在 `rpc-retire/svc.go` 中声明 `ErrRequestInvalid` 和 `//roost:rpc service_type=sample capability=service.sample` 的 `Sample.Ping(ctx context.Context) error`，运行：

```powershell
go run ./codegen/cmd/servicerpc -dir D:/whb_s/.tmp/review-codegen-20260930/rpc-retire
# 将 svc.go 中的 //roost:rpc 标记改为普通注释，保留接口和生成文件
go run ./codegen/cmd/servicerpc -dir D:/whb_s/.tmp/review-codegen-20260930/rpc-retire -check
```

实际输出核心行：

```text
generated: sample_rpc_gen.go
generated: sample_rpc_assembly_gen.go
no //roost:rpc interfaces in D:\whb_s\.tmp\review-codegen-20260930\rpc-retire
CHECK_EXIT=0 BEFORE=sample_rpc_assembly_gen.go,sample_rpc_gen.go AFTER=sample_rpc_assembly_gen.go,sample_rpc_gen.go
```

`CHECK_EXIT` 由紧接第二次命令的 `$LASTEXITCODE` 记录；前后文件列表由 `Get-ChildItem -Filter '*_gen.go'` 得到。正向生成成功，退役后错误地通过检查，不是“初次生成本来没有输出”的假设。

## Protocol 最后一份定义删除

在隔离 module `example.com/review-protocol` 的 `protocol/def/game.go` 中写 `//roost:proto`、`PingRequest/PingResponse` 与 `//roost:msg id=10001`，以同一组选项运行两次 `go run ./codegen/cmd/protocol`：定义目录及 proto/PB/msgid/manifest 均指向隔离 module，`-bind '' -handlers '' -robot-protocol ''` 禁止无关输出。首次生成后把 `game.go` 改为仅 `package protocoldef`，第二次输出：

```text
generated: .../protocol/proto/protocol.proto
generated: .../protocol/pb/protocol.pb.go
generated: .../protocol/msgid/msgid_gen.go
generated: .../protocol/protocol_manifest.json
no protocol structs found in D:\whb_s\.tmp\review-codegen-20260930\protocol-retire\protocol\def
SECOND_EXIT=0 BEFORE=4 AFTER=4
```

后四份文件路径分别为 `protocol.proto`、`protocol.pb.go`、`msgid_gen.go`、`protocol_manifest.json`，均在第二次退出后存在。此复现只证明最后定义删除路径；单条消息删除以及带 `handler-bootstrap` 的分支待下一轮另测。

## 已有测试

- `go test ./codegen/internal/servicerpc ./codegen/internal/roost`：servicerpc **PASS**；roost 中 `TestDeployScriptsCarryNoKnownShellcheckFindings` 因 Windows PATH 没有 `sh`，对 14 个生成 shell 文件的 `sh -n` 返回 `exec: "sh": executable file not found`，该包为 **FAIL（环境）**。日志还提示 shellcheck 未安装；不计为框架缺陷。
- `go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings`：exit **0**；`attribute/cfggen/dao/entity/errcode/eventgen/marker/nest/project/protocol/registry/roost/servicerpc/tablegen/webroute` 均为 `ok`，无测试的 cmd/genutil 为 `? [no test files]`。该跳过仅隔离上述已知缺工具测试，未宣称完整 Linux 静态检查通过。

现有测试中的 `servicerpc.TestCheckDetectsDrift` 覆盖仍有 marker 的文件被手改；`dao.TestRunSweepsOrphansWhenAllDefinitionsRemoved` 覆盖 DAO 退役。当前两个反例是这两类之间的缺口。
