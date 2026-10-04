# 非三大核心第二批：Manager / Admin / Ops

2026-10-04，审查源码为 `3529a569920f08f3f42a45da9a2119e5935b45ae`。本批测试执行于原 `183b0bdd` 加上一批四项 App/HTTP 修复的工作树，与该提交产品源码一致；下面三个有问题的产品文件没有修改。四项新问题 **已复现、未修复**。上一批 RR-20261003-NC-01～04 已关闭，不混为本批的编号。

| 编号 | 优先级 | 触发与结果 | 失败场景 |
| --- | --- | --- | --- |
| RR-20261004-NC-01 | P2 | Manager 停止 panic，中断逆序清理，后续 Stop 也找不到被跳过对象 | 3 |
| RR-20261004-NC-02 | P2 | 同一 Engine 成功后再次 Start，同一 manager 启停各执行两次 | 1 |
| RR-20261004-NC-03 | P3 | Admin 空 schema / 数组内部 map 与注册输入、Get、List 共享引用 | 6 |
| RR-20261004-NC-04 | P2 | Ops Shutdown 开始后取消 context，仍有活跃 handler 却清空 server；重试假成功 | 1 |

[可复跑附件与原始失败](../review/evidence/noncore-review-20261004-02/README.md) · [审查范围与限制](../review/REVIEW-2026-10-04-noncore-02.md) · [机制与实施建议](../review/IMPLEMENTATION-RUNTIME-MANAGER-AND-OPS-OWNERSHIP.md)。17 个叶子/独立项 = 11 个失败反例 + 5 个正常控制 + 1 个仅观察项；父测试的 fail 不重复计数。既有六包 race/vet 通过不代表这些新边界通过。

## RR-20261004-NC-01

**P2：Manager Stop panic 使清理链与启动错误丢失。** `manager/engine.go:203–218` 先把 started 列表移出 Engine，再逆序调用 `stopOne`；`223–233` 没有逐 manager 的 recover。第二个 manager 的普通 Stop 或 StopWithContext panic 会跳过更早成功启动的 manager，重试 Stop 返回 nil，因为 started 已经清空。启动第三个 manager 失败时，同一 rollback 路径还会以 panic 替代原来的 Start error。

实跑两个正常停机反例和一个启动失败回滚反例：`panic=stop boom error=<nil> older_stops=0 retry=<nil>`；rollback 也为 `older_stops=0`，原 start failure 未通过返回值保留。StopWithContext 返回普通 error 的控制能继续清理，说明缺口是 panic，不是所有错误分支。

真实调用链是 Kit ManagerMod → Engine → IManager；App 的 `stopModSafely` 即使在 Mod 外层 recover，也无法回到 Engine 被 panic 打断的循环。此跨层影响是源码推断，本批未另跑完整 App 启停故障进程。未证明用户 manager 自身能在 panic 后结束；不要把“继续清理其他对象”写成“panic 对象已释放”。

实施建议：在既有 `stopOne` 形成逐对象 panic-to-error 边界，逆序循环继续并 Join；回滚同时保留原 Start cause。可参照 lifecycle 的逐 manager 回调保护，保持锁外调用用户代码。验收沿用三个反例、正常 error 控制，补最后一个 Start 与 Stop 交错的独立清理分支，不把整个 Mod 的 recover 当替代。

## RR-20261004-NC-02

**P2：Engine 重复 Start 会重复执行同一生命周期。** `manager/engine.go:119–125` 无条件取得 managers 快照、设置 starting=true，未检查已经开始/成功的状态；每次又将同一个 manager append 到 started。对同一实例执行 Provide → Start → Start → Stop，实际 `second_start=<nil> starts=2 stops=2`。

包将 manager 定义为服务的 singleton；Register 在开始后关闭集合，但同一状态没有限制再次 Start。重复回调可能重复创建后台任务/资源或重复关闭它们，具体业务后果取决于 IManager 实现，本批仅确认次数。没有声称标准 App 一次启动会主动调用两次，也没有执行两个并发 Start 的新反例。

