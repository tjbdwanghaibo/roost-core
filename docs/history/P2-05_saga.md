# P2-⑤：saga 实现合入 core/saga

批次：P2-⑤（收敛方案 §4；统一方案 M-05）
状态：完成（core 侧）；kit 侧旧路径保留到 P3
目标与非目标：Saga 的 JetStream 传输、命令消费者、Mongo 命令收件箱、Data Engine 步骤收件箱、Nest 启动消费者、Mongo 存储带历史合入 core/saga（契约与实现同包，无环）；Mod 留 kit；不改行为、不改 wire / 文档结构
前置批次：P2-①（nestwal、mongotest）、P2-②（core/nats、core/mongo）

## 文件清单

- 搬入：`merge_kit_pkg.py saga --alias coresaga --exclude mod.go,mod_test.go`，`roost-kit/{nats,nestwal,mongo/mongotest}` → `roost-core/...`。
- 改名：`promises_test.go` → `promises_impl_test.go`（core 已有同名文件）；kit 测试助手 `expectErr` 与 core 的同名助手冲突，kit 侧改为 `expectKitErr`（仅测试）。
- 留在 kit：`mod.go`（`Mod`、`NewMod`、`CombineDefinitions`）、`mod_test.go`。`CombineDefinitions` 若被业务直接使用，P3 评估是否下沉为 core 的纯函数。

## 公共 API 变化

导入路径 `roost-kit/saga` → `roost-core/saga`（`NewJetStreamPublisher`、`NewMongoCommandInbox`、`NewDataEngineStepInbox`、`SubscribeMongoStep`、`SubscribeDataEngineStep`、`SubscribeNestStarts`、`StepConsumerConfig`、`NestStartConsumerConfig`、Mongo 存储等）。`kitnats.Permanent` 引用现在解析到 `core/nats.Permanent`（P2-② 已统一）。

## 不变量、持久化 / wire 格式

未触碰 `commandEnvelope` / `WireVersion`、命令与完成摘要、收件箱文档、JetStream 主题。B-14（摘要 `json.Marshal` 丢错）本批**未**顺手改：方案说"迁 saga 时顺手改为返回错误"，但那是行为变更，按批次纪律（只移动不优化）另开 U 单元在新路径做。

## 关联历史 U / B / T

kit U-0051、U-0092 与 core U-0050、U-0052 全部在 `core/saga` 通过；不算新审计。

## 命令与结果

`go build ./...` 0；`go vet ./saga/` 0；`go vet -tags integration ./saga/` 0；`go test -count=1 ./saga/ .` ok。go.mod 未变。

## 顺带：nest 承诺测试去抖

分支 CI 在 4c07272 的 `-race` 下偶发 `TestGroupTransitionRequestsRefuseZeroGroupsUnknownEntitiesAndOverlaps`（"second request while the first is pending = <nil>"），与搬迁无关：工作线程在第二个请求前完成了第一个 join。已在 main 修（持实体锁让 pending 确定，`f556b4d`），cherry-pick 到分支。

## 回退范围

`git revert` 本批两个提交。

## 下一步唯一动作

P2-⑥：nettransport → room / lockstep / spatial。
