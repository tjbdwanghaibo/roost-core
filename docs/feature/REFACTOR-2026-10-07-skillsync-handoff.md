# B1：技能同步与 Host 边界收尾

承接剩余交接 B1，基线 B5 检查点 `56a2006c`。不新增包，分别在 skillsync、syncstream 与 skill 原包内收敛。

## 执行责任

- History 是包身份与恢复链的权威记录；已经 Append 的源事件不能因为 outbox 失败再次 Append。明确区分“未接受”和“已接受但交付准备失败”。
- 清理 History 必须考虑未确认记录，显式 epoch 切换须同步退休旧 outbox；发布重试不能因积压过龄而丧失恢复机会。
- 可见性策略覆盖快照、增量、表现与 reset 的包头；未知 mutation 必须先定义字段投影。
- 发布次数由 outbox 唯一记账，不累加并发操作之间重叠的全局差值。
- 同流失败 / 在途顺序与 observer 关闭责任一起核对，避免用额外无界缓存补生命周期漏洞。
- Host 包装器的能力表与实际转发保持一致；能力准入、nil Host、恢复错误链、carry 解除及资源上限各留行为回归。

## 方向判断

本模块历史已出现 NC-114/115、RR-20261006-22 等可见性口径问题，本次 Y4 又遗漏了时钟元数据：共同根因是多个投影出口自行过滤。各出口应复用同一策略，测试比较完整线包而非只看某个字段列表。Y2/Y3 的共同根因是源游标、History、outbox 三份状态的接受责任不清；以 History 接受结果推进源游标，outbox 仅承担派生交付责任。

## 验证

### observer / manifest 的资源生命周期

Y12 的 closedObservers 不能通过淘汰旧墓碑修复：淘汰后，延迟到达的旧会话请求会被隐式重新打开。改为显式 `OpenObserver`，只保存当前开放会话及未完成的关闭请求，关闭成功删除身份；未知或已关闭 observer 默认拒绝。`MaxObservers` 默认 4096，关闭释放名额；在飞发布的关闭返回可重试错误，拒绝重新开放直到清理完成。接入方初次连接和重启重连都需调用 OpenObserver，属于明确的 API 行为收紧。

manifest 注册加 `MaxPrograms`（默认 1024）及删除入口；重复同 key 的同一 plan 幂等，不同 plan 返回错误，不静默覆盖。key 是整个 Runtime 的同步命名空间，不是筛选 Program 的条件；单个 key 的 manifest 只能代表一个 Program，多个 Program 的 Runtime 不应把不同 key 当成内容过滤器。接入文档明确该边界，业务隔离应建立独立 Runtime/Coordinator。

Y15：只保存 Packet 的 store 无法兑现跨重启年龄，持久 store 必须实现 RecordOutboxStore；缺失 CreatedAt 的记录拒绝。nil store 仍是进程内 outbox。已有 FileOutboxStore 已保存年龄，无需改格式。

R7/R8：只保留正式创建的 entity 衍生物停止逻辑。goto 和正常 finish 不停止 entity 衍生物，删除这两条路径的空调用；取消/失败统一停止施放中的 entity 衍生物。旧 Phase/Cast 常量按 C8 保留并标记不再支持创建，不为只有测试构造的状态保留分支。对应伪造作用域测试改为正式编译/施法的 goto→finish 存活检查。删除空 optional_quantity pass，值与数量校验仍在 type_snapshot 等实际 pass 中，文档不再把空 pass 计作工作。

确认缺陷先红后绿，控制并发交错；测试必须覆盖 late observer、outbox 拒收后重试、ACK / epoch / sweep、关闭时在飞发布、可见性 Header，以及包装 Host 实际施法。Windows 项按维护者规则保留限制。纯文档误述直接校正，不虚构缺陷。每批记录兼容性、实际测试与未覆盖边界。