实施建议：在现有 Engine 锁内先取得唯一 Start 所有权，成功后的第二次请求选择显式拒绝或幂等；测试允许两种策略，但不能再次调用 manager。失败重试、Start 前未 Provide、Stop 后再 Start 的契约一并定义，不能只加 success 布尔而放过并发进入；不要持有状态锁执行用户 callback。验收补并发双 Start、早期失败与重试、Start/Stop 交错，保持已有 shutdown 接管契约。

## RR-20261004-NC-03

**P3：Admin MetadataRegistry 的 schema 副本不完整。** `admin/admin.go:255–280` 的 cloneMeta 只复制 len>0 的 PayloadSchema，已分配空 map 因而共享；cloneMap 对 []any 只复制 slice，本来合法的 JSON Schema `oneOf: [{type: string}]` 中的 map 仍共享。

六个反例分别在 Register 原输入、Get 输出、List 输出修改空 map 或数组内 map，再 Get 得到 `map[type:injected]` 或 `map[oneOf:[map[type:injected]]]`。普通 map 嵌套 map 的控制保持隔离。影响是外部消费者可以无须重新 Register 改变已有 schema，锁外并发改写还有竞态风险；本批没有主动并发改写的 race 实测。Risk/ApprovalRequired 等值字段未因此改变，不把它描述成认证/审批绕过。

实施建议：复用现有 cloneMeta/cloneMap，对非 nil 空 map 也复制、对 []any 元素递归复制 JSON 容器，保留 nil/空语义。明确非 JSON 自定义对象的所有权，避免引入新的反射框架或第二套 registry。验收六个原反例、普通嵌套控制、nil/空容器、嵌套数组与受控并发访问。

## RR-20261004-NC-04

**P2：Ops Shutdown 被取消后提前丢失 server 所有权。** `kit/ops/ops_mod.go:145–158` 在 Shutdown 返回任何结果后都赋值 server=nil。真实 http.Server 上已有 handler 在 gate 等待，Shutdown 关闭监听后才取消 context，返回 context.Canceled；此时 `server_retained=false handler_active=true`，再次 StopWithContext 却返回 nil。

反例使用本轮独占 `127.0.0.1:0` listener、实际 HTTP 请求和通道事件顺序，不用任意 sleep 或 mock Shutdown。释放 gate 后请求正常结束；只关闭自己创建的实例。空闲 server 正常关闭、重复 Stop、明确 StatusFail 的 ready 503 控制通过。DeadlineExceeded 没有独立跑，和并发 Start/Stop、WebSocket/hijack 的语义一样仍为待验，不冒认覆盖。

实施建议：Shutdown 错误时保留同一 server 引用和关闭进度，排空确认后才清除；补启动/停止操作所有权，防止旧 Stop 清除新实例。继续用现有标准库 Shutdown 与 App 停机预算，不以立即 Close 活跃 handler 掩盖问题，不增加无界后台重试。验收原取消反例、取消后再次等待排空、期限超时、正常和重复停机，再补并发操作契约。

## 契约观察，尚未升级为 RR

health.checkOne 在 Status 为空时默认 StatusOK，即使 Err 非 nil。自定义 checker 只返回 Err 时，本批观察 `/readyz` 为 200、ok=true，依赖明细有 error；内置 checker 通常显式返回 StatusFail。需先说明 Status 是否必填、Err 是否应当默认失败以及 warn 的策略，再决定改动。没有把观察测试 PASS 当作该行为正确的验收。

## 第二批修复更新（2026-10-04）

上文为原审查时点，失败和根因保留。本轮用户明确要求修复，NC-01～04 现已修、声明场景验证，未发版。[01](../bugfix/RR-20261004-NC-01.md) · [02](../bugfix/RR-20261004-NC-02.md) · [03](../bugfix/RR-20261004-NC-03.md) · [04](../bugfix/RR-20261004-NC-04.md)。正式原 14 项（11 fail/3 pass）全部转绿，后补共35正式叶子/独立项，11包race/vet通过；[红绿证据与复跑](../bugfix/evidence/noncore-bugfix-20261004-02/README.md)。Health 观察没有被本批改动或升级为确认问题。T-187～190。
