# 次核心：Wiring 装配：设计与使用

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/13-wiring.md) · [模块总目录](../README.md)

## 先理解这一块

Wiring把数据库、调度器、同步总线等能力接进一个App。你通过配置和Mod组合这些能力。

本篇讲接线和资源管理；具体数据如何保存、消息如何执行，要回到对应核心模块理解。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 职责与现实边界

源码已按 [分类方案](../PACKAGE-REORGANIZATION.md)迁移。Wiring 保留便捷接入的初衷：业务通过 Mod、配置和少量 option 使用能力，App 排序依赖并管理生命周期。

Wiring 只负责配置、依赖查询与注册、实例构造和生命周期委托。领域 Server 的 sweep/retry 归 service；运维 HTTP 归 infra/observe/ops；统计采集、窗口、文件和周期运行归 framework/statslog。Gate 的绑定、队列和 NATS 收发按 [Gate 实施方案](../GATEWAY-IMPLEMENTATION.md)推进。

## 2. 装配关系

| Mod 类别 | 创建/连接的能力 | 依赖与关闭责任 |
| --- | --- | --- |
| mongo/redis/nats/etcd | 外部资源驱动 | 配置、连接、健康与有界关闭 |
| nest/lock/manager | 调度、锁和本地单例管理 | 服务完成后排空工作；不重复拥有 App 单实例锁 |
| dataengine | WAL、Projector、Repository；业务 effect 消费者 `SubscribeEffects` | 恢复 gate、投影、outbox、持久资源按依赖关闭；effect 订阅由服务 Shutdown 里 Drain |
| remoteentity | Remote 与 Mirror 运行能力 | 权威存储、缓存、总线与持久 fence |
| syncbus | 服务间状态总线 | 只读 syncbus 配置，不回退旧 room/sync 配置段；缺省前缀 `roost.sync`，流名由前缀推出 |
| saga | 协调器/步骤能力；业务步骤运行时 `Steps`（不是 Mod，服务 Init 里 Start） | 存储、消息与系统期限；Steps 由服务 Shutdown 里 Drain |
| configdata | 表加载/快照与监听 | 数据完整校验后发布 |
| ops/statslog | 探针、管理与周期统计 | 生命周期与有界工作排空 |

### 2.1 连接与凭据的环境变量（`*_env`）

mongo / redis / etcd / nats 四个连接 Mod 的地址与凭据键各有一个伴随键 `<键>_env`，值是**环境变量名**（维护者 2026-10-30 R11，与下游 cube fork 的写法对齐，下游配置不用改）。部署把连接串、账号、口令放进环境变量（Kubernetes Secret 等），配置文件里只写变量名：

```yaml
mongo:
  uri: mongodb://127.0.0.1:27017/?replicaSet=rs0   # 本地缺省；部署时被 MONGO_URI 取代
  uri_env: MONGO_URI
  username_env: MONGO_USERNAME
  password_env: MONGO_PASSWORD
  auth_source: admin
redis:
  addr_env: REDIS_ADDR
  username_env: ""        # 叠加层写空串，清掉下层（例如容器配置）声明的凭据：无鉴权的部署
  password_env: ""
```

| Mod | 有 `_env` 的键 | 账号规则（Init 时检查，错误不带值） |
| --- | --- | --- |
| mongo | `mongo.uri`、`mongo.username`、`mongo.password`（另有 `mongo.auth_source`，不是密钥、没有 `_env`） | 用户名与口令同时写或都不写；写了 `mongo.username` 时 `mongo.uri` 不能再带 `user:password@`（两处都有账号时驱动会静默用后者，这里拒绝）；`mongo.auth_source` 只跟 `mongo.username` 用，URI 里的账号写 `?authSource=`。单独写的账号并进 URI 的认证选项（保留 URI 的 `authMechanism`；不写 `auth_source` 时用 URI 的 `authSource`） |
| redis | `redis.addr`、`redis.username`（ACL 用户，新增）、`redis.password` | 写了用户名就要有口令；只有口令时用 default 用户。`redis.cluster_addrs` 没有 `_env`：服务 Mod 按它判断 Cluster 与检查 hash tag，从变量读就要每个服务 Mod 都解析一遍 |
| etcd | `etcd.endpoints`（变量值逗号分隔）、`etcd.username`、`etcd.password` | 用户名与口令同时写或都不写 |
| nats | `nats.url`、`nats.username`、`nats.password`（后两个新增，经 nats.go `UserInfo`） | 用户名与口令同时写或都不写；写了用户名时 `nats.url` 不能再带 userinfo。token / nkey / creds 文件不支持（可以写在 URL 的 userinfo 里） |

