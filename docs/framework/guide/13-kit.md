# 次核心：Kit 装配：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/13-kit.md) · [模块总目录](../README.md)


## 1. 职责与现实边界

kit 让项目用 Mod 接入 App：声明配置、检查依赖、创建驱动、发布接口、启动后台工作、注册健康、按预算关闭。它复用 core 的状态机，不应该另做 Nest 事务或 WAL 协议。当前七个 service 的领域实现仍位于 kit/service，作为明确例外记录在09篇；这次文档维护不实施代码迁移。

## 2. 装配关系

| Mod 类别 | 创建/连接的能力 | 依赖与关闭责任 |
| --- | --- | --- |
| mongo/redis/nats/etcd | 外部资源驱动 | 配置、连接、健康与有界关闭 |
| nest/lock/manager | 调度、锁和本地单例管理 | 服务完成后排空工作；不重复拥有 App 单实例锁 |
| dataengine | WAL、Projector、Repository | 恢复 gate、投影、outbox、持久资源按依赖关闭 |
| remoteentity | Remote 与 Mirror 运行能力 | 权威存储、缓存、总线与持久 fence |
| syncbus | 服务间状态总线 | 只读 syncbus 配置，不回退旧 room/sync 配置段 |
| saga | 协调器/步骤能力 | 存储、消息与系统期限 |
| configdata | 表加载/快照与监听 | 数据完整校验后发布 |
| ops/statslog | 探针、管理与周期统计 | 生命周期与有界工作排空 |

生成器 catalog 的14个基础设施 Mod 名是特定目录口径，不是全仓 Mod 类型总数；RemoteMirror 与 service 装配不应被错误计入或排除能力范围。

## 3. 接入顺序

声明共享基础设施和服务专属 Mod；依赖图排序由 App 负责，不靠业务“碰巧按顺序加”。服务 Init 从 Registry 取接口，Start/Serve 后才开放业务准入。业务通过自己的 ConfigSchema 声明读键，不把所有新键塞入 Mod 的手写白名单。

## 4. 生命周期

创建资源的一方负责关闭；借用 Registry 能力不重复关闭。StopWithContext 返回超时不代表排空成功，不能释放仍被回调使用的连接。StopBudget 进入部署停机预算，不是文档备注。配置校验和连接失败必须在启动时清楚报告，不以降级零值掩盖缺依赖。

## 5. 扩展新的 Mod

先确定 core 接口和唯一状态机，再定义配置结构/约束及 capability，最后写 Mod 的创建、注册、Start、Stop。添加生命周期、配置、依赖与失败清理测试，并验证真实生成工程可以声明和消费该能力。只有本项目使用的业务规则留在应用，不为方便装配复制一套持久化或并发协议。


## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
