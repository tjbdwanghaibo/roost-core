# 次核心：Service 领域能力：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/09-kit-services.md) · [模块总目录](../README.md)

## 先理解这一块

账号、邮件、聊天、排行等能力具有自己的数据和接口。Service篇说明如何在本进程使用它们，或者把它们拆成单独进程。

先理解owner是持有服务数据的一方，client是调用方；不要把名字里带kit理解成其中一定没有业务实现。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 实际目录归属

| 服务 | 领域实现位置 | 主要责任 |
| --- | --- | --- |
| mail | service/mail | 邮件信封、邮箱、发送意图与领取状态 |
| match | service/match | 排队、分组、匹配结果与取消 |
| session | service/session | 一次运行及关联资源、释放和清扫 |
| account | kit/service/account | 渠道身份验证、会话令牌、选角与建角计划 |
| platform | kit/service/platform | 平台身份和相关管理能力 |
| global | kit/service/global | 静态角色/区服路由 |
| activity | kit/service/global/activity | 活动组、窗口、协调与结果投递 |
| chat | kit/service/chat | 频道、消息保留与读取 |
| rank | kit/service/rank | 排行更新、查询和幂等 |
| directory | kit/service/directory | 全局名字目录的预留/提交/释放 |

只有 mail/match/session 的领域实现已独立到 service，kit 对它们提供别名和装配。其余七个仍在 kit/service 中有服务/存储逻辑；本版文档明确此事实，不通过文档清理搬动代码。十个服务包中 directory 是嵌入能力；game-demo 托管九个框架服务，加 game 共十个进程。

Gate 尚未成为上述目录中的独立服务。当前已提供 `gateway` 边界工具和嵌入 Game 的 TCP 接入实现；后续的跨进程路由、回推和会话恢复见[网关现状与独立 Gate 设计](../GATEWAY.md)。

## 2. owner 与调用方

owner Mod 持有存储并发布公开接口与 .local；Server 注册总线 handler 并运行周期工作。调用方使用 ClientMod/BusClient，只依赖公开契约。同进程也通过同一能力接口；业务不要依赖把公开接口断言成具体 Service。

RPC 生成物分传输半与装配半：接口包持有 wire/handler/BusClient，kit 持有 Server/ClientMod/OwnerCapabilities。运维 Admin、Sweep、ForceRelease 等本地能力不应自动暴露成任意总线调用。affinity 需要 discovery 支撑，缺失时不能装成“看似有效”的路由。

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
