# D1：Degraded 算就绪（`/readyz` 只在 Fail 时 503）

> **状态（2026-10-06 核对）**：已实施（`f6828f17`），已随 v1.21.0 发布。文中“未做：n01b O-H1（checker 没有单独期限）”已在第十二轮实施（每个 checker 1.5s 期限，`7b73aabc`，[记录](DECISIONS-R12-KIT-2026-10-06.md)，随 v1.23.0 发布）。

2026-10-06，分支 `d1mr`，基线 `31b48bc0`。来由：[DECISIONS-PENDING D1](../review/DECISIONS-PENDING-2026-10-05.md)（第五轮决定：按推荐 (b)），[revn01b §3 Health](../review/REVIEW-2026-10-06-n01b.md)，n01s4 O2。

## 1. 问题

`/readyz` = Ops 就绪位 ∧ `health.Snapshot.OK`，而 `Snapshot` 对任何非 OK 结果都置 `OK=false`，所以 Degraded 与 Fail 一样返回 503。Degraded 的四个来源都是“还能服务、需要关注”：

| 来源 | Degraded 条件 | 位置 |
| --- | --- | --- |
| 单实例锁 `singleton` | 续期结果未知，仍在有效窗口内（≤ `renew_interval`） | `app/singleton.go` `checkHealth` |
| entitysync | 主体或会话 ≥ 80% 容量 | `sync/entitysync/manager.go` `CheckHealth` |
| remoteentity | 写许可用满（`WritesInFlight ≥ WriteLimit`） | `kit/remoteentity/remote_entity_mod.go` `checkHealth` |
| DataEngine | 投影积压告警（`BacklogWarning`） | `kit/dataengine/mod.go` 健康检查 |

生成的服务都是单副本，readiness 503 让 k8s 摘掉唯一的 endpoint；entitysync 在 80% 边界上没有滞回，会随负载来回翻转。

## 2. 设计

- **聚合规则只在一处**（`health.Registry.Snapshot`）：`ok` 不影响，`degraded` 只置 `Snapshot.Degraded`，`fail` 与未知 Status 让 `OK=false`。新增 `Snapshot.Degraded` 字段（JSON `degraded`）与 `Snapshot.DegradedResults()`。
- **`/readyz`**（`kit/ops/ops_mod.go` `handleReady`）：200 条件仍是“就绪位 ∧ `deps.OK`”，`deps.OK` 的含义变成“没有 Fail”。响应体新增 `degraded`（bool）与 `degraded_dependencies`（降级项的 `name` / `status` / `message` / `error`，没有时为空数组）；`ok`、`dependencies` 等原字段不变。
- `/healthz` 不变（无条件 200）。
- 没有采用 (c)“每个 checker 声明 Degraded 是否影响就绪”：四个来源的语义一致，多一个开关只增加配置面。

## 3. 兼容

- **行为变化**：有 Degraded 而无 Fail 时 `/readyz` 从 503 变成 200。依赖 readiness 在 80% 容量时卸载流量的部署会失去这个效果（仓内没有这样的配置）；要对降级告警，抓响应体的 `degraded`，或看各来源自己的指标（OBSERVABILITY“健康与就绪”）。
- `health.Snapshot.OK` 的含义从“全部 OK”变为“没有 Fail”。仓内唯一的聚合消费者是 Ops；`/Users/whb/roost/{cube,ssr,chaos}` 没有直接调用 `health.Snapshot` / `health.Check`（`rg` 核对）。需要旧语义的调用方用 `OK && !Degraded`。
- 生成的部署探针全部只看状态码（k8s `httpGet`、compose `/app/healthprobe` 判 2xx、shell `healthcheck.sh` 用 `curl --fail`、`deploy/dev/run.sh` 用 `curl -f`），**不需要改模板**；生成文档“readyz 成功才表示服务可以接流量”仍成立。

## 4. 验证

先红后绿（`kit/ops/readyz_degraded_promises_test.go`）：

- `TestReadyzTreatsDegradedAsReadyAndNamesTheDegradedChecker`：一个 OK、一个 Degraded checker，就绪位为真。修前红：

  ```
  readyz_degraded_promises_test.go:52: /readyz with a degraded checker = 503 ok=false, want 200 ok=true: degraded still serves
  ```

  修后 200、`ok=true`、`degraded=true`，`degraded_dependencies` 只有 `singleton`，带 message / error，`dependencies` 两项。
- `TestReadyzStillFailsOnFailOrNotReady`（控制，修前修后都绿）：Fail 与 Degraded 并存、未知 Status、就绪位为假只有 Degraded，都 503；全部 OK 时 200 且没有降级项。
- `health/health_test.go` `TestRegistrySnapshotCountsDegradedAsAvailable`：聚合规则（新字段，修前不能编译）。

## 5. 文档

OBSERVABILITY 新增“健康与就绪”一节与告警第 10 条；README、kit/README（两处）、USER_GUIDE（单实例锁观测、常见错误）、`app/singleton.go` `checkHealth` 注释、TROUBLESHOOTING 新增 T-267；APP-SINGLETON-LOCK、revn01b、IMPLEMENTATION-RUNTIME-MANAGER、PROGRESS 追加更正。

## 6. 实施状态

已实施（提交号见 DECISIONS-PENDING 第五轮 D1 行）。未做：n01b O-H1（checker 没有单独期限）仍是观察。
