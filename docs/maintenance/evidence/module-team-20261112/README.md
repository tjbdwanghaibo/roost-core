# R7：team 基础验收

基线 `e50465f2`，实现提交 `f5234510`，分支 `feat/business-modules`，2026-11-12。本批完成组队基础与换服联动；匹配、匹配房、turn-pvp 接入以及 demo 队伍迁移未完成，不把 R7 整体标成完成。

## 实现

- 新 `team` 模块：Remote managed 共享 Team 保存权威名单；Player 仅保存引用。建队、接受邀请、离队与解散统一声明 `[Team, Player]`，不把它们拆成两笔提交，不依赖分区场景，不引入 Saga。
- 六个 PB 端点；身份来自会话，sid 来自服务配置，队伍号使用同物理库的持久计数器，不复用。默认 8 人、64 份邀请、60 秒有效期；队长离队交给最小玩家号。解散保留墓碑，其他成员的引用通过权威查询惰性清除。
- rehome 双向检查：队伍引用未清不能换服，换服意图未清不能建队 / 入队。未知结果不擅自清引用。
- 安装器代办现有 remote_entity Mod；未改 Nest / DataEngine / Sync。所有代码仍按 Entity、Controller、handler 的既有组织落点。
- 维护者补充已写入需求：联盟命名 `alliance`；activity 为通用时间阶段框架，排行、结算、奖励按需联动。两者本批未实现。

## 验证

入口：

```sh
GOWORK=off go test ./codegen/internal/roost -run '^TestModuleTeamGenerated$' -count=1 -v -timeout=10m
GOWORK=off go test ./codegen/internal/roost -run 'TestModuleCatalog|TestModuleWith' -count=1
GOWORK=off go test -count=1 .
python3 scripts/check-doc-anchors.py
```

实际结果：最小新工程 team **25 PASS / 0 SKIP**；追加 rehome 后 **67 PASS / 0 SKIP**（team 25、rehome 40、联动 2）。build、vet、ID 与生成物漂移检查通过。移除底座 Remote Mod 后安装模块能补齐；重复反序安装不改文件。8 个变异均被断言捕获；生成工程两轮定向 race 通过。[验收日志](acceptance.log)、[根包](root.log)、[目录/联动](catalog.log)保留结果。

变异覆盖：邀请边界改为严格大于、允许第 9 人、允许第 65 份邀请、队长选最大玩家号、空队伍 ID 复用、允许同时加入第二队、在队伍中放行换服、换服中放行入队。提交拒绝场景另检查双方 DAO 全文不变；提交记录场景确认包含 Remote 与 Player 变化。

开发中验证首先发现并修正：模块错误引用 demo 专属 settings；具体实体指针没有被 Nest 生成器识别为实体参数；测试包级注册被 sync.Once / 清注册表组合破坏；端点夹具遗漏 Entity kind 注册；联动 fixture 在引擎启动后注册 handler；变异定位文本出现两次。这些属于本批接入与夹具调整，最终复跑如上，未改产品核心来绕过检查。

CBM Tier 2：项目 `Users-whb-roost-worktrees-business-modules` 已刷新至 `2026-11-12T05:04:08Z`；21 个本批 Go / 模板路径检查为当前代际。5 个模板有局部解析缺口（ID 占位符、测试复合字面量），已逐段读源码；模板主要为 Module 节点，调用关系以生成文件和实际编译补证。安装器 `moduleCanAddMod` 两条调用路径为前提检查与缺少 Mod 的代办列表。

## 未覆盖

提交器、Remote 外部锁和权威回执使用隔离内存替身；Remote 载荷仍由正式生成的 `BuildRemoteCommitLocked` 构建并经正式 Nest 提交。没有真实 Redis / Mongo / NATS、独立 Gate、跨进程故障恢复或 TCP 端点验收。唯一号的 Mongo 原子计数器沿用 demo 做法，本批没有真实数据库压测。

全模块正反序全新安装、全 demo、全仓以及交接 §6 联合验收按维护者要求留到最终批次。team 已加入完整矩阵入口，本批没有提前执行矩阵。空队伍与已解散墓碑没有自动清理；组队不推送队友视野，后续移交给匹配也尚未接入。
