# P8 / B-8：GM、购买补领与活动的真实联合验收

基线 `b70efc36e541a3dc8a7c6611ddbf5c6c78f6fa32`，分支 `tmp/v147-operations-real`，2026-11-14，macOS arm64 / Go 1.27.3。`GOWORK=off GOFLAGS=-p=2`，私有 mirror-local offset 37500；Mongo 副本集、NATS、etcd 均为本机真实进程。每个测试另起自己的 Redis，使用独占端口、独占持久目录及 `appendonly yes / appendfsync always / save ""`；不重启 mirror 的 Redis，不清共享数据。这是功能验收，没有容量、性能或长稳结论。

两个实际脚本共三条用例，零 SKIP。生成工程完成 ID 检查、`generate --check`、正式应用与 accountctl 编译、夹具 vet；测试驱动带 `go test -race`，服务子进程是普通 `go build`，不宣称全部服务带 race。结果分别见 [GM/purchase 还原绿](gm-purchase-green.log)（两条、41.528s）、[活动还原绿](activity-green.log)（一条、67.264s）。模块默认 verify 场景数没有改变，三条真实入口均追加到 `integration_coverage_test.go`，手动私有依赖档不进 pretag。

## 两份正式工程

GM/purchase 用 `project new planet -module example.com/operations-accept -template game-demo --skip-deps`。活动用另一份工程：

```sh
roost project new planet -module example.com/activity-accept -recipe casual --skip-deps
cd planet
roost module add gm rehome alliance activity_world activity_alliance
```

活动工程实际 18 模块：`activity activity_alliance activity_world alliance bag checkin enter gm login mailbox portal rehome rules servertime shop stamina task wallet`。按 casual 配方的接入说明，安装正式 rehome 后移除配方原先的在途拒绝占位钩子；其余正式 bootstrap、Service、Mod、Nest、生成 DAO、DataEngine 与 RPC 接线不替换。

不能把两份工程当作同一个组合通过：完整 game-demo 的 PlayerDao 已占满 64 个顶层字段，继续安装 activity_world/activity_alliance 会由生成器拒绝第 65 个字段，见 [组合上限原始输出](full-demo-activity-limit.log)。本批没有扩展 uint64 掩码，也没有绕过安装门禁。

## 验收窗口与结果

| 用例 | 真实入口与故障点 | 结果 |
| --- | --- | --- |
| `TestOperationsGMHTTPMailAndRehome` | ops HTTP 拒绝缺 operator；`gm.mail.send` 经独立 mail 进程，玩家 TCP 领取；再由 `gm.player.rehome` 交出真实 DataEngine 实体 | account 最终绑定 1001、Transfer 清空、源 Mongo 文档不存在；跨 sid 重发相同 request_id 得到同一 mail ID，重复领取被拒；钱包始终 55，旧 sid GM 写入被拒 |
| `TestOperationsPurchaseCrashAcrossSIDAndRedisRestart` | 真实 NATS platform callback（开发模拟渠道、正式 HMAC 校验）；TCP Collect 的 strict Sender 成功后、Redis Ack 前停住并 SIGKILL | Redis 同 AOF 目录 SIGKILL 重启后，订单终态与待发快照逐字段不变；冷态玩家换 sid 补领，旧订单不多发；第二笔正常发货，目标再次 SIGKILL 冷启后 `next=acked=Applied=2`、待发为空、钱包 200 |
| `TestOperationsActivityAuthorityMigration` | 普通 Player 经真实账号/TCP 进入、可信 Sender 触发；World/Alliance 经 NATS 激活 pending；共享权威和 Player 分别迁移，再杀目标进程 | 共享初始化 Token 与时间锚点原样迁移，旧 World owner 直接调用被拒；同 WAL 重启后，查询前只读 Mongo 观察 World/Alliance 扫描到 Settling、Player 定时器到 Ended；结算后水位均为 1，旧阶段和已完成 Token 被拒 |

