# 次核心：Service 领域能力：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/09-services.md) · [模块总目录](../README.md)

## 先理解这一块

账号、邮件、聊天、排行等能力具有自己的数据和接口。Service篇说明如何在本进程使用它们，或者把它们拆成单独进程。

owner 是持有服务数据的一方，client 是调用方；领域实现与接口在 service，配置和进程接线在 wiring。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 实际目录归属

| 服务 | 领域实现位置 | 主要责任 |
| --- | --- | --- |
| mail | service/mail | 邮件信封、邮箱、发送意图与领取状态 |
| match | service/match | 排队、分组、匹配结果与取消 |
| session | service/session | 一次运行及关联资源、释放和清扫 |
| account | service/account | 渠道身份验证、会话令牌、选角与建角计划 |
| platform | service/platform | 平台身份和相关管理能力 |
| global | service/global | 静态角色/区服路由 |
| activity | service/activity | 活动组、窗口、协调与结果投递 |
| chat | service/chat | 频道、消息保留与读取 |
| rank | service/rank | 排行更新、查询和幂等 |
| directory | service/directory | 全局名字目录的预留/提交/释放 |

十个领域及服务运行逻辑统一在 service，Wiring 提供本地 Mod、远端 ClientMod 和进程接线；旧领域 alias 已删除。directory 是嵌入能力，game-demo 托管其余九个框架服务，加 game 共十个进程。

## 1A. Session 运行会话

`service/session` 管理副本、试炼等一次运行的记录，领域对象叫 `Run`。它记录 owner、运行种类、请求幂等 ID、期限、终态和外部资源引用。

业务调用 `Enter` 建立运行；分配场景或其他资源后，通过 `Attach` 登记引用；`Finish`、`Leave` 和到期清扫走正式释放流程。资源的含义与具体释放由业务提供的 `Releaser` 实现，配置必须包含该协作者。服务不负责模拟场景、战斗或 Room。

Gate Session 表示网络连接与绑定；Account token 表示登录认证；Sync SessionID 表示同步生命周期。这些身份均与 Run 独立。不能用玩家断线自动代表副本结束，也不能用释放某个 Run 替代关闭玩家连接。

源码入口：[Session 接口](../../../service/session/session_rpc.go)、[Run/Resource](../../../service/session/types.go)、[配置与释放流程](../../../service/session/service.go)、[待清扫入口](../../../service/session/sweep_source.go)。周期运行由领域 Server 持有；Wiring 只构造 runtime 并委托生命周期。

Gate 尚未成为上述目录中的独立服务。当前已提供 `infra/network/gateway` 边界工具和嵌入 Game 的 TCP 接入实现；后续的跨进程路由、回推和会话恢复见[网关现状与独立 Gate 设计](../GATEWAY.md)。

## 2. owner 与调用方

owner Mod 持有存储并发布公开接口与 .local；Server 注册总线 handler 并运行周期工作。调用方使用 ClientMod/BusClient，只依赖公开契约。同进程也通过同一能力接口；业务不要依赖把公开接口断言成具体 Service。

RPC 生成物分传输半与装配半：接口包持有 wire/handler/BusClient，wiring 持有接线 Server/ClientMod/OwnerCapabilities。运维 Admin、Sweep、ForceRelease 等本地能力不应自动暴露成任意总线调用。affinity 需要 discovery 支撑，缺失时不能装成“看似有效”的路由。

## 3. 存储与幂等

这些服务多数以 Redis/versionstore 存储自己的记录，不是玩家 DAO WAL 的旁路。key_prefix 必填且按环境隔离；rank 的 Lua 与 mail 信封等有独立原语。写操作用请求 ID/令牌/状态条件保证业务重试，传输超时和业务拒绝通过不同错误表达。

versionstore 写结果未知仍需调用方保留不确定状态，不能假设每种服务都能自动恢复所有未知结果。重试时必须保持同一个幂等身份和输入摘要；禁止以新 ID 反复发起可能已经生效的扣发。

## 4. 关键业务边界

account 的身份验证、玩家号分配、名字校验由接入方提供，不能用默认信任身份或进程内计数器替代。一个账号一区服一角色由 slot insert-only/建角计划约束；SelectRole 的 accountID 属可信服务边界，外部请求必须先绑定已认证身份。

Player 静态绑定 sid，不自动做动态玩家租约迁移。directory 名字域是全局域；activity groups_file 明确组与 game 列表。activity 的观察重试不自动耗尽失联投递，无法恢复的目标按人工 ExhaustDispatch 等管理流程收口。

## 5. 接入检查

分别验证 owner、本地调用、跨进程调用、重复请求、回复丢失、恢复与停止。业务期限使用业务时钟，系统令牌/裁剪/网络超时使用系统时钟。亚秒期限不能通过整数秒截断后改变行为。默认服务指标可统一关闭，但不能把关闭指标当成关闭周期清扫。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
