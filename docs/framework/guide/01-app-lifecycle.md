# App 与生命周期：设计与使用

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/01-app-lifecycle.md) · [模块总目录](../README.md)

## 先理解这一块

一个游戏服启动时要加载配置、连接资源，退出时要把正在做的工作收好。App就是组织这些步骤的入口。

本篇的重点是“谁创建资源，谁负责关闭”。先理解启动与停机顺序，再看Mod接口。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 职责

App 提供服务选择、Mod 依赖排序、配置声明检查、实例 Registry、单实例身份、生命周期事件与停机。一个二进制可以声明多个服务类型，一次启动选择其中一个服务。Mod 负责基础设施，Service 负责业务运行。

## 2. 启动链路

配置加载/声明检查先于产生外部业务副作用；共享与服务专属 Mod 不能重名。依赖写 Mod 名称，而非某个 Mod 恰好发布的 capability 名称。静态实体 kind、builder、handler、协议路由等注册不做网络 I/O；生成 registry 聚合正式入口。

单实例锁在 Mod 初始化前获取。持有者 token 标识本次 incarnation，避免同 sid 重启把旧进程工作误认为自己的。Registry 是本 App 的能力表，业务通过 app.Lookup 获取接口。装配失败须释放已经成功创建的资源，不能只处理全启动成功后的 Shutdown。

## 3. 运行与失败

RuntimeFailure 是进程级不能继续安全写入的信号；失败回调包含 Nest fence。fence 与 readiness 各有职责：前者保护写入，后者告知流量入口。单实例失锁不能仅记录警告后继续服务。活性查询只回答是否仍持有进程锁，不证明实体写权限或业务健康。

## 4. 三步停机

发起关闭 → 在调用方 context 内等待真实排空 → 排空后释放资源。发起关闭幂等；等待超时返回 context 错误，对象与底层资源保留供继续等待。不能把关闭 channel 当成所有回调退出，也不能因等待超时提前释放单实例锁。

停机顺序（`shutdown.total_timeout` 一个窗口）：发布 `service.stopping`（ops 撤销就绪）→ **关全部外部输入并排空** → `Service.Shutdown` → 逆序停服务专属 Mod、再停共享 Mod → 释放单实例锁 → 发布 `service.stopped`。依赖未排空时，不继续释放它依赖的更底层 Mod。容器终止宽限期应覆盖关输入、Service、所有 Mod 预算和锁释放时间。

### 4.1 停机第一步：关外部输入

不变量：**任何外部输入，在它用到的依赖停止之前，已经关闭准入并排空。** 关闭之后到达的请求得到确定的“没执行、可重试”（`errcode.CodeRetryable`）或 no responders，关闭之前已经接受的答完再往下走。App 统一做，模块与生成模板不各自实现（[方案](../../maintenance/FEATURE-2026-11-04-app-lifecycle.md)）：

- 拥有外部输入的 Mod / Service 实现 `app.IngressCloser`（`CloseIngress(ctx)`：关准入、幂等，在 ctx 内等已接受的处理完；ctx 先结束返回 ctx 错误、可再调；只停“接收”，出向能力留到它的 Stop）。运行中才建立的输入（`Service.Init` 里订阅的消费者）经 `app.AddIngress(registry, name, closer)` 登记；第一步开始之后再登记被拒。
- App 在 `service.stopping` 之后、`Service.Shutdown` 与任何 Mod 停止之前**并发**调用全部 `CloseIngress`（已 Start 的 Mod、Init 已返回的 Service、Registry 登记的），在同一个停机预算内等它们返回。任何一个没排空（ctx 错误、到期不返回、panic）按停机不完整处理：不调 `Shutdown`、不停 Mod、不放锁。等在途的时间花在总预算里，不占某个 Mod 的停机份额。
- 框架里已经接上的输入：总线（nats Mod → `Bus.CloseIngress`：全部已注册 RPC 方法撤下、模块消息不再接收，出向发送照常到 Bus 停止）、ops 的 `/admin/execute`（之后答 503 “service stopping”，健康 / 就绪 / 指标照常）、`kitdataengine.SubscribeEffects` 建的 effect 消费者与 `kitsaga.Steps` 的步骤消费者（Drain 并等在途）。所以 owner Mod、生成的独立进程 `Server`、内置服务的 Mod 都不用自己撤 handler。
- 依赖序已经保证的输入不在第一步：玩家 TCP、网关、实体定时器的 Mod 依赖它们用到的 Mod，逆序停止时先停。业务在 `Service.Init` 里直接用 `IJetStream.Subscribe` 建的消费者不自动登记，由业务 `Shutdown` 负责，或自己 `AddIngress`。

### 4.2 启动中收到停机信号与启动失败

启动中（某个 Mod 的 Start、`Service.Init` 还没返回）收到信号，走同一套有序停机、只停已经开始的部分：停机预算开始计时 → 关已启动部分的输入（在途请求答完，之后的答“没执行”）→ 在预算内等正在跑的那一步返回（Mod 的 Init / Start 与 `Service.Init` 不收 ctx，只能等）→ 它返回了就算已启动 → `Service.Shutdown`（Init 已返回时）→ 逆序停已 Provide 的 Mod → 放锁。那一步在预算内不返回，保留依赖与锁返回。`startup.timeout` 到期（不是信号）只关输入、不等那个已经超时的步骤，保留依赖与锁返回。启动失败（某一步返回错误、启动检查点发现 fail-stop）同样先关输入，再 `Service.Shutdown`、停 Mod。

## 5. 配置与观测

ConfigSchema 声明键和约束，Validator 负责跨字段条件。--check-config 必须检查真实配置存在及值合法，不能把缺文件当成功；入口文件写了 `config.includes` 时检查的是叠加结果，include 缺失同样是错误（[配置 §1.1](07-config.md#11-多文件叠加configincludes)）。/readyz 的 Degraded 仍可就绪，Fail 才阻断；关闭服务应先撤销就绪。不要用“健康接口 200”替代恢复 gate 或写 fence。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
