# 生成 Feature 的跨层契约：数据、完成与业务幂等

基于 [2026-09-17 第三轮](REVIEW-2026-09-17-03.md)：Core dc1aab1、Kit 55f3a35、Codegen 2e09c16。这里解释当前实现和建议方向，建议尚未实施；确认问题与可运行证据见 [五项交接](../bug/REVIEW-2026-09-17-03.md) 和 [复现](../bug/REPRO-2026-09-17-03.md)。

## 1. 能序列化、能标脏、能提交是三个契约

当前 DAO 生成器把嵌套对象转换成 BSON DTO，避免未导出内部字段在编码时变成空对象。U-0224 的实际运行测试覆盖提交文档、回滚快照和 nil 指针；这解决“数据能往返”的问题。

业务更新走的是另一条链：Gem.SetLevel → Gem.Mark → notify → Equip.Mark → notify → DAO 对应字段/key 标脏 → 事务 patch。当前 nested 模板漏了第一段跨对象通知，哪怕 BSON round-trip 正确，更新仍可能无法进入后续持久化链。DAO 顶层对直接 child 的绑定不能替代任意深度的绑定。

修复时要同时处理赋值、解码恢复和对象移除。推荐每个对象只拥有其直接 child 的通知绑定，让父链递归传播；不要让生成代码越级寻找最顶层 DAO。替换/删除必须使旧引用失效，明确禁止共享可变 child 或提供多观察者语义。直接保存一个 notify 闭包通常隐含单父级所有权。性能上 setter 绑定成本应随实际替换的元素数增长，避免每次叶子修改扫描整棵对象树；这是设计建议，未做压测。

验收分层：codec 往返 → child 通知 → DAO dirty/patch → Nest 提交/回滚 → durable 水位与同步。本轮只补到通知层，不宣称全链已验证。

## 2. Session 的幂等完成不等于奖励只发一次

实际链为生成 Controller.HandleFinishDungeon → Kit session 别名 → Core Session.Finish → 生成 AddExp sender → Nest 多实体命令。Session 持久化终态并释放资源；对已有终态再次 Finish，返回已有结果并继续释放待处理资源。这样网络重试可以修复释放中的部分失败。

Controller 却把“返回无错”理解为“本次首次成功完成”，并以请求的 Success 决定发奖。这同时丢失两个事实：原 run 的权威结果，以及奖励是否已提交。仅校验 run.State 可以阻止失败副本发奖，不能阻止成功副本重复发奖。

建议把奖励去重记录与经验修改放进同一 Player/World 事务，使用稳定 RunID 作为业务身份，并保留原返回结果。请求重试只能读取或补全同一奖励操作。先在 Controller 查询是否领取、再调用 AddExp 存在并发窗口；先把领取标记单独写下则存在永久漏奖窗口。业务不需要为了示例引入另一套事务框架，优先使用已有 Nest 事务和 DAO 状态。

去重账本也需保留策略：清理必须与客户端允许重试/存档恢复的窗口一致，不能简单 TTL 后重新发奖。本轮没有测试去重实现，因为当前模板尚未提供它。

## 3. Saga 有两种完成消息来源

普通 worker 完成消息进入 saga stream；原生 Nest 完成先进入事务 effects，再由 outbox 进入 effects stream。消息内容可都描述 Completion，但 stream、subject 与外层载荷契约并不因此相同。Assembly 当前监听普通完成和 Nest 启动，缺少原生完成效果的订阅。

正确复用点是现有完成解码/校验/Engine.Complete 处理逻辑，以及现有持久消费者与生命周期工具；不能只修改一个 topic 字符串期待跨 stream 自动转发。Assembly 应拥有所有创建的订阅，包含部分启动失败回收、健康判断、停止时 drain 与超时硬停。新消费者的 durable 身份和升级重放要写明。

gift demo 选择 SubscribeMongoStep，因而绕开上述原生路径，但其 Nest 修改与 inbox 回执不是同一原子提交，模板已明确限制。要提升为可靠游戏资产流程，应优先修通原生 Nest inbox/outbox 链，再用真实失败注入证明业务效果与回执一致；不是再手写一套临时补偿数据库。

## 4. Attribute 公共运行时与项目生成类型的边界

当前 attribute generator 为本地 profile 生成字段编号、dirty mask、公式更新、元数据及 Snapshot/Container 访问器。输出依赖七个同包类型，而 feature 脚手架不提供契约实现。生成文本正确并不意味着消费工程能构建。

推荐 Core 提供有版本的通用 ID/value、元数据、profile 接口和容器/快照能力，Codegen 生成业务 profile 与访问代码，Kit 只负责可选装配。必须同步调整生成的 receiver：Snapshot/Container 若变为外包类型，不能用 type alias 后继续给它们增加本地方法；选本地 wrapper 或包级函数。公式生成与 dirtyMask 所有权要在文档和真实样例中固定。

验收重点是运行消费工程，尤其两个 profile 共存时是否重复定义公共类型；随后验证公式与快照语义。不要为通过编译随意拼凑七个空类型，那会把运行时契约推给每个使用者。

## 5. 状态同步装配：先走通已有公共接口

Core EntityBase.Sync 暴露 SubjectSyncState，实体设置同步状态时绑定自身访问保护；RoomManager 创建 broadcaster，broadcaster 内部已经拥有 SubscriptionCoordinator。生成器提供 SubjectPackerFactory，网络 feature 提供 NewRoomSink。至少这些公共接口存在，因此“没有自动 mod”不能推出“无法公开组合”。

下一步先写一个使用真实生成实体的例子：创建 manager/room → 注册 entity.Sync() → 订阅会话 → 修改实体并提交 → flush → 验证 sink 帧 → 退订/卸载/关闭。注册、持久化水位和 unload 的次序必须被显式验证。只有接入例子暴露重复业务代码后，才决定哪些装配能力下沉 Kit；不要在 Kit 再创建一个平行 coordinator，分裂已有订阅所有权。

本轮仅源码核对，未运行上述端到端例子。Wanted-05 继续保留观察。

## 6. Feature 测试应沿契约边界推进

本轮完整生成 demo 的 build 和编译检查通过，但奖励重试和 battle grace 均失败；DAO 原 runtime 通过但递归 dirty 失败；普通 Saga 完成匹配但原生完成不匹配。这说明下一批测试最值得投入的是“一个组件成功返回后，下游是否得到同一种承诺”。

建议实施优先级：副本奖励 P1 → DAO 通知/原生 Saga 接线 → attribute 权威契约 → battle grace → 状态同步接入样例。保留各原有测试门，并增加真实消费运行和失败恢复场景。未测的并发、真实网络、生产负载与 Linux 部署必须继续单列，不能用生成文件数量或测试数量充当覆盖率。