GM 的 `accepted` 与真正完成分别检查；[源服审计与 HandOff 日志](gm-handoff.log) 保留了 ops 受理、命令执行与 DataEngine 排空交出的实际记录。正常交出链验证不替代 rehome 的 Q4 故障窗口。邮件的发送去重与领取防重分别验证，不把 mail 接受投递当作 Player 已到账。

购买的故障屏障只加在一次性生成工程中：`purchase.Collect` 收到正式 strict Sender 成功后，写出本进程 PID / 玩家 / 订单 / 序号，阻塞在原 `queue.Ack` 前。父测试确认 PID 与序号、读 Mongo 已有钱包 100 与 Applied 1、Redis 仍有一笔待发，然后 SIGKILL 并核验 OS 信号。**该窗口已观察到 Mongo 投影，不是仅 WAL 未投影恢复。** 随后先停止依赖该 Redis 的全部服务，再杀 Redis 并沿用原 AOF 目录；重启后比较平台订单与完整 pending hash，目标 sid 的正式进入钩子及 TCP Collect 才做补领。没有只回退 Redis，也不声明这种操作受支持。

活动表仅有三个 60 秒的事件触发定义，一期 `Generation=1 / Cycle=1`。开发协作者通过正式 `activity.Register` 让共享初始化和结算 pending，Player 正常推进；验收驱动是临时 main 注册的私有 Mod，只调用公开 Remote manager、真实 World service 和生成 Sender，不直接写 DAO、所有权或数据库，不给生产模块增加测试入口。World 从 4100 迁到 4101，Alliance 在 3000/3001 之间迁移，Player 从 1000 迁到 1001；三者目标分别 SIGKILL、保留各自原 WAL 重启。Player 在期限前已完成进入且 View 为 Open；此后先观察 Mongo 变化再发 Pulse/View，查询不会掩盖丢失的后台驱动。

活动没有关闭投影器、没有以 Mongo 为空作屏障，也没有控制 checkpoint 的具体时刻；正常投影可以在迁移、杀进程之前或恢复期间完成。这验证原 WAL 与 Mongo 共同保留时的进程恢复及推进，不是 WAL-only 实验。迁移前后、重启扫描后和终态的 Snapshot / owner / epoch 保存在 [before-migration.json](before-migration.json)、[after-migration.json](after-migration.json)、[after-restart-scan.json](after-restart-scan.json)、[ended.json](ended.json)；日志另记录查询前 Mongo 的 phase、generation、cycle、start 和 Player 完成水位。

## 反向验证

| 临时生成物变异 | 预期失败 | 还原 |
| --- | --- | --- |
| `OPERATIONS_REAL_MUTATION=purchase-replay`：去掉 `Sequence <= Applied` 的重放成功分支 | 原 strict 已到账、Ack 尚未执行的订单移到新 sid 后无法完成补领，TCP Code 100042 / `an earlier paid grant is still pending`，见 [红](purchase-replay-red.log) | 重新生成原模板，完整 GM/purchase 两条全绿 |
| `ACTIVITY_REAL_MUTATION=world-driver`：World 扫描回调不再调用正式 `Pulse` | 迁移并重启后的 World 在查询前的 Mongo 观察中一直为 Open（phase 3），见 [红](activity-driver-red.log) | 重新生成原模板，三宿主真实用例全绿 |

变异只在脚本创建的一次性工程中生效；失败必须来自指定业务断言，不接受编译失败、缺依赖或端口冲突冒充红。首次活动夹具误读 Remote Mongo 外层（业务 BSON 实际在 `data` 字段）导致 bootstrap 断言失败；修正读取夹具后通过。这不是生产缺陷，也不作为上表的反向证据。

## 复跑

```sh
export GOWORK=off GOFLAGS=-p=2
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/operations-private
export ROOST_MIRROR_LOCAL_OFFSET=37500
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
OPERATIONS_REAL_OUT=/absolute/evidence/operations bash scripts/test-operations-real.sh
ACTIVITY_REAL_OUT=/absolute/evidence/activity bash scripts/test-activity-real.sh
bash scripts/mirror-local.sh down
```

