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
| core | `bus` | 09-06 脚本扫 | 09-06 U-0043（回退 32 条） | 09-02 | 09-02 | 09-06 脚本扫 | 09-02 | 09-06 脚本扫 | 09-02 |
| core | `cache` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `clock` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `cmd/glsvet` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `configdata` | 09-02 | 09-06 U-0046（回退 45 条） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `container` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `dataengine` | 09-02 | 09-06 U-0044（回退 27 条） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `entity` | 09-02 | 09-06 U-0065（回退 7 条） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
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
| core | `mirror` | 09-02 | 09-06 U-0045（回退 14 条） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-04 U-0009 |
| core | `misc` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `mongo` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `nats` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `nest` | 09-02 | 09-06 U-0047（回退 45 条） / U-0062 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
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
| core | `saga` | 09-02 | 09-06 U-0050（回退 40 条） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `security` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| core | `statesync` | 09-02 | 09-06 U-0067（回退 9 条） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
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
| kit | `dataengine`（U-0025：C4 09-06） | 09-02 | 09-02 | 09-02 | 09-02 | 09-06 U-0037 | 09-02 | 09-06 脚本扫 | 09-06 U-0037 |
| kit | `etcd` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `gateway` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `lock` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `lockstep` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `manager` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `mods` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `mongo` | 09-02 | 09-05 U-0012 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `mongo/mongotest` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `nats` | 09-02 | 09-05 U-0012 | 09-02 | 09-02 | 09-06 U-0036 / U-0042 | 09-06 U-0036 | 09-06 脚本扫 | 09-02 |
| kit | `nest` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `nestwal` | 09-02 | 09-06 U-0027 / 09-06 U-0048（回退 40 条） | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-05 U-0013 |
| kit | `nettransport` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `ops` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `redis` | 09-02 | 09-05 U-0012 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-06 U-0061
| kit | `remoteentity` | 09-02 | 09-05 U-0023（真实 Mongo） / 09-06 U-0049（回退 40 条） | 09-02 | 09-04 U-0011 / 09-06 U-0025 | 09-02 | 09-02 | 09-02 | 09-05 U-0023 |
| kit | `robot` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `room` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `saga` | 09-02 | 09-06 U-0051（回退 40 条） | 09-02 | 09-06 U-0025 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `servicerpc` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `spatial` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `statslog` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |
| kit | `syncstream` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-04 U-0009 |
| kit | `versionstore` | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 | 09-02 |

### roost-service（12 包 + CI 工作流）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| service | `（根：CI 工作流）` | 09-06 脚本扫 | 09-05 U-0014 | — | — | 09-06 脚本扫 | — | 09-06 脚本扫 | — |
| service | `account` | 09-06 脚本扫 | 09-04 U-0005 / 09-06 U-0058（回退 30 条） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 09-05 U-0021 |
| service | `chat` | 09-06 脚本扫 | 09-04 U-0007 | 未审 | 未审 | 09-06 脚本扫 | 09-05 U-0022 | 09-06 脚本扫 | 未审 |
| service | `directory` | 09-06 脚本扫 | 09-05 U-0016（全包扫描） | 09-05 U-0016 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| service | `global` | 09-06 脚本扫 | 09-05 U-0019（回退验证） / 09-06 U-0059（回退 33 条） | 未审 | 未审 | 09-06 脚本扫 | 09-05 U-0019 | 09-06 脚本扫 | 未审 |
| service | `global/activity` | 09-06 脚本扫 | 09-05 U-0020（回退验证） | 09-05 U-0020（回调内重置，无问题） | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| service | `mail` | 09-06 脚本扫 | 09-04 U-0006 / 09-06 U-0054（回退 40 条） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| service | `match` | 09-06 脚本扫 | 09-04 U-0008 / 09-06 U-0057（回退 38 条） | 未审 | 未审 | 09-06 脚本扫 | 09-05 U-0022 | 09-06 脚本扫 | 未审 |
| service | `platform` | 09-06 脚本扫 | 09-05 U-0018（回退验证） / 09-06 U-0055（回退 40 条） | 未审 | 未审 | 09-05 U-0018 | 未审 | 09-06 脚本扫 | 未审 |
| service | `rank` | 09-06 脚本扫 | 09-04 U-0004 / 09-06 U-0060（回退 32 条） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| service | `servicemetrics` | 09-06 脚本扫 | 09-05 U-0020（全读） | — | — | 09-05 U-0020 | — | 09-06 脚本扫 | — |
| service | `servicemods` | 09-06 脚本扫 | 09-05 U-0020（全读） | — | — | 09-05 U-0020 | — | 09-06 脚本扫 | — |
| service | `session` | 09-06 脚本扫 | 09-05 U-0017（回退验证） / 09-06 U-0056（回退 40 条） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |

### roost-skill（5 包）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| skill | `combat` | 09-06 脚本扫 | 未审 | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| skill | `combatcomponent` | 09-06 脚本扫 | 09-06 U-0066（回退 4 条） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| skill | `skill` | 09-06 脚本扫 | 09-06 U-0028（回退验证 11 条，4 洞） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| skill | `skillcompose` | 09-06 脚本扫 | 09-06 U-0063（回退 10 条） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |
| skill | `skillsync` | 09-06 脚本扫 | 09-06 U-0064（回退 9 条） | 未审 | 未审 | 09-06 脚本扫 | 未审 | 09-06 脚本扫 | 未审 |

### roost-codegen（16 包）

