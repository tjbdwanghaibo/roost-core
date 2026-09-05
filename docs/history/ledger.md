# 收敛覆盖账本

> 建档 2026-09-04。这是 bug 收敛的唯一状态来源：哪些格子多久没被看过、哪些单元待开、每个单元做了什么。
> 它放在 `docs/history/` 是因为它本身就是历史记录；每周更新一次，每个工作单元完成后追加。

## 1. 工作单元协议

一个单元 = **一次会话 · 一个 Go 包 · 一个缺陷类**。范围故意收窄：小到一次会话读得完、验得完，产出物固定，没有开放式结尾。

| 步 | 动作 | 硬约束 |
| --- | --- | --- |
| 1 | 读检查表 | 只读本缺陷类的检查表（§2），不读别的 |
| 2 | 穷举扫描目标包 | 只扫一个包（含 `_test.go`）；扫描点逐条记录，包括"看过、没问题" |
| 3 | 先写失败测试 | 每个发现先有一条在当前代码上**确实变红**的测试；写不出红测试的发现降级为"观察"，不修 |
| 4 | 修复 | 只修本发现；顺手看到的别的问题记到 §4 待开单元，不动手 |
| 5 | 回退验证 | 临时回退修复，确认测试变红，再恢复。跳过这一步的修复不算完成 |
| 6 | 四件产出 | ① 测试 ② 修复 ③ CHANGELOG 一条 ④ [TROUBLESHOOTING.md](../TROUBLESHOOTING.md) 一行。缺一件不合并 |

停止条件写在单元开头：目标包全部文件扫描记录完成，或发现数达 3 条（多余的开新单元）。
禁止事项：重构、改公开 API、顺手修其他类缺陷、扩大到其他包。

选单元的规则：取"最久未审"的格子；service 优先（正在重构、即将被 game 模板默认接入、11 处 `t.Skip`）；
昨夜故障矩阵的失败项插队。

## 2. 八个缺陷类与检查表

来自 [AUDIT_FINDINGS_2026-09-02](../AUDIT_FINDINGS_2026-09-02.md) 的实际产出率，不是理论分类。

| 编号 | 缺陷类 | 检查表（扫描时逐条问） | 09-02 产出 |
| --- | --- | --- | --- |
| C1 | 锁内远端调用 | `Lock()` 作用域内有无 redis/mongo/网络调用；调用是否带 ctx deadline；deadline 是否可配置 | F9 |
| C2 | 空洞测试 / 宽容替身 | 测试是否有断言；替身对不支持的构造是返回 `ErrUnsupported` 还是静默匹配；`t.Skip` 是否让整段测试从不执行 | F4 F10 F11 |
| C3 | 回调外累积状态 | 可重试回调（driver 重放、事务重试）外是否有累加器 | F2 |
| C4 | 跨包字面量耦合 | 同一常量在两处以字面量出现（含 YAML / CI 里的包路径、指标名、capability 名）；漂移是否报错 | F6 |
| C5 | 静默吞错 | `_ = err`、非 strict 模式吞错、日志后继续 | F5 |
| C6 | 常量指标 | 注册了但永远不变的 counter/gauge；结构上不可能失败的断言 | F4 |
| C7 | 释放无 defer | `Acquire/Lock` 后同函数无 `defer Release/Unlock`；完成链释放义务 | F13 F16 |
| C8 | 快慢路径不对称 | 两条路径对同一状态的判据不同（快/批、本地/远端、首次/重试、两个发布入口） | F1 |

其中 C1 / C2 / C6 / C7 计划写成 `cmd/glsvet` 规则（见 ROADMAP），写成规则后对应列不再消耗人工单元。

## 3. 覆盖矩阵

格子 = 最近一次完成单元的日期（+ 单元号）。`09-02` 表示该包在 2026-09-02 全量审计中被覆盖过一轮；`未审` 表示从未按本协议审过；`—` 表示该包不存在此类风险面。
`cmd/*` 主程序、`examples/*`、`integration` 测试专用包不入账。

### roost-core（57 包）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| core | `actionflow` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `admin` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `ai` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `app` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `app/buildinfo` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `bus` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `cache` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `clock` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `cmd/glsvet` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `configdata` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `container` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `dataengine` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `entity` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `entitysync` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `errcode` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `etcd` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `event` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `failurelog` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `fctx` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `featureflag` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `gateway` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `goroutine` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `health` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `hotcode` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `httpclient` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `httpserver` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `index` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `lifecycle` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `lock` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `lockstep` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `log` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `metrics` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `migration` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `mirror` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-04 U-0009 |
| core | `misc` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `mongo` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `nats` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `nest` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `ownerroute` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `redis` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot/action` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot/loadtest` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot/protocol` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot/runner` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot/scenario` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot/session` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `robot/transport` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `safemap` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `saga` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `security` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `statesync` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `syncbus` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-04 U-0009 |
| core | `syncstream` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `timer` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `webroute` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `worker` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |

### roost-kit（27 包 + CI 工作流）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| kit | `（根：CI 工作流）` | — | — | — | 09-04 U-0001 | — | — | — | — |
| kit | `（scripts/integration 环境脚本）` | — | 09-04 U-0003 | — | — | — | — | — | — |
| kit | `actionflow` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `ai` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `configdata` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `dataengine`（U-0025：C4 09-06） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `etcd` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `gateway` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `lock` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `lockstep` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `manager` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `mods` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `mongo` | 09-02 | 09-05 U-0012 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `mongo/mongotest` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `nats` | 09-02 | 09-05 U-0012 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `nest` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `nestwal` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-05 U-0013 |
| kit | `nettransport` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `ops` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `redis` | 09-02 | 09-05 U-0012 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `remoteentity` | 09-02 | 09-05 U-0023（真实 Mongo） | 09-02 | 09-04 U-0011 / 09-06 U-0025 | 09-02 | 09-02 | 09-02 | 09-05 U-0023 |
| kit | `robot` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `room` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `saga` | 09-02 | 09-02 | 09-02 | 09-06 U-0025 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `servicerpc` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `spatial` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `statslog` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `syncstream` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-04 U-0009 |
| kit | `versionstore` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |

