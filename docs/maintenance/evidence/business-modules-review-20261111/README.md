# 业务模块审查证据

被测提交：`fd0de4cf6e321c032e72afe18cd6cf4670e94b05`，分支 `feat/business-modules`。环境：Linux / Go 1.27.2，日期 2026-11-11。

本目录只有报告证据；`.go.txt` 不参与固定审查分支编译。三项缺陷尚未修复，所以探针的正确性断言在被测提交上预期失败。失败日志与控制组一起保留，不能把这些失败当成普通套件全绿。

## 复跑 account 两项

在上述提交的干净独立检出中，设 `CORE` 为该检出绝对路径、`EVIDENCE` 为本目录绝对路径：

```bash
export GOWORK=off
cp "$EVIDENCE/account_probe_test.go.txt" "$CORE/service/account/review_business_test.go"
cd "$CORE"
go test -race -count=3 -v ./service/account -run '^TestReview'
```

`TestReviewBeginHoldAfterAbort/control` 与 `TestReviewAbortTombstoneSurvivesOtherAbort/control` 各三次通过；`delayed` 与 `second_abort` 各三次失败。使用 account 自带 MemoryStore 夹具，没有启动外部服务。

## 复跑 wallet

用同一检出构建 CLI；不要使用固定审查分支的旧 core。`WORK` 必须是新建且可写的临时目录。

```bash
export GOWORK=off
go build -o "$WORK/roost" "$CORE/codegen/cmd/roost" # 在 CORE 目录执行
"$WORK/roost" project new audit -module example.com/businessaudit -out "$WORK/project" -template game --skip-deps
cd "$WORK/project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
git init -q .
"$WORK/roost" module add wallet checkin chat enter profile bag leaderboard mailbox task stamina shop skillbook
"$WORK/roost" module verify --json
cp "$EVIDENCE/review_wallet.go.txt" game/handler/review_wallet.go
cp "$EVIDENCE/review_wallet_test.go.txt" game/handler/review_wallet_test.go
"$WORK/roost" generate
go test -race -count=3 -v ./game/handler -run '^TestReviewWallet'
```

探针中的 Go module 固定为 `example.com/businessaudit`，换工程 module 要同步改探针 import。wrapper 调公开 `WalletComp().Apply`，生成 Sender / Nest dispatch 保证在正确事务和实体锁内执行；未修改钱包模块逻辑。Memory committer 是模块原有夹具，不是真实 WAL/Mongo。

控制组 delta=-101 不足且余额/回执保持；实验组 delta=MinInt64 返回 nil，余额变负并增加成功回执。三轮结果见 wallet-probes.log。

## 原有回归的命令

account 原有整包在加入探针前运行：

```bash
go test -race -count=1 ./service/account
go test -count=1 -timeout 15m ./codegen/internal/roost -run 'TestModule|Test.*Module|Test.*Runtime'
go test -count=1 ./codegen/internal/dao
go test -race -count=1 -v -timeout 5m ./framework/dataengine/engine -run 'Test.*(HandOff|HandedOff|UnconfirmedProjectionReplayed)'
```

verify-final.json 是正式 CLI 的最终原始 JSON，12 模块 + 4 联动，共 206 个顶层场景通过、0 skip，另有 build/vet/id check/generate --check 四项成功。该命令按模块自带测试筛选，不运行 `TestReviewWallet`；它与新增缺陷探针为两组证据。DataEngine 默认测试未编入带 integration 标签的真实 Mongo 测试。没有压测或完整 game-demo 真实服务演练。