| 模块 | 包 | 锁内远端调用 | 空洞测试/宽容替身 | 回调外累积状态 | 跨包字面量耦合 | 静默吞错 | 常量指标 | 释放无 defer | 快慢路径不对称 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| codegen | `（根：CI 工作流 + ci/）` | — | — | — | 09-05 U-0015 | — | — | — | — |
| codegen | `internal/attribute` | — | 09-06 U-0034（回退 9 条，9 洞） | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/cfggen` | — | 09-06 U-0034（回退 8 条，3 洞） | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/dao` | — | 09-06 U-0041（回退 4 条，4 洞；解析层原有覆盖） | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/entity` | — | 09-06 U-0039 | — | 未审 | 09-06 U-0039 | — | 未审 | 未审 |
| codegen | `internal/errcode` | — | 09-06 U-0030（回退 1 条，1 洞） | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/eventgen` | — | 09-06 U-0038 | — | 未审 | 09-06 U-0038 | — | 未审 | 未审 |
| codegen | `internal/genutil` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/marker` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/nest` | — | 09-06 U-0035（回退 8 条，4 洞） | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/project` | — | 未审 | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/protocol` | — | 09-06 U-0032（回退 6 条，5 洞） | — | 未审 | 未审 | — | 未审 | 未审 |
| codegen | `internal/registry` | — | 09-06 U-0030（回退 4 条，2 洞） | — | 未审 | 09-06 U-0030 | — | 未审 | 未审 |
| codegen | `internal/roost` | — | 09-05 U-0015（部署模板）/ 09-06 U-0026（lifecycle） | — | 09-05 U-0015 / 09-06 U-0029（dev compose） | 未审 | — | 未审 | 未审 |
| codegen | `internal/servicerpc` | — | 未审 | — | 09-05 U-0024 | 未审 | — | 未审 | 未审 |
| codegen | `internal/tablegen` | — | 09-06 U-0033（回退 5 条） | — | 未审 | 未审 | 09-06 U-0033 | 未审 | 未审 |
| codegen | `internal/webroute` | — | 09-06 U-0040（回退 4 条，2 洞） | — | 未审 | 09-06 U-0040 | — | 未审 | 未审 |

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
| B-14 | `core/bus/reliable.go` `requeueMsgID`、`kit/saga` `commandDigest` / 完成摘要：`json.Marshal` 的错误被丢 | C5 | U-0036 扫描 | 今天的结构体都是纯值字段、不会失败；一旦加了 `any` / 函数字段，全部摘要退化为同一个值 → 去重误判。低优先，改成返回错误或在摘要里混入 ID |
| ~~B-15~~ | 故障矩阵第三切片：NATS `timeout` toxic（半开）对 JetStream 发布确认 / RPC 等待 | 故障矩阵 | 第 8 节 | **已完成 → U-0042**（同时补 `nats.ignore_discovered_servers`）。原备注： 现有两条 NATS 测试覆盖 latency 与 reset_peer；半开连接是另一种失败形态（发布方拿不到 ack 也拿不到错误） |
| ~~B-16~~ | core `configdata` 定义校验与 auto 表 cfg 标签规则 | C2 | 回退采样 | **已完成 → U-0046** |
| ~~B-17~~ | core `nest` Cast 辅助函数与管理器守卫 | C2 | 回退采样 | **已完成 → U-0047** |
| B-18 | core `entitysync`（7/9）| C2 | 回退采样 | mirror → U-0045、saga → U-0050 已完成；entitysync 多为参数守卫，低优先 |
| B-19 | kit `redis`（20/25）守卫 | C2 | 回退采样 | nestwal → U-0048、remoteentity → U-0049、saga → U-0051 已完成；redis 多为配置 / nil 守卫，低优先 |
| ~~B-20~~ | 回退复测后剩余的实质守卫 | C2 | 第 9 节复测 | **已完成 → U-0052 / U-0053 / U-0062** |
| ~~B-21~~ | service 回退采样剩余 | C2 | 第 9 节 service 表 | **已完成 → U-0054～U-0060**：mail / platform / session / match / 胶水 / global / rank 各一单元；剩余的是请求参数守卫（playerID ≤ 0 之类）、"vanished during commit" 与需要 Redis 的脚本返回形状守卫，低优先 |
| B-22 | core 首份 nightly gap map 里整片无覆盖的包：`syncbus`、`security`、`ownerroute`、`migration`、`failurelog`、`admin`、`hotcode`、`etcd`、`robot/*` | C2 | nightly-gapmap 34029785123 | `entity` → U-0065、`statesync` → U-0067 已完成；下一个 `security`（鉴权拒绝）与 `syncbus` |
| ~~B-23~~ | skill 首份 gap map | C2 | gapmap 本地跑 | **已完成 → U-0063 / U-0064 / U-0066**；`skill` 包（executor 的程序不变量、memory_host）17/20 留待 nightly 报告后按实质筛 |

## 5. 单元日志

