# v1.47.0 审查证据

被测 main：`603dd1394dcf892a25bfaa142454804a723de554`，v1.47.0 发布提交：`3a05eece48bf79e3f81ba93762c66ba8a83a63c3`。两个版本的本问题运行时代码相同；探针运行在 main。Linux amd64 / Go 1.27.2 / GOWORK=off / GOMAXPROCS=4。

本目录 `.go.txt` 只保存探针，不参与固定审查分支的编译。复现需要上述 main 的独立检出。CORE 为该检出绝对路径，WORK 为新建可写临时目录，EVIDENCE 为本证据目录绝对路径。固定报告分支的旧运行时不是被测版本。

## 核心基线

```bash
export GOWORK=off
cd "$CORE"
go test -race -count=1 -timeout 6m ./framework/sync/entitysync ./framework/sync/entitysync/policy ./gameplay/cellworld ./gameplay/terrain

go test -race -count=1 -timeout 6m ./framework/app ./framework/entity ./framework/nest ./framework/dataengine/engine ./wiring/dataengine ./infra/network/playeraccess ./infra/storage/mongo/driver

go test -race -count=1 -timeout 5m ./codegen/internal/dao ./codegen/internal/entity
```

依次对应 `baseline.log`、`ownership-baseline.log`、`codegen-baseline.log`，共 13 包通过。没有启用 integration build tag，没有根据包级 PASS 统计或宣称外部依赖场景全部执行。

## 正式生成模块与 Driver 缺陷探针

```bash
cd "$CORE"
go build -o "$WORK/roost" ./codegen/cmd/roost
"$WORK/roost" project new audit -module example.com/v147audit -out "$WORK/project" -template game --skip-deps
cd "$WORK/project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
git init -q .
"$WORK/roost" module add rehome
"$WORK/roost" module verify --json

cp "$EVIDENCE/review_driver_copy_unknown_test.go.txt" game/rehome/review_copy_unknown_test.go
go test -race -count=3 -timeout 3m -run '^TestReviewCopyCommittedButReplyLost$' -v ./game/rehome
```

`verify.json`：44 顶层场景，0 skip；build/vet/id check/generate --check 四项通过。缺陷探针不在模块 verify 的 `^TestRehome` 计数中。

`copy-unknown-probe.log`：**go test 退出 1**。控制组正常复制，account 改绑 1001、保留目标副本并删源副本；实验组先在 Documents 替身记下目标副本，再返回 `ErrCommitResultUnknown` + DeadlineExceeded，正式 Driver 中止 account 换服，释放源服保护且没有 Drop 目标副本。两组各三轮，实验均失败；race 未报数据竞争。

Driver 和 LocalResidency 是正式生成代码；Directory/Profile/会话关闭/交出为模块原夹具替身。该探针没有连接真实 Mongo，没有实际两个进程写同一玩家。

## 底层复制与预加载状态验证

```bash
cd "$CORE"
cp "$EVIDENCE/review_engine_copy_unknown_test.go.txt" framework/dataengine/engine/review_copy_unknown_test.go
go test -race -count=3 -timeout 3m -run '^TestReviewUnknownCopyCanBePreloaded$' -v ./framework/dataengine/engine
```

`copy-preload-characterization.log`：**go test 退出 0**，三轮确认风险状态。使用同包原夹具 newCopyServer/newPreloadServer、正式 CopyHandedOffEntity/PreloadLocalEntities 与事务内存 Mongo（mongotest）；仅装饰 ISession.WithTransaction，在真实内存事务成功后返回“提交结果未知”。源记录、目标记录和预加载均通过正式方法操作。

断言 Copy 返回空清单且携带 unknown 哨兵、目标文档确实存在、目标预加载报告 Loaded=1 且管理器确实持有该实体。它验证 Driver 不能忽略的底层状态，不把“测试通过”当成问题修复。该状态不违反 ISession 契约：正式驱动 session.go 对已发出但未确认的提交明确允许已提交结果。

修复后的 Driver 验收应让两个分组都通过；底层状态验证通常仍应通过，因为数据库结果未知本身不是 Copy API 能凭空消除的情况。还需要真实 Mongo 网络丢应答及重启恢复验收。本轮未运行负载测试。
