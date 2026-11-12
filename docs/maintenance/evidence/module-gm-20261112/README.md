# R1 GM 验收（2026-11-12）

工作树 `feat/business-modules`；需求原样接收提交 `7be4f31b`。本轮只改模块、demo 接入与生成验收，没有修改 Nest / DataEngine / Sync。

## 本轮结果

- `TestModuleGMScenariosAndMutations`：新 game 单装 gm，9 PASS / 0 SKIP；增量装 wallet、bag、profile、mailbox、rehome，共 12 行报告、161 PASS / 0 SKIP（其中新 GM 场景 29 条），build / vet / ID / 生成漂移均通过；逆序重复安装无变化。这里的逆序是重复安装检查，全模块正反序新装统一留到最后。
- 8 个可编译变异均被指定断言检出：去掉 operator、去掉 note、忽略 stop、币数翻倍、道具数多一个、经验多一、跳过收件人缺失错误、换服丢失操作者。
- 同一个生成工程 `go test -race -count=2 ./game/gm` 通过。
- `TestModuleCatalog|TestModuleWith` 与 demo 安装步骤三项定向检查通过；根包 `go test -count=1 .` 通过；文档锚点通过。
- 新 game-demo：GM 定向场景通过；保留的经验 / World 双实体事务、溢出拒绝与升级同提交 effect 回归通过；`internal/service/game` 编译通过（`-run '^$'`，不宣称运行了业务服务测试）。第一条 demo handler 筛选正则曾没有选中测试，已用 `^Test(Experience|WorldExperience|LevelUpCarries)` 补跑。

## 失败及修正

开发阶段第一轮新工程编译失败：Sender 包实际名为 `handler_syncsender`，模板缺少显式 import 别名；生成器将 GM 类型名前缀转为 Gm；测试错用了 account 缺失错误名与 rehome DAO 字段名。均按正式生成物修正，没有改生成器来迎合模板。

随后发现两处夹具错误：GM bag 测试未装载模块道具表，合法道具被拒；Profile 新档等级按现有契约为 0，新增回滚断言误写为 1。分别补正式配置表装载、修正固定断言；重新生成的新工程通过，不复用被手工改过的开发工程冒充安装验证。

## 边界与复跑

测试用真实 Nest、生成 Sender、实体与事务回滚；提交器与 account/mail/实体交出是隔离替身。邮件发件不写玩家 DAO，不存在“锁内 RPC 后回滚远程邮件”的保证：失败验证的是玩家未改、稳定请求键保留并可再次投递，真正的 mail 服务持久幂等另由其验收覆盖。

真实 ops HTTP / NATS / Redis / Mongo / TCP、全模块正反序同装、完整 demo 与全仓回归，按需求文档 §2、§6 留到最后统一执行。本报告不覆盖这些项。

```sh
GOWORK=off go test -count=1 -run '^TestModuleGMScenariosAndMutations$' -v ./codegen/internal/roost
GOWORK=off go test -count=1 -run 'TestModuleCatalog|TestModuleWith|TestDemoStepsLeaveModuleComponentsToTheModules|TestDemoTemplateStepsAndShippedFilesAgree|TestDemoLeavesMailToTheMailboxModule' ./codegen/internal/roost
GOWORK=off go test -count=1 .
python3 scripts/check-doc-anchors.py
```

原始精简输出：[模块与变异](acceptance.log)、[其他检查](checks.log)。
