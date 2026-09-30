# 2026-09-30 Codegen 第三轮：Entity/Nest 修复与生成器入口盘点

## 基线与口径

三仓 fetch：Core `fded47b2` → `5fedc652`，中间三个提交为 Cache 修复/文档及远程测试，没有改本轮生成器；Kit `f0e5b67a`、独立 Codegen `1e028fa4` 不变。实际实现仍在 Core `codegen/`。在隔离工作树修 [RR-20260930-04](../bugfix/RR-20260930-04.md) 与 [RR-20260930-05](../bugfix/RR-20260930-05.md) 后继续 review；新 [RR-06～10](../bug/REVIEW-2026-09-30-codegen-03.md) 均只记录。

Codebase Memory `roost-core` 项目 HEAD 指向当前 Core，但覆盖 generation `2026-09-29T14:50:03Z` 早于本批改码；候选路径和 `codegen/internal`、`codegen/cmd` scope 无记录缺口，相关路径 freshness 均 `metadata_changed`。图谱用于定位 `findEntityDirs`、`generateBootstrapNest`、`GenerateDir`、`scanGameDirTo`、`convertCSVToJSON` 及调用方向；全部物质结论均由当前源码和隔离 CLI/正式测试补证，不把旧图称为新实现的穷尽索引。

## 修复验证

| 问题 | 修前失败 | 修后行为与边界 |
| --- | --- | --- |
| RR-04 Entity | 新正式测试先在原源码失败：wire、companion guard test 留存 | 当前标记集合与可识别生成文件对账；零标记移除两份；同包首实体退休时剩余实体接管 `RegisterEntity`；手写同名文件保留。真实 NewProject/Add/Generate 工程中的暂存差异与提交删除测试通过。 |
| RR-05 Nest | 新正式测试先在原源码失败：wrapper、异步/同步 sender 和 guard test 四份留存 | 当前 marker 集合与可识别文件对账；删最后标记移除四份；`-sender=false` 只退役 sender 三份、保留 wrapper；手写同名文件保留。 |

定向正式测试与 `-race` 均通过；Codegen 全包回归见[证据](evidence/codegen-review-20260930-03/README.md)。正式外部工程的 `go mod tidy` 尝试在本机默认 Go 模块缓存写锁拒绝，未完成消费者编译；本地暂存/提交链独立验证通过。原协议/公开生成 API 消失后的调用方迁移仍需业务版本控制。

## Codegen 入口盘点

这轮按 `roost.generatorsFor` 的实际执行表，把生成器入口逐个归位。下表的“源码入口”表示追到扫描、写入/退役主分支；不代表逐行、分支或正式生产消费者的完成率。旧批证据在[第一轮](REVIEW-2026-09-30-codegen-01.md)、[第二轮](REVIEW-2026-09-30-codegen-02.md)。

| 主链/入口 | 本阶段实际证据 | 当前停点 |
| --- | --- | --- |
| DAO | 孤儿扫除源码与既有 `TestRunSweepsOrphansWhenAllDefinitionsRemoved` 对照 | 深层 DAO 类型/持久格式另按核心域验收 |
| servicerpc、protocol | RR-01/02 修前红、修后 CLI/包回归、暂存提交/漂移检查 | 跨版本消费者与协议窗口 |
| entity、nest | RR-04/05 红绿、同包/发送模式及暂存删除 | 正式外部工程编译、game bootstrap 具体业务消费 |
| registry | 聚合按当前 `//roost:register` 重写，已有空集合和确定性测试 | 上游若保留陈旧生成 marker，registry 会如实聚合；依赖各生成器退役 |
| attribute | 当前源码与隔离 CLI 删除最后标记，RR-06 | 旧生成文件退役未实现 |
| event | 当前定义与 handler 扫描源码、隔离 CLI 删除最后定义，RR-07 | 部分 receiver 删除需单独动态验证；旧 ID 兼容 |
| webroute | `GenerateDir` 当前包集合源码、隔离 CLI 删除最后路由，RR-08 | 正式启动注册结果未动态验证 |
| config/table | tablegen Go 空 registry 路径、CSV→JSON、roost config-data 空输入分支；隔离 CLI 删除最后 meta，RR-09；cfggen 单固定 `cfg_gen.go` 输入/覆盖源码已读 | JSON 退役未实现；cfggen 分组/引用组合、业务数据升级未全验 |
| errcode | `Run` 每轮提取定义并重写固定 CSV，含重复 code 检查；当前 CLI 证实注释里的 Define 被误提取，RR-10 | AST 真实调用提取未实施 |
| roost 编排 | 暂存、输入快照、规划、提交/回滚及生成器顺序已读；本轮新增 Entity 暂存提交测试 | 强杀/磁盘失败、各脚手架命令和升级分支未全面故障注入 |

因此**主生成器入口的这一轮盘点完成，Codegen 整体尚不能标为 review 完成**：新四项 P2 和一项 P3 尚未修复，事件部分退役、业务消费者编译、cfggen 的复杂 schema/升级、roost 项目脚手架/升级和故障恢复仍有有界缺口。下一轮先处理 RR-06～10，再按该表的未验主链继续；不要把测试包全绿当作没有缺陷。

## 新发现与实现认识

四项退役问题的共同点是生成器只知道“当前要写的文件”，却没有计算“此前自己写过而当前不应存在的文件”；`roost` 暂存差异规划不会替下层推断退役。Event 与 Webroute 的聚合会在零输入时直接绕过写入；Tablegen 的 JSON 无 Go 生成头，需明确所有权或 manifest 才能安全删除。Errcode 则是另一类边界：原始文本正则把注释当实际调用。详见[问题和实施方向](../bug/REVIEW-2026-09-30-codegen-03.md)与[机制更新](IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md)。