| 编号 | 日期 | 目标 | 缺陷类 | 发现 | 测试 | 回退验证 | 定位文档 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| U-0001 | 2026-09-04 | roost-kit `.github/workflows/ci.yml` | C4 | 1：基准步骤仍写 `./sync`，包已在 v1.10.0 改名 `room`，该步每次失败而 `go test ./...` 绿 | `TestCIWorkflowPackagePathsExist`（kit 根） | 修复前运行为红（`ci.yml references ./sync`），修复后绿；基准命令本地按 CI 原样跑通 | T-08 |
| U-0035 | 2026-09-06 | roost-codegen `internal/nest` 解析器（remote tag / 接收者） | C2 | 八条回退：三条已有测试红；"重复 alias"两处检查互为冗余（任去一处仍红）；缺快照类型、未知 `k=v` 选项、重复快照类型、同文件混用接收者四条全绿 | `promises_test.go` 四条 | 四处回退变红；包绿 | — |
| U-0067 | 2026-09-06 | roost-core `statesync` 增量帧编解码（B-22） | C2 | nightly `statesync` 5/5；线格式两侧的结构与尺寸规则整段无测试；**方法坑五**：`if _, ok := m[k]; ok {` 这类带初始化语句的守卫，`(cond) && false` 整体中和会编译失败、`grep "--- FAIL"` 计 0 被误读为绿——必须 `if init; (cond) && false` | `codec_promises_test.go` 两条 | 九处守卫回退各红 | — |
| U-0066 | 2026-09-06 | roost-skill `combatcomponent` 资源命令 / 费用支付 | C2 | 本地 gap map 18/20；负扣减、超池、负结果、溢出是"凭空造资源"级别的守卫 | `resource_promises_test.go` 两条 | 四处守卫回退各红 | — |
| U-0065 | 2026-09-06 | roost-core `entity` `RemoteCommit.Validate`（B-22 首项） | C2 | nightly gap map `entity` 5/5；远端提交校验是不变量宿主却整段无测试；快照键有效性依赖实体 id 编码 kind——夹具首版用裸 id 被"invalid snapshot"拒绝 | `remote_commit_promises_test.go` 一条（25 变异） | 七处守卫回退各红 | — |
| U-0064 | 2026-09-06 | roost-skill `skillsync` 应用器准入 / 记录规则 | C2 | 本地 gap map 17/20 无覆盖；准入规则是复制体视图不被污染的最后一道门 | `applier_promises_test.go` 三条 | 九处守卫回退各红 | — |
| U-0063 | 2026-09-06 | roost-skill `skillcompose` 合同构建 / 校验 | C2 | 本地 gap map 19/20 无覆盖；`ValidateContract` 的每条规则都藏在"摘要不符"后面——变异后必须重算摘要才测得到；builder 的"重复来源"被 validate 的同类检查掩盖（冗余） | `contract_promises_test.go` 两条（9 + 20 个变异） | 十处回退九处红、一处冗余 | — |
| U-0062 | 2026-09-06 | roost-core `nest` 锁组迁移请求守卫（B-20 收尾） | C2 | 组 0、未知实体、迁移在途的第二次请求三条无测试 | `group_transition_promises_test.go` 一条 | 三处守卫回退各红（两处同文本 `groupID == 0`，按行号回退） | — |
| U-0061 | 2026-09-06 | roost-kit `redis` 客户端 ctx 截止期（故障矩阵第四切片发现） | C8 | go-redis 默认 `ContextTimeoutEnabled=false`：3s 注入延迟下 500ms 预算的 `Acquire` 等满 2s ReadTimeout；快路径（连接错误）尊重 ctx、慢路径（等回复）不尊重 | `client_deadline_test.go`；`TestToxicRedisLatencyKeepsAcquireWithinItsDeadline`（501ms 返回） | 关掉选项单元测试红、集成测试红（首跑即红） | T-43 |
| U-0060 | 2026-09-06 | roost-service `rank` `decodeEntry` 严格解码 | C2 | 解码失败必须是错误而非零分（零分写回即覆盖真实分数） | `member_promises_test.go` 一条（5 种坏成员） | 字段数守卫回退红 | — |
| U-0059 | 2026-09-06 | roost-service `global` 迁移 / 租约 / 分页守卫 | C2 | 回退 33 条 19 条全绿；**方法坑**：回退脚本按文本替换命中第一处——`ReleaseLease` 的 "incarnation is required" 与 `RenewLease` 同文本，首轮回退的是后者、误判前者无覆盖；按行号回退后红 | `guards_promises_test.go` 两条 | 五处守卫回退各红 | — |
| U-0058 | 2026-09-06 | roost-service `account` 生成的 RPC 胶水（servicerpc 模板，九服务同源） | C2 | 三条模板守卫在九个 `*_rpc_gen.go` 里各自全绿；在 account 钉一次即覆盖模板 | `rpc_glue_promises_test.go` 两条 | 两处守卫回退各红 | — |
| U-0057 | 2026-09-06 | roost-service `match` Commit / Cancel | C2 | 回退 38 条 21 条全绿；"同一票号两次"首版断言文本同时匹配 subject 级规则（回退绿）→ 收紧到 `ticket X appears twice`；subject 级规则经公开 API 不可达 | `commit_promises_test.go` 两条 | 四处守卫回退各红 | — |
| U-0056 | 2026-09-06 | roost-service `session` Run / Resource / EnterRequest 校验 | C2 | 回退 40 条 32 条全绿；"无截止期"与"空幂等键"是资源泄漏 / 重复分配级别的守卫 | `validate_promises_test.go` 两条 | 四处守卫回退各红 | — |
| U-0055 | 2026-09-06 | roost-service `platform` Order / Credential / Verified 校验 | C2 | 回退 39 条 32 条全绿；金额非正（免费送货）与空 secret（被替换实现接受的形状）两条安全守卫无测试 | `validate_promises_test.go` 两条 | 三处守卫回退各红 | — |
| U-0054 | 2026-09-06 | roost-service `mail` Envelope 校验 | C2 | 回退 40 条 32 条全绿；信封校验整段无测试 | `validate_promises_test.go` 一条（12 子用例） | 三处守卫回退各红 | — |
| U-0053 | 2026-09-06 | roost-kit `nestwal` 目录布局 / 帧头损坏检测 | C2 | 复测剩余里的段不连续、起始段 > 1 无确认、帧魔数 / 头 CRC / 长度三字段；魔数与长度被头 CRC 覆盖——测试改字段后必须重算 CRC 才能钉住该规则（首版"魔数"用例回退绿） | `corruption_promises_test.go` 两条 | 五处守卫回退各红 | — |
| U-0052 | 2026-09-06 | roost-core `saga` 引擎 Register / Start / List / Resume / Compensate / Complete | C2 | 复测剩余 15 条中的 12 条实质规则；夹具用不跑协调循环的引擎 + 直接落库的记录 | `engine_promises_test.go` 三条 | 十一处守卫回退各红 | — |
| U-0051 | 2026-09-06 | roost-kit `saga` 消费者配置 / 入站解码 | C2 | 回退 40 条 37 条全绿；首版"缺 id / 异 topic"用例被载荷解码失败掩盖（回退绿）——换成能独立通过的 start 载荷后才真正钉住；超大帧守卫是纵深防御（去掉后 JSON 解码仍拒绝） | `promises_test.go` 四条 | 六处守卫回退各红；一处冗余保留 | — |
| U-0050 | 2026-09-06 | roost-core `saga` 引擎选项 / Command / Completion / 效果编解码 | C2 | 回退 40 条 34 条全绿；`Command.Validate` 是一条 17 子句的 `\|\|`——回退法要按子句做，整条中和没有意义 | `promises_test.go` 四条（11 条选项规则、17 + 8 个子句、编解码拒绝） | 六处回退各红（两处为子句级） | — |
| U-0049 | 2026-09-06 | roost-kit `remoteentity` 写批次生命周期 / 准入 / Mod sid | C2 | 回退 40 条 37 条全绿；二次 finalize、commit 早于 finalize 属于"WAL 已拿到提交后改写"级别；`mongo_committer` 的 tx 复用检查是冗余互掩（已有测试） | `promises_test.go` 三条 | 五处守卫回退各红 | — |
| U-0048 | 2026-09-06 | roost-kit `nestwal` 选项 / 编解码 / Ack 围栏 / 健康阈值 / 检查点解码 | C2 | 回退 40 条 37 条全绿；其中 Ack 越尾拒绝、检查点校验和、durability 校验属于持久路径的正确性守卫 | `promises_test.go` 五条 | 七处守卫回退各红（回退"空目录"守卫时 Open 真在包目录下建了 `"  "` 目录——回退法的副作用，已清理） | — |
| U-0047 | 2026-09-06 | roost-core `nest` Cast / 引擎生命周期 / RollbackTx | C2 | 回退 45 条 44 条全绿；Cast 类型断言失败与 getter 数量不等两条是"处理器拿到零值实体"级别的契约 | `promises_test.go` 四条 | 五处守卫回退各红（类型断言那条以去掉 `ok` 检查的方式回退） | — |
| U-0046 | 2026-09-06 | roost-core `configdata` 定义校验 / auto 表标签规则 | C2 | 回退 45 条 34 条全绿；原 `TestRegisterAutoTableTagMistakesFailAtRegistration` 只断言 `err != nil`（"只要有错就算通过"）；重复 key 无测试 | `promises_test.go` 三条（重复 key 定位、七处定义缺口 + 三处类型不匹配 + 注册 nil、十四条标签规则按文本） | 五处守卫回退各红 | — |
| U-0045 | 2026-09-06 | roost-core `mirror` 信封线协议（订阅 / 发布两侧） | C2 | 回退 14 条 13 条全绿：外层 / 内层 topic、内层版本、未知 op、零 key 等线协议规则只有"伪造内层 key"一条测试 | `promises_test.go` 三条（七种坏消息 store 零写入；外层补齐内层；发布侧四种拒绝） | 四处守卫回退各红 | — |
| U-0044 | 2026-09-06 | roost-core `dataengine` 准入校验（`ValidateMutation` / `ValidateCommitRecord` / `LeaseFence`） | C2 | 脚本回退 27 条守卫 20 条全绿：十二条形状规则、远端提交与头部交叉校验、效果 / 回执 / 围栏回执定位、围栏六字段——只有"版本连续"与"patch 不回落"两条有测试 | `validate_promises_test.go` 四条（30 个子用例） | 五处守卫回退各红（加括号后）；复测 27 条剩 8 条全绿、全是 nil 守卫 | — |
| U-0043 | 2026-09-06 | roost-core `bus` RPC 信封解码 / 生命周期 / 死信能力 | C2 | 脚本回退 32 条守卫 28 条全绿（首轮，含 `\|\|` 条件的假阴性）；实质缺口：信封版本 / 失败无错误体 / 成功无载荷三条线协议规则、module / name 必填、停止后注册、无传输调用、无死信存储 | `promises_test.go` 五条 | 版本检查与必填检查回退红；复测 32 条剩 14 条全绿、全是 nil 守卫 | — |
| U-0042 | 2026-09-06 | roost-kit `dataengine` × NATS 半开（故障矩阵第三切片）+ `nats` 客户端 | 故障矩阵 / C5 | `timeout` toxic 下发布有界失败、恰好一次成立；但客户端从 gossip 学到成员真实端口、重连**绕开代理**（`reconnected url=…:14222`）——没有"只走配置 URL"的开关；另：U-0037 的 `waitFor` 与 integration 文件重名，v1.12.4 的 integration 构建红而未被发现（盯错了工作流） | `TestToxicNATSHalfOpenAckLossIsBoundedAndDeliversExactlyOnce`；`options_discovered_test.go` | 半开测试对真实环境两次通过（7.8s / 12.8s）；不开 `ignore_discovered_servers` 时日志可见绕开 | T-41、T-42 |
| U-0041 | 2026-09-06 | roost-codegen `internal/dao` 生成器层守卫 | C2 | 抽样回退 4 条：redis DAO 未实现 mode / 缺 key / 缺 key 类型、Mongo DAO 未知 dbscope 去掉后全绿（解析层 tag 陷阱早有测试）——两组测试之间那一层 | `gen_promises_test.go` 两条（按错误文本 + DAO 名） | 四处回退各红 | — |
| U-0040 | 2026-09-06 | roost-codegen `internal/webroute` 标记解析 | C2 / C5 | 十二处拒绝只有两条测试（"signature"、"duplicate"）；拼错键被接受后报 `unsupported method ""`，缺键同样含混 | `promises_test.go` 四条（十一种坏标记按文本、raw 路由类型化请求、三种签名缺陷、同目录混包） | 四处守卫回退（两新两旧）对应用例各红 | T-40 |
| U-0039 | 2026-09-06 | roost-codegen `internal/entity` 标记解析 | C5 | 未挂接的 `//roost:entity` 静默消失（0 实体、0 错）；坏键 / 裸词等同于没写；`remote=bogus`、`lifetime=forever`、`sync=ture` 回落默认 | `marker_promises_test.go` 六条（两条未挂接、坏键、坏值、六种合法写法放行） | 三处守卫各自回退 → 对应测试红 | T-39 |
| U-0038 | 2026-09-06 | roost-codegen `internal/eventgen` 处理器扫描 | C5 | `scanFile` 对解析失败 `return nil, nil`（该文件全部 `DealEventXxx` 从分发里消失、事件永不投递、无报错）；处理器引用未声明事件时照常生成 `case *event.EventGhost:`（编译错落在生成文件里） | `handler_promises_test.go` 三条（拒绝坏文件、拒绝未声明事件、放行已声明） | 两处守卫各自回退 → 对应测试红 | T-38 |
| U-0037 | 2026-09-06 | roost-kit `dataengine` outbox 认领循环 / health 行 | C5 / C8 | `run` 里 `_, _ = worker.RunOnce(ctx)`：store 失败只加 `storeFailures`，无日志；health 行只报 `publish_failures`（saga 两侧都报） | `outbox_worker_failures_test.go` 三条（连败只 1 Warn、恢复 1 Info、停机不报 failing）+ `dataEngineHealthMessage` | 改成每次失败都打 → 5 条 Warn 红；去掉 ctx 取消守卫 → 停机测试红 | T-37 |
| U-0036 | 2026-09-06 | roost-kit `nats` JetStream 结算路径 | C5 | `_ = msg.Ack()/Nak()/NakWithDelay()/Term()` 四处丢弃；失败后 broker 重投，但无计数无日志，与"处理器一直失败"不可分 | `jetstream_settle_test.go` 两条（三种 op 各计一次；成功不碰计数器） | `settleErr == nil || true` → 红 | T-36 |
| U-0034 | 2026-09-06 | roost-codegen `internal/attribute`、`internal/cfggen` | C2 | attribute 九条校验回退全部全绿（742 行、1 条测试）；cfggen 八条中 C1"key 必填"与 C2"key 字段未声明"互为掩护（去掉任一条另一条仍红——原测试只要"有错"）、C7 bean 名为关键字无覆盖 | `attribute/validation_test.go` 九条 + 合法样例；`cfggen/validation_messages_test.go` 四条按错误文本断言 | 十二处回退各自变红；两包绿 | — |
| U-0033 | 2026-09-06 | roost-codegen `internal/tablegen` | C6 / C2 | 1 缺陷 + 4 无测试：`unique="true"`、`min=`、主键唯一性从 tag 读出、印进 CSV 规则行，**从未执行**——重复 id 进 JSON、生成的 loader 静默保留最后一行；必填空格、解析错误行列、`-force`、跳过标题/类型/规则行四条行为无测试 | `validateRows`（生成期校验，`ref=` 留给 loader）；`csv_rules_test.go` 七条 | 五处回退各自变红；codegen 全量绿 | T-35 |
| U-0032 | 2026-09-06 | roost-codegen `internal/protocol` 解析器校验 | C2 | 六条结构校验回退全部全绿（重复 struct、未导出类型、重复字段号、req/resp id 相等、枚举首值 0、枚举重复值名）；2.8k 行的包只有 4 条测试。"req id == resp id"是死分支（`RespID` 直接取 id）——观察，不补 | `validation_test.go` 表驱动五条，每条只破坏一处合法定义 | 补后五处回退变红；包绿 | — |
| U-0031 | 2026-09-06 | roost-kit `remoteentity` `MongoCommitter` 删除路径（不变量 ③） | C8 | 0 缺陷：删除提交后旧 fence 的迟到写入被 `ErrRemoteVersionConflict` 拒绝、tombstone 与数据文档都不复现；当前 fence 的写入允许且为新版本（显式重建） | `TestRealDeleteIsNotResurrectedByAStaleFence`（真实副本集，`integration` 标签） | 本地 0.22s 绿；kit CI integration 绿。四个不变量至此各有至少一条真实依赖上的测试 | — |
| U-0030 | 2026-09-06 | roost-codegen `internal/registry`、`internal/errcode`（生成器包首格） | C2 | 五条承诺回退：未知 phase、方法上的标记两条有测试红；**无法解析的源文件静默跳过**（会让聚合少注册）、**返回 error 的注册函数在生成的 `RegisterAll` 里不检查**（编译仍过、失败不可见）、**重复错码不报错** 三条全绿 | `registry/promises_test.go` 两条、`errcode/promises_test.go` 一条 | 补后三处回退变红；两包绿 | — |
| U-0029 | 2026-09-06 | roost-codegen `renderCompose`（启动门禁首跑触发） | C4 | 1：`deploy/dev/docker-compose.yaml` 用 `mongo:27017` 初始化副本集成员，而服务配置在宿主机拨 `127.0.0.1:27017`；驱动发现成员地址后解析 `mongo` 失败 → 任何带 dataengine 的进程在开发机上 `ReplicaSetNoPrimary`。两处字面量一个事实 | `TestDevComposeReplicaSetMemberIsTheAddressTheConfigDials` 把成员地址与配置拨的地址钉在一起 | 门禁首跑：mail 起来（不用 Mongo）、game 停在 ReplicaSetNoPrimary；成员改 `127.0.0.1:27017` 后 released / source-head 两个 full 场景 game 起来；minimum 场景仍红——kit v1.12.0 还带 U-0025 的依赖缺陷，把 kit 下限抬到 v1.12.2 后六格全绿。门禁两跑抓两处，值回票价 | T-32 |
| U-0028 | 2026-09-06 | roost-skill `skill`（首格） | C2 | 三条注释承诺回退：跨 owner 能力枚举拒绝（已有测试红）；`RestoreRuntime` 改走 `NewRuntime` 快进+压缩路径（会删检查点后的事件）全绿；asset cache 等待中的 `Acquire` 去掉预留引用（首个 Release 逐出并卸载）全绿 | `promises_test.go` 两条：压缩型 MemoryHost 检查点后追加事件再恢复；门控 loader 制造"加载中 + 等待 + 首个 Release" | 补后两处回退变红；`-race` 绿。第二、三批再回退四条：cast window 表达式钳入 [min,max]、cooldown 写点记录器、`commit_tick ≤ windup_ticks_min`——均有测试红；**保留上限逐出仍被引用的已完成 cast**（去掉 `castEvictableLocked` 守卫）全绿 → 补 `TestReferencedCompletedCastsSurviveTheRetentionBound`。第四批四条：版本号、presentation 游标过期、pending 任务先挂起三条红；**检查点校验和不匹配必须拒绝恢复**去掉比对全绿 → 补 `TestRestoreRefusesACheckpointWhosePayloadWasTampered`。十一条中四处洞 | — |
| U-0027 | 2026-09-06 | roost-kit `nestwal` `TestWALCloseDrainsAdmittedAppends`（v1.12.2 tag Windows 首跑红） | C2 | 1：测试前置条件"全部 append 已接纳"只等"队列非空"；慢机器上 Close 抢在 31 个 goroutine 到达 `Append` 之前，它们得到的 `ErrClosed` 合法——测的是调度不是排空。且 `Stats.Queued` 在写入协程取走一批后归零，无法表达"全部接纳" | `Stats` 新增 `Admitted`（接纳计数，`Admitted − Appended` = 在途量）；测试等到 `Admitted == 32` 再 Close，否则 Fatal 说明前置条件不成立 | 用 `Queued < total` 作前置条件 5s 内从未满足（证明旧统计不可用）；改 `Admitted` 后 `-count=20 -race` 绿；tag 重跑绿 | — |
| U-0026 | 2026-09-06 | roost-codegen `add lifecycle`（game 模板 World + Player 触发） | C2 | 1：每个 Entity 的 lifecycle 文件都声明包级 `FromRegistry`，第二个 Entity 起同包重复声明、编译不过。从未有测试在一个工程里加两个 lifecycle | `TestGameTemplateScaffoldsWorldAndPlayer`：真建模板工程，go/parser 扫 lifecycle 包无重复顶层声明，且含 `PlayerFromRegistry` / `WorldFromRegistry` / `EnsureWorld` | 改为 `<Entity>FromRegistry` 后模板工程 build / vet / `generate --check` 通过；文档与 help 同改。已生成工程的文件是业务所有，不会被改写 | — |
| U-0025 | 2026-09-06 | roost-kit `dataengine` / `saga` / `remoteentity` Mod 依赖声明（模板 game 进程首次启动触发） | C4 | 3：`DependsOn` 写了 `mods.ModHealth`（Registry 内建项，非 Mod）与 `mods.ModNatsJetStream`（nats Mod 的 capability，非 Mod）；app 按 Mod 名解析 → `unknown mod dependency "health"`。**默认生成的工程（mods 含 nest → dataengine）一个都起不来**；kit 自己的集成测试手工 Init/Provide、不经 app 排序，所以从未发现。与 U-0024 同一类：capability 名当 Mod 名 | kit 根目录 `TestEveryModDependencyNamesAKitMod`：构造全部 14 个 kit Mod，依赖名 ⊆ Mod 名集合 | 回退（stash 三处修复）测试报 5 条；修后绿；kit 全量绿。模板 game 进程对真实 Mongo 副本集 + NATS 集群 + Redis 连续两次启动 `service init` 通过、进程存活（本地环境需清空 JetStream store，否则旧 stream 预留容量触发 "insufficient storage"——环境问题，非缺陷） | T-31 |
| U-0024 | 2026-09-05 | roost-codegen `internal/servicerpc` 模板（game 模板首次启动暴露） | C4 | 1：生成的 `ClientMod.DependsOn` 返回 `mods.ModBus`——总线 **capability** 名，而 app 按 Mod **名字**解析依赖，没有 Mod 叫 `bus`。任何进程把 `NewClientMod()` 与 nats Mod 装配在一起都在启动时 `unknown mod dependency "bus"`；roost-service 八个客户端全部如此。service 的集成测试手工 Init/Provide、不经 app 排序，所以从未发现 | `TestTheGeneratedClientDependsOnTheModThatPublishesTheBus`（codegen）；`TestEveryClientModDependsOnTheNATSMod`（service，九个客户端——手工重生成漏了 `global/activity`，`tool` 指令对齐 v1.13.5 后 `go generate` 补上） | 修复前模板 game 进程启动即退；改为 `mods.ModNats`、重生成 service 八包后 game 进程带四个 ClientMod 起来并存活，mail / match / chat / account 四个托管子命令各自注册 handler（本地 Redis + NATS）| T-30 |
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

