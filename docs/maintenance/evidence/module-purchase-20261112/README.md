# R2 Purchase 生成模块验收（2026-11-12）

实现提交 `cc0d7855`，基线 `6d9e6431`，工作分支 `feat/business-modules`，macOS、本地当前 core（生成工程 go.mod replace 指向本工作树）。实现与边界见 [purchase](../../../../modules/purchase/README.md)。不合 main、不发版；不兼容旧生成工程或旧订单格式。Player schema 4 删除 PurchaseClaims，常量大小的已发货序号与奖励同事务。

## 执行结果

- `GOWORK=off go test -count=1 -run '^TestModulePurchaseScenariosAndMutations$' ./codegen/internal/roost`：通过，见 [acceptance.log](acceptance.log)。最小工程 purchase 21 + wallet 14；加 bag / enter 后 6 行、**72 PASS / 0 SKIP**（purchase 21、wallet 14、bag 17、enter 14、purchase+bag 4、purchase+enter 2）。build/vet、ID、生成漂移与反向重复安装均由测试核对。是三个模块的增量安装/重复安装，不是全模块正反序矩阵。
- 同一入口完成 13 个变异，逐条要求命中指定行为失败，见下表；并运行 `go test -race -count=2 -run '^TestPurchase|^TestWithPurchase' ./game/handler ./game/purchase ./game/controllers/purchase ./internal/service/platform`。
- 临时独立 Redis（本机 redis-server，随机 loopback 端口，测试后只停止该子进程），生成工程 `ROOST_PURCHASE_REDIS_ADDR=<isolated> GOWORK=off go test -tags=integration -race -count=2 -v -run '^TestPurchaseRedis' ./game/purchase`：3 条 × 2 轮，**6 PASS / 0 SKIP**，见 [redis.log](redis.log)。65 笔分批清理后 hash 只剩两个游标；16 个并发 Record 同单仅分配一个序号；错金额/类型拒绝无空号。没有启用持久化，不声称验证了断电恢复或副本集。
- 新生成 game-demo 定向 `TestPurchase|TestWithPurchase|TestDemoPurchase` 通过（初版 18 条基础场景时生成），见 [demo-tests.log](demo-tests.log)。player controller、game service、loadtest **仅编译**通过，见 [demo-build.log](demo-build.log)；新加的终态/订单身份三条由最终新工程验收覆盖。全量 demo 延后，不把 `-run '^$'` 算行为测试。
- `GOWORK=off go test -count=1 ./service/platform`；清单/联动/安装步骤定向；`GOWORK=off go test -count=1 .`；文档链接检查：通过，见 [checks.log](checks.log)。文档检查当时 306 个文件，0 断链；报告自身暂存后再检查 308 个文件、0 断链。

## 变异

| 改坏的约束 | 检出场景 |
| --- | --- |
| 金币多发 1 | GrantCommitsRewardAndCursor |
| 取消旧序号重放返回 | ReplayReturnsOriginalWithoutWriting |
| 取消连续序号检查 | OutOfOrderWaitsForPrevious |
| 接受其他玩家号 | OtherPlayersGrantIsRejected |
| 结清订单也发奖 | SettledOrderOnlyAdvancesCursor |
| 512 笔后拒绝旧回执 | MoreThanAReceiptWindowKeepsOldReplaySafe |
| 不核对金额 | MismatchedMoneyDoesNotQueue |
| 有投递在途仍发货 | CollectionWaitsForInFlightDelivery |
| 背包多发 1 | WithPurchaseBagGrantAndReplay |
| 缺省渠道放行 | ChannelDefaultsToDeny |
| 未终结订单仍发货 | CollectionWaitsForNonterminalOrder |
| 平台返回另一订单仍发货 | CollectionRejectsWrongOrder |
| 同订单换付款摘要 | ExistingOrderRejectsChangedPayment |

## 初次失败与修正

- 手工生成 demo 首次用了不存在的 `-dir` 参数，CLI 拒绝，没有生成结果；按帮助改用 `-out` 后生成成功。
- 根包最初拒绝：真实依赖脚本还选择已删除的 `TestPurchaseDeliveryDurableWinner`。入口迁到模块三个 `TestPurchaseRedis*`；依赖发现同时支持显式 integration 模板经 helper 读环境变量，保证这三个都被登记，未放宽空测试检查。
- 文档首次 3 个断链：CARD-RPG / MERGE-CASUAL / SLG 仍指向旧 GrantPurchase；改为模块现行入口及精确验收口径后 0 断链。
- 最后复核补了平台订单号核对，并补终态/付款摘要拒绝场景，基础数从初验 18 增至 21，变异从 10 增至 13。保留初验与最终验收的区别。

## 未验收与恢复前提

最终批次仍需全模块正反序新装、全量 game-demo、全仓、TCP/NATS/持久 Redis/Mongo/WAL 联合链路、双 sid 与故障恢复。当前严格事务场景用真实 Nest/生成 DAO、替身提交器；真实 Redis 测试只证明 Lua 行为，不能拼成未经运行的端到端验收。

平台终态且 PendingAttempts 为空是安全清理的前提；管理入口仅可重开 exhausted，不可重开 delivered/settled。人工清除未知投递前须确认旧工作已停止。队列、Player 与平台账本要协调恢复，不能单侧重置；head 订单异常会暂缓该玩家之后的发货，平台次数上限保持。

CBM 刷新至 `2026-11-12T03:16:41Z`，核对本批 41 个现存代码路径；组件号、错误码、协议号模板占位符的局部解析缺口已逐行读取。demo 离线迁移教学目录被索引排除，已直接读源码；`.go.tmpl` 无函数节点时采用源码补证，没有据此作全仓完整性结论。
