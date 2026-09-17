# ARCH-05：六个 RPC 服务与 directory 的迁移交接

审查 Core `6fb36e7`、Kit `4830150`、Codegen `242b438`，来源 W-2026-09-17-01。本文是现状与待实施方案，不表示已移动代码。承接 ARCH-01～04，优先复用 versionstore、servicemetrics、servicerpc 的 transport/assembly 分拆。

## 结论和边界

六个服务均含领域实现，应迁入 Core；account 依赖的 directory 是第七个领域包，应先迁。判据是是否决定身份、状态、并发、持久化、去重和失败语义，而不是包名是否叫“平台”“路由”。Kit 提供配置、连接、生命周期、注册和具体接入装配，业务实现真实渠道、命名规则、发货动作。

下列路径相对 Kit service/，Core 保持同层级目标；全表都是待实施，非全功能正确性证明。

| 包 | Core 的领域文件与理由 | Kit/业务保留 |
| --- | --- | --- |
| directory | directory.go、store.go、redis_store.go：预约/确认/撤销、token、唯一归属与过期 | directory_mod；业务 Normalizer |
| account | types/service/redis_store、identity 领域接口：Login、CreateRole 的 slot/name/role 及回滚、SelectRole；Accounts RPC | account_mod、server_run、assembly；真实 verifier/allocator/name validator |
| chat | chat.go/store/service/redis_store：频道权限、顺序与保留、特权发布、会话查询；Messaging RPC | chat_mod、ticker、日志/取消；业务策略和频道来源 |
| global | types/service/redis_store：Bind/Rebind epoch、租约 incarnation、CAS；Routing RPC | global_mod、server_run、assembly；状态化路由不是普通胶水 |
| global/activity | types/service/redis_store/admin：开窗、进度账本、完成与投递/ACK、审计状态操作；Coordinator RPC | activity_mod、周期调度；业务活动规则与消费 |
| platform | types/service/identity/redis_store/admin：认证、订单去重、发货状态/重试、审计操作；Platform RPC | platform_mod、owner 循环、HTTP 接入与真实渠道实现；整个包不是 SDK 薄封装 |
| rank | types/store/member/redis_store/rank：排序编码、CAS/去重、排名查询；Rank RPC | rank_mod、server_run、assembly；业务选榜规则 |

领域错误码、sentinel、类型和 transport 半随实现迁；Mod/server_run/assembly 留 Kit。redis_store 中的领域键、TTL、一致性规则不是连接装配；admin.go 当前是受审计的状态转移，随 Service 迁，管理路由注册另论。不要新增第二套 versionstore。

直接依赖检查：排除测试、Mod、server_run、assembly，七包依次 6/6/5/6/7/6/3 份领域/transport 文件，共 39。将 servicemetrics 改指 Core、account 的 directory 改指 Core 后，无剩余 Kit 直接 import。这只是源码闭包检查，不是迁移后编译通过；现有七包 race 测试通过。

## 不能只移动文件加别名

- Chat server_run.go:49/60 访问 `service.cfg.PruneChannels` 和 `service.pruneTargets`。别名不能访问另一包私有成员。建议 Core 暴露有界维护入口（例如 PruneOnce 或明确的候选/配置查询），Kit 安排 tick；保持 ChannelRef 私有 key 只能经 Resolve 创建，不导出整份 cfg。
- Activity server_run.go:87/105/113/128/135 写 `service.report`。建议一次 sweep 的领域推进、持久索引退休和计数进入 Core 有界方法；Kit 保留周期/context/日志。也可设计明确维护事件接口，不能为编译删掉失败计数或导出整个 report。
- 上述维护方法尚不存在，需明确错误返回、单轮上限及取消，不能把无界后台循环塞进 Core 构造函数。

## account 的 RegistryBound 与 directory

identity.go 的 RegistryBound 是装配钩子，account_mod.go Provide 调用它。建议声明移到 Kit 装配文件，保持旧 `kit/service/account.RegistryBound` 名称与 BindRegistry 签名；Core 仅保留 IdentityVerifier/PlayerIDAllocator/NameValidator 领域方法。结构化接口允许业务对象同时实现绑定和领域接口，无需新全局 Registry。

实际绑定顺序由 DependsOn 决定，不能泛称所有其他 Mod 都已 Provide。当前 account 只声明 Redis；自定义 collaborator 依赖 HTTP/Mongo 时，验收其提供顺序或显式配置依赖，失败阻止启动。本轮未构造该排序的独立功能故障。

directory 的 Directory/Claim/Owner/Entry/State、错误、Normalizer、构造与实现迁 core/service/directory，Kit 用别名/薄转发兼容。account.Config.Names、回滚 Claim 与 ErrClaimStale 必须保持同一类型/sentinel，禁止复制一套实现或让 Core 反向 import Kit。

## 实施顺序和验收

1. directory → account；rank/global 可独立；chat/activity 先解决私有维护边界。platform [RR-20260917-04](../bug/REVIEW-2026-09-17-02.md) 的行为修复和位置迁移应可分别审查。
2. 使用现有 `servicerpc -emit transport` 生成 Core 半；Kit 使用 `-emit assembly -dir github.com/tjbdwanghaibo/roost-core/service/<包> -out .`。保持 service_type、capability/owner capability、RPC 方法/参数/错误码与 affinity；不把 admin-only 方法加进 RPC。
3. Kit alias.go 保持所有旧导出项、类型身份、sentinel、构造入口；领域与私有测试随实现迁，Kit 保留 Mod/绑定/Server/取消测试。
4. 隔离环境验证 Core 不依赖 Kit、相关包 race、旧/新 import 混用接口与 errors.Is、生成 demo 本地依赖 build/test/generate。发布模式必须在实际版本可解析后验收；本轮未执行尚未实施的迁移生成。
5. 原 Redis 前缀、JSON、TTL、token、账本和排序编码保持不变；要变则另立数据升级。版本按依赖顺序可批量发布，不要求每包发一版，不让 Kit 引用未发布 Core。

## 性能和恢复

包位置本身不减少 Redis 往返或 CAS 竞争，不能承诺迁入 Core 就更快。保持 rank 原子排序视图、chat 有界保留、activity 窗口与投递索引，容量/公平性另做压测。

platform 的固定空 retryOrders 说明状态存在不等于恢复任务可发现。优先借鉴 activity 的持久索引及 versionstore，定义有界来源、重启续接、错误观测和终态退休；迁移不能自动补齐这些能力。

本次范围为七包职责/直接依赖/维护入口与现有测试，不穷尽 Kit 或六个服务全部功能。后续优先 platform 恢复/发货幂等、directory/account 回滚、rank 真实 Redis 和迁移后跨进程兼容。