### U-0036 / U-0037 扫描记录（2026-09-06，service + kit 三类脚本扫描）

- **C5（errcheck `-blank -ignoretests`）**：service 零条。kit 60 余处 `_ =`，逐条判读：`nestwal/codec.go` 的 `binary.Write` 写 `bytes.Buffer`（不可能失败）、`recover()`、关停路径的 `Close/Stop/Shutdown`、`SetDeadline` 复位、`remoteentity` 的 `unlockObserved`（内部已 `recordReleaseFailure` 计数）、错误路径上仅用于改善报错文本的 `refreshMarked`、有注释说明的 `RenewRemoteSnapshotInterest`、`entity_delete.go` 的 `Abort/Indeterminate`（之后紧跟 `runtime.fail` / `closeBatch` 上报）——都是**有意为之且有观测**。真洞两处：`nats/jetstream.go` 四处结算（U-0036）、`dataengine/outbox_worker.go` 的 `RunOnce`（U-0037）。存疑一处未动：`saga` 的 `commandDigest` / 完成摘要用 `json.Marshal(c)` 丢错——`Command` 是纯值字段，Marshal 不会失败，但若将来加了 `any` 字段会让全部命令摘要相同、去重误判；记入待开 C5。
- **C7（`Lock()` 后无紧随 `defer Unlock()`）**：service 零处非 defer 锁。kit 160 余处，全部在同函数内配对释放（多为条带锁批量加锁 + 一个 defer 逆序解锁、或返回解锁闭包）；无真洞。
- **C1（持锁区域内的 ctx 调用）**：service 零处。kit 两处命中：`room_broadcast.go` 条带 flush 锁内 `coordinator.DistributeBatch(ctx)`——进程内协调器、按设计串行化每条带的帧；`ownership.go` 每实体 `ownershipMu` 内做分布式锁释放——每实体互斥、就是写路径的设计。均记"看过、无问题"。
- **core / skill 同套扫描（同日）**：errcheck——core 26 处、skill 15 处 `_ =`，逐条判读全部有意：关停路径、`bytes.Buffer`/hasher 写入、纯值结构体的 `json.Marshal` 摘要、错误路径的二次清理（skill 五处 `_ = stopProcesses(cast, true)` 都在"停止已报错后强制清场"）、`ensureAmmoRecharge` 里被丢的 `scheduleSystem` 错误不可达（`compile_shape` 已校验 `rechargeTicks > 0`）、reliable bus 的 inbox "failed" 标记失败时 key 仍是 SetNX 写下的 "processing"（去重不受影响）。C7——core 3 处、skill 1 处非 defer 锁全是"批量条带加锁 + 一个 defer 逆序解锁"。C1——core 7 处命中：statesync 每会话 `sendMu` 内发包（保序设计）、jetstream_rpc 启动期 `lifeMu` 内订阅（一次性）、configdata `s.mu` 内 `build/commit(ctx)`（`s.mu` 只有两个 Reload 路径持有，读者走 `s.current.Load()`，锁只串行化重载）。**四仓三类扫描零真洞新增**；矩阵上 core / skill 的 C1 / C5 / C7 列从"未审"改为"09-06 脚本扫"。
- **方法结论**：三类脚本扫描对 service 全清零，说明 service 这一层的 C1 / C5 / C7 可以在矩阵上从"未审"改为"09-06 脚本扫"（12 包）；kit 的真洞集中在"失败→重试"的结算/轮询循环——失败本身被正确处理（重投/重轮询），但**不可见**，这是 C5 在成熟代码里的典型形态。

