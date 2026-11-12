# R4/R5 事件、任务与初始等级验收（2026-11-12）

基线 `61a1cc25`，分支 feat/business-modules，macOS；实际生成工程替换依赖到本工作树 core。实现提交 `35153b75`。没有修改 Nest / DataEngine / Sync 核心、合 main 或发版。

## 实现与边界

- wallet / shop / stamina / dungeon 提供命名登记点，task 是实际使用者。去重命中不重复报事件；DAO 与进度同事务，错误交还 Nest，使用冻结业务时间。排行是事务外成功答复后的尽力通知，不能用于恰好一次结算。
- task 使用配置表和按事件索引，支持每日、每周、永久、前置链、金币/道具奖励；ID 固定在 1～4096，记录有界。默认三条每日任务与原 18 条场景保留。规则及热更边界见 [task](../../../../modules/task/README.md)。
- 新角色在首次 EnterInit 事务中初始化为 1 级，失败回滚、不触发升级钩子；attribute 在同事务按初始等级刷新。实体构造只分配零值 DAO，不能在构造函数写持久字段。见 [profile](../../../../modules/profile/README.md)。
- 普通购买、领奖和本地多实体事务不默认使用 Saga；只有跨独立提交方、需要补偿的复杂交易使用 Saga。

## 已运行

`GOWORK=off go test -count=1 -v -run '^TestModuleTaskEventsPeriodsAndMutations$' ./codegen/internal/roost` [通过](acceptance.log)：最小 task + wallet 49 PASS / 0 SKIP；组合工程 **215 PASS / 0 SKIP**。task 基础 29、八组 task 联动 25；其余是实际依赖模块及 profile/enter、dungeon/leaderboard 联动。核对 build/vet、协议号、生成物漂移和反向重复安装不变；不是全模块正反序新装矩阵。

同入口 15 个可编译变异均被指定行为断言检出：wallet 回执重扣、消费次数翻倍、购买数量/体力数量缩为 1、升级数多 1、副本秒/毫秒混用、回拨清零、跳过前置、进度溢出上限、永久奖励重领、金币/道具多发 1、事件映射串错、跨日读取非冻结时间、吞掉钩子错误。随后 `-race -count=2` 运行三个包的 task / with-task 场景。

`GOWORK=off go test -count=1 -run '^TestModule(ProfileMailScenariosAndMutations|AttributeEquipmentScenariosAndMutations|EquipmentInstallsIndependentlyAndAddsAttributeLater|GMScenariosAndMutations)$' ./codegen/internal/roost` [通过](profile-enter-acceptance.log)：复核 profile 新依赖对邮件、GM、attribute/equipment 的影响；邮件组合 99、GM 组合 199，零跳过，原变异与定向 race 通过。profile + enter 4 条分别覆盖初始 1 级不触发升级、重进不重置、拒绝建档可重试、存档重载；attribute + enter 2 条覆盖初始派生和回滚重试。

根包、模块清单/with、文档锚点检查 [通过](checks.log)；新增文件暂存后为 313 文件、0 断链。全仓、完整 demo、全部模块排列与真实依赖统一留到最终批次，不把替身提交器/Rank 当作 TCP、WAL、Mongo、NATS 或跨进程验收。

## 开发中发现的失败

- 早期夹具的 raw DAO 等级从 0 开始，断言误按已建档的等级计算。按维护者默认 1 级要求，task + profile 夹具现在先走正式 EnterInit，再验升级差值；底层 profile 组件场景仍可以显式验证未建档的零值 DAO。
- 初版尝试在组件构造函数 setter 初始化，生成 DAO 正确拒绝事务外写入；已撤销，改为首次进入事务。没有放松持久字段守卫或伪造 getter 默认值。
- 一次变异暴露了测试夹具在 Nest worker 内调用 t.Fatal，Goexit 让请求等待；已改为 worker 内只采集快照、测试 goroutine 内断言，未修改生产调度。最终全部变异正常结束。
- 配置 CSV 转换只解析、不执行表约束；测试已走 Store.Load 检查 min/enum。配置框架不支持 max tag，ID 上限由业务目录明确验证，没有写一个无效 tag 冒充守卫。
- 冻结时间场景的测试 handler 最初在 Start 后注册，被 Nest 拒绝；改为 init 登记，通过请求参数控制时钟，未改变 Nest 注册规则。

CBM 已刷新，图谱 generation `2026-11-12T04:14:23Z`，覆盖记录更新于 `04:17:42Z`。47 个代码路径均为当前元数据；7 个模板局部缺口仅为组件号/错误码/协议号占位，已逐行读取并通过生成工程验证。无记录缺口不等于完整解析证明。
