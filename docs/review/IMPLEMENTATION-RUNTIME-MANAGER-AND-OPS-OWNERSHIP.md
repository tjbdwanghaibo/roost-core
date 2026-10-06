# 运行生命周期、元数据和 Ops 的所有权

2026-10-04，学习基线 `3529a569`。本文描述实际代码与下一步修复建议，**四项新问题尚未实施**；[问题与复现](../bug/REVIEW-2026-10-04-noncore-02.md)、[范围和未验项](REVIEW-2026-10-04-noncore-02.md)。

## 两种 Manager 的职责

app.IManager 的服务级 singleton 由 manager.Engine 驱动：Kit ManagerMod 发布 capability 和转发 Provide/Start/Stop；Engine 用 Order 排依赖，成功启动的对象才交入 started。失败 manager 的 Start 自己清理，Engine 逆序回滚之前成功对象。starting 关闭后续 Register，stopping 让未完成 Start 发现停机并接管自己的清理；这些已有规则应保留。

lifecycle.ManagerGroup 面向另一个泛型 Init/Start/Stop 契约，Init 后对象由 Group 持有，失败清理 initialized 集合；opMu 串行生命周期操作，mu 保护状态，逐对象回调保护 panic。它与 Engine 的对象阶段/失败所有权不同，不能为了复用直接换掉 Engine；应借鉴逐对象保护和明确状态，而非新增第三种生命周期框架。

Engine 当前提前转移 started 列表避免重复 Stop，却没有在回调前建立 panic 边界，导致 NC-01；starting 只用于注册门槛，未拦第二次 Start，导致 NC-02。最小修复分别补 stopOne 错误边界和 Start 进入门槛，但要同时考虑旧 shutdown 接管、失败重试与锁外 callback。App 外层 recover 属 Mod 级保护，无法完成 Mod 内被打断的逆序循环。

## 生命周期 Hook 和 Health

Lifecycle Registry 在锁内复制本次 hooks，释放锁后调用；本次执行中的替换只影响下次 Emit。EmitAll 在停止时收集错误并继续，单个 panic 转 error；两项新控制证实快照/清理行为。普通 Emit 的提前返回与停止 EmitAll 的策略不同，应按 Phase 契约调用。ManagerGroup 的用户 callback 阻塞仍占用 opMu，不代表系统能杀掉该 callback。

Health Registry 同样取 checker 快照再锁外执行，但按顺序同步聚合；传入 context 并不强制终止不配合的 Checker。空 Status 默认为 OK，Err 只填写 error 文本；自定义 checker 的错误默认策略需要明确，尚非本批确认 bug。将它接 `/readyz` 时应理解 StatusFail 才会使聚合失败的现行规则，不能仅靠 Err 分类推定 readiness。

## Admin 注册与复制边界

Command Registry 在锁外执行业务 handler，并为 panic/返回值提供边界；MetadataRegistry 保存面板可发现的 schema/risk/target 等，不替业务 handler 自动实施审批政策。JSON Schema 包含 map、数组与数组内 map，copy slice 只隔离数组元素槽位，不能隔离元素引用；len=0 也不等于 nil。NC-03 的修复应在现有复制 helpers 内完成递归 JSON 容器复制，输入和输出都遵循同一所有权规则。

不能由 schema 引用泄漏推定 token 或审批绕过；真实鉴权链还需接 N02。保持当前锁内复制/锁外使用的契约后，复制复杂度应是 schema 大小的 O(n)；这是机制分析，未有本轮容量/benchmark 数字。

## Ops server 的关闭与主链

OpsMod 用 health/lifecycle/admin/metrics 能力提供运维 HTTP，维护自己的 ready 标记，关闭用标准库 http.Server.Shutdown。Shutdown 开始会停止接受新连接，但返回取消/超时不等于活跃 handler 已结束。NC-04 正是将“停止监听”当成“所有请求排空”，并清空唯一可重试关闭的引用。

修复优先保留现有 server 和预算：错误时仍持有同一实例，排空成功才释放；若允许并发 Start/Stop，需要让旧操作无法清除新实例。不要以强制 Close 忽略 handler 或后台无限重试兑现假成功。测试顺序应由 handler 入场、listener Close、context cancel 和 handler 退出事件控制，超时只作 harness 上限，不是失败判定来源。WebSocket/hijacked 连接还需单独所有权契约。

本批真实回环请求只证明取消中的引用丢失；Manager 回调漏清理和 schema 外部修改是其他独立资源问题。下一步修复可沿用 docs 中反例转为正式 promises 测试，分别先红后绿，并补各条列出的邻接场景。

## 第二批修复后的实际实现

上文“尚未实施”为原审查时点，本批 NC-01～04 已修、声明场景验证，未发版。[运行与兼容](REVIEW-2026-10-04-noncore-03.md)。stopOne 内 recover 保护逐对象清理与启动 rollback；error panic cause 保留。Engine 原 starting/stopping 门槛在锁内取得唯一 Start 权，不新建状态机，已开始后的重试显式 ErrStartState，缺 Provide 不取得权。

schema 在现有 helpers 中递归复制 JSON 容器，包括空 map/嵌套数组，nilness 保持；非 JSON 类型不可变规则已经写入 API 注释。Ops 状态短锁与标准库 Shutdown 等待分开，错误保留实例、成功比较后释放，Start 不覆盖未关闭实例，ListenAndServe 捕获局部实例。

正式原14项全绿，追加并发与取消/超时/重试等共35叶子/独立项通过，11包race/vet通过。Health 空 Status/Err 策略仍仅观察；永久阻塞与完整 App 故障、hijack/bind/生产HA尚未验证。复制成本随schema大小增长，未增加本批性能结论。

## 2026-10-06 更新（revn01b）

- Ops 的 bind 从后台 goroutine 移到 `Start`：`Start` 返回 nil 才表示探针端点已在监听（NC-230）。关闭仍是 `http.Server.Shutdown`，A3 骨架套到 OpsMod 后绿；Ops 没有 hijack 入口。admin 命令带 `ops.admin_timeout` 期限，写超时比它长 5s，到期 504 = 结果未知（N02 O1）。
- 停机路径上的每个回调（Service.Shutdown、Mod 停止、service.stopping / stopped hook）都是“goroutine + 在 shutdownCtx 内等，超时保留依赖”（NC-231 补上 hook）；启动阶段的 hook 与 `ManagerGroup` 的回调没有预算，前者由 startupProbe 兜底，后者无生产调用方、注释写明不要用于需要预算的对象。
- `run` 的返回值包含整个生命周期里的 RuntimeFailure（NC-232），退出码因此能反映停机期间的 fail-stop。
- Health：`/healthz` 无条件 200，`/readyz` = 就绪位 ∧ 全部 checker OK（Degraded 同 Fail）。Degraded 是否算就绪待 DECISIONS-PENDING D1。[本轮](REVIEW-2026-10-06-n01b.md)
  - **更正（2026-10-06，D1 已实施）**：Degraded 算就绪，`/readyz` = 就绪位 ∧ 没有 checker 为 Fail；降级项在响应体 `degraded_dependencies` 里列出。[方案](../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md)