## 6. 方向二进度：game 模板

**第一切片（2026-09-05，codegen 66b2d19）已落地**：`services.<name>.framework`（account / mail / match / chat 作为独立子命令托管：Server + owner Mod，redis / nats 自动补齐）、`services.<name>.uses`（业务 Service 装配 ClientMod、生成类型化访问器）、`versions.service` 与 `-roost-service-version` / `upgrade -service`、`-template game`。协作者文件 `internal/service/<name>/collaborators.go` 只生成一次、默认全部拒绝。验证：模板工程 build / vet / `generate --check` 通过；对本地 Redis + NATS 五个子命令全部起来（game 进程需要修复 U-0024 后的 roost-service）。framework-compat 的 full 场景已加 `-template game`。

**设计选择与理由**：app 的模型是"一个子命令一个 Service"，因此托管服务是独立进程而不是塞进 game 进程；这与 roost-service `examples/split` 的形状一致，部署产物（compose / k8s / shell）随 `services` 自动覆盖每个托管服务。协作者不给宽容默认（拒绝而非放行）是 roost-service 自己的原则。

**发布（2026-09-06）**：service v1.5.2（重生成的八个传输层 + U-0019～U-0022）→ codegen 清单 service v1.5.2 → codegen v1.13.3：gate / consumer-acceptance / binary-smoke ×3 / publish 全绿；framework-compat 六格与 upgrade-compat 绿。随之把生成器的版本下限抬到 core / kit v1.12.0、skill v1.10.3、service v1.5.2——roost-service v1.5.x 要求 core / kit v1.12.0，钉旧版本的工程一托管框架服务就解析不了；下限从此是"能整体解析的最老组合"。**遗留**：roost-service go.mod 的 `tool` 指令仍指向 codegen v1.12.1（其模板仍生成 `ModBus` 依赖），下一次 service 发布时对齐到 v1.13.3 并用 `go generate` 复核八个包无 diff。

