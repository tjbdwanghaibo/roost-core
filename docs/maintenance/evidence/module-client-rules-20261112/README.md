# R6 商品表与统一客户端规则（2026-11-12）

基线 `aad91b4a`，feat/business-modules，macOS，正式生成工程以 replace 使用本工作树 core。实现提交 `29fd4f25`；没有改 Nest / DataEngine / Sync 核心、合 main 或发版。

## 当前行为

shop 从配置表读取商品，装载时检查价格/数量、限购类型、道具引用；移除生产代码里的可变 map 与 SetShopGoodsForTest。旧目录不保留兼容分支。规则入口新增 rules 模块，shop/stamina 自动依赖 rules/enter；各功能只提供自己的 PB 数据与登记，统一在一条 Player Nest 消息中读取，解锁后推送。

进入与重连推一次完整 RulesNow，RulesGet 可补取；错误不返回半套规则。业务日仍由 servertime 下发，没有修改登录线。客户端规则不是成交承诺，跨请求可以热更；无主动热更广播、大目录分页或体力逐次购买价格表。用法与边界见 [rules](../../../../modules/rules/README.md)、[shop](../../../../modules/shop/README.md)。

## 验证

- `GOWORK=off go test -count=1 -v -run '^TestModuleClientRulesAndShopTable$|^TestModuleCatalog|^TestModuleWith' ./codegen/internal/roost`：[通过](acceptance.log)。rules+enter 最小 24 条；组合 **104 PASS / 0 SKIP**（rules10、enter14、shop20、stamina25、wallet14、bag17、两组联动各2）。包含真实 Nest + 生成 PB、只读零提交、冷目标拒绝、进入/重连、推送失败后补取、表热更与旧快照独立、默认商品表约束。
- 同一入口：build/vet、ID、生成物漂移、反向重复安装不变；6 个变异检出（取消快照拷贝、重复消息号放行、体力毫秒变秒、下架商品仍导出、显示价格多1、购买忽略新表价格）；随后三个包 `-race -count=2` 通过。
- 影响复验 `GOWORK=off go test -count=1 -v -run '^TestModuleTaskEventsPeriodsAndMutations$' ./codegen/internal/roost`：[通过](task-impact.log)。组合 **232 PASS / 0 SKIP**，R4/R5 原15个变异、两轮 race 通过；增加的17条来自 rules10、两联动4、shop表约束3。
- 根包 [通过](root.log)；shell 语法、diff 空白和文档锚点检查通过（新增文档暂存后 316 文件、0 断链）。全模块正反序新装、全 demo、全仓与真实 TCP/Gate/存储联合验收仍按交接 §2/§6 留在最终批次。当前传输、提交器使用替身，不当成真实网络或持久验收。

## 失败记录与 CBM

首次目录校验拒绝在 with 目录独占协议号声明；协议定义移回模块 files，提供方仍在 with。遵守现有分配约束，没有放宽安装器。另一次目录校验因测试进程启动后又新增模板，发现 embed 与当前磁盘不一致；固定源码快照重跑后通过，原用例未改为跳过。商品表引用使用现有 `ref:"item"` 语法，三个装载拒绝场景检查真实 generated 注册器。

CBM generation `2026-11-12T04:33:34Z`，本批18个代码路径元数据匹配；3个 PB 定义局部缺口均为 ID 占位符，已逐行阅读、实际生成编译补证。模板节点的调用关系不完整，具体链路以当前源码与运行结果为准。
