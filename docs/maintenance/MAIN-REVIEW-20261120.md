# v1.59.0 和 main 续审 · 2026-11-20

从上次已审的 `9fca5547a3d1304915999ce688d94dd6e94c7fab` 到 `605bdba165b10d4fe591a2b3e55266e4d867e84f`，共 23 个提交（含合并）、83 个文件。远端 `main` 和 `v1.59.0^{commit}` 是同一提交，两者共用本轮测试。上轮两项未结问题均按原场景关闭，本轮新增一项普通图落点错误和一项停机演练误判。[提交范围](https://github.com/tjbdwanghaibo/roost-core/compare/9fca5547a3d1304915999ce688d94dd6e94c7fab...605bdba165b10d4fe591a2b3e55266e4d867e84f) · [上轮报告](MAIN-REVIEW-20261119-04.md)

## 新发现

**RV-20261120-01 · P2 · 换服到账前冷重连丢失合法原点。** 玩家首次进入目标普通图，按配置落在出生点 `(400,410)`。通过正常移动处理器前往可站立的 `(0,0)`，正式离场保存，再从持久 DAO 重建玩家。重新落位前读到已保存场景号 1038、坐标 `(0,0)`，换服仍在等待到账。落位成功，却返回 `(400,410)`。原用例使用 `(430,450)`，同样的离场和冷加载能回到该点。两组各跑三次，结果一致。[探针结果](evidence/main-review-20261120/probes.log)

新入口在已有 `PlacedID` 时复用 `handlerEnterScene`；该函数用 X 和 Y 同为零判断“尚无存档”，覆盖了有效原点。这个判断在上轮基线已有，本版新增的“到账前重试保留已保存位置”也走到了它。验收应覆盖已有合法原点、非零存档和首次无存档三种情况，令前两者恢复存档、首次进入使用出生点。[重连分支](https://github.com/tjbdwanghaibo/roost-core/blob/605bdba165b10d4fe591a2b3e55266e4d867e84f/demo/game/handler/enter_scene.go.tmpl#L56) · [零坐标判断](https://github.com/tjbdwanghaibo/roost-core/blob/605bdba165b10d4fe591a2b3e55266e4d867e84f/demo/game/handler/enter_scene.go.tmpl#L38)

**RV-20261120-02 · P3 · 停机演练可能把提前保存的位置计为排空写入。** 演练先读到旧位置，再停服，要求日志含该玩家的 `ordinary map drain saved resident` 且库中为新位置。排空代码在离场成功后就输出这行；若周期保存已把坐标写入，`MapBasic.Save` 会直接返回成功。两种时序下，演练所见都是 `(500,500) → (620,540)` 且有同一条日志。用生成工程的 Nest 提交器逐笔检查该玩家的坐标字段，排空阶段实际写入次数分别为 **1 和 0**，三轮一致。影响是演练错误归因，玩家在这两组实验中的最终坐标均正确。验收应让提前保存的样本显示“无法证明由排空写入”，让确有排空坐标提交的样本通过。[演练判定](https://github.com/tjbdwanghaibo/roost-core/blob/605bdba165b10d4fe591a2b3e55266e4d867e84f/scripts/test-demo-ordinary-drain.sh#L34) · [日志产生处](https://github.com/tjbdwanghaibo/roost-core/blob/605bdba165b10d4fe591a2b3e55266e4d867e84f/demo/internal/service/game/playerowner.go.tmpl#L864) · [提交次数](evidence/main-review-20261120/probes.log)

## 旧问题复验

| 编号 | v1.59.0 与 main 的实测结果 |
| --- | --- |
| RV-20261119-06 · Mongo 保留名撞库 | 已修。由本版 CLI 生成八个工程，三组 `admin/admin_data`、`local/local_data`、`config/config_data` 的业务库与 SID 1000 库均不同；两个普通短名仍用原名。例如 `local-data_game_1000` 与 `local_data_game_1000`。结果适用于新生成配置，历史已有数据需按原映射处理。[全部句柄](evidence/main-review-20261120/reserved.log) |
| RV-20261119-07 · 换服快照查错分服库 | 已修。对普通短名、保留名、超长名执行脚本原有配置派生和快照查询，两个 SID 各自命中对应服务配置的实际句柄。单独将目标服前缀改为 `target_zone`，查询结果也只改变目标库。验证覆盖实际查询表达式和 Player DAO 选库，Mongo 文档及绑定查询用替身。[四组对照](evidence/main-review-20261120/snapshot.log) |
| RV-20261118-10～13 | 四项继续通过。前导零默认端口配置被拒，规范 Origin 预检 204；Connecting 回调立即断开后取票 0 次；pretag 的范围选择自测通过；越界 IPv4 被生成工程的 account 配置检查拒绝，正常 IPv4 通过。另复跑了 TS 回调抛错与 portal IPv6 的对照。[Origin、pretag](evidence/main-review-20261120/historical.log) · [TS](evidence/main-review-20261120/gate.log) · [portal](evidence/main-review-20261120/portal.log) |

## 其余增量与边界

- **普通图与 AOI。** 开图时 `NewHostedSpace` 用宿主已有的同步 Manager 创建这张图的 `Interest`。普通登录先在 Nest 事务中通过容量检查并落位；随后的 `Scene.Join → Space.EnterRegistered → Interest.Enter` 才登记玩家实体与跟随它的主视野。进入和移动产生待处理的空间变更，普通图使用 `ApplySync`；同步周期结算 AOI、视野和关系视图的贡献后，Manager 才得到具体玩家对实体的订阅。断开会话后仍可保留地图席位。生成工程覆盖了借用 Manager、重新入图、容量、离场、排空和分区接线。[宿主接线](https://github.com/tjbdwanghaibo/roost-core/blob/605bdba165b10d4fe591a2b3e55266e4d867e84f/demo/internal/service/game/scene.go.tmpl#L526) · [进入主视野](https://github.com/tjbdwanghaibo/roost-core/blob/605bdba165b10d4fe591a2b3e55266e4d867e84f/framework/sync/entitysync/policy/interest.go#L458) · [订阅提交](https://github.com/tjbdwanghaibo/roost-core/blob/605bdba165b10d4fe591a2b3e55266e4d867e84f/framework/sync/entitysync/policy/interest.go#L1070)
- **Saga 与投递观测。** 核对了 broker 入流至订阅回调、回调至 worker 开始两段的采样和首投／重投维度，运行了 Saga、DataEngine 和 NATS 包测试，以及赠礼日志关联的两项离线测试。没有重跑百人、二百人压测，本轮数据不能用于判断实际线上排队时间。[包测试](evidence/main-review-20261120/packages.log) · [离线测试](evidence/main-review-20261120/trace.log)
- **验证环境。** CLI、模块模板、TS SDK 和生成项目引用的 core 均来自被测提交。使用 Linux 本机，位置探针经过 Nest 事务、DAO 提交或恢复。未跑真实多服务 SIGTERM 停机重启、七阶段换服、生产 Mongo／NATS／Redis 集群或全仓 pretag。代码图谱工具不可用，本轮按 Git 差异和源文件核对。[复跑方法及日志](evidence/main-review-20261120/README.md)
