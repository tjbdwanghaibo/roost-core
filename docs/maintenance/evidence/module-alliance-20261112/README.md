# R10 联盟模块（2026-11-12）

基线 `7e62d6f6`，feat/business-modules，macOS。生成工程 replace 到当前工作树；本批不合 main、不发版。使用与权威边界见 [alliance](../../../../modules/alliance/README.md)。

## 实现

独立联盟服务保持单写者、路由 RPC、成员预约/守卫修复、宣战重试与缩容入口。通用实现从 demo 迁入模块；game 侧独立 AllianceComponent、Mirror、三个跨域 Saga、在线通知与 enter/rehome 联动。demo 仅保留金币 9001 规则、PlayerOwners 启动适配和场景运维。无旧 API、字段兼容，不改 Nest / DataEngine / Sync。

HomeSID 在当前绑定确认后，通过相同入盟批次和旧 SID 比较修正，回滚遵循正式严格 Nest 事务。它是尽力通知提示，不是权威归属；并发往返换服可能暂时落后，下一次对账继续修正。未知 Membership 结果不清展示。

## 定向验证

[验收原始输出](acceptance.log)：`GOWORK=off go test -v -count=1 -run '^(TestModuleAlliance|TestDemoAlliance)' ./codegen/internal/roost`，126.326s。

| 组合 | 场景 |
| --- | --- |
| 最小安装 alliance + bag | 49 + 17 = 66，0 SKIP |
| 加 enter / rehome / team | 158 PASS，0 SKIP；联盟49、背包17、进入14、换服40、队伍27、alliance/enter2、alliance/rehome2、rehome/enter5、team/rehome2 |
| 生成工程检查 | VerifyModules 的 build / vet / ID / generate --check；重复安装无变化 |
| 安装事务 | 故意引入生成失败，新增服务/配置、Saga 定义、共享 DAO 回执及 manifest 与基线一致 |
| race | 联盟 RPC、Nest handler、runtime、Controller、联动包，`-race -count=2` |
| game-demo | 联盟、换服、队伍及真实 PlayerOwners/account 内存模型的步骤准入定向测试通过 |

[demo 生成契约](demo-contract.log)单独通过（25.243s），未运行完整 TestDemo 套件；初次发现 R1 迁移 GM 后遗留的源码断言仍在旧文件查 ID 解析，改为检查正式 PlayerIDs 接入，[初次输出](demo-contract-initial.log)保留。

6 个变异均被场景检出：去掉入盟批次比较、去掉旧 SID 比较、不写新 SID、把未知超时当不存在、允许旧 Mirror 批次修正地址、绕过 rehome 当前绑定。不是依赖编译失败杀变异。组合与 demo 覆盖了先读 Mirror 后装 Team、先装 Team 后装联盟两种装配路径；全模块正反序仍未跑。

[清单/联动检查](catalog.log)与[demo 模板边界检查](demo-scaffold.log)通过。[根包检查](root.log)通过（入暂存区后复验 10.010s）；[文档锚点检查](docs.log)通过。

## 开发中发现并修正

[demo 初次失败](demo-initial.log)：分区场景夹具没给 owners，原检查吞掉配置错误并允许迁服。夹具补合法属主；生产检查对加载错误直接返回，新增非法配置拒绝场景。移除旧联盟禁迁规则，仍保留分区 SceneActor 限制；Team 互斥由模块负责。

[根包初次失败](root-initial.log)：迁移到 modules 后，真实依赖清单还指向 demo 的旧测试名；指标扫描只认 demo 模板。现行脚本和注册清单改为实际新路径/名字，指标扫描覆盖全部 modules 模板；不加豁免，不删指标。引用中的本证据文档随后补齐。

生成阶段还发现并修正了遗漏 RPC run 骨架、旧 demo 私有准入引用、测试重置全局 handler 与生成注册 sync.Once 冲突、BSON 嵌套断言类型以及变异造成未使用 import。最终记录是修正后的完整运行，未把失败/未运行当作通过。

## 尚未覆盖

全模块矩阵、完整 demo、全仓 go test、真实 Mongo/Redis/NATS/etcd、TCP/Robot、双 game sid 的迁服 kill -9 保留交接 §5/§6 最终批次。没有新增生产规模或压测结论；旧联盟性能报告属于其原始版本。

CBM 按 Tier 2 定位安装/启动调用与模板，generation `2026-11-12T06:56:11Z`；113 个代码/模板路径逐一检查，16 个模板局部解析缺口逐段读源码，1 个被排除的开发脚本直接全文核对；模板多为 File 节点，不能据图谱空调用关系声称没有调用。启发式把标准库 Clone/Join 关联到其他包的边已用源码核对，不作为证据。
