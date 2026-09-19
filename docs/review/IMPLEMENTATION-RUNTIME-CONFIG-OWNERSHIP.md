# ARCH-07：运行期配置必须按所有者拆分

## 当前事实

`fctx.RuntimeConfig() any` 看起来像“进程配置”，实际同时被两种生命周期不同的对象写入：

- `app.run` 写入进程级 `*viper.Viper`；
- `configdata.Store.commit` 写入可热更、可回滚的 `*configdata.Snapshot`；
- `fctx.Context.init` 与 Nest request/async snapshot 捕获当时的值，使一次执行看到固定配置代际。

后两条组成了一项有用能力：**请求执行捕获活动 configdata 代际**。第一条只是启动时占位，真正的进程配置一直由
`app.Registry.Config()` 持有。把二者放进一个 `any` 槽位导致 U-0244：调用者把名字理解成进程配置，类型断言失败后取得 sid=0。

## 目标契约

| 数据 | 唯一所有者 / 入口 | 生命周期 |
| --- | --- | --- |
| 进程启动配置 | `app.Registry.Config()` | 进程启动后只读；不进入 fctx 快照 |
| 活动业务配置快照 | configdata store | 发布时原子切换，可回滚 |
| 一次 Nest / fctx 执行看到的配置 | 请求创建时捕获活动快照 | 随请求结束；同一请求内不漂移 |

`RuntimeConfig` 不再允许表达“任意运行时配置”。它若保留，只能是旧 API 的兼容别名，并明确表示活动请求配置快照。

## 推荐实施顺序

1. 给 `Registry.Config()` 与旧 `RuntimeConfig` 补互相指向的文档：读 sid、端口、服务装配一律走 Registry；业务配置走 configdata snapshot。
2. 新增语义化入口，例如 `fctx.SetActiveConfigSnapshot` / `ActiveConfigSnapshot`；configdata 成为唯一写入者，Nest/fctx 改读新名。
3. 移除 `app.run` 对该槽位写 viper。验证没有 configdata 时上下文值为 nil 是受支持状态，不再借 viper 冒充默认快照。
4. 将旧 setter/getter 标记 deprecated，先代理新入口；一个兼容周期后删除公开 setter，避免业务代码重新成为第三个写入者。
5. 再做类型化：在不引入 `fctx → configdata` 依赖环的中立层定义只读 snapshot 接口，或把活动快照存储留在 configdata、由请求创建处显式注入。

不建议只把注释改成“最后写入者获胜”。请求配置代际需要一个明确发布者；任意写者会让 rollback 恢复到另一种类型，语义仍不成立。

## 验收矩阵

- app 启动、configdata 首次发布、二次 reload、listener 拒绝和 rollback 后，进程配置始终从 Registry 可读且指针不变；
- 新请求看到最新成功代际，旧请求继续看到创建时的代际；失败发布不泄漏 target，回滚恢复 previous snapshot；
- 没有 configdata 的进程不 panic，活动快照为空；
- 全仓禁止新增 `RuntimeConfig().(*viper.Viper)` 和公开 setter 调用，U-0244 spawner 对照保留；
- local Nest、NATS Nest 与 async envelope 都覆盖快照捕获，避免只修一种传输。

## 与本轮问题的关系

这是 W-2026-09-18-11 的分流结果，不单列功能 RR。U-0244 已修复实际消费者，ARCH-07 负责消除下一次误用的入口。
它不改变 configdata 热更机制，只把已经存在的两种配置所有权写成编译和 API 都能表达的契约。