**第二切片（2026-09-06，codegen 360386c）已落地**：`-template game` 补 nest，生成 Player / World 两个 Entity 与 lifecycle、`game/lifecycle/world_singleton.go`（`WorldUniqueID = 1`，`EnsureWorld` = GetOrCreate）与在 `Init` 里确保 World 的 game Service。World 的"单例"是进程属性：一个 game 进程一份，首次创建、之后加载，没有 World 不启动。真启动暴露两个装配级缺陷（U-0025 kit、U-0026 codegen），这正是"模板是后续验证载体"的意义。**教训**：三个"装配级"缺陷（U-0024/25/26）都只有真的 `app.Execute()` 才能发现，单元与手工 Init/Provide 的集成测试全部看不见——下一步应把"生成工程真启动"做成 CI 门禁（见待做 ⑥）。

**第三步（2026-09-06）**：③ 用到框架服务的工程生成 `docs/SERVICES.zh-CN.md`；`project doctor` 新增 `collaborators:<服务>`（协作者文件仍含 fail-closed stub 即失败）、`project next` 在业务链完成后提示；④ service 的 `tool` 指令对齐 codegen v1.13.5，`go generate` 顺带补上漏掉的 activity ClientMod。

## 7. 方向一进度：运行时观察与文档复审

**运行时观察（2026-09-06，kit）**：statslog 本来就每分钟把 goroutine / 堆 / 实体按 category、kind 的数量写进 JSONL，但只在文件里。现在每次采集同步发布 gauge（`runtime.goroutines`、`runtime.heap_alloc_bytes`、`runtime.heap_sys_bytes`、`runtime.sys_bytes`、`runtime.num_gc`、`entity.count`、`entity.count_by_category{category}`、`entity.count_by_kind{kind}`），ops `/metrics` 与 Grafana 直接可见；ops 新增 `GET /statsz` 返回当前一次观察的 JSON（未装配 statslog 时 404 并说明）。**边界**：内存以进程堆为观察量，实体自身占用没有分配追踪无法归属，实体侧给数量——有意的取舍。**Grafana**（2026-09-06）：总览加"进程运行时"一行六个面板。

**文档细节复审 第一轮（2026-09-06，core `docs/`）**：脚本化——抽出 13 篇文档里所有反引号标识符（Go 名、路径、配置键），对五仓源码索引核对存在性，再人工判读。真漂移 5 处、已修：INTERNALS 的包表用了改名前的 `replica/`、`sync/`、`replication/`（现 `mirror/`、`syncbus/`、`statesync/`）；STATIC_REGISTRATION 把 nest handler 的生成位置写成旧布局 `game/bootstrap/nest.go`（现 `internal/registry/nest_gen.go`）；ROADMAP M8 行引用了改名前的测试名；两篇 2026-09-01/02 的历史文档满是 `cube-*` 路径，加了"历史文档"头注而非逐处改写。误报类型：环境变量名、示例占位（`CHANGE_ME`、`Xxx`）、JSON 字段名、标准库标识符——下一轮把这些加入白名单后扫 kit / codegen / service 的 docs 与 README。

**第二轮（2026-09-06，kit / skill / service / codegen 共 26 篇）**：加白名单后 275 个可疑项，逐个核对源码：绝大多数是生成工程的相对路径、文件名、标记（`//roost:*`）、示例名（`ItemTableFrom` 是 `<表名>TableFrom` 的示例、`BagSender` 是 `New<Handler>Sender` 的示例）。真漂移 1 处、已修：skill 的 visual-sync 指南把发布器写成 `kitroom.PublisherWithOptions`，实际是 `roost-kit/syncstream.NewPublisherWithOptions`（room 包没有它）。历史性提法（`global.ActivityService → activity.Service`、`cube.skill/v2 → roost.skill/v2`）是迁移说明，保留。结论：五仓文档标识符层面的漂移在两轮后基本清零；剩下的复审要靠人读语义，而非脚本。

**第三轮（2026-09-06，指标名）**：脚本化——从五仓所有 `.md`（不含 CHANGELOG / history）抽出反引号里"点分小写"的指标样名字，与源码里 `metrics.IncCounter/SetGauge/AddGauge/Observe*` 的字面量名（88 个，无一处动态拼名）比对；Grafana 看板 38 个 PromQL 指标名全部能对到源码。真漂移 2 处、已修：TROUBLESHOOTING T-06 写的 `entity.total` / `entity.by_category` 从来不是 gauge 名（是 statslog 记录的 JSON 字段），gauge 是 `entity.count` / `entity.count_by_category{category}`；OBSERVABILITY 的指标表把基数丢弃计数器写成 `metrics.series.dropped`，源码是 `obs.series.dropped`（同文档第 106 条的 Prometheus 名 `obs_series_dropped_total` 是对的——同一份文档两处不一致）。附带 C6 扫描：`SetGauge/AddGauge` 常量值 5 处全是合法的开关 / 进出计数（`manager.started` 停止置 0、`robot.loadtest.active` 1/0），无常量指标；没有只在测试里出现的指标名。

**发布（2026-09-06 第四轮）**：kit v1.12.6（Redis 客户端 ctx 截止期 U-0061、`nats.ignore_discovered_servers`、两个故障切片、gap map 工具）→ codegen 清单 kit v1.12.6 → codegen v1.13.11。tag 工作流按名核对：kit ci ✓、codegen ci ✓、codegen framework-release ✓。

**发布（2026-09-06 第三轮）**：kit v1.12.4 → **ci 红而当时没发现**：U-0037 的测试辅助函数 `waitFor` 与 `//go:build integration` 文件里的同名函数重复，只有带 tag 的构建能看见；本地 `go test` / pretag 不带 tag 全绿，而盯 CI 时按"main 上最新一次运行"取到的是 `codeql` 工作流、不是 `ci`（两个工作流都由 push 触发，`gh run list --limit 1` 取到哪个看时序）。修复：改名、pretag 加 `go vet -tags integration ./...`、**看 CI 一律 `--workflow ci`**。v1.12.4 不改写，补丁 v1.12.5，codegen 清单跟进（T-41）。codegen v1.13.9 的 ci / framework-release 按工作流名核对，均绿。

**发布（2026-09-06 第二轮）**：kit v1.12.3（gauge / `/statsz` / `Stats.Admitted`）、service v1.5.3 → **tag CI 红**：`tool` 指令升级后 go.sum 残留两行未 tidy，本地 pretag 不查这一项 → 补 tidy、五仓 pretag 全部加"tidy 校验"、service v1.5.4；codegen 清单 kit v1.12.3 / service v1.5.4 → codegen v1.13.6、v1.13.7。教训记入 T-33。

**本轮小结（2026-09-06 第三轮，"脚本扫描 + 生成器收口"）**：换方法——对 service / kit / core / skill 四仓做三类脚本扫描（errcheck `-blank`、非 defer 锁、持锁 ctx 调用），四仓合计约 100 处 `_ =`、170 处非 defer 锁、9 处持锁 ctx 调用，逐条判读后**运行时真洞两处**（U-0036 JetStream 结算失败不可见、U-0037 outbox 认领循环失败不可见），其余都是有意且有观测的丢弃或"批量条带加锁 + defer 逆序解锁"。经验：成熟代码里的 C5 不再是"吞掉后走错分支"，而是"失败被正确处理（重投 / 重轮询）但**不可见**"——修法是计数 + 只打转折日志，而不是每次失败一行。生成器侧收口：eventgen（U-0038）、entity（U-0039）、webroute（U-0040）三个解析器都存在"看得见的问题不出声"（跳过坏文件、丢掉未挂接标记、接受拼错的键），dao 生成器层四处守卫无测试（U-0041）；至此 codegen 16 个包全部至少过了一遍承诺回退法。文档复审第三轮（指标名）修 2 处。发布：kit v1.12.4、codegen v1.13.8（含 tablegen 执行约束）、v1.13.9（含四个生成器单元）。C3 包级可变状态扫描：service 2 处、kit 1 处，全是只读查表。

