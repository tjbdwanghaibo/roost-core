# Mailbox 邮件与升级奖励

邮件列表、三步领取、Player 领取账本、附件种类、`SendTo` 发件入口见[模块清单](../README.md)。本模块带基础 32 条场景，金币内置；bag 联动提供道具附件，enter 联动拉取全服邮件，gm 联动提供补偿发件。

## 与 profile 的升级奖励联动

`roost module add mailbox profile` 安装 named level-up hook、可靠 effect 的生产/消费以及模块启动接线，无需手写 Service.Init。奖励规则归工程：`game/entities/player/mailbox_profile_rules.go` 中的 `MailboxProfileReward(from,to)`，缺省返回零值，不发奖也不发 effect。这个函数只在启动前设置；热更时在函数内部读取线程安全的规则源，不并发改写函数变量。

一次经验发放从 from 升到 to，**只调用规则一次、生成一封邮件**，奖励覆盖 from+1…to。例如每升一级送 10 币，返回 `mailbox.Coins(int64(to-from)*10)`；装 bag 后也可返回 `mailbox.Item(id,count)`。不要逐级发邮件或在规则里 I/O。非法附件返回错误给 Nest，升级、属性成长和奖励 effect 同时回滚。

`ProfileReward` 在升级事务中冻结玩家号、起止等级与附件，`nest.Emit` 使它与 DAO 变化进入同一条 WAL。消费者不再查奖励表，热更仅影响之后的升级，不能改变已经提交的奖励。topic 为 `mailbox.profile_reward`，消费者 durable 为 `<业务服务>-profile-reward-mail`；普通升级邮件保留 7 天，不自动兼容旧 demo 的 player.level_up 载荷。

消费走正式 `SubscribeEffects`：从 DataEngine Mod 读取流和 Mongo inbox 的声明，邮件跨服务发送用 `RequestID = EffectID` 去重。邮件已发但 inbox/ACK 尚未完成时，重试返回原信封；服务不会以新奖励覆盖第一次的邮件。坏载荷或永久非法附件标记 poison 并告警，传输错误保留重试。订阅登记为 App 输入，App 先排空它，再关闭 Nest 与存储；不在业务 Shutdown 另管一套订阅。

`game-demo` 也用这组联动，只覆盖奖励规则并调 `game/rewards` 的热更点：默认一次升级操作送一瓶法力药水，即使连升多级也只送一瓶；项目可替换该规则。demo 保留 Player + World 的经验事务，邮件仍随同一事务提交。

## 验收边界

联动 10 条场景：默认无奖励、一级一封、多级合并、提交回滚、非法奖励、消费重放、坏载荷、空身份、传输重试与保留首次奖励。根仓 `TestModuleProfileMailScenariosAndMutations` 在新工程执行它们，附 6 个变异与两轮 race。

生产者测试使用正式 Nest/生成 DAO、替身提交器；消费重放调用正式 mail.Service、内存存储。它们证明事务内容和幂等调用契约，不代表真实 WAL/JetStream/Mongo/Redis 故障恢复已验收；联合验收按维护交接最后统一执行。
