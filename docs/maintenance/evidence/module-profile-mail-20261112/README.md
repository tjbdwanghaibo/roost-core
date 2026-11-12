# R3 升级邮件联动验收（2026-11-12）

实现提交 `7f91c18f`，基线 `8891da69`，分支 feat/business-modules，macOS、本工作树 core 的生成工程替换依赖。规则、生命周期及恢复前提见 [mailbox](../../../../modules/mailbox/README.md)。没有修改 Nest / DataEngine / Sync 核心，也没有合 main 或发版。

## 实现

mailbox 的 with/profile 用已有 OnProfileLevelUp 登记点；工程规则缺省无奖，一次 from→to 只产一封总奖励邮件。玩家、等级区间和附件在升级时冻结到可靠 effect，重试不重查表。模块启动 SubscribeEffects，App 管理订阅排空；消费者用 EffectID 发件，Mongo inbox 与 mail 服务幂等各承担自己的窗口。

demo 删除自己的 effect 定义、升级钩子与消费者，仅覆盖 mailbox_profile_rules 调热更规则；Player + World 双实体事务保留。热更的非法奖励现在在升级时拒绝并回滚，不能提交升级后再静默丢掉奖励。旧 player.level_up 不兼容，当前 topic 为 mailbox.profile_reward。

## 定向验证

- `GOWORK=off go test -count=1 -run '^TestModuleProfileMailScenariosAndMutations$' ./codegen/internal/roost`：[通过](acceptance.log)。新 game 工程，mailbox 32、profile 20、wallet 14、联动 10，**76 PASS / 0 SKIP**；build/vet、ID、生成物、反向重复安装通过。
- 同入口 6 个变异检出：零奖励也 Emit、篡改最终等级、丢掉连升起始等级、取消奖励校验、同 EffectID 根据奖励生成新 RequestID、所有 EffectID 用同一个常量请求键；均要求指定场景与错误文字失败。`-race -count=2` 执行两个包的全部 10 条联动场景。
- 新 game-demo：`go test -count=1 -run '^TestWithMailboxProfile|^TestLevelUpCarries|^TestExperience|^TestWorldExperience' ./game/handler ./game/mailbox` [通过](demo-tests.log)。game service、player controller、loadtest [编译通过](demo-build.log)，`-run '^$'` 不是行为验收。
- 清单/联动/模块安装步骤定向、根包和现行文档检查通过（新增报告暂存后 310 文件、0 断链）；新增联动后的 GM 定向组合也通过，171 PASS / 0 SKIP、原 8 个变异及 race 未退化；见 [checks.log](checks.log)。

## 原始失败

[第一次验证](initial-failure.log) 9/10：测试误要求同一发件 RequestID、改变附件后 mail 服务报错。源码 service/mail/service.go 的 Send 先查发送账本、返回首次信封，这是明确的既有语义。修正场景为调用正式 mail.Service 后断言实际仍只有一封、附件仍为第一次的 30 币；未修改 mail 服务或放宽幂等。变异把奖励拼入 RequestID 会产生第二封，测试确实失败。

## 尚未验证

生产者使用正式 Nest / 生成 DAO / 替身提交器，消费者使用正式 mail.Service / 内存存储。真实 WAL、JetStream/Mongo inbox、Redis 邮件与进程崩溃联合链路未跑，按新任务 §6 统一执行。全量 demo、全模块正反序与全仓也延后；不把这些离线场景拼成端到端结论。

CBM 更新至 `2026-11-12T03:32:29Z`，本批 15 个现存代码路径核对为新鲜、无记录缺口。模板节点不能提供完整函数关系时已读取源码，没有据此断言索引完整。