**本轮小结（2026-09-06 第四轮，"gap map 收口"）**：按第 9 节的地图开了七个 C2 单元——core `mirror`（U-0045）、`configdata`（U-0046）、`nest`（U-0047）、`saga`（U-0050），kit `nestwal`（U-0048）、`remoteentity`（U-0049）、`saga`（U-0051）——每个都是"回退采样 → 按错误文本写表驱动测试 → 逐守卫回退验证"。复测：七个包的无覆盖守卫合计 236 → 141，其中 configdata 34→9、saga 34→15、mirror 13→4 基本收口；nest / nestwal / remoteentity / kit saga 剩余的一半以上是 nil 守卫。**三个方法教训**：① `a || b` 条件回退必须整体加括号，否则假阴性（首轮 bus 误判）；② 巨型单条件（`Command.Validate` 17 个子句）要按子句回退，整条中和没有意义；③ 夹具必须让"只有被测规则能拒绝"——kit saga 的"缺 id / 异 topic"首版用 `{}` 载荷，被载荷解码失败掩盖、回退绿，换成能独立通过的 start 载荷才真正钉住；同类还有 remoteentity `mongo_committer` 的 tx 复用检查（被前置同类检查掩盖，属冗余互掩而非缺口）。回退法的副作用：回退"空目录"守卫时 nestwal 的 Open 真在包目录下建了 `"  "`，复测脚本每次跑完要 `git status` 核对。

**本轮小结（2026-09-06 第五轮，"B-20 / B-21 收口"）**：九个 C2 单元——core `saga` 引擎状态机（U-0052）、kit `nestwal` 损坏检测（U-0053）、service `mail`（U-0054）、`platform`（U-0055）、`session`（U-0056）、`match`（U-0057）、生成 RPC 胶水（U-0058，在 account 钉一次覆盖九服务同一模板）、`global`（U-0059）、`rank`（U-0060）。回退采样脚本首次跑完 service 12 包（第 9 节 service 表）：手工承诺回退过的包仍有 16–32 条无覆盖守卫，说明手工回退每包只看了几条。三处方法坑：`decodeStepCommand` 类的"两条规则同一句错误文本"要收紧断言；帧头字段被 CRC 覆盖时改字段后要**重算 CRC**；人工回退必须按行号（同文本守卫命中第一处）。B-21 全部完成；B-20 只剩 core `nest` group_transition 四条。

**本轮小结（2026-09-06 第六轮，"工具入库 + 故障矩阵抓到第二个运行时缺陷"）**：① gap map 采样器入库（core / kit / service / skill 四仓同一份），`nightly-gapmap` 工作流首跑成功（core 35 包 1.5 分钟，112/146 @max=5），第 9 节的数字此后由 nightly 产出；② 故障矩阵第四切片（Redis 延迟）首跑抓到 **U-0061**：kit Redis 客户端不把 ctx 截止期带到网络上，500ms 预算等满 2s——修复后 501ms；kit v1.12.6 / codegen v1.13.11 发布，tag CI 按名核对均绿；③ 六个 C2 单元：core `nest` group_transition（U-0062，B-20 收尾）、skill `skillcompose`（U-0063）、`skillsync`（U-0064）、`combatcomponent`（U-0066），core `entity` `RemoteCommit.Validate` 二十五条（U-0065，B-22 首项）。方法：变异后**重算摘要 / CRC**（skillcompose、nestwal）与"用生产构造器造夹具"（Redis toxic 夹具改用 kit 自己的客户端构造器，否则夹具的选项和生产不一致，测出来的是夹具）。

## 8. 故障矩阵（toxiproxy）

**第一切片（2026-09-06，kit）**：隔离环境脚本在有 `toxiproxy-server` 时为三个 NATS 节点各起代理（24222–24224，API 18474），导出代理 URL；`heal` 清 toxic。两条集成测试：3s 延迟下提交与投影 2.5s 内完成（提交点是 WAL + Mongo，总线不在同步路径）、延迟清除后效果恰好一次；reset_peer 下提交仍被接纳、outbox 保留、恢复后恰好一次。本地对真实环境两条各 0.7s 通过。`nightly-fault-matrix` 工作流每日 03:00（Asia/Shanghai）以 `ROOST_IT_TOXIPROXY=1` 跑整套。**边界**：Mongo 不代理——副本集发现把驱动引到成员各自地址，代理会被绕开；Mongo 侧的故障仍由 `fault mongo-primary`（进程级）覆盖。**第二切片（2026-09-06）**：隔离 Redis 节点（16379，`--set-proc-title no` 让脚本能按命令行认领自己的进程——没有它 `down` 中途拒绝"外来" pid、留下孤儿 mongod / nats，这是本切片踩到的第一个坑）+ toxiproxy 代理（26379）。两条锁测试：`Release` 回复被吞 → uncertain、拒绝再 Acquire、恢复后值守卫删除收敛且不误删他人；`SETNX` 回复被吞 → 不重试、收敛后 key 已释放。U-0012 的契约第一次由真实丢包驱动。nightly 以 `ROOST_IT_TOXIPROXY=1` 跑通。**下一切片**：NATS `timeout`（半开）对 JetStream 发布确认；Mongo 侧只能进程级；四个不变量对照表——① 成功不早于提交点（NATS 延迟 ✓）、② 不确定即围栏（Redis 丢回复 ✓）、③ 删除防复活（待：remote entity 删除 + 网络重置）、④ 准入即执行（NATS 重置 ✓）——③ 已由 U-0031 在真实 Mongo 上补齐（存储层；网络层的"删除 + 重置"对 Mongo 走不了代理，进程级 `fault mongo-primary` 已覆盖故障转移）。

**第三切片（2026-09-06，NATS 半开）**：`timeout` toxic（timeout=0）黑洞化三个代理的下行——连接不断、字节不回，是与 latency / reset_peer 都不同的失败形态：客户端既拿不到 ack 也拿不到错误。测试钉住三件事：提交 + 投影不受影响（2.5s 内）；outbox 的发布**有界失败**而非永远挂住（`PublishFailures ≥ 1`——来自 ping 超时 → EOF）；恢复后恰好一次（broker 可能已存下未 ack 的发布，靠 `Msg-Id = effect ID` 去重兜住）。**首跑暴露夹具缺陷**：nats.go 从 INFO gossip 学到成员真实端口（14222–14224），重连时绕开代理——之前两条 NATS 测试也是这样"痊愈"的，故障注入的保真度打折。补 kit 层配置 `nats.ignore_discovered_servers`（代理、NAT 部署同样需要），代理夹具自动开启后重连落在 24223（仍是代理）。四个不变量的 NATS 侧现在覆盖 latency / reset / 半开三种形态。**下一切片**：Redis 半开对 `Acquire`（现有两条是"回复被吞"，形态相同，可能只需参数化）；JetStream RPC（bus/jetstream_rpc）在半开下的 call_timeout。

