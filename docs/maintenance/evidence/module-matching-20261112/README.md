# R7 队伍、匹配与匹配房（2026-11-12）

基线 `6e4049bb`，feat/business-modules，macOS；生成工程 replace 到本工作树。没有合 main 或发版。当前使用与边界见 [matchmaking](../../../../modules/matchmaking/README.md)、[matchroom](../../../../modules/matchroom/README.md)、[team](../../../../modules/team/README.md)。

## 完成范围

- 整队作为一个 ticket，同服、同人数的两队成局；各成员的意图与 Team 持有标记同一笔 Nest 事务。取消与成局竞态以托管 match 的终态为准，未知结果保留准入屏障；不会因为历史到期而盲目重新排队。
- MatchID 持久绑定唯一 RoomID，初始化与保留名单同事务；陌生人不能占位，重复移交不重开已关闭房间。Mongo 映射先读再插入，竞争者读取获胜记录，不覆盖其 SID/成员名单。
- turn-pvp 接入排队→成局→查询房号→入房→回合推进。demo 删除重复 Team 实现，保留通过命名事务钩子接入的 cellworld 投影；旧 lockstep 匹配路径属于 R11，未替换。
- Team Controller 生命周期修正已在 `6e4049bb` 提交；本批另修正协议注册早于 module runtime 启动的依赖时序，Controller 请求时才获取运行实例。

## 验证

- [定向生成验收](acceptance.log)：`go test ./codegen/internal/roost -run '^TestModuleTeamGenerated$|^TestModuleMatchroomGenerated$|^TestModuleMatchmakingGenerated$' -count=1 -v -timeout=15m`，125.068s。组合 **121 PASS / 0 SKIP**（team27、matchmaking16、matchroom12、room22、rehome40、两组联动各2）。包括取消/成局竞态、重复/延迟通知、掉线后查询、未知排队结果、组队回滚、换服双向互斥、房号并发与查询重放、投影钩子失败整体回滚。
- 上述入口同时检查 build/vet、生成物/ID、重复安装；Team 原8个变异和匹配房陌生人准入变异检出；相关包两轮 race 通过。
- [turn-pvp](turn-tcp.log)：真实 loopback TCP、Robot 和生成 PB，两个客户端排队进入同房并推进回合；7个配方场景、零跳过，含 race、工程测试/build/vet/ID/漂移，62.406s。Nest 提交器、Remote、match 存储仍为替身；Room 分配使用 mongotest，不是真 Mongo。
- [demo 定向](demo-team.log)：队伍、分区、rehome 场景与全工程 build，58.860s。未运行完整 demo、dev-run/dev-smoke、seamless 跨 Base 演练或完整玩法矩阵。
- [清单检查](catalog.log)与[根包检查](root.log)通过。根包首次因 demo_test.go 未 gofmt 失败，保留[原始输出](root-initial.log)，格式化后通过。

## 开发失败与验证边界

首次 demo 编译暴露移除 TeamController 依赖后的未用 import，以及已有包级 rehome handler 与测试夹具重复登记；已修正 import，夹具只登记缺失的全局 handler，不清空注册表。Room 绑定改为不可变 InsertOne + 竞争读取，避免重复请求增加计数器；不是为 mongotest 放宽生产约束。

CBM generation `2026-11-12T06:02:46Z`，本批候选路径已检查覆盖；11个模板局部解析缺口逐段读源码补证，文档和脚本按排除目录直接核对。模板图谱调用关系不完整，以源码及生成运行结果为准。

真实 Remote、match、Mongo/WAL 与跨进程验收仍属交接 §6。本批不能证明容量或生产恢复能力。Room 自动断线/广播、跨 SID 房间路由、长期历史治理与全组合验收不在本批完成范围；R9 activity、R11 lockstep 按维护者要求待做。
