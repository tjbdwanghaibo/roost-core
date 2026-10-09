# 次核心：Service 领域能力：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/09-services.md)

## 如何阅读

账号、邮件、聊天、排行等能力具有自己的数据和接口。Service篇说明如何在本进程使用它们，或者把它们拆成单独进程。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`service`、`infra/network/servicerpc`、`infra/observe/servicemetrics`、`wiring`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 公开接口与 .local 管理接口分开。
2. 幂等身份跨重试保持，未知结果不当作未执行。
3. account 身份、玩家号和名字规则是必需协作者。
4. 领域模型、存储、RPC 传输和周期运行均在 service；Wiring 只构造实例并委托生命周期。

## 2A. 一个服务的两层实现

以mail为例，[service.go](../../../service/mail/service.go)的New/Send/deliverAndRecord承担领域输入、请求身份、信封和投递结果；存储能力由store/redis_store提供。[mail_mod.go](../../../wiring/mail/mail_mod.go)从配置和Registry接线，生成mail_rpc_assembly_gen.go发布能力、注册Server/ClientMod，不能在Mod里另发一份绕过Send幂等的邮件。

account是当前分层例外：[account_mod.go](../../../wiring/account/account_mod.go)的NewMod要求IdentityVerifier、PlayerIDAllocator、NameValidator及Reporter；[service.go](../../../service/account/service.go)仍在同一kit包承担领域逻辑。prefix通过声明必填，不能静默共用所有项目的默认键。

接口层的参数身份属于可信服务调用边界；外部玩家接入必须把认证连接身份映射到参数。RPC信封还原业务错误不等于自动校验终端玩家权限。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 源码定位与回归

接口、模型、存储和运行循环都在同一领域包；生成的传输文件为 `*_rpc_gen.go`。接线包保留 Mod、ClientMod 和 `*_rpc_assembly_gen.go`，生成声明指向领域源码，不保留领域类型别名。

| 领域 | 运行实现 | 配置与进程接线 |
| --- | --- | --- |
| account | [account](../../../service/account) | [wiring/account](../../../wiring/account) |
| activity | [activity](../../../service/activity) | [wiring/activity](../../../wiring/activity) |
| chat | [chat](../../../service/chat) | [wiring/chat](../../../wiring/chat) |
| directory | [directory](../../../service/directory) | [wiring/directory](../../../wiring/directory) |
| global | [global](../../../service/global) | [wiring/global](../../../wiring/global) |
| mail | [mail](../../../service/mail) | [wiring/mail](../../../wiring/mail) |
| match | [match](../../../service/match) | [wiring/match](../../../wiring/match) |
| platform | [platform](../../../service/platform) | [wiring/platform](../../../wiring/platform) |
| rank | [rank](../../../service/rank) | [wiring/rank](../../../wiring/rank) |
| session | [session](../../../service/session) | [wiring/session](../../../wiring/session) |

`service/session` 管理副本/试炼的 Run，不是 Gate 网络连接。Account 和 Platform 的 RegistryBound 协作者检查留在 Wiring；领域包通过明确注入的接口访问业务能力。

改接口后重生成传输与接线两半，并编译实际消费者。运行回归分别验证本地领域行为、Init/Provide/Start/Stop、周期任务与停止所有权；Redis/NATS 集成测试需要真实资源，默认单元测试的跳过不表示真实资源测试通过。

```sh
GOWORK=off go generate ./service/... ./wiring/...
GOWORK=off go test ./service/... ./wiring/...
GOWORK=off go test ./codegen/internal/servicerpc ./codegen/internal/roost
```

完整文件数量与分类见 [全包索引](../PACKAGES.md)。
