# 业务模块新增玩法审查（2026-11-12，Asia/Shanghai）

被测 `feat/business-modules`：**64fb2fff5c31be7a167e09cae133ad12a81fc814**；前次基线 `e4627b597113538e19ad0f4bb3ff2fb2427a9a21`，差异 395 文件，+23950/-1474。远端 main 为 `2ec5c57735f3a7ed6aa9384629b606ce7db29ddd`。结束前 ls-remote 确认 feature 没有继续变化。运行日志采用 UTC 2026-11-11，报告日期采用北京时间。

结论：新增一项 P2 地形恢复故障代际问题；前轮 P1 换服源服保护问题仍可复现，另一项 demo 公会检查问题相关代码未改。全部 29 个模块生成工程的 598 个模块/联动场景通过、零跳过，不能据此关闭这三项问题。本轮只提交报告和探针到固定审查分支 `fix/core-game-server-review`。

## RV-20261112-BM-01：并发恢复的旧完成回调可以清掉新的地形投影故障（P2）

位置：[world.go.tmpl:132–135](https://github.com/tjbdwanghaibo/roost-core/blob/64fb2fff5c31be7a167e09cae133ad12a81fc814/modules/terrain/files/game/terrain/world.go.tmpl#L132)。调用链：[Runtime.Refresh:356](https://github.com/tjbdwanghaibo/roost-core/blob/64fb2fff5c31be7a167e09cae133ad12a81fc814/modules/terrain/files/game/terrain/service/runtime.go.tmpl#L356) 读取 `Failures()`，重建投影后调用 `Recovered(generation)`；整个 Refresh 没有按地图串行化。

`MarkFailure` 用计数器递增，`Recovered` 用 CAS 将计数器归零。这个数不是单调代际：归零后，新失败可以再次取得旧数值。因此 CAS 不能兑现注释中的“不吞掉并发新失败”。

可发生的顺序：

1. 建筑投影失败，计数为 1；两个 Refresh 都读取代际 1。
2. 第一个恢复成功，CAS(1, 0)。另一个已经完成旧数据投影，停在最后 Recovered 调用之前。
3. 玩家又提交一次建筑移除，DAO 已成功提交，但隐藏建筑的投影失败；计数从 0 再次变成 1。
4. 旧 Refresh 执行 CAS(1, 0)，清除了它没有修复的新故障。
5. 相同移除请求的幂等重试返回 `ProjectionPending=false`，实际没有重新发布建筑隐藏，持久事实和本地显示仍可能不一致。

确定性探针使用正式生成模块的 Controller、Nest、DAO、AfterCommit 和一次完整 Runtime.Refresh；提交器/同步传输沿用原夹具替身，建筑发布注入错误。探针直接调用 `Recovered(oldGeneration)` 模拟第二个恢复的最后一步，**没有声称运行了两个完整 Refresh 的并发端到端实验**。这一步与生产调用使用相同公开方法，源码确认没有恢复互斥或单调代际保护。

三轮实验均失败，三轮控制均通过，race 未报数据竞争：

```text
control:         old=1 new=1 after=1 duplicate=true pending=true
late_completion: old=1 new=1 after=0 duplicate=true pending=false
```

修复建议：使用不归零的故障序号和独立已恢复水位，保证旧恢复不能覆盖更新的故障；或按地图串行化整个 Refresh，并继续保留对期间新失败的保护。补测“双恢复 + 中间新失败”和“单恢复期间新增失败”。修复不应重放已经成功提交的业务编辑。

## 之前问题的状态

| 编号 | 本轮结果 |
| --- | --- |
| RV-20261111-BM-04，P1 | **仍未修复**。在本轮全新 29 模块工程运行原探针，三轮实验失败、三轮控制通过：account 已绑定 1001，但源服重试旧 Commit 得到 Stale 后解除 held/tracked，旧 session Admit 返回 nil。真实 account.Service + MemoryStore，交出/Profile 为原夹具替身。 |
| RV-20261111-BM-05，P2 | **未见修复**。`modules/rehome`、demo 的 `rehome_checks.go.tmpl` 与 `playerowner.go.tmpl` 相对前轮没有差异，仍只在 Prepare 检查公会。此项本轮只做源码复核，没有重新生成 demo 跑动态探针；动态证据仍是前轮基线。 |
| RV-20261111-BM-01/02/03 | 保留前轮在 e4627b59 的修复复验结论，本轮未重复这些原探针，不将历史测试冒充本轮执行。 |

完整触发链、影响及修复建议见 [前轮报告](BUSINESS-MODULES-FOLLOWUP-20261111.md)。BM-04 的新日志在本轮证据目录。

## 本轮检查范围与验证

采用仓内 roost-genre 按需求组合流程。CLI、模块模板、依赖 replace 均来自被测 feature，工程 module 为 `example.com/businessaudit`。未在固定报告分支的旧运行时代码上运行验证。图谱工具本会话不可用，采用提交差异、精确源码、rg 和生成工程实验；不声称图谱覆盖完整。

主要检查了 actor/scene 的移动与提交后位置、场景关闭/排空；monster 的实例和生命代际、停机；combat 的事务快照、恢复校验、后台推进；attribute/equipment 的附加 DAO 与属性联动；dungeon 的 Level 接线与领奖幂等；naming/cdkey 的跨存储恢复；gacha/idle/friends/notice 的请求账本和边界；terrain 的编辑、建筑投影与恢复。另检查 Nest settled 多实体请求与生成器 import 变化。除上述确认问题，检查范围内未确认其他新缺陷；这不是所有并发顺序和所有组合的穷尽证明。

| 验证 | 结果 |
| --- | --- |
| 全部 29 模块正式安装后的 `module verify --json` | 43 个模块/联动分组，共 598 个顶层场景通过，0 skip |
| 该生成工程 build / vet / id check / generate --check | 四项通过 |
| 新模块定向 race，count=1 | 10 包通过；筛选式与包名见证据说明，未将其描述为这些包的全部测试 |
| core Nest、combatcomponent、codegen/internal/nest，race count=1 | 三包通过 |
| 新地形故障探针，race count=3 | 控制组三轮通过，实验组三轮失败，保留为未修复证据 |
| 旧 BM-04 探针在新工程，race count=3 | 控制组三轮通过，实验组三轮失败，保留为未修复证据 |

本轮没有运行全仓全量测试、真实 Redis/Mongo/WAL 多进程恢复、跨机故障、Unity/C# 客户端或容量/长稳验收，也没有重跑两种 recipe 的完整生成专项。生成模块的验证采用原有替身提交器；598 场景不等于生产部署验收。没有执行新压测，没有删除或改动历史压测材料。单进程地图所有权、固定技能/装备规则、有界请求账本等文档已声明的业务范围，没有直接列为框架缺陷。

命令、探针和原始输出：[本轮证据](evidence/business-modules-review-20261112/README.md)。
