# room / Gate 两项修复复验（2026-11-17）

**RV-20261116-01、RV-20261116-02 均已修复，原故障场景复验通过；本轮检查范围内未确认新增缺陷。** 本次只收尾上轮两项问题，不把结论扩大为 v1.53.0 全量审查或生产上线验收。

运行测试的 main 为 `00d6eade0c22deddc409247e231cbfa4f268e3fc`，CLI、模板和运行依赖均来自该检出。收尾 fetch 得到 `adffd62c95b7ce3ced3e0bc984b122662b484bfc`：新增正式发版及生成物最低 core 提升到 v1.53.0；room、Gate、Nest、App、gateway 和 Gate 生成模板未变。本轮没有重跑最低版本下载兼容验收。最初取得的 v1.52.0 `d1a6ab2a7` 尚不含修复，其测试不计入本报告结论。

只向 `fix/core-game-server-review` 提交文档、日志和探针，不修改运行时、不提 issue / PR；固定审核分支上的旧运行时不是被测对象。

## 1. RV-20261116-01：room 提交后的连接登记

修复提交：[`0b2df03ff`](https://github.com/tjbdwanghaibo/roost-core/commit/0b2df03ffb8937d445970402d9d7dade212d8ddf)。原问题与修前证据见 [11 月 16 日报告](MAIN-REVIEW-20261116.md)。

修复合理：Join / Reconnect / Leave / Close 在 handler 中登记提交后回调，成功提交才更新 Presence，不再以控制器是否收到成功回包为准。按 `(roomID, playerID)` 保留最高席位代际；旧 Track 不能覆盖新连接，旧 Leave 只能撤掉对应代际及更旧的登记。Leave 先检查实际席位，避免幂等空操作把客户端任填代际写入墓碑。登记后的存活检查和补扫继续覆盖“连接关闭早于登记”。

复用原审核探针，仅因 v1.52.0 控制器合包，将 `package room` 改成 `package controllers`、文件移到 `game/controllers/`；故障交错和断线断言未放宽。

| 场景 | 本次结果 |
| --- | --- |
| 旧 Join 回包迟到，新 Reconnect 已完成，之后关闭连接 | 三轮 race 通过，新席位被正确置离线 |
| 旧 Leave 回包迟到，同玩家已重新 Join，之后关闭连接 | 三轮 race 通过，新登记保留并能处理断线 |
| strict 提交已进入，取消 Sender 等待，随后提交成功 | 三轮 race 通过，已提交席位仍登记并能处理断线 |
| 对应的顺序执行、正常等待三个对照 | 各三轮 race 通过 |

另外，完整生成工程 `game/controllers`、`game/modules/room`、`game/handler` 三包 race 通过，包括当前正式的 Presence 代际测试。

仍按实现边界理解：Presence 是每进程一个、内存登记；撤销墓碑和关房标记保留 1 分钟。本次覆盖短时确定性交错，没有验证提交后回调暂停超过保留期、进程故障或真实 WAL 恢复。不能把这次通过描述成任意迟到时间下的持久登记保证。

## 2. RV-20261116-02：Gate 自定义服务名

修复提交：[`0f2dd3384`](https://github.com/tjbdwanghaibo/roost-core/commit/0f2dd3384e6c351451248bd9aa86d0c5ab989c43)。原问题与修前证据见 [Gate 续审](MAIN-GATE-REVIEW-20261116.md)。

修复合理：`Options.GateService / GameService` 将传输协议角色映射成 App 实际持锁的服务名，身份检查、发现登记、目标解析保持一致；NATS 协议角色保持原契约。本端锁键与声明服务名不匹配时提前拒绝装配。生成器同时将名称写入 Gate 与 Game Ingress 两端；不是仅绕过 singleton 校验。

验证使用本机私有 NATS 2.12.3 和真实 TCP：

| 验证层 | 本次结果 |
| --- | --- |
| 正式 `TestRealGateHandshakeUsesActualServiceNames` | `gate/game`、`edge/game`、`gate/world`、`edge/world` 四组各三轮 race 通过，零 SKIP |
| 正式 CLI 创建工程、安装模块、执行 `roost add gate edge` | 成功；doctor 的五项 `gate:*` 检查及生成物检查通过 |
| 实际生成的 `NewGateMod` + 正式 GameIngress，原审核网络探针 | `edge/game` 三轮 race 通过，完成握手和 echo 转发；锁检查记录为真实 `edge:11` / `game:22` |
| `wiring/gate` 完整单元测试 | race 通过，包含默认名称、非法名称及装配拒绝检查 |

网络探针的适配有两处：只选择本工程实际声明的 `edge/game`；手工装配 GameIngress 的测试代码设置对应的两个名称，与生成的 `internal/bootstrap/generated.go` 一致。没有将一份固定名称的生成器包装冒充其他名称工程；四种组合由正式 wiring 集成测试覆盖。

身份权威、account 和 discovery 为替身；本轮没有独立重跑真实 etcd 单实例锁及完整 App 进程演练。main 已有的演练日志不冒充本轮执行结果。

## 3. 工程验收与证据

Linux x86_64、Go 1.27.2、`GOWORK=off`、`GOMAXPROCS=4`；生成 module 为 `example.com/mainaudit`，通过本地 replace 钉住被测 main。

`roost module verify --json`：enter 29、login 37、login+enter 6、portal 12、portal+enter 2、room 34，共 **120/120 场景、零 SKIP**；`go build ./...`、`go vet ./...`、`roost id check`、`roost generate --check` 均通过。该检查在插入临时审核探针之前执行。doctor 仍有 Player 未声明自身同步字段的普通 WARN，不是 Gate 失败。

证据及复跑命令见 [本轮证据目录](evidence/fix-verification-20261117/README.md)。未增加压测，未改动既有压测资料；未验跨机、HA、Unity 客户端和生产容量。代码图谱工具本会话不可用，使用精确源码与 `rg` 核对，没有声称完成全仓图谱穷尽审查。
