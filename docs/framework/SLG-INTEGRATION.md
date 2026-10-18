# SLG 框架接入与验收

本手册把 SLG 的主要业务路径映射到 core 的正式接口。验收对象是业务如何接入框架、事务和恢复保证是否成立；建筑数值、兵种、战斗规则、赛季政策和部署容量由游戏定义。主仓库是 core，ssr 仅作业务经验参考。

## 可运行的业务纵切

在仓库根目录使用 go.mod 声明的工具链，运行：

```bash
bash scripts/test-slg-generated.sh
```

脚本打印并保留临时消费者目录。从 `game` 模板开始，经 CLI 添加 Player DAO、Player 持久定时器和 Tile 实体 / DAO；业务只修改 DAO 定义、回调与 handler，组件、访问器和 Sender 都由生成器生成。最后执行 race 行为用例、ID 检查和生成状态检查。

| 场景 | 业务选择 | 检查的框架契约 |
| --- | --- | --- |
| 建造 → 行军 → 抵达 | 一个建造队列，建造扣 10 金，2 秒完成后登记 5 秒行军；目标坐标限定在示例范围内 | 生成 Sender → Nest → DAO 与期限同事务；回调创建的下一阶段仍进入驱动索引 |
| 重放、取消与退款 | 目标建筑等级单调递增；已完成等级的重放不扣款；取消未完成节点只退款一次 | 重复调用不重复改变业务事实，取消的节点不再触发 |
| 独立研究队列 | 研究扣 5 金、3 秒完成，可与建造同时推进；目标等级重放不扣款 | 同一实体多个期限独立到期，不相互覆盖或提前触发 |
| 建造事务失败 | 业务返回错误，整次建造不成立 | 资源、待建节点、节点种子和提交后的驱动索引都回滚，不存在独立内存期限 |
| 离线资源产出 | 一次回调按 Deadline / Now / Interval 补算，最多补 10 期，库存上限 1000 | DAO 序列化重建后只补触发一次，之后从当前业务时间恢复周期；补算公式和上限由业务提供 |
| 两名玩家抢占同一地块 | Tile 在世界锁档，Player 在更高锁档；地块只能有一个拥有者，获胜者支付 1 金 | 多实体 Sender 和锁顺序；并发竞争只有一人获胜，归属与扣款一起提交，拥有者重放不再支付 |

上述数值是测试场景，不是公共框架规则。[业务文件与测试](../../scripts/testdata/slg/)使用正式生成的 DAO、组件、Sender、Nest 和定时器 Host。提交器是替身，离线恢复使用真实 DAO 的序列化 / 水合接口；它证明业务组合行为，文件 WAL fsync、Mongo 和跨进程恢复另由下面的真实依赖入口验收。

定时器回调只改自己已锁实体的 DAO、操作本实体定时器或记录 effect。需要交易、战斗结算等其他实体一起变化时，交给多实体 Nest handler；需要其他服务时，通过事务性 effect 或 Saga，不能在回调持锁期间同步做 I/O。

## 完整接入清单

| SLG 方向 | 接法与证据入口 | 业务仍需定义的内容 |
| --- | --- | --- |
| 登录、角色、断线与归属 | [玩家接入](PLAYER-ACCESS.md)，game-demo 的 account / EnterGame；真实凭据机器人与重连回归 | 渠道认证、区服开放与角色政策 |
| 城建、研究、行军与离线收益 | 上面的纵切；[业务时间与实体定时器](guide/10-time.md#4-按实体的持久定时器) | 队列、速度、消耗、加速、离线补算与库存上限 |
| 地图、领地竞争和兴趣同步 | Tile 竞争纵切；demo 地图 / Scene / terrain；[Block AOI](INTEREST-BLOCK-AOI.md) 与 [Sync](guide/04-sync.md) | 地块所有权、寻路限制、观察权限、战斗迷雾；空间索引不充当持久权威 |
| 联盟、跨实体交易与跨服结算 | demo Remote Guild；[Remote](guide/05-remote-mirror.md)、[Saga](guide/06-saga.md)；送礼的成功和补偿路径 | 联盟权限、名称唯一、成员上限、收件人路由和人工补偿政策 |
| 奖励、邮件、活动、排行与赛季 | demo 正式端点；通关计数与奖励同事务，排行提交累计快照；活动只在首次付款时贡献 | 奖励额度、活动窗口、赛季切换、补发政策；活动尽力贡献失败不自动补报是已接受取舍 |
| 商城、充值与资产到账 | demo platform → 玩家 grant → 账本，真实 Redis 交付回归 | 产品、支付校验、退款与资产规则；重复订单的业务语义 |
| 战斗、技能与配置 | demo Lockstep 机器人、技能目录编译；[技能 / 战斗](guide/08-skill.md)、[配置热更](guide/07-config.md) | SLG 战斗算法、兵种和技能 Host、版本钉住与数值平衡；帧同步和状态同步按实际玩法选择 |
| 持久化、恢复、搬服和运维 | 下面三套真实生成工程脚本；[schema](SCHEMA-MIGRATION.md)、[搬服](PLAYER-MIGRATION.md)、readiness / metrics / admin | 恢复目标、部署容量、监控接收、跨机与生产操作流程 |

业务命名、经济和战斗等规则缺少默认实现不自动构成框架缺陷。合法接口调用下出现丢期限、错误取消、重复提交、状态越权或生成物编译失败，则应在公共实现或 demo 接法中修复。维护者已明确接受的离线实体不被定时器准点唤醒、Remote 定时器不支持等边界，见 [已知限制](../maintenance/KNOWN-LIMITS.md)。

## 真实依赖与恢复验收

先按[隔离环境说明](../../wiring/scripts/integration/README.md)准备独立资源并设置脚本要求的地址变量，再运行：

```bash
bash scripts/test-demo-generated.sh
bash scripts/test-dataengine-generated.sh
bash scripts/test-remote-generated.sh
```

第一套覆盖正式 demo 的 Mongo 发号恢复、schema 迁移、Redis 交付、排行和活动榜、独立 Gate 消费者；第二套经正式 DAO / Entity / Nest 与文件 WAL / Mongo 跑三个进程阶段；第三套覆盖 Remote 的正式提交和 Mirror 接法。D12 新增消费者另有 `TestRealStepsRunNativeAndMongoStepsThroughTheMods` 与 `TestRealSubscribeEffectsDeliversEachEffectOnce`（integration 标签，需要真实 Mongo 和三节点 NATS JetStream）。每条脚本和测试必须检查退出状态、实际执行及 skip，不把编译或未开门变量算作功能通过。

完整 demo 场景用 `go run ./cmd/loadtest -count 4`（在新生成消费者中，依实际地址传 `-endpoint`、`-account-nats`）。它覆盖多条服务链，但不替代上面的建造 / 行军纵切，也不等同于所有 SLG 规则和部署规模均已验证。

本机功能、Cluster、跨机故障和长稳分别给结论；数据中心断电、跨机网络分区、目标规模或客户端引擎缺少实际证据时，继续保留为未验收项。当前执行结果在 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md) 维护，不为每一轮另建报告。
