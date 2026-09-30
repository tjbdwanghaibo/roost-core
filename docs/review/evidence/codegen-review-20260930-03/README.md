# Codegen 第三轮修复与新审查证据

基线 Core `5fedc6526d49378246f76fa6ea9cf9e582435eee`；隔离工作树修改 Entity/Nest。新问题可从仓库根目录执行 [Run-Repro.ps1](Run-Repro.ps1)：脚本在系统临时目录创建唯一 `FIXTURE` 路径，复现后保留夹具供核对；夹具不提交。这里记录可复跑的输入变化和结果，不把临时夹具当正式包回归。

## RR-04/05 修前红、修后绿

先添加 `codegen/internal/entity/retirement_test.go` 与 `nest/retirement_test.go`，在原源码运行：

```text
go test ./codegen/internal/entity ./codegen/internal/nest -run TestRunRetires -count=1
FAIL entity: retired output remains player_gen_wire.go / player_gen_wire_test.go
FAIL nest: retired output remains handler_nest_gen.go / sender/handler_nest_gen.go / sender/handler_nest_gen_test.go / syncsender/handler_nest_gen.go
```

修后定向 `TestRunRetires|TestRunMoves|TestRunWithout|TestStagedProjectCommitSeesEntityRetirement` 在 entity、nest、roost 三包通过；同范围 `go test -race ... -count=1` 三包通过。`go vet ./codegen/internal/entity ./codegen/internal/nest ./codegen/internal/roost` 通过。正式 NewProject/Add/Generate 暂存测试调用 `copyProject`、Entity generator、`planStagedProjectCommit`、`commitSyncChanges`，确认原项目旧 wire 实际删除。

曾尝试完整 `GenerateTransactional` 的业务项目消费者链；`go mod tidy` 在默认 `C:/Users/tjbdw/go/pkg/mod/...v1.17.2.lock` 写入被本机权限拒绝，因此该尝试不是产品失败，也不计为成功。暂存/提交独立测试不需下载模块。本机最终源码执行 `go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings`，所有 Codegen 测试包 `ok`、无测试文件包单列，exit 0；具名跳过是 Windows PATH 无 `sh` 的既有环境限制。Linux shellcheck 没有在本机运行。

## 新问题的隔离 CLI

先写合法输入并运行当前 `go run ./codegen/cmd/<generator>`，确认生成物存在；再把最后一个 marker/定义改为普通输入并用相同参数重跑，均 exit 0：

```text
attribute -dir <attribute>: ATTRIBUTE_OLD_EXISTS=True
eventgen -def <event/def> -out <event/out> -eventpkg example.com/game/event: EVENT_OLD_COUNT=3
webroute -dir <webroute>: WEB_OLD=webroute_gen.go
tablegen -meta <tablegen/configs/schema> -csv <.../table> -json <.../data> -force: TABLE_JSON_OLD_EXISTS=True
errcode -root <errcode> -out <errcode/errcode.csv> (only a commented-out Define): ERRCODE_COMMENT_GHOST=True
```

Event 的动态结果只证明**最后一个定义**删除，部分 handler/receiver 删除依据源码，尚未独立运行。Tablegen 的 CSV fixture 为单行 `monster.csv`；生成 `monster.json` 后清空唯一 schema，旧 JSON 留存。Errcode 的 `errors.go` 只有 `// retired: var ErrGhost = errcode.Define(500999, "ghost", "removed")` 注释，CSV 却包含 500999。以上均是复现，不是修复。