取值规则只在 `wiring/mods` 的 `EnvValues` 实现一次，四个 Mod 与走同一规则的运维入口（`wiring/nats.Connect` / `ServerURL`、`wiring/mongo.Connection`、单实例锁的 `wiring/redis.SingletonStore`、`wiring/sidmerge`）共用：

| 情况 | 结果 |
| --- | --- |
| `<键>_env` 没写或为空串 | 取 `<键>` 本身的值（与没有这条规则时相同） |
| `<键>_env` 写了变量名，变量有值 | 变量的值（去掉两端空白，与配置里字符串的读法相同）取代 `<键>`。**两者都写时变量优先**：配置里的 `<键>` 留作本地缺省（下游 eks 配置就是这样写的，cube 同样），不报冲突 |
| 变量没设置、为空或只有空白 | Mod Init 拒绝启动，错误点名 `<键>_env`、变量名与 `<键>`，不带任何值；同一个 Mod 的缺失一次报全 |
| 变量名不是字母 / 数字 / 下划线（或以数字开头） | 配置检查就拒绝，只点名键、不回显写进去的内容（可能把口令本身写进了 `_env`） |

**配置检查不读环境变量**：App 启动前的声明检查、`--check-config`、doctor 只检查 `_env` 的变量名形状（键都在 Mod 的 ConfigSchema 里声明，doctor 的 `config-schema` 认识它们），不要求变量已设置，所以镜像构建、CI 里没有部署密钥时检查照样通过；变量是否设置在 Mod Init 时判断（App 启动时仍是先于任何连接失败）。生产要求有 Redis 地址的规则把 `redis.addr_env` 算作已写地址。

**不进日志与错误**：Mongo 的 “connected” 日志只记脱敏后的 URI（口令换成 `***`），单独写的口令不进日志；NATS 的连接失败错误、“connected” / “reconnected” 日志里的地址整段盖掉 userinfo（token 在用户名位置，用户名一并盖掉）；Redis / etcd 只记地址。

生成器 catalog 的14个基础设施 Mod 名是特定目录口径，不是全仓 Mod 类型总数；RemoteMirror 与 service 装配不应被错误计入或排除能力范围。

## 3. 接入顺序

声明共享基础设施和服务专属 Mod；依赖图排序由 App 负责，不靠业务“碰巧按顺序加”。服务 Init 从 Registry 取接口，Start/Serve 后才开放业务准入。业务通过自己的 ConfigSchema 声明读键，不把所有新键塞入 Mod 的手写白名单。

## 4. 生命周期

创建资源的一方负责关闭；借用 Registry 能力不重复关闭。StopWithContext 返回超时不代表排空成功，不能释放仍被回调使用的连接。StopBudget 进入部署停机预算，不是文档备注。配置校验和连接失败必须在启动时清楚报告，不以降级零值掩盖缺依赖。

## 5. 扩展新的 Mod

先确定 core 接口和唯一状态机，再定义配置结构/约束及 capability，最后写 Mod 的创建、注册、Start、Stop。添加生命周期、配置、依赖与失败清理测试，并验证真实生成工程可以声明和消费该能力。只有本项目使用的业务规则留在应用，不为方便装配复制一套持久化或并发协议。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
