# 业务模块续审证据

被测提交：`e4627b597113538e19ad0f4bb3ff2fb2427a9a21`（feat/business-modules），2026-11-11，Linux / Go 1.27.2 / `GOWORK=off`。远端 main 为 `f78495440c52ff8d651a35b4b5eaa5758014815e`。复现不要使用固定审查分支的旧 core。

`.go.txt` 只作为证据存储，不参与该分支编译。旧问题探针现在应全部通过；两项新问题探针的实验组在被测提交上应失败（go test 退出 1），不能合并称作全绿。二者均三轮实验失败、三轮控制通过，未报告 race。

先在同提交的干净独立检出中设置 `CORE`（该检出绝对路径）、`EVIDENCE`（本目录绝对路径）、`WORK`（新建的可写临时目录）。以下使用固定的测试工程 module 路径；修改 module 时也要修改探针 import。

## account 旧问题复验

```bash
export GOWORK=off
cd "$CORE"
go test -race -count=1 -timeout 5m ./service/account
cp "$EVIDENCE/account_probe_test.go.txt" service/account/review_business_test.go
go test -race -count=3 -timeout 5m ./service/account -run '^TestReview' -v
```

日志为 account-baseline.log 与 account-fixed-probes.log。相对旧探针只把合法后续的 `second`/`new` 请求改为 `nextTransfer` 取得当前 ExpectSeq；原样重放的旧请求不改。依赖 account 同包原有测试夹具，MemoryStore，无外部服务。

## CLI、十五模块与钱包复验

```bash
cd "$CORE"
go build -o "$WORK/roost" ./codegen/cmd/roost
"$WORK/roost" project new audit -module example.com/businessaudit -out "$WORK/project" -template game --skip-deps
cd "$WORK/project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
git init -q .
"$WORK/roost" module add wallet checkin chat enter profile bag leaderboard mailbox task stamina shop skillbook room turn rehome
"$WORK/roost" module verify --json
cp "$EVIDENCE/review_wallet.go.txt" game/handler/review_wallet.go
cp "$EVIDENCE/review_wallet_test.go.txt" game/handler/review_wallet_test.go
"$WORK/roost" generate
go test -race -count=3 -timeout 5m ./game/handler -run '^TestReviewWalletMinDebit$' -v
```

verify.json：15 模块 + 5 联动，共 298 个顶层场景通过，零跳过；build、vet、ID、生成物漂移四项通过。wallet-fixed-probes.log：普通不足与 MinInt64 各三轮正确拒绝、余额及回执不变。生成的钱包 wrapper 只调用公开 Apply，经过正式 Sender/Nest/DAO；committer 是模块原有替身。

## BM-04：旧换服提交结论误判

在上述 game 工程中：

```bash
cp "$EVIDENCE/review_conclusion_test.go.txt" game/rehome/review_conclusion_test.go
go test -race -count=3 -timeout 5m ./game/rehome -run '^TestReviewLostCommitAfterLaterAbort$' -v
```

rehome-conclusion.log 保存结果。真实 account.Service + MemoryStore；真实模块 Driver 与 LocalResidency。Directory 包装器执行真实 Commit 后仅注入丢失应答，实验组在重试前执行另一笔真实 Begin/Abort，控制组不执行。Profile、断连和 HandOff 是模块原有测试替身；held 是替身交出句柄是否仍持有的观测值。没有两进程/Mongo 双写演练。

## BM-05：demo 公会前置条件在换服期间变化

```bash
cd "$CORE"
"$WORK/roost" project new demoaudit -module example.com/businessdemo -out "$WORK/demo" -template game-demo --skip-deps
cd "$WORK/demo"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
git init -q .
cp "$EVIDENCE/review_rehome_membership_test.go.txt" internal/service/game/review_rehome_membership_test.go
go test -mod=mod -race -count=3 -timeout 10m ./internal/service/game -run '^TestReviewRehomeMembershipChangesAfterPrepare$' -v
```

demo-membership-probe.log 保存结果。使用同包原有 newRouteWorld/newScenePlayer/sceneGetter/sceneCommitter/noticePush 夹具；正式 Controller Driver、Profile、demo 注册的公会前置检查、PlayerOwners、Nest 事务和 account.Service。成员通知的发送端及落盘/交出使用替身。交错点固定在 Begin 成功而应答尚未交给 Driver 时；后台结束用有限轮询观察，不以概率延时制造交错。控制组在 Prepare 前显示入会并正确拒绝；实验组在 Prepare 后显示入会却成功提交。

## 新增路径的既有回归

```bash
cd "$CORE"
go test -count=1 -timeout 15m ./codegen/internal/roost -run 'TestModule|Test.*Module|TestDemo'
go test -race -count=3 -timeout 5m ./infra/network/gateway -run '^TestGateForwardExposesTheBoundConnectionIdentity$' -v
cd "$WORK/project"
go test -race -count=1 -timeout 5m ./game/rehome ./game/handler ./game/controllers/room ./game/controllers/turn -run '^Test(Rehome|Room|Turn)'
cd "$WORK/demo"
go test -mod=mod -race -count=1 -timeout 5m ./internal/service/game -run '^(TestAStepForAPlayerThisProcessIsMovingOutIsBounced|TestALoginWithClaimsFromBeforeTheMoveIsRefused|TestAMemberNoticeReachesAnOnlineMember|TestAGuildOrTeamMemberMayNotChangeServers)$' -v
```

依次对应 codegen.log、gateway-binding.log、new-modules-race.log、demo-rehome-baseline.log。既有回归均通过；筛选式不包括新增 TestReview 探针。Gateway 使用本机测试监听端口，其余新场景不证明外部服务/跨机行为。

本轮没有全仓全量测试、联合真实依赖演练、kill -9 恢复、真实 WAL/Mongo 交出、Unity 或容量测试，没有运行新压测。复跑后仅移除自己复制的探针，勿删除原仓测试或历史压测材料。