### roost-service（12 包 + CI 工作流）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| service | `（根：CI 工作流）` | — | 09-05 U-0014 | — | — | — | — | — | — |
| service | `account` | 未审 | 09-04 U-0005 | 未审 | 未审 | 未审 | 未审 | 未审 | 09-05 U-0021 |
| service | `chat` | 未审 | 09-04 U-0007 | 未审 | 未审 | 未审 | 09-05 U-0022 | 未审 | 未审 |
| service | `directory` | 未审 | 09-05 U-0016（全包扫描） | 09-05 U-0016 | 未审 | 未审 | 未审 | 未审 | 未审 |
| service | `global` | 未审 | 09-05 U-0019（回退验证） | 未审 | 未审 | 未审 | 09-05 U-0019 | 未审 | 未审 |
| service | `global/activity` | 未审 | 09-05 U-0020（回退验证） | 09-05 U-0020（回调内重置，无问题） | 未审 | 未审 | 未审 | 未审 | 未审 |
| service | `mail` | 未审 | 09-04 U-0006 | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |
| service | `match` | 未审 | 09-04 U-0008 | 未审 | 未审 | 未审 | 09-05 U-0022 | 未审 | 未审 |
| service | `platform` | 未审 | 09-05 U-0018（回退验证） | 未审 | 未审 | 09-05 U-0018 | 未审 | 未审 | 未审 |
| service | `rank` | 未审 | 09-04 U-0004 | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |
| service | `servicemetrics` | — | 09-05 U-0020（全读） | — | — | 09-05 U-0020 | — | — | — |
| service | `servicemods` | — | 09-05 U-0020（全读） | — | — | 09-05 U-0020 | — | — | — |
| service | `session` | 未审 | 09-05 U-0017（回退验证） | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |

### roost-skill（5 包）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| skill | `combat` | — | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |
| skill | `combatcomponent` | — | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |
| skill | `skill` | — | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |
| skill | `skillcompose` | — | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |
| skill | `skillsync` | — | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 | 未审 |