选择空闲端口并串行运行。GM/purchase 默认应用范围 45500～45850、etcd 57979/57980；活动 46500～46850、etcd 58179/58180。可用各自 `*_REAL_PORT_BASE` / `*_REAL_ETCD_PORT` 改端口；私有 Redis 使用 base+350。输出目录保留生成/ID/漂移检查、完整过程、各服务日志和活动快照；临时工程及用例自有资源正常清理。负向复跑分别加上述 MUTATION；购买可另设 `OPERATIONS_REAL_RUN='^TestOperationsPurchaseCrashAcrossSIDAndRedisRestart$' OPERATIONS_REAL_EXPECT=1`。负向应退出非零，还原时必须去掉 MUTATION。

## 范围与 KNOWN-LIMITS 回写文本

包含真实 account RPC、玩家嵌入 TCP/PB、ops HTTP、NATS、Mongo、文件 WAL、Redis 与 etcd；不含独立 Gate、真实支付渠道、Linux/跨机/断电、Redis HA 或单侧回退、外部奖励副作用、持续联盟并发、长稳与容量。活动仅一期事件触发，不推广到所有日历/循环配置；Player 离线准点推进仍无保证。本批不改生产模块实现、不改存储格式。

供主线程统一回写，本文没有修改 KNOWN-LIMITS：

> **GM 联动真实入口**（2026-11-14）：`scripts/test-operations-real.sh` 已在本机正式 game-demo 验证 ops HTTP→跨进程 mail→TCP 领取，以及 GM rehome 的真实 DataEngine 交出、最终绑定与源档删除。相同邮件键跨 sid 去重、附件不重复到账，旧 sid GM 写入被拒。不是完整 GM 矩阵，rehome Q4 与跨机/长稳边界不变。

> **purchase 恢复**（2026-11-14）：真实 NATS/TCP/Mongo/WAL 下，在 strict 成功、Mongo 已投影、Ack 前 SIGKILL；用例自己的 AOF always Redis 原目录重启后，平台终态和待发快照不变。跨 sid 补领及目标冷启后 next=acked=Applied=2、待发空、钱包200；删除重放处理变异红。不是 WAL-only；真实渠道、HA/断电、积压/容量未验。Redis/Player/platform 不能单侧回退。

> **活动驱动**（2026-11-14）：`scripts/test-activity-real.sh` 已验证真实 Mongo/WAL/NATS 与跨进程迁移，普通 Player 持久定时器、World/Alliance 权威扫描分别在目标 SIGKILL 后恢复。pending Token/锚点迁移不变，查询前 Mongo 已推进，旧 owner/旧完成 Token 被拒；World 去扫描变异红。正常投影未暂停，不是仅 WAL 恢复；长稳/跨机/容量与真实外部奖励未验，离线玩家准点保证不变。活动使用 casual 加模块的独立18模块工程；完整 game-demo PlayerDao 已达64字段，不能再叠装 activity_world/activity_alliance，两个工程的通过不能合称同组合通过。

索引按 Verify / Tier 2 使用项目 `Users-whb-roost-worktrees-v147`、generation `2026-11-14T02:55:37Z`。模板函数搜索未充分覆盖，`.go.txt` 新夹具未被该代际跟踪；已按 coverage 对相关 partial 范围及新增源码回退读取。Remote 转移与 platform callback 做精确 snippet/trace 验证，不把零搜索结果或 scope 无记录缺口当作缺实现或完备结论。

[定向根包守卫](root-check.log) 覆盖真实入口登记、pretag 清单及 Go 格式；两个 shell 语法、两个安装器的 Python 语法通过。变更文档锚点检查 9 文件、0 坏链。未跑整仓慢套件。私有应用、Redis、etcd 和 mirror 依赖在测试结束后停止并核对端口无监听，独立工作树和本报告保留。