**第四切片（2026-09-06，Redis 延迟）**：`latency` toxic 3s 下带 500ms 预算的 `Acquire` **等了 2.0s** 才返回——go-redis 默认不把 ctx 截止期带到网络上（`ContextTimeoutEnabled=false`），只看 `ReadTimeout`；锁后面的每个处理器跟着停 2s。这是故障矩阵直接抓到的第二个运行时缺陷（第一个是 NATS 客户端绕开代理）。修复后 501ms 返回，原有两条丢回复测试也从 2.0s 缩到 0.7s（它们的 700ms 预算此前同样没被尊重）。Redis 半开（`timeout` toxic）与"回复被吞"同形态，第二切片已覆盖，不另开。**下一切片**：JetStream RPC（bus/jetstream_rpc）在 NATS 半开下的 `call_timeout` 是否同样被尊重——同一类问题在另一条路径上。

**待做切片**：⑤ 发布：service v1.5.2 → codegen v1.13.3 已完成；kit v1.12.2（U-0025，tag CI Windows 首跑红为 U-0027 的测试前置条件问题，重跑绿）→ codegen 清单 kit v1.12.2 → codegen v1.13.4 已打（结果见 CI）；⑥ **启动门禁**（2026-09-06 落地，codegen 4f8db77）：framework-compat 的 full 场景用生成工程自带的 `deploy/dev/docker-compose.yaml` 起 Redis / Mongo 副本集 / NATS，依次真启动 `mail`、`game`，要求到达 `service init` 且存活。首跑即抓到 U-0029（副本集成员地址），第二跑证明 kit 下限必须是 v1.12.2。codegen v1.13.5 带门禁与两处修复发布。

## 9. 脚本化承诺回退（gap map，2026-09-06）

**方法**：`revertsample.py` 对包内每条"`if <cond> {` 且下一行 `return … err`"的守卫，把条件改成 `(<cond>) && false`（**必须加括号**：首轮用 `<cond> && false` 只中和了 `a || b` 的最后一个析取项，把 bus 的重启拒绝误判为无覆盖），跑包测试；仍绿 = 该守卫没有任何测试钉住。每包上限 40–45 条，先到先取，nil 守卫也计入（判读时剔除）。

| 包 | 采样 | 无覆盖 | 判读 |
| --- | --- | --- | --- |
| core `bus` | 32 | 28 → **14**（U-0043 后） | 剩余全是 nil 守卫 |
| core `dataengine` | 27 | 20 → **8**（U-0044 后） | 剩余全是 nil / loader 空资源守卫 |
| core `configdata` | 45 | 34 → **9**（U-0046 后复测） | 剩余：panic 转错误、空数据目录、一处并列丢弃分支——低价值 |
| core `nest` | 45 | 44 → **30**（U-0047 后复测） | 剩余多为 nil 守卫与 CastTwo / Three 的第二、三位；实质：group_transition 四条、`CheckContainAllLock` 死锁风险、participant 可比较（B-20） |
| core `saga` | 40 | 34 → **15**（U-0050 后复测）→ U-0052 又钉 11 条 | 剩余应只有 nil / 存储错误透传 |
| core `mirror` | 14 | 13 → **4**（U-0045 后复测） | 剩余：`PublishDelete` 零 key、发布侧 op（两处 nil 守卫） |
| core `entitysync` | 9 | 7 | 多为参数守卫 |
| kit `redis` | 25 | 20 | 含配置 / nil 守卫（B-19） |
| kit `nestwal` | 40 | 37 → **23**（U-0048 后复测）→ U-0053 又钉 5 条 | 剩余：committer 重试区间、短写、nil 守卫 |
| kit `remoteentity` | 40 | 37 → **31**（U-0049 后复测） | 剩余多为 nil / 配置守卫与 ownership 的实体种类校验 |
| kit `saga` | 40 | 37 → **29**（U-0051 后复测） | 剩余：completion 消费者配置（与 nest-start 同形）、step inbox claim 状态、L2 快照 CAS 响应 |
| kit `room` | 40（实跑 10） | 9 | 前 9 条全绿、多为 subjectID / roomID 为零的参数守卫；第 10 条（`room_broadcast.go:478` 的 ctx 取消 → 返回）去掉后整包测试**挂起** >600s——守卫是循环退出条件，算被覆盖；脚本首版遇超时中止整包，现改为记 HANG 继续 |

**service（2026-09-06 第五轮补跑）**

| 包 | 采样 | 无覆盖 | 判读 |
| --- | --- | --- | --- |
| `mail` | 40 | 32 → **U-0054 钉 12 条** | 剩余：请求参数守卫、"vanished during commit" |
| `platform` | 39 | 32 → **U-0055 钉 14 条** | 剩余：请求参数守卫、admin 的"not recorded" |
| `session` | 40 | 32 → **U-0056 钉 13 条** | 剩余：请求参数守卫、ErrRunMissing 透传 |
| `match` | 38 | 21 → **U-0057 钉 5 条** | 剩余：胶水（U-0058 覆盖）、ErrTicketMissing 透传 |
| `account` | 30 | 19 → **U-0058 钉胶水 3 条** | 剩余：请求参数守卫、"vanished during commit"、非本人角色 |
| `global` | 33 | 19 → **U-0059 钉 7 条** | 剩余：胶水（U-0058 覆盖）、ErrRouteMissing / ErrLeaseMissing 透传 |
| `rank` | 32 | 16 → **U-0060 钉解码** | 剩余：胶水、Redis 脚本返回形状守卫（需要 Redis） |
| `chat` | 40 | 9 | U-0007 / U-0022 时已密 |
| `directory` | 15 | 7 | 多为 nil 守卫 |
| `servicemods` | 7 | 2 | — |

**回退脚本的第四个坑（2026-09-06 第五轮）**：按文本替换只命中第一处。同一包里同文本的守卫（global 的 `RenewLease` / `ReleaseLease` 都写 `if incarnation == ""`）会让回退落在别的函数上、把被测那条误判为无覆盖。人工回退要**按行号**；采样脚本本身是按行号改的，不受影响。

**工具入库（2026-09-06 第六轮）**：采样器进 `roost-core/scripts/gapmap/`（kit / service 同拷贝），`scripts/gapmap.sh` 对每个有测试的包采样并写 `gapmap-report.md`，`nightly-gapmap` 工作流每日跑、报告进 job summary，不阻塞。本节的数字此后以 nightly 报告为准，人工只做判读。

**首份 nightly 报告（2026-09-06，core，`workflow_dispatch max=5`，1.5 分钟跑完 35 个包）**：146 条采样守卫 112 条无覆盖。已开过单元的包立刻可见：configdata 0/5、saga 0/5、mirror 1/5、nest 2/5、dataengine 2/5、bus 3/5；从未碰过的包整片全绿：`entity` 5/5、`statesync` 5/5、`syncbus` 5/5、`security` 5/5、`ownerroute` 5/5、`migration` 5/5、`failurelog` 5/5、`admin` 5/5、`hotcode` 5/5、`robot/*` 5/5 ×3、`etcd` 4/4。这些是 B-22。默认 `max=20` 的夜间跑预计 6 分钟以内。

**回退脚本的第五个坑（2026-09-06 第六轮）**：带初始化语句的守卫（`if _, ok := m[k]; ok {`）不能整体套括号——`if (_, ok := …; ok) && false` 编译不过，`go test` 输出里没有 `--- FAIL`，按行数统计会记成 0 条红、误读为"已覆盖"。人工回退用 `if init; (cond) && false`；采样脚本本身已这么做，且把编译失败记为 `skip` 而不是绿。**统计红行时要同时数 `build failed`。**

**读法**：矩阵里 core 的 454 格"09-02"是当日**通读式**基线，不是回退验证——这张表说明基线包里守卫级的测试缺口普遍在 70–95%。生成器包（codegen）经过 U-0030～U-0041 已收口；运行时包的守卫缺口是下一阶段的主战场，且比生成器更值钱（守卫直接对应不变量 ①～④ 的准入）。

