# 其他包与公共基础设施：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/14-foundation.md) · [模块总目录](../README.md)

## 先理解这一块

除了核心链路，框架还有集合、索引、网络驱动和游戏AI等工具包。它们各自解决较小的问题。

按需要查表，不必把所有工具都接入项目。公共API存在，也不代表框架内部每条链路都在使用它。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 放置原则

只服务某个核心模块的实现优先靠近该模块；被多模块复用的并发、集合、日志、资源协议保留为基础设施。保留公开工具 API 不需要虚构框架内部调用者；目录存在也不能推导已完成生产环境验收。

## 2. 包的职责和使用限制

| 包 | 提供什么 | 接入与维护边界 |
| --- | --- | --- |
| ai | 黑板、行为树、策略与控制器 | 游戏 AI，由实体宿主推进；不是此次删除的开发 AI 提示词 |
| container | 桶、池、拓扑排序等 | 池对象释放后不能继续持有可变引用；遍历契约逐 API 判断 |
| safemap | 分片并发 map | Range 回调与锁内 Read/Compute 不同；不能把可重入承诺扩展到全部回调 |
| index | 泛型内存索引 | 不是数据库索引或持久权威，生命周期由宿主管理 |
| misc | 整数约束、hash 等叶子工具 | 输入范围和溢出按具体类型约束 |
| migration | 显式版本步骤 Registry | 调用方管理步骤和落库；不再由 EntityRepository 自动迁移 DAO schema |
| etcd、etcd/driver | 发现、选举、watch 与具体驱动 | 服务发现不等于实体写权限；连接、重订与关闭由 Mod 管理 |
| nats、nats/driver | 消息/JetStream 契约和驱动 | ACK/NAK/Term、最大投递、关闭状态；不同业务定义不同错误结算 |
| mongo、redis 及 driver | 数据库契约和具体客户端 | 写结果未知保留；驱动与接口分包，core 契约不反向链接具体驱动 |
| bus、servicerpc | 服务间调用和 RPC 运行能力 | 传输成功、业务成功、幂等与 affinity 分开 |
| ownerroute | 所有者路由 | 路由提示不能替代持久 fence 校验 |
| security | 令牌、签名和限流原语 | 业务授权、密钥轮换、生产网络由宿主负责 |
| httpclient、httpserver、webroute | HTTP 出入站及生成路由支撑 | 限长、超时、取消、认证和停机须按应用配置 |
| internal/operation、internal/stopcontract | 操作准入/排空、共用停机协议 | internal 非外部稳定 API；停止返回 nil 必须符合真实完成语义 |
| internal/rangecontract | 集合回调契约测试辅助 | 测试工具，不是业务存储 API |

## 3. 与其他分区互链

actionflow/event/fctx/lock/worker/goroutine 随 Nest 说明；cache/versionstore 与持久化一起说明；app/lifecycle/manager 归进程生命周期；clock/timer 归时间；metrics/health/log/failurelog/admin 归运维；attribute/spatial 归技能战斗。

## 4. 完整目录

完整包目录、源码文件数、测试文件数与主分区映射见 PACKAGES.md。该表从 Git 跟踪的 Go 源文件提取，排除 docs 与 testdata；包含示例、命令与脚本包，因此不是 go test ./... 的包数，也不是语义 review 完成率。C#/Unity 客户端另见 client/README。

## 5. 修改时的核对

先确定拥有者、回调是否持锁、是否允许重入、对象是否可复用、关闭能否取消。协议适配修改要验证取消与结果未知，不只验证 happy path；集合修改要验证遍历中增删、提前返回和资源回收。性能比较使用同机同参数多轮，不把一个容器微基准外推为整服容量。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