### roost-codegen（16 包）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| codegen | `（根：CI 工作流 + ci/）` | — | — | — | 09-05 U-0015 | — | — | — | — |
| codegen | `internal/attribute` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/cfggen` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/dao` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/entity` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/errcode` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/eventgen` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/genutil` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/marker` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/nest` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/project` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/protocol` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/registry` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/roost` | — | 09-05 U-0015（部署模板）/ 09-06 U-0026（lifecycle） | — | 09-05 U-0015 | 未审 | — | 未审 | 未审 |
| codegen | `internal/servicerpc` | — | 未审 | — | 09-05 U-0024 | 未审 | — | 未审 | 未审 |
| codegen | `internal/tablegen` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/webroute` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |

## 4. 待开单元

| 编号 | 目标 | 缺陷类 | 来源 | 备注 |
| --- | --- | --- | --- | --- |
| ~~B-01~~ | `core/mirror` + `kit/syncstream` 的 SyncMsg 发布路径 | C8 | ROADMAP M1 状态表 | **已完成 → U-0009**：三条发布路径统一经 `syncbus.DeliveryIDs` |
| ~~B-02~~ | `kit/remoteentity/versioned_lock.go` | C8 | ROADMAP M4 | **已完成 → U-0011**：§4.2 第 1–5 项原本已实现（09-04 关键词误判），第 6、7 项与两条缺失测试在 U-0011 补齐 |
| ~~B-10~~ | `kit/dataengine` Remote commit 的 fence 竞争 | C8 | FEATURE_LOGIC §4.2 第五条测试 | **已完成 → U-0023**：`remoteentity/mongo_committer_integration_test.go` 对真实副本集并发高低 fence 提交，恰一个落库；集成脚本与 CI 的 integration job 已含 `./remoteentity`。原记录： "高 fence 与低 fence 的 Mongo 提交竞争最多一个成功，失败方得到版本冲突并进入既有隔离流程"尚无测试；需要真实 Mongo 副本集，放进 integration 套件（`dataengine/*_integration_test.go`） |
| ~~B-03~~ | `service/mail`、`service/rank`、`service/integration` 的 `REDIS_ADDR` 门控测试 | C2 | 09-04 巡检 | **已完成 → U-0014**：三处早已带 `//go:build integration` 标签，缺的是设置 `REDIS_ADDR` 的自动化——roost-service 此前没有任何 CI，32 个测试靠 skip 保绿。新建工作流，integration 段把含 `REDIS_ADDR` 字样的 skip 判失败；另 8 处 `twoDistinctSentinels` 的 skip 是防御分支，各包都有 ≥2 个 sentinel，不会触发，不算洞 |
| ~~B-04~~ | `kit` integration 套件进 CI | 流程 | AUDIT F8 / FEATURE_LOGIC 阶段 E | **已完成 → U-0010**（job 已写入 `ci.yml`，actionlint 通过；首次 GitHub 运行结果待确认） |
| ~~B-05~~ | M2 M3 M5 M6 M7 M8 M9 各一单元 | 对应类 | ROADMAP 状态表 | **已完成 → U-0012**：15 处回退，9 处已有测试红，6 处补测试后红 |
| ~~B-06~~ | `roost-skill` HEAD 领先 v1.10.0 三个 commit 且仍依赖 core v1.10.0 | 发布链 | 09-04 巡检 | **已完成**（09-05 收尾）：skill 升 core v1.12.0 并打 v1.10.2 / v1.10.3，production-gates 绿 |
| ~~B-08~~ | `service/account` `CreateRole` 提交尾部的两处失败分支 | C8 | U-0005 观察 | **已完成 → U-0021**：提交点移到最后的槽位写入，两处尾部失败全部撤销（版本校验删角色、取消/释放名字、释放槽位）。原记录： `Names.Commit` 失败直接返回，不回滚：角色已插入、名字 claim 到期后失去保留，之后别的账号可占用同名；`Slots.Update` 失败同样返回错误但角色已存在，客户端重试得到 `ErrRoleLimit`。两条分支都没有测试（没有会失败的 directory 替身）。修法要改提交顺序或加重试/修复路径，超出单个 C2 单元 |
| ~~B-09~~ | `service/chat/server_run.go` `pruneChannels`、`service/match/server_run.go` `sweepQueues` | C6 | U-0007 / U-0008 观察 | **已完成 → U-0022**：`chat.prune_channels` + `Mod.WithPruneChannels`、`match.sweep_queues` → `Config.SweepQueues`；未配置启动告警。原记录： 两个 run 钩子都返回硬编码 `nil` 且没有任何配置入口：默认部署里 chat 的 `Prune` 与 match 的 `Sweep` 永远不被调用，`retention_age` / `ticket_ttl` 的后台执行形同虚设（match 的过期仍在每次变更与读取时内联执行，所以票据不会永远等待；chat 的年龄保留则完全没有执行者）——正是这两个包自己批评的"看起来存在、什么也不做的机制"。需要由部署注入的枚举 provider，属设计改动 |
| ~~B-11~~ | `kit/nestwal` committer | C8 | 09-05 CI 巡检 | **已完成 → U-0013**。登记时的判断（"测试断言了契约不保证的性质"）**不对**：读代码发现 WAL 只串行化了 Replay 的读取，运行循环与 `Flush` 的两条 pass 在"读完 → ack 落地"窗口重叠会重复 apply——是实现的竞争窗口，测试是对的。加 `replayMu` 覆盖整条 pass |
| ~~B-12~~ | roost-codegen CI 三处债 | 流程 | 09-05 CI 巡检（v1.12.1 起即红） | **已完成 → U-0015**：登记的三处之外，逐轮推进又暴露四处（compose 短语法卷、minimum 集与生成器下限脱节、upgrade-compat 历史版本写 cube-* 路径、kustomize 祖先布局）加 Dockerfile Go 版本，共八处，四条工作流全绿。原登记：① `quality` 的 actionlint/shellcheck 对 `release.yml` 第 50/114/196 行报 SC2251/SC2035；② `generated-project-release-smoke` 的 shellcheck 对生成的 `deploy/*/*.sh` 报 SC1007（`CDPATH= cd`）/SC2194；③ `framework-release` 的 consumer-acceptance 在生成工程目录里跑 actionlint，因非 git 仓库报 "no project was found"。三处都不是本轮改动引入；本轮的清单修复让 ③ 前面的 gate 首次通过 |
| ~~B-13~~ | 发布清单与最新 tag 的错位 | 发布链 | 09-05 | **已完成**（09-05）：service v1.5.1（tag CI 首跑 rank 并发测试偶发 `lost 8 compare-and-swaps` → 测试按契约重试 ErrConflict，重跑绿）→ codegen 清单 kit v1.12.1 / skill v1.10.3 / service v1.5.1 → codegen v1.13.1（release 的 consumer-acceptance 首次真正跑 actionlint，报出生成 release 工作流的 SC2251/SC2035）→ 修模板 → codegen v1.13.2：gate / consumer-acceptance / binary-smoke ×3 / publish 全绿。原记录： kit v1.12.0 / skill v1.10.1 的 tag CI 因既有问题红，修复后补打了 kit v1.12.1、skill v1.10.2；codegen `ci/framework-release.yaml` 仍指向 v1.12.0 / v1.10.1（有效 tag，`framework verify` 通过）。下一周期发布时对齐并顺带 service / codegen 补丁版 |
| ~~B-07~~ | `service/*` × C2 全部 12 包 | C2 | 选单元规则 | **已完成 → U-0004～U-0008、U-0016～U-0020**：12 包全部过了一遍 C2（承诺回退法），其中 8 包各有修复或补测。原记录： service 的替身是自写的 `fake_redis_test.go` / `fake_envelopes_test.go`；09-02 产出最多的一类先做 |

## 5. 单元日志

| 编号 | 日期 | 目标 | 缺陷类 | 发现 | 测试 | 回退验证 | 定位文档 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| U-0001 | 2026-09-04 | roost-kit `.github/workflows/ci.yml` | C4 | 1：基准步骤仍写 `./sync`，包已在 v1.10.0 改名 `room`，该步每次失败而 `go test ./...` 绿 | `TestCIWorkflowPackagePathsExist`（kit 根） | 修复前运行为红（`ci.yml references ./sync`），修复后绿；基准命令本地按 CI 原样跑通 | T-08 |
| U-0026 | 2026-09-06 | roost-codegen `add lifecycle`（game 模板 World + Player 触发） | C2 | 1：每个 Entity 的 lifecycle 文件都声明包级 `FromRegistry`，第二个 Entity 起同包重复声明、编译不过。从未有测试在一个工程里加两个 lifecycle | `TestGameTemplateScaffoldsWorldAndPlayer`：真建模板工程，go/parser 扫 lifecycle 包无重复顶层声明，且含 `PlayerFromRegistry` / `WorldFromRegistry` / `EnsureWorld` | 改为 `<Entity>FromRegistry` 后模板工程 build / vet / `generate --check` 通过；文档与 help 同改。已生成工程的文件是业务所有，不会被改写 | — |
| U-0025 | 2026-09-06 | roost-kit `dataengine` / `saga` / `remoteentity` Mod 依赖声明（模板 game 进程首次启动触发） | C4 | 3：`DependsOn` 写了 `mods.ModHealth`（Registry 内建项，非 Mod）与 `mods.ModNatsJetStream`（nats Mod 的 capability，非 Mod）；app 按 Mod 名解析 → `unknown mod dependency "health"`。**默认生成的工程（mods 含 nest → dataengine）一个都起不来**；kit 自己的集成测试手工 Init/Provide、不经 app 排序，所以从未发现。与 U-0024 同一类：capability 名当 Mod 名 | kit 根目录 `TestEveryModDependencyNamesAKitMod`：构造全部 14 个 kit Mod，依赖名 ⊆ Mod 名集合 | 回退（stash 三处修复）测试报 5 条；修后绿；kit 全量绿。模板 game 进程对真实 Mongo 副本集 + NATS 集群 + Redis 连续两次启动 `service init` 通过、进程存活（本地环境需清空 JetStream store，否则旧 stream 预留容量触发 "insufficient storage"——环境问题，非缺陷） | T-31 |
| U-0024 | 2026-09-05 | roost-codegen `internal/servicerpc` 模板（game 模板首次启动暴露） | C4 | 1：生成的 `ClientMod.DependsOn` 返回 `mods.ModBus`——总线 **capability** 名，而 app 按 Mod **名字**解析依赖，没有 Mod 叫 `bus`。任何进程把 `NewClientMod()` 与 nats Mod 装配在一起都在启动时 `unknown mod dependency "bus"`；roost-service 八个客户端全部如此。service 的集成测试手工 Init/Provide、不经 app 排序，所以从未发现 | `TestTheGeneratedClientDependsOnTheModThatPublishesTheBus`（codegen）；`TestEveryClientModDependsOnTheNATSMod`（service，八个客户端） | 修复前模板 game 进程启动即退；改为 `mods.ModNats`、重生成 service 八包后 game 进程带四个 ClientMod 起来并存活，mail / match / chat / account 四个托管子命令各自注册 handler（本地 Redis + NATS）| T-30 |
| U-0023 | 2026-09-05 | roost-kit `remoteentity` `MongoCommitter`（B-10） | C8 | 0 缺陷：`applyCommit` 的过滤（`_ver`==base 且 `_lock_fence`<=提交 fence）在真实副本集上成立。此前只有 mongotest 假客户端的证据 | `TestRealCompetingFencedCommitsAdmitAtMostOne`（`integration` 标签）：同 base version 高低 fence 并发提交，恰一个成功、败方 `ErrRemoteVersionConflict`；低 fence 在正确版本上仍被拒、当前 fence 通过 | 首跑断言写成 `fmongo.ErrVersionConflict` 变红，committer 层已映射为协议错误，改断言后绿；本地脚本与 kit CI integration job 均绿。本地第二、三次重跑 dataengine 包因 JetStream "insufficient storage" 全红——环境问题，CI 全绿 | — |
| U-0022 | 2026-09-05 | roost-service `chat` + `match` run 钩子（B-09） | C6 | 2：两个后台钩子遍历一个返回硬编码 `nil` 的方法、无配置入口——chat 的 `retention_age` 没有执行者，match 的 `ticket_ttl` 只有内联兜底 | `TestTheRetentionLoopPrunesTheEnumeratedChannels`、`TestTheExpiryLoopSweepsTheConfiguredQueues`（可调 tick 驱动真实 run 循环）+ 配置解析 fail-closed 两条 | 把 provider 换回硬编码 `nil` 两条循环测试都红（5s 超时）；`-race` 绿；Redis 集成套件绿 | T-29 |
| U-0021 | 2026-09-05 | roost-service `account` `CreateRole`（B-08） | C8 | 2：角色记录之后 `Names.Commit` / `Slots.Update` 失败直接返回，角色留下——名字 claim 到期后可被他人占用；重试得 `ErrRoleLimit` | `TestACreateThatFailsAfterTheRoleRecordLeavesNothingBehind`（`namesFailingCommitOnce` / `slotsFailingUpdateOnce` 两个替身） | 修复前 `the role record survived a failed create`；提交点移到最后的槽位写入、之前全部撤销后绿；同账号立刻同名重试成功，他人被 `ErrNameTaken` 拒 | — |
| U-0020 | 2026-09-05 | roost-service `global/activity`、`servicemetrics`、`servicemods`（B-07 收口） | C2 | 六条承诺回退：四条红；"有界扫描先完成最早截止"与"扫描补建缺失投递并出窗"两条全绿 → 补测试；"扫描不完成 pending"回退仍绿是 `completeExpired` 在 CAS 内二次校验（双重保险）。两个小包全读、每个分支有测试 | `sweep_promises_test.go` 两条（键序与截止序相反；`dispatchesFailingOnce`） | 补后两处回退变红；`-race` 绿 | — |
| U-0019 | 2026-09-05 | roost-service `global`（B-07） | C2 / C6 | 九条承诺回退：七条红；"非持有者的拒绝不泄露 incarnation"无测试；**重试的 `CompleteMigration` 计为 accepted**（应为 replayed）；"incarnation 在 CAS 重试间只铸一次"回退编译失败无结论 → 用先输一次 CAS 的替身直接验证 | `promises_test.go` 三条（`contendedLeases` 替身、错误文本不含 token、replay 计数） | 修复前 `accepted:complete_migration=2, replayed=0`；修后 1/1；`-race` 绿。顺带修正 `AcquireLease` 注释里不存在的"同 incarnation 重取" | — |
| U-0018 | 2026-09-05 | roost-service `platform`（B-07，回退验证） | C2 / C5 | 五条注释承诺逐项临时回退：两条已有测试变红（settled ≠ held、送达后清 LastError）；三条全绿——投递方错误原因被常量替换、被超车的提交移动送达时间且不计 conflict、领取时预算已尽只原地拒绝不落 exhausted。补测试时发现 **`HandleCallback` 首次路径构造 Receipt 丢掉 `Replayed`**：`AttemptDelivery` 置位了，回调层抹平，"我发了货"与"货已被别人发过"在回调驱动的投递里不可区分 | 强化 `TestAFailedDeliveryIsRecordedNotDiscarded`（断言原因文本）；新增 `race_test.go`：`blockingDeliverer` 让重试超车慢投递、种一条 attempts==max 的 reserved 订单 | 三条回退补测试后全部变红；`HandleCallback` 补传 `Replayed` 后超车测试绿，`-race`、`-count=20` 绿 | T-28 |
| U-0017 | 2026-09-05 | roost-service `session`（B-07，回退验证） | C2 | 六条注释承诺逐项临时回退：四条已有测试变红（失败的释放保持 pending、过期在 sweep 前可读、空 run id 拒绝、…）；`releaseClaim` 的 run id 守卫回退后全绿但与版本校验删除等价，不算洞；**`Enter` 在账本 `Create` 失败时返回错误**回退为返回成功后全绿——是洞 | `TestEnterReportsALostLedgerWriteInsteadOfSuccess`（`ledgerThatFailsOnce` 替身） | 补测试后回退该处变红：`Enter answered success although the replay ledger was never written`；`-race` 绿 | — |
| U-0016 | 2026-09-05 | roost-service `directory`（B-07，7 个文件全部扫描） | C3 | 1：`Reserve` / `Commit` 在 `versionstore.Update` 回调内上报 `accepted` / `replayed` / `refused`；kit 契约允许 Mutate 多次调用，内存后端从不重试所以指标测试全绿；换"先输一次 CAS"的替身，一次预留 `accepted:reserve=2` | `TestAContendedWriteIsAcceptedOnce`（`contendedStore`：先对当前值跑一遍回调再委托） | 修复前 `accepted=2, replayed=2`；回调改为只做决定、Update 返回后上报一次 → 全 1；`-race` 绿。service CI 首轮 integration 绿 | T-27 |
| U-0015 | 2026-09-05 | roost-codegen 四条工作流 + 部署模板（B-12） | C4 / 流程 | 8：① `release.yml` 三处 `! grep` 独立语句不受 errexit 约束（SC2251）——发布卫生检查从未真正失败过；② `sha256sum *.tar.gz` 裸通配；③ `framework-compat` minimum 集钉 v1.8.0 而生成器下限 v1.10.0；④ `upgrade-compat` 用写 cube-* 路径的 v1.9.0/v1.10.0 造历史工程，永远解析不了；⑤ consumer-acceptance 在无 `.git` 的生成工程跑 actionlint；⑥ 生成 compose 用短语法挂配置，相对 `ROOST_CONFIG_ROOT` 被当命名卷；⑦ 六个部署脚本 SC1007/SC2194；⑧ kustomize v5.7+ 拒绝 base 是 overlay 祖先的布局，且 sync 认不出无头的旧清单 → `removed=0`；⑨ Dockerfile `golang:1.25` 构建 `go 1.27.0` 工程 | `deploy_hygiene_test.go` 十一条：工作流 minimum 集 == `minimumVersions`、矩阵 ≥ v1.11.0、release.yml 无裸 `!`/裸通配、compose 显式 bind、绝对配置根、脚本无已知 shellcheck 项（有 shellcheck 则真跑）、base 非祖先（有 kubectl 则真渲染两个 overlay）、老布局 sync 后旧清单消失而手写文件保留、Go 版本钉本仓 go.mod | 每条先红后绿；远端逐轮：ci 三红因 → 二 → 一 → 绿，framework-compat 六格绿，upgrade-compat 绿。本地 `kubectl kustomize` 复现 cycle detected 并在修复后渲染 9 个对象 | T-23 T-24 T-25 |
| U-0014 | 2026-09-05 | roost-service `.github/workflows/ci.yml`（新建，B-03） | C2 / 流程 | 1：仓库无任何 CI；mail / rank / integration 三处 Redis 套件带标签且以 `REDIS_ADDR` 门控，无人设置 → 32 个测试永远 skip。首轮远端又暴露：`-race` 步骤多包并行共用一个 Redis，`TestEveryModWritesUnderItsConfiguredPrefix` 把 mail 包的键当越界 | 根目录 `ci_test.go` 钉工作流四事实；integration 段对含 `REDIS_ADDR` 的 skip 判失败 | 本地 `-tags integration` 无 addr 计 32 skip；远端 unit / integration / release-hygiene 三段绿；`-p 1` 后 `-race` 步骤绿 | T-22 T-26 |
| U-0013 | 2026-09-05 | roost-kit `nestwal` committer（B-11） | C8 | 1：运行循环与 `Flush` 各自调用 `replayPass`，WAL 只串行化读取，一条 pass 在另一条"读完、未 ack"的窗口进入会从旧 fence 重读并再次 apply。契约允许（at-least-once）但是纯浪费的竞争，也是 CI 偶发 `apply calls=2` 的根因。登记 B-11 时判为"测试过严"是错的 | `TestConcurrentReplayPassesApplyEachRecordOnce`：构造期 seam 把第一条 pass 停在读与 ack 之间，放第二条进去 | 修复前确定性 `apply calls=3, want 1`（含被唤醒的循环 pass）；加 `replayMu` 后 1；`-race`、`-count=3`、单测 ×10 全绿。中途 seam 先做成运行期字段触发 race detector（测试写 / 循环读），改为构造期 option | T-21 |
| U-0012 | 2026-09-05 | FEATURE_LOGIC M2/M3/M5–M9 的实现（core mirror/configdata/lifecycle，kit saga/nats/etcd/redis/mongo） | C2（回退验证） | 15 处逐项临时回退：9 处已有测试变红；6 处无一变红——redis distLock 的 TTL 校验、SETNX 与 Release 回复丢失后的 uncertain 态、per-acquisition token；nats nil handler；mongo 未知 WriteModel。另发现 `TestInvokeNatsHandlerContainsPanic` 为空断言（置一个永远为 true 的标志） | 新增 redis `lock_state_test.go`（4 条 + `scriptedRedis` 求值型替身）、nats 2 条、mongo 1 条；空测试改为断言 `nats.subscription.handler_panic.total` +1 | 补测试后重跑 6 处回退全部变红；kit 28 包绿 | T-20 |
| U-0011 | 2026-09-04 | roost-kit `remoteentity`（B-02 / M4） | C4（鸭子类型代替公开契约）+ 缺测试 | 2：① `batch.go` 用 `interface{ Fence() uint64 }` 鸭子类型而非 core 已公开的 `redis.IFencedVersionedLock`；无 fence 的锁工厂被接受，直到每次共享操作才以 `ErrRemoteFenced` 拒绝 ② §4.2 要求的"第一代迟到 unlock 不得删除第二代 owner"没有测试（机制正确） | `TestManagerRefusesALockFactoryWithoutFences`（构造 + 创建两处拒绝）、`TestStaleFirstGenerationUnlockCannotEvictSecondOwner` | ① 修复前 `a wrapper was created over an unfenced lock` 红，改公开契约 + 构造探针 + Provide 失败后绿 ② 新测试直接绿（补测试）。kit 全量 build、remoteentity 测试与 vet 绿 | T-19 |
| U-0010 | 2026-09-04 / 09-05 | roost-kit `.github/workflows/ci.yml`（B-04） | 流程（F8） | 1：集成套件只 vet 不跑。远端首跑又暴露 3 处脚本 `/private/tmp` 硬编码（macOS 专属，Linux 上 `mkdir /private` 被拒） | 新 `integration` job：装 mongod 8.0 / mongosh / nats-server v2.14.5 / jq / nc → 环境自检 → `dataengine-env.sh test` → 失败打印 status → 总是 down；脚本根目录改为 `$(cd /tmp && pwd -P)` 派生 | **回填**：run 33963458514 红（`mkdir /private`），33963686240 红（`GOCACHE=/private/tmp`），33964579095 **全绿**（Mongo 副本集 + NATS 集群 + dataengine/nestwal/saga 三包）。同一 run 里其余四个 job 也绿 | T-18 |
| U-0009 | 2026-09-04 | roost-core `syncbus` + `mirror`、roost-kit `syncstream`（B-01，跨包因为它就是"三条路径一条规则"） | C8 | 1：三条 SyncMsg 发布路径两套身份规则——`PatchSyncer` 有进程唯一 ID，`mirror`（无 MessageID 也无 sid → 无去重键）与 `syncstream`（元组含序号 → 重启撞键）没有。具体后果：同 key 同 version 的 upsert/delete 会共享去重键；重启发布者与自己撞键，新帧被 broker 当重复丢弃 | `TestPublishedMessagesCarryDistinctDeliveryIDs`（mirror）、`TestFramesCarryDistinctDeliveryIDsAcrossPartsAndPublishers`（syncstream）、`DeliveryIDs` 两条单测 | 两条新测试修复前红（`carries no delivery id`），加 `DeliveryIDs` 并接入后绿；core/kit 全量 build、syncbus/mirror/syncstream/room/remoteentity 测试与 vet 绿 | T-17 |
| U-0008 | 2026-09-04 | roost-service `match`（11 个文件全部扫描） | C2 | 2：① Mutate 回调不纯（实为 C3 类，经替身语义差异发现）：就地挪移 `Waiting` 后返回"不保存"，MemoryStore 存储值尾部重复，`Candidates` 给出同一张票两次；四个回调无一 clone，测试从未走"过期 + 放弃保存"的组合 ② `newStoreEach` 助手原样返回同一 store，注释声称隔离单票规则，实为死脚手架 | `TestAnAbortedMutationLeavesStoredStateUntouched`（过期票 + 回放式入队 → Candidates 无重复、长度为 1）；删除助手 | ① 修复前 `2 entries, ticket … seen 2 times` 红，加 `clone()` 后绿；② 删除后测试仍绿（它靠不同 player id 而非助手通过）。全包绿、全模块 13 包绿、vet 绿 | T-16 |
| U-0007 | 2026-09-04 | roost-service `chat`（11 个文件全部扫描） | C2 | 2：① 幂等键去重不绑定发送者：同频道另一名玩家撞键时拿回前者消息且报成功，自己的消息静默丢失；角色可"回放"系统消息的键；无测试覆盖跨发送者撞键 ② `TestOnlyThePrivilegedEntryPoint…` 两处只断言 `err != nil` | `TestAnIdempotencyKeyIsBoundToItsSender`（角色/角色、角色/系统、系统/角色三种撞键 + 原发送者回放仍有效 + 计数）；两处断言改具体哨兵 | ① 修复前 `got <nil>, want ErrConflict` 红，修复后绿；② 收紧断言前后均绿。全包绿、全模块 13 包绿、vet 绿 | T-15 |
| U-0006 | 2026-09-04 | roost-service `mail`（16 个文件全部扫描） | C2 | 3：① `Send` 回放路径不重投递，与代码注释"重试会重新尝试投递"矛盾——广播 fanout 失败一次即永久"已发送"，直投部分失败重试到不了漏掉的人；无测试覆盖投递失败后的重试 ② `List` 游标按 id 相等定位，游标邮件被删后翻页提前结束且无游标（静默截断）；翻页测试没有在两页之间删邮件 ③ `TestAnOversizedLimitIsClamped` 只断言 `>` 上限 | 3 条新测试：广播失败后重试投递计数为 2、直投漏投的收件人在重试后收到、游标邮件被删后 25 封全部可达；clamped 断言改 `!=` | ①② 修复前红（`attempted 1 times, want 2`；`never reached the recipient`；`reached 7 of 25`），修复后全包绿；③ 为收紧断言，前后均绿。全模块 13 包绿、vet 绿 | T-13 T-14 |
| U-0005 | 2026-09-04 | roost-service `account`（11 个文件全部扫描） | C2 | 2：① `TestSelectRoleDoesNotPersistWhenSigningFails` 空测试——用不存在的 id 0 触发失败，签名从未执行，断言的是无关角色；把 SelectRole 的签名/落库倒序它仍绿 ② `UpdateProfile` 超长载荷返回 `ErrConflict`，`ErrRangeInvalid` 定义/配对/测过却无任何生产路径产出；原测试只断言 `err != nil` | 重写为在 store 种 id 0 角色（core 拒签）并断言版本与时间戳不变；oversize 断言 `errors.Is(err, ErrRangeInvalid)` | ① 倒序生产代码：旧测试绿（空洞证明）、新测试红；恢复后绿，`git diff` 确认 service.go 恢复原样 ② 修复前 `account: conflict: profile is 4097 bytes`，修复后绿；全包 + 全模块 13 包绿、vet 绿 | T-12 |
| U-0004 | 2026-09-04 | roost-service `rank`（14 个文件全部扫描） | C2 | 2：① `CodeConflict` 声明了但没有带码哨兵，CAS 耗尽经 `errors.New` 返回，RPC 信封报 `CodeInternal`；`errcode_test` 的手写配对表漏掉它、`segmentAllocated=7` 与表而非与常量对齐，所以"每个哨兵都带码"对"有码无哨兵"盲 ② `TestAroundCentres…` 名字承诺"居中"，只断言 owner 在窗口内 | `TestSubmitContentionReportsCodeConflict`；配对表 +1、`segmentAllocated` 8；Around 断言中位与连续 rank | 修复前 `code 1 ("server error"), want 540108` 红；修复后全包绿、vet 绿 | T-11 |
| U-0003 | 2026-09-04 | roost-kit `scripts/integration/lib/nats.sh` | C2 | 1：`nats_cluster_ready` 只查 JetStream 已启用 + 路由数，不等元集群选出 leader；冷启动后第一个夹具在 `ensure effect stream` 上等满 30s 超时，`TestRealMongoPrimaryFailoverContinuesProjection`（包内第一个执行的测试）在故障注入前就失败 | 环境自检 `dataengine_env_test.sh` + 完整套件 | 修复前两轮完整运行同一处确定性失败（不是 flaky）；加入 `/jsz .meta_cluster.leader` 判定后第三轮三个包全绿，`status` 显示三节点一致的 meta_leader | T-10 |
| U-0002 | 2026-09-04 | roost-codegen `ci/framework-release.yaml` + `framework_release.go` | C4 | 1：清单落后两个次版本（core v1.9.1 / kit v1.9.2 / skill v1.9.1）且无 service 字段，release 门禁一直校验旧组合 | `TestFrameworkReleaseManifestStrictValidation/no-service` | 去掉 `service:` 字段清单不再通过校验 | T-09 |

### U-0004 扫描记录（看过、没问题的项也记）

`fake_redis_test.go`：按脚本文本子串分派三段 Lua，默认分支返回 `ErrCASInvalidCommand`（fail-closed）；swap / remove 语义与 Lua 逐条对照一致；`toStr` 对未知类型返回空串是宽容点，但当前调用方只传 string / int64，记为观察不修。
`redis_store.go`：`luaString` 对非字符串返回空串（观察）；`_ = current` 死赋值（观察）；replay 路径两段重复注释（观察）。
`store_test.go` 15 个测试全部有断言，无 `t.Skip`；`member_test.go` 用独立比较器验证字节序，非自证。
`errcode_test.go` 的 `twoDistinctSentinels` skip 分支在本包不可达（8 个哨兵）。
`redis_integration_test.go` 3 个测试覆盖 Lua 文本本身，`REDIS_ADDR` 门控 → B-03。
`rank_rpc_gen.go` 生成物本包无直接测试，靠 codegen golden；`rank_mod_test.go` 5 个测试覆盖配置、依赖声明、缺 capability。

### U-0005 扫描记录

跨服务巡检（只用于选单元）：U-0004 之后十个服务的 `Code*` 常量全部有 `errcode.Define` 哨兵且进入配对表（`global/activity` 用切片而非 map 配对，形状不同但覆盖完整）。
`account_test.go` 21 个测试：其余 19 个断言具体哨兵或具体值，无 `t.Skip`；`TestValidateSession…` 里 `_ = cfg` 死变量（观察）。
`account_mod_test.go` 3 个测试覆盖三件必需协作者缺失、空 secret、缺 Redis capability。
替身：无自写 fake，用 kit `versionstore.NewMemoryStore`、service `directory` 内存态、`servicemetrics.NewRecorder`——都是可执行实现而非桩，宽容度取决于 kit MemoryStore 与 RedisStore 的语义一致性（kit × C2 已在 09-02 覆盖）。
生产代码观察（不修）：`MaxPageSize` 无使用（本包没有列表接口）；`CreateRole` 只拒绝 allocator 返回 0，负数放行；`ErrVerifierUnavailable` 为无码错误按设计走 `CodeInternal`（渠道故障就是服务侧故障，注释写明）。

### U-0006 扫描记录

替身两份：`fake_envelopes_test.go` 对 Create 严格不覆盖、计数单读/批读次数，`failGetMany` 钩子无任何测试使用（观察：死脚手架）；`redis_store_test.go` 的 `fakeRedisEnvelopes` 求值 SetNX / Get / MGet 语义，含短回复与损坏值注入，均被断言。
`mail_test.go` 30 个测试全部断言具体哨兵、计数或结构，无 `t.Skip`；并发测试 3 个。`rpc_test.go` 12 个测试覆盖生成传输的本地/总线一致性与错误码穿透。`redis_integration_test.go` 5 个（`REDIS_ADDR` 门控 → B-03）。
生产代码观察（不修）：`Send` 早期对 `AudienceBroadcast && Broadcast == nil` 的拒绝发生在账本查询之前，因此无 deliverer 的进程回放一条广播记录也会被拒——语义正确；`Deliver` 对 `nowUnix <= 0` 回退到当前时钟，测试大量传 0，实际时间由夹具时钟决定，可接受。
在途状态：`mail/client.go`、`client_mod.go`、`rpc.go`、`server.go` 四个文件在 git index 中为"已暂存新增"、工作树中已删除（被生成的 `mail_rpc_gen.go` 取代）。工作树编译通过；若不带 `-a` 提交会把这四个文件带回并与生成物重复定义。未处理，等仓主决定。

### U-0007 扫描记录

替身：`allowAllPolicy`（仅测试）、`recordingPolicy`（计数 + 可拒绝，被断言）、`brokenState`（Get/Update 可注入失败，被断言）、共享 `servicemetrics.Recorder`；状态用 kit `MemoryStore`。无自写 Redis 替身——chat 的存储完全经 kit `versionstore`。
`chat_test.go` 26 个测试全部断言具体哨兵 / 序列 / 计数，无 `t.Skip`；反射测试钉死请求类型字段集；时钟倒跑证明排序只来自序列。`chat_mod_test.go` 3 个。
生产代码观察（不修）：`pageOf` 空页对 `BeforeSeq` 分支把 `NextCursor` 设为 `LastSeq`、`PrevCursor` 留 0——滚动到最旧后客户端以 `HasMore=false` 判终止，语义可用但两个游标含义在空页上不对称；`Prune` 不回收 `Requests` 键（已注释说明，由 `trim` 按数量兜底）；`AppendSystem` 对非 SystemOnly 的共享频道也放行（设计：系统可在世界频道发公告）。

### U-0008 扫描记录

替身：仅 kit `MemoryStore` 与共享 `servicemetrics.Recorder`，无自写 fake。发现 ① 正是 MemoryStore 与 RedisStore 的语义差异（交出存储值 vs 每次解码）暴露出来的：match 没有像 chat 那样在回调开头 `clone()`。
`match_test.go` 22 个测试全部有断言、无 `t.Skip`；并发测试 2 个；`TestResolvedTicketsLeaveTheWaitingList` 直接断言内部不变量并写明原因。`_ = expiring` / `_ = live` / `_ = fmt.Sprint()` 三处死变量（观察）。
生产代码观察（不修）：`Enqueue` 回放命中已终态票据时按原样返回（同请求同结果，可接受）；`Refused` 计数在回调内但错误路径不重试，不会重复计数；`Enqueue` 成功后额外一次 `QueueLength` 读取用于深度指标；`Ticket()` 读路径把已到期票据报为 expired 而不落库（注释已说明）。

### U-0015 扫描记录

工作流四条（ci / framework-compat / upgrade-compat / release）与 `ci/framework-release.yaml` 逐步骤读过。未修的观察：`release.yml` 的 `binary-smoke` 三平台矩阵与 `consumer-acceptance` 仍以 tag 触发，本轮没有 tag 所以远端未验证（B-13 打 tag 时看）；`security.yml` 一直绿；`nightly.yml` 未读。生成器侧：`render_cicd.go` 生成的 CI 与本仓 `ci.yml` 的 smoke 步骤是同一组命令的两份手抄（compose / kustomize / shellcheck），本轮两边同改——是 C4 候选，未建单元。sync 的"过时产物删除"只认 `Code generated` 头与 `isGeneratedData` 路径，k8s 清单此前无头，是本次 `removed=0` 的根因；已给 base/ 清单加头，旧位置按固定文件名 + `roost` 命名空间识别。

### U-0016 扫描记录

`directory.go` / `store.go` / `redis_store.go` / `directory_mod.go` 全读。四个不变量对照：Reserve 过期即缺席（Update 内判定，无读-决-写）；Commit 先验 token 再验过期，"提交别人的预留"不可表达；Cancel / Release 版本校验删除，输掉即 no-op 并计数；无 Set。替身仅 kit `MemoryStore` + `servicemetrics.Recorder`，无自写 fake；`directory_test.go` 13 个测试全部断言具体哨兵或计数，并发测试 2 个。跨包扫描（脚本：Update/Create/Mutate 回调体内的 `report.*` / `.Add(`）：account / global / match 的回调内只有 `Refused` 且随错误返回——回调不会重跑，不计重；`.Add(` 全是 `time.Add`。只有 directory 在回调内 `Accepted`。

### U-0017 扫描记录

`session/service.go` 771 行按承诺注释读过 30 处；六条可低成本回退的逐项试：P5 释放失败保持 pending（3 个测试红）、P6 Get 在 sweep 前读为 expired（1 红）、P11 空 run id 拒绝（1 红）、P2 releaseClaim run id 守卫（绿，等价于版本校验，仅影响 `claim.release_not_ours` 计数——观察）、P8 账本写失败返回错误（绿 → 补测试）、P10 输掉 claim 时 discard 本 run（片段出现两次，未回退——观察：`TestAnOwnerRacingItselfGetsOneRun` 是否断言 run 数量待查）。替身：`recordingReleaser` 按资源计成功与尝试并可注入失败次数——是求值型替身，不是宽容桩。

### U-0018 扫描记录

`platform/service.go` 612 行读 `AttemptDelivery` / `HandleCallback` / `recordFailure` 全路径；`admin.go` 只读承诺注释。领取、判定、耗尽全在一个 CAS 回调内且回调只做决定（`outcome` 变量、返回后上报）——与 U-0016 的 directory 形成对照，platform 没有回调内计数问题。替身：`recordingDeliverer` 按订单计成功、可注入失败次数与固定错误，是求值型；`acceptingVerifier` / `resolver` 为最小可执行实现。观察（不修）：`raced` 分支的注释说"若走到这里是值得看的 bug"，但它其实是可达的正常竞态（慢投递 + 退避到期的重试），本次测试正是靠它成立——注释语气偏强，行为正确；`_ = found` 死赋值。B-07 至此覆盖 account / chat / directory / mail / match / platform / rank / session 八包，剩 global、global/activity、servicemetrics、servicemods。

### U-0019 / U-0020 扫描记录

global：`service.go` 447 行全读；`Refused` 全在回调内且随错误返回，不会重跑；`AcquireLease` 的铸币缓存正确处理了 CAS 重试。观察（未修）：`LiveGames` 对每个候选一次 `Get`，N 次往返，是性能而非正确性问题。activity：`service.go` 1155 行按承诺读；回调开头显式重置所有决策变量（"mutate may run again on a lost compare-and-set"），与 U-0016 的 directory 形成对照。servicemetrics（392 行）与 servicemods（296 行）全读：`Sink` 对 nil reporter 全方法安全、`Dropped(0)` 不上报；`KeyPrefix` / `Secret` / `Duration` / `RequiredDuration` 的每条拒绝分支各有测试。B-07 的 12 包至此全部过了一遍 C2。

### U-0021 / U-0022 说明

U-0021 的设计选择：撤销而非"向前修复"。角色记录尚未交给调用方，版本校验删除是安全的；名字在 Commit 前按 claim 取消、Commit 后按 owner 释放；撤销本身失败计 `rollback.failed`（与既有语义一致）。U-0022 的设计选择：枚举由部署提供而不是扫描 keyspace——两处注释早已写明理由；chat 的静态项在 Provide 时逐条 `Resolve`，pair 类频道无法用配置表达（需要 participant），走 `WithPruneChannels`；match 的 `SweepQueues()` 以可选接口暴露，Store 的测试替身不必实现；两处未配置都在启动时告警一次，不再沉默。

## 6. 方向二进度：game 模板

**第一切片（2026-09-05，codegen 66b2d19）已落地**：`services.<name>.framework`（account / mail / match / chat 作为独立子命令托管：Server + owner Mod，redis / nats 自动补齐）、`services.<name>.uses`（业务 Service 装配 ClientMod、生成类型化访问器）、`versions.service` 与 `-roost-service-version` / `upgrade -service`、`-template game`。协作者文件 `internal/service/<name>/collaborators.go` 只生成一次、默认全部拒绝。验证：模板工程 build / vet / `generate --check` 通过；对本地 Redis + NATS 五个子命令全部起来（game 进程需要修复 U-0024 后的 roost-service）。framework-compat 的 full 场景已加 `-template game`。

**设计选择与理由**：app 的模型是"一个子命令一个 Service"，因此托管服务是独立进程而不是塞进 game 进程；这与 roost-service `examples/split` 的形状一致，部署产物（compose / k8s / shell）随 `services` 自动覆盖每个托管服务。协作者不给宽容默认（拒绝而非放行）是 roost-service 自己的原则。

**发布（2026-09-06）**：service v1.5.2（重生成的八个传输层 + U-0019～U-0022）→ codegen 清单 service v1.5.2 → codegen v1.13.3：gate / consumer-acceptance / binary-smoke ×3 / publish 全绿；framework-compat 六格与 upgrade-compat 绿。随之把生成器的版本下限抬到 core / kit v1.12.0、skill v1.10.3、service v1.5.2——roost-service v1.5.x 要求 core / kit v1.12.0，钉旧版本的工程一托管框架服务就解析不了；下限从此是"能整体解析的最老组合"。**遗留**：roost-service go.mod 的 `tool` 指令仍指向 codegen v1.12.1（其模板仍生成 `ModBus` 依赖），下一次 service 发布时对齐到 v1.13.3 并用 `go generate` 复核八个包无 diff。

**第二切片（2026-09-06，codegen 360386c）已落地**：`-template game` 补 nest，生成 Player / World 两个 Entity 与 lifecycle、`game/lifecycle/world_singleton.go`（`WorldUniqueID = 1`，`EnsureWorld` = GetOrCreate）与在 `Init` 里确保 World 的 game Service。World 的"单例"是进程属性：一个 game 进程一份，首次创建、之后加载，没有 World 不启动。真启动暴露两个装配级缺陷（U-0025 kit、U-0026 codegen），这正是"模板是后续验证载体"的意义。**教训**：三个"装配级"缺陷（U-0024/25/26）都只有真的 `app.Execute()` 才能发现，单元与手工 Init/Provide 的集成测试全部看不见——下一步应把"生成工程真启动"做成 CI 门禁（见待做 ⑥）。

**待做切片**：③ 生成工程的 QUICKSTART / FIRST_BUSINESS 文档加托管服务一节，`project next` 识别未实现的协作者；④ roost-service 的 `tool` 指令对齐到含 U-0024 的 codegen 版本；⑤ 发布：service v1.5.2 → codegen v1.13.3 已完成；本轮再发 kit v1.12.2（U-0025）→ codegen 清单 kit v1.12.2 → codegen v1.13.4；⑥ **启动门禁**：在 codegen 的 framework-compat 或 kit 的 integration job 里，对生成的模板工程真的 `game` / `mail` 起进程（Redis + NATS + Mongo 服务容器），断言 `service init` 通过——三个装配级缺陷都只有这一步能抓到。

