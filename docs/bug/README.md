# Roost Review 问题索引

**阶段发布：v1.23.1（d367b893）已推送。**B1～B8确认缺陷含RR-25全部进入此tag；最终验收和仍保留的观察项见[阶段清单](../review/STAGE-v1.23.1-CLOSURE-2026-10-08.md)。下方早期日期叙述保留历史时点。

**2026-10-08 RR-25：**有限投递方案已实施并通过真实NATS/race；本轮B1～B8确认缺陷收口，发布验收见 [阶段清单](../review/STAGE-v1.23.1-CLOSURE-2026-10-08.md)。旧日期状态保留历史含义。

- [RR-20261008-39](RR-20261008-39.md)：Remote 真实依赖验收仍要求已废弃的发布阻塞语义（验收修正，v1.23.1已发布）。

- [RR-20261008-38](RR-20261008-38.md)：新清单仍宣称独立 skill 和 service 版本（已修复，v1.23.1已发布）。

- [RR-20261008-37](RR-20261008-37.md)：普通业务注释被当作生成文件（已修复，v1.23.1已发布）。

- [RR-20261008-36](RR-20261008-36.md)：模板生成失败仍留下半成品目标目录（已修复，v1.23.1已发布）。

- [RR-20261008-35](RR-20261008-35.md)：负日志轮转周期静默关闭轮转（已修复，v1.23.1已发布）。

- [RR-20261008-34](RR-20261008-34.md)：生产管理令牌校验与 doctor 的监听地址规则相反（已修复，v1.23.1已发布）。

- [RR-20261008-33](RR-20261008-33.md)：日志 server_time_ms 混用业务钟与系统钟（已修复，v1.23.1已发布）。

- [RR-20261008-32](RR-20261008-32.md)：投影与 outbox 积压缺少 gauge（已修复，v1.23.1已发布）。

- [RR-20261008-31](RR-20261008-31.md)：访问 statsz 消耗日志计数窗口（已修复，v1.23.1已发布）。

- [RR-20261008-30](RR-20261008-30.md)：管理命令缺少关联审计，内部错误误报为客户端错误（已修复，v1.23.1已发布）。

- [RR-20261008-29](RR-20261008-29.md)：Lua 裁剪死信没有计数（已修复，v1.23.1已发布）。

- [RR-20261008-28](RR-20261008-28.md)：玩家推送连接缺少心跳和每连接限流（已修复，v1.23.1已发布）。

- [RR-20261008-27](RR-20261008-27.md)：玩家 handler panic 无栈且派发失败只打 Debug（已修复，v1.23.1已发布）。

- [RR-20261008-26](RR-20261008-26.md)：TCP doctor 接受零停机预算，生成器冻结 Nest 预算继承（已修复，v1.23.1已发布）。

- [RR-20261008-25](RR-20261008-25.md)：JetStream 退订窗口误 ACK 和消费者重叠（保留上限已修复，v1.23.1已发布）。

- [RR-20261008-24](RR-20261008-24.md)：缓存复制删除忽略版本，旧更新复活删除值（已修复，v1.23.1已发布）。

- [RR-20261008-23](RR-20261008-23.md)：Group 重开会话后丢订阅（已修复，v1.23.1已发布）。

- [RR-20261008-22](RR-20261008-22.md)：entitysync 健康检查没有生产注册（已修复，v1.23.1已发布）。

- [RR-20261008-21：非法存量定时器被静默忽略且永不清理](RR-20261008-21.md)（已修复，v1.23.1已发布）
- [RR-20261008-20：缺省业务时钟和系统时钟混用及偏移精度不一致](RR-20261008-20.md)（已修复，v1.23.1已发布）
- [RR-20261008-19：运维回滚被撤回时漏记指标](RR-20261008-19.md)（已修复，v1.23.1已发布）
- [RR-20261008-18：配置 closed 与 YAML 路径及数值校验失真](RR-20261008-18.md)（已修复，v1.23.1已发布）
- [RR-20261008-17：异步 Nest 自续发链固定旧配置代际](RR-20261008-17.md)（已修复，v1.23.1已发布）
- [RR-20261008-16：配置撤回后 featureflag 仍停留在新视图](RR-20261008-16.md)（已修复，v1.23.1已发布）
- [RR-20261008-15：配置表生成器字段映射与单例边界不一致](RR-20261008-15.md)（已修复，v1.23.1已发布）

- [RR-20261008-14：启动窗口未注册 WriteGate 时业务请求被放行](RR-20261008-14.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-13：玩家转服响应遗漏 owner_sid](RR-20261008-13.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-12：RPC 生成器类型标记与输出包校验不完整](RR-20261008-12.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-11：Session 未准入 Run 无法回收且慢 claim 可接纳过期 Run](RR-20261008-11.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-10：迁移完成与匹配提交无法按原参数重放](RR-20261008-10.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-09：versionstore 重发与竞争预算相乘且 Resume 漏计 CAS](RR-20261008-09.md)（已修复，B3 已验收，v1.23.1已发布）

- [RR-20261008-08：活动坏记录阻塞整组并误用 Create 竞争返回值](RR-20261008-08.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-07：platform 显式关闭后台重试不生效](RR-20261008-07.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-06：directory Release 绕过预约 token 且冲突分类错误](RR-20261008-06.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-05：chat 请求身份与重放及零保留期限不一致](RR-20261008-05.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-04：服务身份与区服状态校验不完整](RR-20261008-04.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-03：缺失邮件信封仍生成永不过期未读](RR-20261008-03.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-02：秒级截止时间接受亚秒时长](RR-20261008-02.md)（已修复，B3 已验收，v1.23.1已发布）
- [RR-20261008-01：session 并发释放 claim 后无法重取](RR-20261008-01.md)（已修复，B3 已验收，v1.23.1已发布）

- [RR-20261007-31：路径搜索预算与并发地形视图失真](RR-20261007-31.md)（已修复，v1.23.1已发布）

- [RR-20261007-30：未改变状态的战斗操作仍标 DAO 脏位](RR-20261007-30.md)（已修复，v1.23.1已发布）

- [RR-20261007-29：战斗 Host 资源与 catalog 验证不一致](RR-20261007-29.md)（已修复，v1.23.1已发布）

- [RR-20261007-28：Buff 层数和永久期限边界失真](RR-20261007-28.md)（已修复，v1.23.1已发布）

- [RR-20261007-27：属性修饰求和回绕且不满足精确聚合承诺](RR-20261007-27.md)（已修复，v1.23.1已发布）

- [RR-20261007-26：战斗回避与治疗边界错误](RR-20261007-26.md)（已修复，v1.23.1已发布）

- [RR-20261007-25：能力禁用覆盖缺少数量上界](RR-20261007-25.md)（已修复，v1.23.1已发布）

- [RR-20261007-24：Host 付费探针与正式 Runtime 请求不一致](RR-20261007-24.md)（已修复，v1.23.1已发布）

- [RR-20261007-23：默认 MemoryHost 声明资源但不能按句柄付费](RR-20261007-23.md)（已修复，v1.23.1已发布）

- [RR-20261007-22：Host 自检将乘法属性归零并遗留探针衍生物](RR-20261007-22.md)（已修复，v1.23.1已发布）

- [RR-20261007-21：carry 解除失败后丢失重试状态](RR-20261007-21.md)（已修复，v1.23.1已发布）

- [RR-20261007-19](RR-20261007-19.md)：持久 store 必须保留年龄；已修复、目标 race 通过，v1.23.1已发布。
- [RR-20261007-20](RR-20261007-20.md)：observer/manifest 生命周期有界；已修复，skillsync 与 sync-e2e race 通过，v1.23.1已发布。

**10-07 B1 第一批：RR-20261007-07～13 已修复，skillsync / syncstream race 通过，待全仓验收，未发布。**

- [RR-20261007-07](RR-20261007-07.md)：可见性时钟泄露与未知 mutation 放行。
- [RR-20261007-08](RR-20261007-08.md)：并发发布计数重复累计。
- [RR-20261007-09](RR-20261007-09.md)：快照后重放旧源事件。
- [RR-20261007-10](RR-20261007-10.md)：outbox 拒收重复追加 History。
- [RR-20261007-11](RR-20261007-11.md)：空闲清理与过龄积压阻断恢复。
- [RR-20261007-12](RR-20261007-12.md)：发布失败后同流越序。
- [RR-20261007-13](RR-20261007-13.md)：observer 关闭早于在飞发布结束。


**10-07 B5 续批：已修复，全仓与生成工程验证通过，未发布。**

- [RR-20261007-04](RR-20261007-04.md)：pipelined 标记生成。
- [RR-20261007-05](RR-20261007-05.md)：事务标记 TTL 校验。
- [RR-20261007-06](RR-20261007-06.md)：App 启动期限与终止信号。

**10-07 B5 第一批：App 配置/Mod 名字与 glsvet 方法 handler（未发布）。**

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261007-01](RR-20261007-01.md) | 配置检查把缺失的默认文件报告为成功（F01-8） | 已修复（`aa12460b`），目标 race 通过；全仓 build/vet/test（130 个包）通过 |
| [RR-20261007-02](RR-20261007-02.md) | 服务 Mod 可以与共享 Mod 重名（F01-9） | 已修复（`aa12460b`），目标 race 通过；全仓 build/vet/test（130 个包）通过 |
| [RR-20261007-03](RR-20261007-03.md) | glsvet 漏查方法 handler 的异步执行（F02-3） | 已修复（`aa12460b`），目标 race 通过；全仓 build/vet/test（130 个包）通过 |

**v1.23.0 已发布（2026-10-07，tag → `28912cd6`，代码冻结点 `5e72ca4d`）**：维护者第十～十三轮决定（含 A2 ③ 写令牌、A3 ② 传输层排空、A4 ① Mod 配置声明、B3 ③ Host 能力表、saga 重开可观测、skill 衍生物分区）、收尾四批、真实进程演练与 RR-20261006-02～40 随本版发布；下方 10-06（v1.22.0 之后）～10-07 各段原写“未发版”处已改为“v1.23.0”。

**v1.22.0 已发布（2026-10-06，tag → `9bf690fb`）**：业务时间只许前进、configdata 键大小写敏感、Mirror 第 6 步本机替代与 RR-20261006-01 随本版发布；下方“未发版”指发布前状态。

**v1.21.0 已发布（2026-10-06，tag → `4881f2b7`）**：维护者第四～九轮决定（B4/B6/B9/B10/C2/C5/C9/D1、MissionRunner 延后队列、求值上下文表、O33～O37、D-L1～D-L3、saga 方向 ①②、Mirror 第 1～5 步、O4）、单元留项修复与发版前审查跟进随本版发布；下方“未发版”指发布前状态。

**v1.20.2 已发布（2026-10-06，tag → `c85d4565`）**：维护者 10-05 第二、三轮决定项（A1～A5、B1、B2、B3、B7、C1、C4、C6、C7）与 v1.20.1 之后的非核心 review 修复（N05、N09 第三 / 四批、N12、N13 含复审、N14、N15、NC-170～174、NC-208 补修等）随本版发布；下方“未发版”指发布前状态。

**10-07 remote entity / bus 一组（分支 `remfix`，`a9bb8923`，框架文档发现 F05-1～F05-7、F09-R2、F09-R3），未发版。** 兴趣广播拖住同条带读、发布失败让 WAL 投影队头阻塞、JetStream RPC 超过 AckWait 重复执行、Assemble 不校验快照段、消息标签与死信桶无上界、轻量 RPC 截到 5s、ClientMod 不读 transport；F05-6 / F05-7 是文本与文档，随同一批修正。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-74](RR-20261006-74.md) | P1 `nats.rpc.transport=jetstream` 的部署里生成的 ClientMod 总走轻量传输，每次调用得 `ErrRPCCapturedByJetStream`（F09-R3） | 已修复（`a9bb8923`），未发版 |
| [RR-20261006-73](RR-20261006-73.md) | P2 轻量 RPC 每次尝试固定 5s，`call_timeout` > 5s 不生效；超时 / 取消不带 ctx 语义（F09-R2） | 已修复（`a9bb8923`），未发版 |
| [RR-20261006-72](RR-20261006-72.md) | P3 异步消息的指标标签与死信列表键取对端未注册的 `(module, msg)`，无上界（F05-5） | 已修复（`a9bb8923`），未发版 |
| [RR-20261006-71](RR-20261006-71.md) | P3 `remoteentity.Assemble` 不校验快照段配置（`SnapshotInterestTTL=0` 被接受）（F05-4） | 已修复（`a9bb8923`），未发版 |
| [RR-20261006-70](RR-20261006-70.md) | P1 JetStream RPC handler 超过 AckWait 被重投、实例间重复执行（真实 NATS 3 次）（F05-3） | 已修复（`a9bb8923`），未发版 |
| [RR-20261006-69](RR-20261006-69.md) | P1 Remote 发布失败让 owner 的 WAL 投影队头阻塞，其后普通 DAO 一起停住（F05-2） | 已修复（`a9bb8923`），未发版 |
| [RR-20261006-68](RR-20261006-68.md) | P2 兴趣续租在条带锁里同步广播，总线一慢同条带 L1 命中也排队（F05-1） | 已修复（`a9bb8923`），未发版 |

**10-07 结构守卫（分支 `guards`，`fe8ef362`，框架文档发现 F11 N1～N3、F00 方向守卫、F12 G7），未发版。** 指标名门禁抓到总览仪表盘查不存在的指标、demo 仪表盘与 README 按不存在的标签聚合；另加三大块依赖方向、生成 game-demo build + vet、pretag `go generate` 漂移三项守卫（[REFACTOR-2026-10-07-structural-guards](../feature/REFACTOR-2026-10-07-structural-guards.md)）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-67](RR-20261006-67.md) | P3 总览仪表盘查不存在的 `entitysync_flush_gate_deferred_total`；demo 仪表盘与 README 按不存在的 `subject` 聚合 `nats_jetstream_terminal_total`、README 的 `bus_rpc_*` 开关写错（F11 N1、N2） | 已修复（`fe8ef362`），未发版 |

**10-07 lockstep 五条（分支 `lockstep`，`4ce70763`，框架文档发现 F04-5～F04-9，维护者选 A），未发版。** 指标名丢前缀致 desync 告警失效、广播预算不扣传输开销致满载整房被拒、慢追帧客户端卡死 `Tick`、2 人房 desync 永不裁决、机器人缺口不够大时不追帧；补端到端门禁 `sync/lockstep/e2e_gate_test.go`（两客户端 + 机器人经 KCP 回环，满载 + desync + 丢包追帧 + 卡住的追帧）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-65](RR-20261006-65.md) | P1 `LockstepBot` 只在 256 帧乱序缓冲溢出时追帧，丢包超过冗余深度但不够溢出时 `Next` 永久停住；追帧被服务端放弃后同一缺口永不重发（F04-9） | 已修复（`4ce70763`），未发版 |
| [RR-20261006-64](RR-20261006-64.md) | P1 2 人房默认 quorum=2，任何哈希分歧都裁不出（2:2、全不同同理），无回调无指标（F04-8） | 已修复（`4ce70763`），未发版 |
| [RR-20261006-63](RR-20261006-63.md) | P2 `Reliable` 直连 KCP / QUIC 且 `Tick` 的 ctx 无期限时，一个对端停住的追帧客户端让 `Tick` 无限阻塞、全房停帧（F04-7） | 已修复（`4ce70763`），未发版 |
| [RR-20261006-62](RR-20261006-62.md) | P1 Room 预算按 1232 字节包长算，没扣传输开销（KCP / QUIC 载荷 1200，UDP 加密 +32）；最坏包 1201..1232 的配置过 `NewRoom`，满载时整房广播被拒（F04-6） | 已修复（`4ce70763`），未发版 |
| [RR-20261006-61](RR-20261006-61.md) | P2 合仓 `c8249526` 让 lockstep 指标丢了 `lockstep.` 前缀，`OBSERVABILITY.md` 的 desync 告警永不触发（F04-5） | 已修复（`4ce70763`），未发版 |

**10-07 代码生成器一批（分支 `genfix`，`63573f33`～`8dd984eb`，框架文档发现 F12 G1～G3、F09-R1、F09-R4 marker 一项），未发版。** 标记键名拼错静默取缺省、`versions.codegen` 下限指向不含生成器的 core、`generate --changed` 在仓库子目录里几乎不跑、生成的 RPC 客户端 affinity 不生效。

**10-07 skill 衍生物“施放 → 移交 → 停止”（分支 `skspawn`，`50a0fe88`，框架文档发现 F08-R R1～R4、F08-H+ H1；维护者要求先补端到端用例再修），未发版。** 施放中的衍生物在移交前不推进、启动步被拒时 Host 侧残留、衍生物回调丢事件链与 EventID 每 tick 相同、proc 第 0 号效果撞号、事件派发出错卡住 Runtime 且付费后删 cast（用例 `skill/spawn_lifecycle_e2e_promises_test.go`）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-66](RR-20261006-66.md) | P3 协调器在别的进程时 `EmitStart` 只按 4 MiB 校验，超过 `saga.max_payload_bytes` 的启动意图随业务事务提交（F06-S7 跨进程，RR-43 后续） | 已修复（`133a4e88`），未发版 |
| [RR-20261006-60](RR-20261006-60.md) | P3 手写 `HandlerMeta{Rollback: state\|undo}` 不写 `Durability` 时零值即 memory，注册照收，要到运行期第一次改持久字段才报错（F02-8，RR-41 后续） | 已修复（`2a8b2e64`），未发版 |
| [RR-20261006-59](RR-20261006-59.md) | P1 生成的 RPC 客户端固定 `CallChecked(ctx, 0, …)`、不带 discovery，match 7 个 / activity 11 个带 affinity 的方法随机路由（F09-R1） | 已修复（`8dd984eb`），未发版 |
| [RR-20261006-58](RR-20261006-58.md) | P2 `generate --changed` 拿相对仓库根、带引号的 porcelain 路径比相对工程根的前缀，工程在子目录时只跑 registry 还报成功（F12 G3） | 已修复（`a461ee2f`），未发版 |
| [RR-20261006-57](RR-20261006-57.md) | P2 `versions.codegen` 下限 v1.15.0（该版 core 无生成器），Makefile 按它跑生成器：钉 v1.15.x 的 make 全失败、钉 v1.16～v1.22 用旧生成器（F12 G2） | 已修复（`410392e0`），未发版；发版时 core 下限升 v1.23.1 |
| [RR-20261006-56](RR-20261006-56.md) | P1 `//roost:nest` / `dao` / `redisdao` / `attribute` / protocol / table / rpc 标记键名拼错被静默忽略取缺省（`durabilty=strict` → async，`dbscop=sid` → 全局库）（F12 G1、F09-R4） | 已修复（`63573f33`），未发版 |
| [RR-20261006-55](RR-20261006-55.md) | P2 被动候选入队失败（Program 不在 Host 能力表里）时 `dispatchEvent` 整个返回错误，cursor 不前进、Runtime 永久卡住；`payCostList` 付费后 drain 失败，已付费的启动被删、费用不退（F08-H+ H1） | 已修复（`50a0fe88`；后续 `b8e39056`：`RootEventLimit` 须大于施法 / 衍生物引用根数上界，表满兜底跳过事件并计数；施放中 Shutdown 跑 cancel 回调；后续二 `e744704d`：排队任务上限计入上界、满了入口拒绝，`RemoveProgram` 跑 cancel 回调），未发版 |
| [RR-20261006-54](RR-20261006-54.md) | P3 proc 施放的事件 ID 与第 0 号效果事件 ID 相同（F08-R R4） | 已修复（`50a0fe88`），未发版 |
| [RR-20261006-53](RR-20261006-53.md) | P2 衍生物回调不继承施法的 RootEventID / ProcDepth，回调效果每 tick、每目标同一 EventID；`max_depth` 管不住衍生物链（F08-R R3） | 已修复（`50a0fe88`），未发版 |
| [RR-20261006-52](RR-20261006-52.md) | P2 运动衍生物启动步 Frame 之后被 Host 拒绝，Host 侧已登记的衍生物无人停止（F08-R R2） | 已修复（`50a0fe88`），未发版 |
| [RR-20261006-51](RR-20261006-51.md) | P2 施放中的衍生物在移交前不推进，移交前的 tick 永久丢失（F08-R R1） | 已修复（`50a0fe88`），未发版 |

**10-07 框架文档发现 F04-1 / F04-2 / F04-15（sync，分支 `syncfix`，`04378f7c`），未发版。** 排队兴趣事实被之后的 Leave / Hide 作废时卡死每次 Flush、永不被接受的 retry pair 让 Drain 永不结束、syncstream 首条 WAL 半截打不开 / 回放不恢复活动时间 / Recover 覆盖 provider 的 schema 版本。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-50](RR-20261006-50.md) | P1 syncstream：(a) 首次 checkpoint 前第一条 WAL 半截，History 永久打不开（`epoch is required`）；(b) 回放不恢复 LastActivity，重启后 SweepIdle 误删活跃流；(c) `Recover` 用请求方 SchemaVersion 覆盖 provider 包的版本（F04-15） | 已修复（`04378f7c`），未发版 |
| [RR-20261006-49](RR-20261006-49.md) | P2 排队模式下永远不会被接受的 retry pair 让 `Drain` 只能等 ctx 超时，kit 停机走不到 Close；Refusal 无日志（F04-2） | 已修复（`04378f7c`），未发版 |
| [RR-20261006-48](RR-20261006-48.md) | P1 `Interest` 排队事实就绪后、应用前同 id 被 `Leave` / `Hide`，之后每次 Flush 报 `policy: unknown interest id`，Manager 下所有 subject 停发，Stop / Drain 一直报错（F04-1） | 已修复（`04378f7c`），未发版 |

**10-07 框架文档发现 F06（saga，分支 `sagafix2`，`63bc1d66`；F06-S5 维护者 2026-10-07 选 A），未发版。** 退避中送达的成功被丢弃、Nest 启动消费者对确定性错误 nak 到 `MaxDeliver`、原生步骤消费者坏信封 nak、坏记录拖累整批、步骤超时与 AckWait 无校验、`Completed` 可被人工补偿；F06-S4 / S6 是文档项，同批改正、不单独编号。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-47](RR-20261006-47.md) | P3 `Engine.Compensate` 不拒绝 `Completed`，已完成的 saga 被带回补偿、逐步撤销，不计 `reopened_total`（F06-S5） | 已修复（`63bc1d66`），未发版 |
| [RR-20261006-46](RR-20261006-46.md) | P3 步骤 `Timeout` 与步骤消费者 `AckWait` 无校验，`Timeout ≥ AckWait` 照常启动、处理中被重投（F06-C1） | 已修复（`63bc1d66`），未发版 |
| [RR-20261006-45](RR-20261006-45.md) | P3 `MongoStore.ClaimDue` 遇一条坏记录返回错误、协调循环整批丢弃；`List` 整次失败（F06-S3） | 已修复（`63bc1d66`），未发版 |
| [RR-20261006-44](RR-20261006-44.md) | P3 原生步骤消费者对坏信封 nak 到 `MaxDeliver`（约 8.7 天），与其余四个消费者及 SAGA.md 不一致（F06-S2） | 已修复（`63bc1d66`），未发版 |
| [RR-20261006-43](RR-20261006-43.md) | P2 Nest 启动消费者对 `ErrInvalidRecord`（Data 超过 `MaxPayloadBytes`）/ `ErrIdentityConflict` nak 到 `MaxDeliver`，启动意图静默丢失；`EmitStart` 按 4 MiB 校验（F06-S7） | 已修复（`63bc1d66`），未发版 |
| [RR-20261006-42](RR-20261006-42.md) | P2 重试退避期间送达的成功按 `ErrNotWaiting` Term；之后截止 / 人工 Compensate / 定义缺失关闭操作，这一步已生效却不计入、不补偿、不告警（F06-S1） | 已修复（`63bc1d66`），未发版 |

**10-07 框架文档发现 F02-1 / F03-1（分支 `memtx`，`c2616106`，维护者 2026-10-07 选 A），未发版。** `durability=memory` 的事务改持久字段时静默丢失，改为提交点整笔失败回滚、点名实体与字段（[修复](../bugfix/RR-20261006-41.md)）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-41](RR-20261006-41.md) | P2 `rollback=state` / `undo` 加 `durability=memory` 的 handler 改持久字段：`durableCommit` 的 memory 分支直接返回 nil，回复成功、committer 与 `PrepareMutation` 0 次，重载后回到旧值（带 Remote 批次的 memory handler 改本地实体、`AddMutation` / receipt 同根） | 已修复、声明场景验证，未发版 |

**10-07 B3 ③ / A4 ① 收尾（分支 `gaps`，v1.23.0 发版前维护者要求“交给 review 前不留能绕过检查的分支和 WARN”），v1.23.0。** B3 ③ 的能力准入对不声明能力表的 Host 跳过（[方案 §12](../feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md)）；A4 ① 留下 game-demo 业务键无声明的 doctor WARN 与生成工程无守卫（[方案 §7](../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-40](RR-20261006-40.md) | P3 game-demo 业务代码直接读 viper，activity.* / platform.* 没有任何 game 的声明（doctor 对新生成工程报 WARN），生成工程不在“读了没声明 / 声明了没读”守卫范围内 | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-39](RR-20261006-39.md) | P3 Host 不实现 `HostCapabilityProvider` 时 Runtime 跳过能力准入：`RecordingHost` 包着如实声明没有召唤物的 Host，summon 照样执行到施法中途、扣费之后才失败 | 已修复、声明场景验证，v1.23.0 |

**10-07 A4 ① 每个 Mod 声明自己的配置（分支 `a4s`，维护者第十三轮“原下个大版本项本版完成”），v1.23.0。** 配置键、类型、缺省值、范围 / 枚举、必填写在 Mod 的配置结构体上，App 启动前、生成器、doctor 共用（[方案](../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)）；生成器的配置段接上声明后发现 RR-20261006-38（P3），已修复、声明场景验证。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-38](RR-20261006-38.md) | P3 生成的 `shutdown` 段写着 `serve_wait_timeout`，没有任何代码读它（生成器的配置段是手写字符串，与运行时读取没有对照） | 已修复、声明场景验证，v1.23.0 |

**10-07 B3 ③ Host 取值能力表（分支 `b3cap`，维护者第十三轮“原下个大版本项本版完成”），v1.23.0。** Host 能读 / 支持的取值做成能力表随编译环境下发、算进 authority digest，编译器、Runtime、Host 共用（[方案](../feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md)）。守卫第一次运行时发现 RR-20261006-37（P3），已修复、声明场景验证，v1.23.0。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-37](RR-20261006-37.md) | P3 motion `enabled_slots` 只在定义写了 steering / offsets 时才查，Runtime 却对每个运动衍生物每步都发这两种步骤：关掉槽位的环境照样编译，施法扣费之后 Host 拒绝 | 已修复、声明场景验证，v1.23.0 |

**10-07 A3 ② 排空下沉到传输层（分支 `a3d`），v1.23.0。** `ISyncBus` 的退订带 ctx 排空（`syncbus.Subscription.Unsubscribe`），[方案](../feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md)。穷举订阅方时发现 RR-20261006-36（P3），已修复、声明场景验证，v1.23.0。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-36](RR-20261006-36.md) | P3 `syncbus.PatchSyncer.Stop`、`cache.ReplicaSyncer.Stop`、`syncstream` 的退订函数返回时 `Apply` / `Store` / handler 仍可能在跑（传输的退订不等在途回调，这三处没有自己补等待） | 已修复、声明场景验证，v1.23.0 |

**10-07 A2 ③ versionstore 一次性写令牌（分支 `a2t`，维护者第十三轮“原下个大版本项本版完成”），v1.23.0。** versionstore 信封带写令牌，回复丢失的写由 store 核对（[方案](../feature/A2-3-VERSIONSTORE-WRITE-TOKEN-2026-10-07.md)）；实施中发现带索引写的 NaN 分数漏洞：

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-35](RR-20261006-35.md) | P3 带索引的 versionstore 写，`Entry` 返回 NaN 分数时脚本先 SET 后 ZADD 报错：值写进去了、索引没动，调用方拿到错误（替身用 Go `ParseFloat` 接受 NaN，单测看不见） | 已修复、声明场景验证，v1.23.0 |

**10-07 skill 衍生物分区存放与源文档 digest（分支 `skpart`），v1.23.0。** 维护者第十三轮“skill 衍生物两张表”：衍生物记录按字段分进施放中 / 已移交 / 待停止 / 已停止四个分区，删掉重复的 owned 表，checkpoint 只存一份（版本 6），[方案](../feature/REFACTOR-2026-10-07-skill-spawn-partition.md)。召唤物改名时发现的源文档 digest 冲突登记为 RR-20261006-33（P3）；分区实施中核对停止循环发现 RR-20261006-34（P2，`3fad5b6e` 引入）。两条已修复、声明场景验证，v1.23.0。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-34](RR-20261006-34.md) | P2 `Shutdown` / `RemoveProgram` 停不下前一个衍生物时，待停止上限删掉列表里后面的记录，循环取回 nil 后空指针 panic（持着 Runtime 锁） | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-33](RR-20261006-33.md) | P3 源文档 digest 是 `json.Marshal(Definition)`：接口值不带类型、`json:"-"` 字段被跳过，只差效果类型（`set_memory` / `add_memory`）、消耗数量、cast window 表达式、策略 / 输入 / 形状 / 过滤器类型的两个定义摘要相同 | 已修复、声明场景验证，v1.23.0 |

**10-07 停止入口统一（维护者“停止入口统一”，RR-20261006-21 后续二，分支 `skstop`），v1.23.0。** 衍生物的全部停止入口（施法失败与施法里的停止、衍生物启动失败清理、移交、tick 回收、`RemoveProgram`、`Shutdown`）只调一个停止函数，宿主拒绝后统一转 `stop_pending` 由 Runtime 退避重试；修前 `Shutdown` / `RemoveProgram` 停不下时衍生物残留、Runtime 不再重试，见 [RR-20261006-21 后续二](RR-20261006-21.md#后续二停止入口统一维护者-2026-10-07)、[方案](../feature/REFACTOR-2026-10-06-skill-spawn-stop-unified.md)。实施中另发现一处：RR-20261006-32（P3），已修复、声明场景验证，v1.23.0。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-32](RR-20261006-32.md) | P3 施法里的衍生物停止（Interrupt / Cancel / goto / 收尾 / 启动失败清理 / 移交）被宿主拒绝后同一请求再停一次，cancel 回调跑两遍，启动失败的衍生物跑了成功路径不跑的 cancel 回调 | 已修复、声明场景验证，v1.23.0 |

**10-06 RR-20261006-21 后续：Runtime 负责重试停止（维护者“如果是技能本身的问题是不是技能自己处理比较好”，分支 `skretry`），v1.23.0。** 宿主停不下的进程标成 `stop_pending`、按退避重试、有上限与告警、写进 checkpoint（格式版本 3），见 [RR-20261006-21 后续](RR-20261006-21.md#后续runtime-负责重试停止维护者-2026-10-06)。实施中另发现两处：RR-20261006-30（P3）、-31（P2），已修复、声明场景验证，v1.23.0。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-31](RR-20261006-31.md) | P2 移交后的进程到期 / 失效时宿主拒绝 `StopProcess`，`Advance` 每次先重做它、失败即返回，Runtime 的 tick 停在原地，这个 Runtime 上所有施法冻住 | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-30](RR-20261006-30.md) | P3 被钉住的终态 cast 多于 `CompletedCastLimit` 时（如上限 1、两个移交后仍在运行的召唤），live Runtime 合法持有，checkpoint 恢复却判 corrupt | 已修复、声明场景验证，v1.23.0 |

**10-06 真实进程演练追加两项（drill3）：RR-20261006-27（P3）、-28（P3）、-29（P3）已修复、声明场景验证，v1.23.0。** 上一轮留下的两处未闭环：两个 sid 时 loadtest 全部成功仍 `rc=1`、Redis Cluster 下没跑过演练，[记录](../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md) ⑤ ⑥。p95 的 16.384s 是直方图估计取了桶上界、不是延迟，失败行也不说哪条阈值（-27）；生成工程整体切 Cluster 时生产校验只认 `redis.addr`、accountctl 与 dev run.sh 只会连单机（-28）；每个活动窗口开头最多 5s 的通关不计入、机器人判失败（-29，与 Cluster 无关）。Cluster 下单实例锁等待 / 接管、失锁 fail-stop、静态绑定、在途 saga 接手全部符合，与单机一致。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-29](RR-20261006-29.md) | P3 game-demo 活动窗口由循环每 5s 开一次，边界后最多 5s 的通关 `activity: not found`、贡献丢失，机器人判失败 | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-28](RR-20261006-28.md) | P3 生成工程不能整体切到 Redis Cluster：生产校验只认 `redis.addr`，accountctl / dev run.sh 只会连单机（其余节点 `MOVED`） | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-27](RR-20261006-27.md) | P3 loadtest 的分位数取桶上界（两个 sid 时 p95 = 16.384s，整次运行才 9.6s），全部成功仍 `rc=1`；失败行不点名阈值 | 已修复、声明场景验证，v1.23.0 |

**10-06 真实进程演练四项（drill2，恢复自 `wip/drill`）：RR-20261006-24（P3）、-25（P4）、-26（P4）已修复、声明场景验证，v1.23.0。** 当前代码上重跑 game-demo 单实例锁 / fail-stop / 静态绑定演练（含 6b）、Init 中途失败收尾（NC-193）、停机 hook 卡住时 SIGTERM（NC-231）、kit Mongo / NATS Mod 接真实依赖的 Close（RR-20261006-10），[记录](../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md)。演练发现：nats 驱动关闭后订阅 / JetStream / RPC CallAsync 的错误不是 `fnats.ErrClosed`（-24）；停机 hook 超时的错误不点名 hook（-25）；排空被硬关后 `Connected()` 约 5s 内仍为 true（-26，nats.go 排空协程翻回 `DRAINING_PUBS`）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-26](RR-20261006-26.md) | P4 nats 排空被硬关后 nats.go 把状态翻回 DRAINING_PUBS，驱动 `Connected()` 最长约 5s 仍为 true | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-25](RR-20261006-25.md) | P4 停机 lifecycle hook 超时的错误只报阶段、不点名卡住的 hook | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-24](RR-20261006-24.md) | P3 nats 驱动关闭后 Subscribe / QueueSubscribe / JetStream / RPC CallAsync 的错误 `errors.Is` 不到 `fnats.ErrClosed` | 已修复、声明场景验证，v1.23.0 |

**10-06 SKILL-1 / SKILL-3 补测（sktest2）：RR-20261006-21（P2）、-22（P3）、-23（P2）已修复、声明场景验证，v1.23.0。** 补发版文档 SKILL-1 / SKILL-3 review 检查点里没有专门用例的分支：Interrupt 停进程出错、toggle release 回调出错一次通过（变异证明能红）；charge enter 起进程后失败，已停进程的记录留在被复用的 cast ID 下、Checkpoint 报 corrupt（-21）；同一机制下起过进程的 cast 进程结束后也永不回收，超过 `CompletedCastLimit` 后 checkpoint 恢复判 corrupt（-23）；reset 的进程条目用 lifecycle 实体冒充 `PrimaryTarget`，与增量交给策略的事件不一致（-22）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-23](RR-20261006-23.md) | P2 起过 entity 进程的 cast 在进程结束后也永不回收（已停的进程记录被当作引用），cast 与进程记录无界增长，超过 CompletedCastLimit 后 checkpoint 恢复判 corrupt | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-22](RR-20261006-22.md) | P3 presentation reset 的进程条目把 lifecycle 实体当 PrimaryTarget 交给策略，仍归施法的进程与增量不一致，按 PrimaryTarget 判定的策略在 reset 里放出增量挡住的表现 | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-21](RR-20261006-21.md) | P2 起过进程的未提交启动失败后，已停进程的记录留在被复用的 cast ID 名下，之后每次 Checkpoint 报 corrupt；宿主停不下进程时运行中的记录被下一个 cast 接走 | 已修复、声明场景验证，v1.23.0（含后续：Runtime 负责重试停止，`skretry`；后续二：停止入口统一，`skstop`） |

**10-06 NONCORE-46 维护者选 A（tpstat）：RR-20261006-20（P4）已修复、声明场景验证，v1.23.0。** N13 O9 后半：`goroutine.TaskPool` 入队后才加 total、`GetStats` 先读 total，统计瞬间读到结束数大于提交数；revleft 曾以“无确定性红、零调用方”不改，现用测试缝确定性复现。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-20](RR-20261006-20.md) | P4 TaskPool 统计瞬间读到 completed + failed > total（入队后才计提交、先读 total 再读结束数） | 已修复、声明场景验证，v1.23.0 |

**10-06 发版文档 OWN-5 / OPS-2 的“未做”闭环（oa）：RR-20261006-17、-18、-19（均 P3）已修复、声明场景验证，v1.23.0。** 协调器读了活动组文件却不核对开窗的 expected 集合（C4 方案“留待需要时”，-17）；nest 派发器销毁后 `nest.dispatch.*{dispatcher}` 序列一直留着（第十二轮 O3，-18）；可靠 RPC 的 `method` 标签没有上界、没有注销可以挂删除（O3，-19）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-19](RR-20261006-19.md) | P3 可靠 RPC 的 `bus_rpc_pending{method}` 等 method 标签取任意调用参数 / 对端 envelope 名字，没有上界（只靠注册表 2048 兜底、连真实方法一起丢） | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-18](RR-20261006-18.md) | P3 nest 派发器排空销毁后 `nest.dispatch.*{dispatcher}` 序列停在最后的值，每个新名字多 6 条 | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-17](RR-20261006-17.md) | P3 协调器设置了 `activity.groups_file`，`OpenActivity` 仍接受组外 sid、文件里没有的组 | 已修复、声明场景验证，v1.23.0 |

**10-06 RR-20261006-15 未验证项闭环（rr15v）：RR-20261006-16（P2）已修复、声明场景验证，v1.23.0。** 修前进程与修后进程实跑混跑时发现：升级前（或混跑中旧进程在上限边上）留下的 4097 份不带 `outcome` 的 claim 让新进程也超过 4096（-16）。同批 RR-15 的服务端耗时实测后补索引（复核残余，不另编号）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-16](RR-20261006-16.md) | P2 升级前就卡住的步骤（4097 份不带 outcome 的 claim）升级到 RR-20261006-15 之后仍报 ErrConflict，混跑中旧进程留下的同样挡住新进程 | 已修复、声明场景验证，v1.23.0 |

**10-06 未验证项闭环（fixr2），无新 RR，v1.23.0。** RR-20261006-11 的溢出水位到期经兴趣表时钟注入缝实测（到期前挡、到期后清、每个 consumer 至多一个）；兴趣 handler 出错时 JetStream 结算在真实 NATS 上核对为 Ack（不重投、不 Term，与设计一致，[MIRROR-STEP-4 §6.10](../feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)）；RR-20261005-01 的回归在 C4（`277e1252`）改名为组文件形态、承诺仍在，补“组上限不超过一次 Live 上限”守卫，见两份记录末节。

**10-06 saga / 驱动疑点闭环（fixs）：RR-20261006-14（P3）、-15（P2）已修复、声明场景验证，v1.23.0。** 发版文档 SAGA-2 / SAGA-8 的 review 检查点按“不留 Wanted”本轮闭环：stepTransition 守卫看不到经包级 helper 改写请求的绕过（-14）；同一步骤的尝试跨 Resume 累积超过 4096 后这一步永远 Reserve 不了（-15）。同批 redis/driver 测试按实际断言改名、两组 Mongo 步骤延迟数字标明出处（不登记 RR）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-15](RR-20261006-15.md) | P2 同一操作实例的尝试跨 Resume 累积（每生 ≤1000、Resume 不限次数、claim 保留 30 天），超过 4096 份后 Reserve 每次报 ErrConflict，修好原因再 Resume 也执行不了 | 已修复（含残余补修），v1.23.0 |
| [RR-20261006-14](RR-20261006-14.md) | P3 saga stepTransition 守卫只按语法查 Engine 方法，包级 helper 清空请求的 CloseOperation 后自己调 Store.Apply，守卫照样通过 | 已修复、声明场景验证，v1.23.0 |

**10-06 W-2026-10-06-01 转 RR 与 glsvet A1 跟进（fixn）：RR-20261006-12、-13（均 P3）已修复、声明场景验证，v1.23.0。** nest 派发取锁在没有 Guard 作用域时把调用方的 Guard 放回池，组迁移重试使同一 Guard 两次归还（来源 W-2026-10-06-01，收尾第 4 批 A2 的根因）；glsvet A1 提示看不见经同包 helper 函数登记的 undo。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-13](RR-20261006-13.md) | P3 glsvet A1 提示只看组件方法体里的直接 `RecordUndo` / `RecordUndoToken` / `DeferRollback`，经同包包级 helper 登记的看不见 | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-12](RR-20261006-12.md) | P3 nest `releaseDispatchLocks` 无 Guard 作用域时把调用方的 Guard 归还池，组迁移重试使同一 Guard 两次进池、之后两个快 worker 共用（生产入口都在作用域里，不可达） | 已修复、声明场景验证，v1.23.0 |

**10-06 REM 疑点闭环（fixr）：RR-20261006-11（P3）已修复、声明场景验证，v1.23.0。** 兴趣表满时 release 不留撤销水位，表里随后空出一格就让迟到的旧续租复活已撤销的租约（发版文档 REM-4 疑点）。同批其余疑点（L1 直接写入点、`After` 注释、MIRROR-M6 / USER_GUIDE / B2 / CHANGELOG 的口径）以守卫测试与按源码更正闭环，见[修复记录](../bugfix/RR-20261006-11.md) §2。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-11](RR-20261006-11.md) | P3 兴趣表满时 release 不留撤销水位，迟到的旧续租复活 consumer 已撤销的租约（owner 继续推送、占配额与容量，最长一个兴趣 TTL） | 已修复、声明场景验证（到期与 JetStream 结算 fixr2 实测），v1.23.0 |

**10-06 驱动 Close 口径统一（wclose）：RR-20261006-10（P3）已修复、声明场景验证，v1.23.0。** 重复 Close 有三种口径（返回 nil / 粘滞返回第一次结果 / 每次报错，redis 单机与 Cluster 不同），并发 Close 的后到者不等第一个做完，kit Redis / Mongo / Nats Mod 并发停止有数据竞争，etcd Client 关闭后的调用阻塞到截止时间（来源 W-2026-10-06-02）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-10](RR-20261006-10.md) | P3 驱动与 Mod 的 Close 口径不一致：redis 单机重复 Close 返回 ErrClosed 而 Cluster 返回 nil，etcd / 单实例锁 store 粘滞返回第一次错误，nats Assembly 每次返回 ErrClosedUndrained；并发 Close 不串行、kit Mod 停止有数据竞争；etcd 关闭后的调用不快速失败 | 已修复、声明场景验证，v1.23.0 |

**10-06 维护者第十二轮 kit / core 批（bkit）：RR-20261006-09（P3）已修复、声明场景验证，v1.23.0。** robot Stage 先缩后扩，扩回的机器人从 `launched+1` 编号，复用刚停掉的机器人的序号与 PlayerID（N12 O9）。同批其余八项是决定实施，见 [第十二轮 kit 批](../feature/DECISIONS-R12-KIT-2026-10-06.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-09](RR-20261006-09.md) | P3 robot Stage 先缩后扩，新机器人复用刚停掉的机器人的序号与 PlayerID | 已修复、声明场景验证，v1.23.0 |

**10-06 收尾第 4 批 kit / core 小修（cb4）：RR-20261006-05～08（均 P3）已修复、声明场景验证，v1.23.0。** global `Bind` 结果未知后同参数重试被报成已绑定（A7）；saga 步骤预算只差大小写的类型 / 步骤名静默互相覆盖（A12）；`app.run` 启动阶段失败的退出原因不进文件日志（A13）；mongotest `$in` 不支持具名切片、saga 用例绕开 `ClaimDue`（A14，O-S5-6）。同批 A2 / A3（nest 用例在 `-shuffle` 下失败、glsvet `-tests` 3 条）是用例隔离问题，不登记 RR。[记录](../bugfix/CLOSING-BATCH-4-2026-10-06.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-08](RR-20261006-08.md) | P3 mongotest `$in` 操作数为具名切片（`[]saga.Status`）或 `[]int64` 时报 ErrUnsupported，`MongoStore.ClaimDue` 在替身上不可用（O-S5-6） | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-07](RR-20261006-07.md) | P3 `app.run` 启动阶段失败直接返回，文件日志里没有退出原因（生成 main 的 `server exit` 在 Close 之后，只到 stderr） | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-06](RR-20261006-06.md) | P3 saga 步骤预算的已知表按小写建，只差大小写的类型 / 步骤名静默互相覆盖 | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-05](RR-20261006-05.md) | P3 global `Bind` 结果未知后用同样参数重试，被报成 `ErrConflict "already bound"` | 已修复、声明场景验证，v1.23.0 |

**10-06 收尾第 3 批 skill 小修（cb3）：RR-20261006-02（P2）、-03（P3）、-04（P3）已修复、声明场景验证，v1.23.0。** 默认值为 null 的实体持久状态 `modify_state set` 每次报类型不匹配（O33 实施时发现）；checkpoint 恢复仍接受编译期已拒绝的 `phase_timeout` 任务（O20）；文件 outbox 不清理崩溃遗留的 `outbox-*.tmp`（O6）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-04](RR-20261006-04.md) | P3 文件 outbox 不清理写入中途崩溃遗留的 `outbox-*.tmp`，每次崩溃泄漏一个、不计入上限（O6） | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-03](RR-20261006-03.md) | P3 checkpoint 恢复仍接受 `phase_timeout` 任务，恢复后执行即 ErrProgramInvariant（O20） | 已修复、声明场景验证，v1.23.0 |
| [RR-20261006-02](RR-20261006-02.md) | P2 默认值为 null 的实体持久状态，`modify_state set` 在 MemoryHost 上每次报 ErrRuntimeTypeMismatch | 已修复、声明场景验证，v1.23.0 |

**10-06 Mirror 第 6 步本机替代（mirror6）：RR-20261006-01（P2）已修复、声明场景验证，未发版。** 删除 Managed Remote 实体（strict）已提交到 Mongo，确认却交给被同一事务清空的实例、生成代码按身份拒绝，删除的发布（L2 墓碑 + 推送）整段被跳过，调用方得到“结果未知”，只读方在 L2 TTL 内读到已删除实体。[记录](../feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261006-01](RR-20261006-01.md) | P2 Remote 实体删除已提交，确认报“身份不符”，删除不发布（L2 无墓碑、无推送），只读方在 L2 TTL 内读到已删除实体 | 已修复、声明场景验证，未发版 |

**10-06 N09 skill 第六批（revn09f，维护者第五轮决定“求值上下文表”）：NC-280～283（均 P2）已修复、声明场景验证，未发版。** memory 默认值读另一个 memory 时编译结果随 map 顺序变化、通过时每次 Activate 类型不匹配；持久状态默认值按施法作用域检查、在进程回调里读写即 ErrProgramInvariant；cast_start / phase_start 读取可缺省实体时读取处的 exists 守卫在采样点不生效；投射引用（`$primary_target.position`、`$event.*.position`、`$input.target.position`）编译通过却求不出。均为做“求值上下文 → 可用引用”表时逐格对照 Runtime 求值点发现。[本轮](../review/REVIEW-2026-10-06-n09-batch6.md) · [表方案](../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-283](RR-20261005-NC-283.md) | P2 skill 投射引用（`$primary_target.position`、`$event.*.position`、`$lifecycle_entity.position`）编译通过、每次求值 ErrProgramInvariant；`$input.target.position` 自 B3 起 LOWER_UNRESOLVED | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-282](RR-20261005-NC-282.md) | P2 skill cast_start / phase_start 读取的实体是可缺省引用时 exists 守卫在采样点不生效，缺省即施法失败 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-281](RR-20261005-NC-281.md) | P2 skill 持久状态默认值按施法作用域检查，在进程回调里读写时 ErrProgramInvariant | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-280](RR-20261005-NC-280.md) | P2 skill memory 默认值读另一个 memory：编译结果随 map 顺序变化，通过时每次 Activate 类型不匹配 | 已修复、声明场景验证，未发版 |
**10-06 N11～N13 留项与小防护（revleft）：NC-260～270（P3，NC-270 潜伏）已修复、声明场景验证，未发版。** Mongo 客户端已断开时 Mod 的 Stop 永远失败（第三方 Close 不幂等第三例）；robot Call 发送不受 ctx 约束、迟到应答被当推送；日志轮转失败丢行 / 控制台出错连带文件丢行；Prometheus 标签转义；Coalescer.Close 不等 flush；Duration 截断记 completed；ObjectPool 重复 Put、拓扑排序切片别名、TaskPool 重启生命周期；game-demo 寻路系统停止时无锁清空 terrain。N11 O6 / O7 / O9 与 N13 O10 是需求选择，见 DECISIONS-PENDING。另加根包冲突标记门禁。[本轮](../review/REVIEW-2026-10-06-revleft.md)

**10-06 N06 S5 Saga 剩余项（revn06s5）：NC-250（P3）已修复、声明场景验证，未发版。** 定义缺失 fence 时重试退避中的步骤没有关闭为“放弃”，之后生效的成功以 `ErrNotWaiting` 静默丢弃。租约接管后的晚到 Apply、outbox supersede / Ack 未知、真实 NATS + Mongo 两进程强杀恢复均无缺陷；TTL 之后的重投不加协调器计数（方向判断），观察 O-S5-1～7 与方向建议待维护者。[本轮](../review/REVIEW-2026-10-06-n06s5.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-250](RR-20261005-NC-250.md) | P3 定义缺失 fence 时，重试退避中的步骤没有关闭为“放弃”，之后生效的成功被静默丢弃 | 已修复、声明场景验证，未发版 |

**10-06 N10 第二批（revn10b）：NC-241～245 P2、NC-240 / 246 / 247 P3 已修复、声明场景验证，未发版。** ai Parallel 结果已定仍 Tick 后面的子节点、策略回调里切换策略让旧树在 Stop 后继续发起动作；actionflow Update 的 fn panic 让 runner 报废、替换时旧动作 Cancel panic 让交出的 ID 永无结论（O-A4）；hotcode 接口变量导出的 PatchBundle 永远加载失败（真实 .so 首次跑到）、插件部分应用不回滚（O-H1）、闭包补丁误报未打补丁（O-H2）、Resolve[T] 静默回落（O-H3）。[本轮](../review/REVIEW-2026-10-06-noncore-n10b.md)（含 MissionRunner / O-A1 的待维护者选项与方向判断）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-270](RR-20261005-NC-270.md) | P3（潜伏） game-demo PathFindSystem.Stop 无锁清空 terrain，与场景外的并发寻路数据竞争 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-269](RR-20261005-NC-269.md) | P3 TaskPool 先 Shutdown 再 Start 之后再也停不下来；Shutdown 后 Start 把池标成 running 却拒绝一切任务 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-268](RR-20261005-NC-268.md) | P3 TopologicalSortCache 与调用方共享依赖切片 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-267](RR-20261005-NC-267.md) | P3 ObjectPool 同一对象 Put 两次，之后两次 Get 交出同一个对象 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-266](RR-20261005-NC-266.md) | P3 loadtest 运行被 profile 的 Duration 截断时停止原因记成 completed | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-265](RR-20261005-NC-265.md) | P3 robot Coalescer.Close 不等最后一次 flush 就返回 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-264](RR-20261005-NC-264.md) | P3 Prometheus 导出的标签值转义不符合 exposition 格式 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-263](RR-20261005-NC-263.md) | P3 日志轮转打不开新分片时整行丢失；控制台写失败时文件也收不到；两者都没有计数 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-262](RR-20261005-NC-262.md) | P3 robot 超时之后才到的应答被当成推送分发 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-261](RR-20261005-NC-261.md) | P3 robot Session.Call 的发送不受 ctx 约束，写阻塞时 Call 越过自己的超时 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-260](RR-20261005-NC-260.md) | P3 Mongo 客户端已断开时 Mongo Mod 的 Stop 永远失败（mongo-driver Disconnect 不幂等） | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-247](RR-20261005-NC-247.md) | P3 hotcode Resolve[T] 类型不符时静默回落 fallback | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-246](RR-20261005-NC-246.md) | P3 hotcode.list 用代码指针判 Patched，闭包补丁误报为未打补丁 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-245](RR-20261005-NC-245.md) | P2 hotcode 插件 Apply 中途失败，已替换的点留在插件版本 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-244](RR-20261005-NC-244.md) | P2 hotcode LoadPlugin 的 *Bundle 分支因 ok 被遮蔽永远失败 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-243](RR-20261005-NC-243.md) | P2 actionflow 替换时旧动作 Cancel panic，交出的 ID 永无结论、组空闲 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-242](RR-20261005-NC-242.md) | P2 actionflow Update 的 fn panic 后 runner 报废（B7 引入） | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-241](RR-20261005-NC-241.md) | P2 ai 策略回调里切换策略立即执行，旧树在 Stop 之后继续发起动作 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-240](RR-20261005-NC-240.md) | P3 ai Parallel 结果已定仍 Tick 后面的子节点，发起没人要的动作 | 已修复、声明场景验证，未发版 |

**10-06 N01 留项 + N14 O3 / O4（revn01b）：NC-230～234（均 P3）已修复、声明场景验证，未发版。** ops 端口 bind 失败时 Start 仍返回 nil；停机阶段的 lifecycle hook 不受 `shutdown.total_timeout` 约束；停机开始之后的 RuntimeFailure 不进 run 的返回值（以 0 退出）；Redis Mod 关闭出错后再调 Stop 永远失败；remote_entity Mod 停止失败仍记 stopped。N02 O1 以 `ops.admin_timeout` 落实（不占 RR），Health 的 Degraded 映射待维护者决定（DECISIONS-PENDING D1）。[本轮](../review/REVIEW-2026-10-06-n01b.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-234](RR-20261005-NC-234.md) | P3 remote_entity Mod 停止失败时仍记 “stopped” | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-233](RR-20261005-NC-233.md) | P3 Redis Mod 关闭出错后，再调 Stop 永远不会成功 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-232](RR-20261005-NC-232.md) | P3 停机开始之后的 RuntimeFailure 不进 run 的返回值，进程以 0 退出 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-231](RR-20261005-NC-231.md) | P3 停机阶段的 lifecycle hook 不受 shutdown.total_timeout 约束，忽略 ctx 的 hook 让停机永远不返回 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-230](RR-20261005-NC-230.md) | P3 ops 端口 bind 失败时 OpsMod.Start 仍返回 nil，进程在没有探针端点的情况下运行 | 已修复、声明场景验证，未发版 |

**10-05 维护者决定 A4 / C1 / A5（a4config）：NC-192（P2）按方案 1 已修复，NC-203 含 A5 残余补修，均声明场景验证、未发版。** 生产校验只要求有读取方的设置；框架配置一律严格读取（A4，接 NC-190）；隔离环境的全局运维命令运行期间持有验收锁。[A4 方案](../feature/REFACTOR-2026-10-05-strict-config-reads.md) · [NC-192 修复](../bugfix/RR-20261005-NC-192.md)。

**10-06 N09 skill 第五批（revn09e）：NC-220、NC-223、NC-224 三个 P2 与 NC-221、NC-222 两个 P3 已修复、声明场景验证，未发版。** 缓存型快照点（cast_start / phase_start / process_start）不按采样上下文检查；被动的 max_depth 0 与位置 / 方向输入永不触发；非 spawn 效果上的 process / on 被丢弃；B3 回归：以局部变量为实体的 read_attribute 从 v1.20.2 起编译失败；spawn 进程每步重新求值的字段读施法输入 / memory / 局部变量，下一 tick 起 ErrProgramInvariant。[本轮](../review/REVIEW-2026-10-06-n09-batch5.md)。

**10-05 N09 skill 第四批（revn09d）：NC-210、NC-211、NC-214 三个 P2 与 NC-212、NC-213、NC-215、NC-216 四个 P3 已修复、声明场景验证，未发版。** memory 效果名字不查声明、落到槽位 0；移交后的 area 回调 finish 让 Advance 报 ErrProgramInvariant；status / attribute / resource 名字不查 catalog、落到 handle 0；catalog key 不查唯一；chain 间隔 / 重复与 modifier 叠层只编译不传 Host；Host 都拒绝的取值能编译；NegotiateSchema 接受空区间。[本轮](../review/REVIEW-2026-10-05-n09-batch4.md)（含编译 ⇒ 可执行的变异性质测试与方向判断）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-224](RR-20261005-NC-224.md) | P2 skill spawn 进程每一步重新求值的字段（area 选择、follow / tracking / carry 目标、path 点、parabola 目的地等）读 `$input` / `$memory` / `$local` 能编译，下一 tick 起 ErrProgramInvariant | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-223](RR-20261005-NC-223.md) | P2 skill B3 回归（v1.20.2）：lowerSnapshots 用空作用域 lower 全部快照计划的实体，以局部变量为实体的 read_attribute 编译失败 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-222](RR-20261005-NC-222.md) | P3 skill 非 spawn 效果上的 process / on 能编译，Runtime 从不启动进程、回调从不执行 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-221](RR-20261005-NC-221.md) | P3 skill max_depth 0、输入不是 none / entity 的被动能编译，每次触发都被静默压制 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-220](RR-20261005-NC-220.md) | P2 skill 缓存型快照点不按采样上下文检查：phase 流程里的 process_start 有 owned spawn 时每次施法 ErrProgramInvariant；实体是局部变量时 LOWER_UNRESOLVED at `$` | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-216](RR-20261005-NC-216.md) | P3 skillsync NegotiateSchema 在一边 Min 为 0（空区间）时仍返回版本 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-215](RR-20261005-NC-215.md) | P3 skill 两个参考 Host 与 Runtime 都拒绝的取值能编译（modifier operation / 时长、status 时长 0、resource operation、负 cost、compare op） | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-214](RR-20261005-NC-214.md) | P2 skill status / attribute / resource 名字不查 catalog，lower 兜底成 handle 0：施法失败或静默作用在 handle 0 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-213](RR-20261005-NC-213.md) | P3 skill chain allow_repeat / hop_interval_ticks、attribute_modifier stack_policy / max_stacks 只编译不传 Host | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-212](RR-20261005-NC-212.md) | P3 skill CompileEnvironment 不查 catalog key 唯一，同一 key 被解析成两个条目 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-211](RR-20261005-NC-211.md) | P2 skill 施法先结束后，移交的 area 回调 finish 让 Advance 返回 ErrProgramInvariant | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-210](RR-20261005-NC-210.md) | P2 skill set / add / clear_memory 的名字不查声明，落到槽位 0（静默改写或 ErrProgramInvariant） | 已修复、声明场景验证，未发版 |

**10-05 N15 scripts / cmd 与非 Go 资产（revn15）：NC-200（P2）与 NC-201～208（P3）已修复、声明场景验证，未发版（NC-208 的 kit/dataengine 部分留给核心线）。** gapmap 收尾丢未提交修改；Cluster 套件脚本、toxiproxy pid、验收锁、toxic 用例全局 reset 让并行会话互相干扰共享隔离环境；glsvet / pretag / 故障矩阵三处门禁对没检查到的东西报通过；生成的 .gitignore 不忽略 WAL。[本轮](../review/REVIEW-2026-10-05-n15.md)（含方向判断）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-208](RR-20261005-NC-208.md) | P3 toxic 用例 POST /reset 清掉同一 toxiproxy 上别人的毒 | 已修复（kit/dataengine 部分已补修），未发版 |
| [RR-20261005-NC-207](RR-20261005-NC-207.md) | P3 故障矩阵把 no tests to run 的格记为 PASS | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-206](RR-20261005-NC-206.md) | P3 生成的 .gitignore 不忽略 data/wal | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-205](RR-20261005-NC-205.md) | P3 origin 不可达时 pretag 跳过远端同名 tag 检查 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-204](RR-20261005-NC-204.md) | P3 glsvet 对不存在 / 解析失败的目录退出 0 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-203](RR-20261005-NC-203.md) | P3 验收锁只有持锁者看，其他入口照常改环境，reset 连锁一起删 | 已修复（含 A5 残余补修：全局命令运行期间持锁），未发版 |
| [RR-20261005-NC-202](RR-20261005-NC-202.md) | P3 toxiproxy pid 不校验所有权，down 杀掉复用该 pid 的进程 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-201](RR-20261005-NC-201.md) | P3 redis-cluster-suites.sh 继承 ROOST_DATAENGINE_IT，把全环境故障套件跑到共享环境 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-200](RR-20261005-NC-200.md) | P2 gapmap.sh 收尾 git checkout 丢掉运行前已有的未提交修改 | 已修复、声明场景验证，未发版 |

**10-05 N14 Kit 跨域装配（revn14）：NC-190、NC-191（P2）与 NC-193、NC-194（P3）已修复、声明场景验证，未发版；NC-192（P2）已确认，待维护者选方案。** 写错类型的配置值被 viper 静默读成零值（`singleton.enabled: on` 关掉单实例锁、不带单位的时长读成纳秒并通过校验、`redis.cluster_addrs` 列表退回本机单点）；Mongo 启动日志带口令；生产配置校验要求的九个开关无读取方；启动失败时 Service 组件未停完就拆 Mod、释放锁；无定义时大小写混写的 saga 步骤覆盖不生效。[本轮](../review/REVIEW-2026-10-05-n14.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-194](RR-20261005-NC-194.md) | P3 Mod 没拿到 saga 定义时，大小写混写的 `saga.steps` 覆盖静默不生效 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-193](RR-20261005-NC-193.md) | P3 启动失败时 Service 已启动的部分没停完，App 就停 Mod、释放单实例锁 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-192](RR-20261005-NC-192.md) | P2 生产配置校验要求的九个开关没有读取方，通过校验不代表限流 / 鉴权 / WAL 持久生效 | 已修复（维护者决定 C1 方案 1）、声明场景验证，未发版 |
| [RR-20261005-NC-191](RR-20261005-NC-191.md) | P2 Mongo Mod 启动日志原样记录带口令的 `mongo.uri` | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-190](RR-20261005-NC-190.md) | P2 写错类型的配置值被静默读成零值（单实例锁 / 可靠总线开关、不带单位的时长、cluster 地址列表） | 已修复、声明场景验证，未发版 |

**v1.20.1 已发布（2026-10-05，tag → `be7407ab`）**：U-0279 / U-0280（含复审补修）/ U-0281、NC-100 / NC-101（含复审补修）、RR-20261005-01，以及截至 `be7407ab` 的非核心 review 修复（NC-50～52、60～65、70～75、80～83、90～93、100～102、110～117、120～123、140～147）随本版发布；`be7407ab` 之后提交的（如 N05 的 NC-130 / NC-131 / RR-20260913-01 残余）未发版。下方“未发版”指发布前状态。

**10-05 N13 container / safemap / goroutine / misc / internal（revn13）：NC-180 P2（潜伏）与 NC-181～185 P3 已修复、声明场景验证，未发版。** BucketHolder 持桶读锁调用遍历回调，EntityManager.Range 回调里 Destroy 卡死；RangeAll 的 false 不跨桶停止；FastMap 遍历回调里改已有键重排后交出零值键（生成 DAO 写进提交）；TaskPool Submit / Shutdown 并发 panic；拓扑排序遇未注册依赖误报 / 掩盖环；KeyMap 遍历删除当前键漏键。[本轮](../review/REVIEW-2026-10-05-n13.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-185](RR-20261005-NC-185.md) | P3 KeyMap 遍历中删除当前键，漏掉一个键并交出零值键 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-184](RR-20261005-NC-184.md) | P3 TopologicalSortCache 遇未注册的依赖误报环，或把真正的环藏起来 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-183](RR-20261005-NC-183.md) | P3 TaskPool Submit 与 Shutdown 并发 panic “send on closed channel” | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-182](RR-20261005-NC-182.md) | P3 FastMap 遍历回调里改已有键触发重排，交出零值键、漏键 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-181](RR-20261005-NC-181.md) | P3 RangeAll 的 false 只停当前桶，EntityManager.Range 的提前停止不成立 | 已修复（含残余补修，未发版） |
| [RR-20261005-NC-180](RR-20261005-NC-180.md) | P2 BucketHolder 持桶读锁调用遍历回调，回调改同一容器即自锁 | 已修复（含残余补修，未发版） |

**10-05 同形停机核实（stopshape）：NC-170～174 五个 P3 已修复、声明场景验证（NC-172 含真实 NATS、NC-173 含真实 etcd），未发版；NC-83 记录的第 4 处（bus JetStream RPC）已由 NC-90 修掉。** 停机超时后把“已停止”记成清空的字段 / 取走的列表、退订不等在途回调、等待不看 ctx。统一按三步停机修复。[证据与方向判断](../bugfix/evidence/noncore-bugfix-20261005-stopshape/README.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-174](RR-20261005-NC-174.md) | P3 remoteentity Assembly.Stop 停复制只退订，不等已进入 ApplyReplica 的 handler | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-173](RR-20261005-NC-173.md) | P3 etcd Deregister 等注册循环不看 ctx；Assembly.Close 注销失败仍关 client，重试必然失败；（A3 复核）停完后再 Close 返回 context canceled | 已修复（含残余补修，未发版），声明场景验证（含真实 etcd） |
| [RR-20261005-NC-172](RR-20261005-NC-172.md) | P3 JetStream 同步总线 Stop 不等在途 handler，NATS 连接在 handler 底下关闭 | 已修复、声明场景验证（含真实 NATS），未发版 |
| [RR-20261005-NC-171](RR-20261005-NC-171.md) | P3 Nest Mod 卸载后重载停止超时即丢句柄，重试关闭 entitysync 时 worker 可能仍在运行 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-170](RR-20261005-NC-170.md) | P3 manager Engine 停止超时后重试报告成功，同一次调用在过期 ctx 下继续停依赖 | 已修复、声明场景验证，未发版 |

**10-05 N12 metrics / log / failurelog / robot（revn12）：NC-160、NC-161 P2 与 NC-162～165 P3 已修复、声明场景验证（含真实 Redis / 真实网关），未发版。** failurelog 把脚本“结果未知”当“没执行”再走非原子降级（真实 Redis 上一条死信写两份）；loadtest 阈值把没有样本判通过（真实网关 `-duration 1s` 零完成仍退出码 0）；robot 重连后 push capture 不再注册（真实网关重连后 0 帧）；websocket 拨号不看 ctx / DialTimeout；statslog 实体计数 gauge 清空不归零；log.Close 后默认 logger 写已关闭文件。[运行记录](../review/REVIEW-2026-10-05-n12-revn12.md) · [红证据](../review/evidence/noncore-review-20261005-n12/) · [修复](../bugfix/README.md)。审查 `b248a199`，修复 `f750ce43`～`e798a759`。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-165](RR-20261005-NC-165.md) | P3 log.Close 只关文件，只配文件 sink 时退出原因与停机尾部日志丢失 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-164](RR-20261005-NC-164.md) | P3 statslog entity.count_by_kind / by_category 在实体清空后停在旧值 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-163](RR-20261005-NC-163.md) | P3 robot websocket 拨号不看 ctx 与 DialTimeout，握手不回时永久阻塞 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-162](RR-20261005-NC-162.md) | P3 robot 重连后再次 EnsurePushCapture 是空操作，新会话推送被丢弃 | 已修复、声明场景验证（含真实网关），未发版 |
| [RR-20261005-NC-161](RR-20261005-NC-161.md) | P2 loadtest 阈值把没有样本判通过，CI 门禁假绿 | 已修复、声明场景验证（含真实网关），未发版 |
| [RR-20261005-NC-160](RR-20261005-NC-160.md) | P2 failurelog 脚本结果未知时再走非原子降级：死信重复 / 多删 / 清空时删掉新死信 | 已修复、声明场景验证（含真实 Redis），未发版 |

**10-05 N05 remoteentity mirror / ownerroute（revn05）：NC-130、NC-131 两个 P3 与 RR-20260913-01 跨节点 L2 删除水位残余已修复、声明场景验证（含真实 Redis / 自起 Redis Cluster），未发版。** 共享 L2 以 CAS 拒绝旧快照时 L1 冷的节点仍装下它、读取停在比 L2 旧的版本（含迁移后旧 route epoch）；本机兴趣表被过期条目占满后健康检查一直 Fail；版本化删除只在本机留墓碑，别的节点的在途加载 / 迟到消息把已删除快照写回 L2。ownerroute 与赠礼静态 sid 路由核对无缺陷；真实 JetStream 重放、兴趣容量为观察。[本轮](../review/REVIEW-2026-10-05-n05-revn05.md) · [证据](../review/evidence/noncore-review-20261005-n05/README.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-131](RR-20261005-NC-131.md) | P3 本机兴趣表只被过期条目占满时 Remote 健康一直报 capacity exhausted | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-130](RR-20261005-NC-130.md) | P3 L2 CAS 拒绝旧快照后本机 L1 仍装下它，读取停在比 L2 旧的版本 | 已修复、声明场景验证（含真实 Redis），未发版 |

**10-05 N11 spatial / timer / clock / index（revn11）：NC-140、NC-141 P2 与 NC-142～147 P3 已修复、声明场景验证，未发版。** World 定时器堆不随 Nest 事务回滚；Tick 期间取消 / 改期同样到期的定时器仍按旧期限触发；过期截止时间报告已武装却未武装；BlockRect 在 int64 上界溢出；index 在 NaN 值、混合动态类型接口键、零值 OrderedIndex 上 panic 或丢写；重入 Tick 提前结束外层推迟语义。[本轮](../review/REVIEW-2026-10-05-n11.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-140](RR-20261005-NC-140.md) | P2 World 定时器堆不随 Nest 事务回滚 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-141](RR-20261005-NC-141.md) | P2 Tick 期间取消 / 改期同样到期的定时器仍按旧期限触发 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-142](RR-20261005-NC-142.md) | P3 ScheduleActivityPhase 对过期截止时间报告已武装却未武装 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-143](RR-20261005-NC-143.md) | P3 BlockRect 在 int64 上界附近溢出 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-144](RR-20261005-NC-144.md) | P3 index.Upsert 遇到 NaN 值 panic | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-145](RR-20261005-NC-145.md) | P3 OrderedIndex 默认排序对混合动态类型接口键 panic | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-146](RR-20261005-NC-146.md) | P3 零值 OrderedIndex 丢写、nil 写入 panic | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-147](RR-20261005-NC-147.md) | P3 重入 Tick 提前结束外层推迟语义 | 已修复、声明场景验证，未发版 |

**10-05 N09 skill 第三批（revn09c）：NC-150 P2、NC-151 P2、NC-152 P3、NC-153 P3、NC-154 P3 已修复、声明场景验证，未发版。** 严格 Parse 被 encoding/json 大小写不敏感匹配绕过；phase 的 recast / timeout 事件与 `timeout_ticks` 只编译不执行、`timeout_ticks` 让 fallthrough 通过编译；tick 非负规则五处缺口；VisualPlanCache 共享加载用第一个调用者的 ctx；skillcompose 对空 / 重复 source 无诊断。[本轮](../review/REVIEW-2026-10-05-n09-batch3.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-154](RR-20261005-NC-154.md) | P3 skillcompose ValidateCandidate 对空 / 重复 candidate source 判 invalid 却无诊断 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-153](RR-20261005-NC-153.md) | P3 skill VisualPlanCache 共享加载用第一个调用者的 ctx，它取消后其他等待者也失败 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-152](RR-20261005-NC-152.md) | P3 skill tick 非负校验缺口：cooldown / phase timeout / repeat interval / add_status 时长 / chain hop 间隔负值能编译，负 wait 定位到 `$` | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-151](RR-20261005-NC-151.md) | P2 skill phase 的 recast / timeout 事件与 `timeout_ticks` 只编译不执行，`timeout_ticks` 让 fallthrough 通过编译、tap 施法 ErrProgramInvariant | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-150](RR-20261005-NC-150.md) | P2 skill 严格 Parse 被大小写不敏感匹配绕过（`id` + `ID`、`Cooldown_Ticks`） | 已修复、声明场景验证，未发版 |

**10-05 N10 第一批（revn10）：NC-120 / NC-121 P2、NC-122 / NC-123 P3 已修复、声明场景验证，未发版。** ai Controller 冻结时丢结束通知、行为树 Recover 后永远 Running；actionflow 丢弃排队动作不发 OnEnded、启动失败的 Cancel 重入留下孤儿动作；hotcode 并发 Replace / Revert 留下 Patched 与 Meta 矛盾的补丁点。ai / actionflow 目前无生产使用方。[本轮](../review/REVIEW-2026-10-05-noncore-n10.md)（含方向判断）。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-123](RR-20261005-NC-123.md) | P3 hotcode 并发 Replace / Revert 留下当前函数与 Meta 互相矛盾的补丁点 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-122](RR-20261005-NC-122.md) | P3 actionflow 启动失败、Cancel 里重入启动的动作被清出当前位成孤儿 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-121](RR-20261005-NC-121.md) | P2 actionflow ClearQueue / EndAll / ClearMission 丢弃排队动作不发 OnEnded | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-120](RR-20261005-NC-120.md) | P2 ai Controller 冻结期间丢弃动作 / 任务结束通知，行为树 Recover 后永远 Running | 已修复、声明场景验证，未发版 |

**10-05 N09 skill 第二批（revn09b）：NC-114 P2、NC-115 P2、NC-116 P3、NC-117 P2 已修复、声明场景验证，未发版。** skillsync 的 presentation reset 不经过可见性策略、state 快照与增量的可见性口径不一致（ability handle、三类 remove）、Applier 被一个畸形 full 包永久卡死；提交前失败的 cast 不进完成队列、永不回收，超过 CompletedCastLimit 后 checkpoint 无法恢复。[本轮](../review/REVIEW-2026-10-05-n09-batch2.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-117](RR-20261005-NC-117.md) | P2 skill 提交前失败的 cast 不进完成队列、永不回收，超过 CompletedCastLimit 后 checkpoint 无法恢复 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-116](RR-20261005-NC-116.md) | P3 skillsync Applier 拒绝 BaseSequence 非零的 full 包后永久 ErrApplyInProgress | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-115](RR-20261005-NC-115.md) | P2 skillsync state 可见性快照与增量口径不一致：ability handle、cast / process / persistent remove 放行不可见实体 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-114](RR-20261005-NC-114.md) | P2 skillsync presentation reset 不经过 VisibilityPolicy，不可见施法者的持续表现发给所有 observer | 已修复、声明场景验证，未发版 |

**10-05 N04 接续（revn04）：NC-100 P1、NC-101 P2、NC-102 P3 已修复、声明场景验证，未发版。** Redis 驱动在回复丢失后自动重放 Lua 脚本，versionstore 一次 Update 把 mutate 写两次并返回成功；`mongo.transaction_timeout` 不约束提交，网络黑洞时事务阻塞到恢复为止；mongotest 唯一索引把数组当一个值比较。[本轮](../review/REVIEW-2026-10-05-n04-revn04.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-revn04/README.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-100](RR-20261005-NC-100.md) | P1 Redis 驱动回复丢失后重放脚本，versionstore.Update 写两次 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-101](RR-20261005-NC-101.md) | P2 transaction_timeout 不约束提交，网络黑洞时无界阻塞 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-102](RR-20261005-NC-102.md) | P3 mongotest 唯一索引数组语义与真实 Mongo 不一致 | 已修复、声明场景验证，未发版 |

**10-05 N08 codegen（revn08）：NC-70～74 五个 P3 与 NC-75 P2 已修复、声明场景验证，未发版（NC-75 运行时 required 待决定）。** 中断 go 命令窗口后暂存树（整份工程副本）留在工程旁；`roost id` 的错误码扫描与生成器口径不同（NC-63 残余）；`project diff` / `upgrade --dry-run` 漏列 sync 将刷新的三份应用自有配置；`roost help cfggen` 指向 tablegen 的输出目录。cfggen 运行期往返、旧工程显式 upgrade / 改名退役 / 失败回滚、Unix 信号用例与 9 条具名环境 skip 在 macOS 实跑通过。[本轮](../review/REVIEW-2026-10-05-n08-codegen.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-75](RR-20261005-NC-75.md) | P2 tablegen `ref=` 哪里都不查；`-check` 不按 schema 校验 JSON | ref / -check 已修复、声明场景验证，未发版；运行时 required 已由 [B10](../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) 实施（方案 A，未发版） |
| [RR-20261005-NC-74](RR-20261005-NC-74.md) | P3 dev-run 的 `.dev/` 与 `data/wal` 被当成应用输入，generate / sync 报 inputs changed | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-73](RR-20261005-NC-73.md) | P3 `roost id` / `add errcode` 漏别名导入定义、把注释算成占用 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-72](RR-20261005-NC-72.md) | P3 `roost help cfggen` 输出到 `configs/generated`，与 tablegen 同包重复声明 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-71](RR-20261005-NC-71.md) | P3 `project diff` / `upgrade --dry-run` 漏列 sync 刷新的应用自有配置 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-70](RR-20261005-NC-70.md) | P3 Ctrl-C 打断 go 命令后 `.roost-deps-*` / `.roost-generate-*` 残留 | 已修复、声明场景验证，未发版 |

**10-05 N07第二批：NC-65 P2与NC-64 P3已修复、声明场景验证，未发版。** 生成game-demo起global+game真实进程经`gm.config.reload`热更：成功/四种失败/rollback与flags、scene refresh读取符合契约；handler内两次读不跨代（新增控制用例，含判别反例）。确认的两条在模板层：玩家加载后Gear层与attr_final没重建（NC-65）、开关热更说明指向CSV照做无效（NC-64）。[本轮](../review/REVIEW-2026-10-05-noncore-n07b.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n07b/README.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-64](RR-20261005-NC-64.md) | P3 game-demo开关热更说明指向configs/table CSV，reload只读configs/data JSON | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-65](RR-20261005-NC-65.md) | P2 game-demo玩家加载后不按穿戴重建Gear层、attr_final为空 | 已修复、声明场景验证，未发版 |

**10-05 N09 skill 第一批（revn09）：NC-110 P2、NC-111 P2、NC-112 P2、NC-113 P3 已修复、声明场景验证，未发版。** 施法终止路径各自手写收尾：启动失败复用 cast ID 却留下排程任务、Cancel / Release 中途出错停在半终止占住施法者、失败的 policy cast 不释放槽位；combatcomponent 的 Combatant 副本共享 map。combat 状态随 Nest 回滚恢复（NC-61 同形已核对，不成立）。[本轮](../review/REVIEW-2026-10-05-n09-batch1.md)。

**10-05 N03 通信 / etcd（revn03）：NC-90 / 91 / 92 三个 P2 与 NC-93 P3，已修复、声明场景验证（含真实 NATS / etcd），未发版。** 真实 JetStream 停止时在途 handler 不被排空、回包注定丢失；轻量 RPC 被派发队列拒绝不回包并误入死信；JetStream 部署里的轻量调用被请求流截获“报错却执行”；选主 Resign 不受预算约束。[本轮](../review/REVIEW-2026-10-05-noncore-n03.md) · [证据](../review/evidence/noncore-review-20261005-n03/README.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-93](RR-20261005-NC-93.md) | P3 etcd 选主正常 Resign 不受调用方预算约束，etcd 无响应时阻塞到 session TTL | 已修复（[记录](../bugfix/RR-20261005-NC-93.md)），未发版 |
| [RR-20261005-NC-92](RR-20261005-NC-92.md) | P2 JetStream 部署里的轻量 RPC 被请求流截获：调用方得到 version 0 错误，请求却被无期限执行 | 已修复（[记录](../bugfix/RR-20261005-NC-92.md)），未发版 |
| [RR-20261005-NC-91](RR-20261005-NC-91.md) | P2 轻量 RPC 被派发队列拒绝时不回包（调用方等满超时），开可靠总线时误入死信 | 已修复（[记录](../bugfix/RR-20261005-NC-91.md)），未发版 |
| [RR-20261005-NC-90](RR-20261005-NC-90.md) | P2 Bus 停止不等在途 JetStream RPC handler，回包注定丢失、连接在 handler 底下关闭 | 已修复（[记录](../bugfix/RR-20261005-NC-90.md)），未发版 |

**10-05 N06 S1/S2/S3/S6 复核（revn06）：NC-50 P3、NC-51 P3、NC-52 P2 与 RR-20261001-06 残余已修复、声明场景验证，未发版。** 换名也释放名字已被他人提交的死建角计划、补偿失败重新计数；activity sweep 对已确认 Keys 先验证键合法/归属；versionstore RedisStore 退避后重读，消除伪 ErrConflict。[本轮](../review/REVIEW-2026-10-05-n06-revn06.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-revn06/README.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-113](RR-20261005-NC-113.md) | P3 combatcomponent Combatant / InitCombatant 共享 ElementMultipliersBP，事务外改权威状态 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-112](RR-20261005-NC-112.md) | P2 skill 失败的 policy cast 不释放槽位，下次激活变成对失败 cast 的 toggle-off | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-111](RR-20261005-NC-111.md) | P2 skill Cancel / Interrupt / Release 中途出错，cast 半终止并永久占住施法者 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-110](RR-20261005-NC-110.md) | P2 skill 启动失败复用 cast ID 却留下排程任务，旧任务落到新 cast、checkpoint 无法恢复 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-52](RR-20261005-NC-52.md) | P2 versionstore RedisStore.Update 退避后用旧值重试，伪 ErrConflict | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-51](RR-20261005-NC-51.md) | P3 activity sweep 不校验已确认 Keys，跨组写与 Delivering 永久残留（B9 补修：Delivering / PendingActivities / RetireDelivered 经统一入口） | 已修复（含残余补修，未发版） |
| [RR-20261005-NC-50](RR-20261005-NC-50.md) | P3 account 建角补偿失败不计 `rollback.failed`（B9 补修：并发释放同一死计划 `plan_released` 计两次） | 已修复（含残余补修，未发版） |
| [RR-20261001-06 残余](RR-20261001-06.md#复核后的补修2026-10-05) | P2 死建角计划只在同名重试时释放，换名永远 ErrRoleLimit | 已修复（含残余补修，未发版） |

**10-05 N01 App singleton 增量 + N06-S4 global / App 替代：RR-20261005-01 P2 已修复，未发版。** 单实例锁状态机、`run` 退出路径、`OnFail`、kitredis 后端与生成器装配本轮无新确认缺陷；activity 候选集含重复 sid 或超过 `Live` 单次上限时每个窗口都开不出来。[本轮](../review/REVIEW-2026-10-05-n01s4.md)

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-01](RR-20261005-01.md) | P2 activity.game_sids 重复 / 超上限时启动成功、此后永不开窗 | 已修复（[记录](../bugfix/RR-20261005-01.md)），v1.20.1；C4 后由活动组文件兑现（10-06 复核见记录末节，v1.23.0） |

**10-05 N02续审：NC-80/83两个P2、NC-81 P3已修复、声明场景验证，未发版；NC-82 P3合并前复核按维护者指示修复（每主体key上限+满表O(1)拒绝），声明场景验证，未发版。** 生成Webroute/Ops响应编码失败不再回2xx空体，响应开始后panic中止连接，生成TCP接入停机超时保留所有权直到排空；业务鉴权全链与连接/请求容量控制通过。[本轮](../review/REVIEW-2026-10-05-noncore-n02.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n02/README.md)。真实网关/客户端单列未验。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-80](RR-20261005-NC-80.md) | P2 httpserver.JSON先写状态再编码，编码失败成2xx空体 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-81](RR-20261005-NC-81.md) | P3 recover把响应开始后的panic改写成完整2xx、吞ErrAbortHandler | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-82](RR-20261005-NC-82.md) | P3 RateLimiter全局key表可被单主体占满、满表O(N)锁内扫描 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-83](RR-20261005-NC-83.md) | P2 生成TCP停机超时后丢所有权、重试假成功、订阅者阻塞不受ctx | 已修复（生成器）、声明场景验证，未发版 |

**10-05 N07第一批：NC-60/61/63三个P2与NC-62 P3已修复、声明场景验证，未发版。** attribute快照锁、game-demo属性层随事务回滚、属性生成器拒绝不可表示声明、errcode扫描拒绝读不懂的Define并查重名；4组反例红→绿。event/configdata本批无确认缺陷，矩阵与观察见[本轮](../review/REVIEW-2026-10-05-noncore-n07.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n07/README.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-60](RR-20261005-NC-60.md) | P2 Container.Snapshot锁外复制，与Apply/ClearDirty竞争 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-61](RR-20261005-NC-61.md) | P2 game-demo属性层不随事务回滚，下次提交持久化虚高属性 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-62](RR-20261005-NC-62.md) | P3 attribute生成器接受float/超64位/超AttrID声明 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-63](RR-20261005-NC-63.md) | P2 errcode扫描静默跳过非字面量/别名Define，不查重名 | 已修复、声明场景验证，未发版 |

**10-05 N06第三批：NC-41 P2、NC-42 P3及RR-20261001-09残余已修复、声明场景验证，未发版。** outbox并发领取复查due，公开Open拒绝畸形持久计划、sweep非法键/跨组写前隔离；7反例红→绿，含恢复/诊断/对账13新增叶子。[本轮](../review/REVIEW-2026-10-05-noncore-27.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-17/README.md)。外部Mongo/Redis/HA未验，不自动迁移坏记录。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-41](RR-20261005-NC-41.md) | P2 陈旧outbox候选绕过并发Nack退避 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-42](RR-20261005-NC-42.md) | P3 Open重投执行畸形持久Intent | 已修复、声明场景验证，未发版 |
| [RR-20261001-09残余](RR-20261001-09.md#复核后的补修2026-10-05) | 非法/跨组opening仍执行或阻塞组 | 已修复（含残余补修，未发版） |

**10-05 N06第二批：NC-39/40两个P2已修复、声明场景验证，未发版。** 原始启动身份与运行状态分离、原生完成路由写前校验；11行为反例红→绿，含兼容/事务/原生取消共26新正式叶子。[本轮](../review/REVIEW-2026-10-05-noncore-26.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-16/README.md)。旧已推进缺摘要记录不自动回填，明确启动身份冲突；N06仍部分完成。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-40](RR-20261005-NC-40.md) | P2 原生完成信封路由与payload异键仍推进Saga | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-39](RR-20261005-NC-39.md) | P2 启动幂等比较可变Data/DeadlineAt | 已修复、声明场景验证，未发版 |

**10-05 N06第一批：NC-37/38两个P2已修复、声明场景验证，未发版。** Saga第三消费者健康与Resume持久代际六反例红→绿，20新正式叶子；[本轮/进度](../review/REVIEW-2026-10-05-noncore-25.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-15/README.md)。N05本机停机交错补证后接续N06，整域仍部分完成。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-38](RR-20261005-NC-38.md) | P2 Resume代际未持久化，重读后复用旧CommandID | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-37](RR-20261005-NC-37.md) | P2 原生Nest完成消费者关闭后健康仍ok | 已修复、声明场景验证，未发版 |

**v1.20.0 已发布（2026-10-05，tag → `999dc672`）**：App 层单实例锁与 game-demo 静态绑定（[方案](../feature/APP-SINGLETON-LOCK-2026-10-05.md)），A 线 RR-20261004-10～14、DAO `//roost:dao nocoll`，B 线 RR-20261004-NC-31、RR-20261005-NC-32～36 及旧 RR-20260913-08 残余补修随本版发布；下方这些条目里的“未发版”指发布前状态。

**10-05 N05权威回填：NC-35/36两个P2与旧RR-20260913-08残余已修复、声明场景验证，未发版。** 12读取与2消费反例红→绿，含恢复/重订阅共19新正式叶子；[本轮进度](../review/REVIEW-2026-10-05-noncore-24.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-14/README.md)。N05仍场景部分完成。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261005-NC-36](RR-20261005-NC-36.md) | P2 权威读取最终L1低于最低版本仍成功 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-35](RR-20261005-NC-35.md) | P2 loader异键结果写入其他缓存视图 | 已修复、声明场景验证，未发版 |

**10-05 N05接入：新增NC-33/34两个P2，均已修复、声明场景验证，未发版。** 缓存与interest的第三层payload身份原先未绑定，10副作用反例红→绿，新增13正式叶子通过。[本轮进度](../review/REVIEW-2026-10-05-noncore-23.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-13/README.md)。N05仍场景部分完成，Mirror DTO方案未实施。

| 编号 | 问题 | 当前状态 |
| --- | --- | --- |
| [RR-20261005-NC-34](RR-20261005-NC-34.md) | P2 interest payload可续租/撤销信封之外的订阅 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-33](RR-20261005-NC-33.md) | P2 cache replica payload可写信封之外的键/版本 | 已修复、声明场景验证，未发版 |

**10-05 当前：新增 [NC-32](RR-20261005-NC-32.md) P2 已修复、声明场景验证，未发版。** 正式 DAO wire 恢复后子通知指向临时父副本，深层修改漏掉提交；9正式/6消费行为红转绿，53金样与28消费者通过。[修复](../bugfix/RR-20261005-NC-32.md) · [本轮进度](../review/REVIEW-2026-10-05-noncore-22.md)。N04本机嵌套迁移/持续CAS缺口已补，整个功能域仍部分完成；下一N05增量，外部资源/容量留项。

**10-05 当前：[NC-31](RR-20261004-NC-31.md) 已修复、声明场景验证，未发版。** 提交前复用目标装载/身份检查；12新正式、17生成消费通过。接续多DAO/CAS/真实WAL进程恢复六项review无新增确认缺陷，真实Mongo/HA等仍待验。[修复](../bugfix/RR-20261004-NC-31.md) · [范围/停点](../review/REVIEW-2026-10-05-noncore-21.md)。下方10-04“NC-31未修”为原发现时点。

**v1.19.2 已发布（2026-10-04，tag → `4ee44f34`）**：RR-20261004-08 / 09、RR-20260921-03 / 04 / 05 随本版发布。

**10-04 N04正式迁移消费：[RR-20261004-NC-31 P2](RR-20261004-NC-31.md)已确认、未修复。** 输出缺少持久提交前目标解码/身份校验：坏BSON/ID进WAL，错误字段已写成目标schema后重载失败。正式生成消费者3反例/8控制；[本轮范围/进度](../review/REVIEW-2026-10-04-noncore-20.md)、[实施方向与复跑](RR-20261004-NC-31.md)。本轮只补中文注释，未修行为。

**v1.19.1 已发布（2026-10-04，tag → `d3e69336`）**：RR-20261004-02～07（NC 修复复审确认，其中 02 / 03 / 06 是 v1.19.0 回归）与 RR-20261004-NC-30 随本版发布。

**最终接手状态（b9625f4f）**：NC-30已修、未发版；上游RR-20261004-02～07均已实施。合并后241相关叶子、12 NC-30正式、16 review、11生成消费、根包12及mongotest115叶子通过；真实etcd/Mongo对照不冒认本机验收。新W-2026-10-04-02连接drain超时重试候选留待真实NATS复现，优先于迁移接入。[最终同步记录](../review/REVIEW-2026-10-04-noncore-19.md#最后增量同步)。下方旧“未修”及RR-07待修为接手时点。

**10-04 第十批修复 / N04第六批：[NC-30 P2](RR-20261004-NC-30.md) 新确认并修复，未发版。** registry清理竞争留下无TTL孤儿；2反例/4控制转绿，12正式、六包221叶子、11正式生成消费者通过。[修复](../bugfix/RR-20261004-NC-30.md) · [16新review场景](../review/REVIEW-2026-10-04-noncore-19.md)。无新增待修RR，N04仍部分完成；下方旧“未修”散文保留历史。

| 编号 | 问题 | 当前状态 |
| --- | --- | --- |
| [RR-20261004-NC-31](RR-20261004-NC-31.md) | P2 迁移坏输出进入WAL/权威文档 | 已修复、声明场景验证，未发版 |
| [RR-20261005-NC-32](RR-20261005-NC-32.md) | P2 DAO恢复后的深层更新没有提交记录 | 已修复、声明场景验证，未发版；须重生成关联代码 |
| [RR-20261004-NC-30](RR-20261004-NC-30.md) | P2 Set/Delete清理遗漏并发schema新键 | 已修复、声明场景验证，未发版 |

**v1.19.0 已发布（2026-10-04，tag → `74e1ba39`）**：A 线 RR-20260930-12～24、RR-20261001-01～09、RR-20261004-01 与 B 线 RR-20261003/04-NC-01～29、RR-20260930-CG-12～14 随本版发布；下方已列入本版的历史条目“未发版”指发布前状态；NC-30为其后新增、仍未发版。

**最终状态更新（2026-10-04）：RR-20261004-01已由上游3bb901fb/5d386146修复，本轮独立验收通过，未发版。** 本轮原4场景在真实Redis全部转绿；13条取锁/释放未知正式回归race与Remote vet通过，另3条真实Redis集成通过。下方本轮“新wanted未修”保留发现时点，以本条及独立验收为准；三资源生成消费者、authority故障矩阵与长稳未在本机验收。 [验收证据](../review/evidence/noncore-review-20261004-18/README.md#独立验收上游修复)。

**提交前新增wanted结论：W-2026-10-04-01 → [RR-20261004-01](RR-20261004-01.md)，P2已确认未修。** versioned TryLock实际取得后丢回复遗失token，自占长TTL租约无法取回；真实Lua4场景2fail/2控制，[证据](../review/evidence/noncore-review-20261004-18/README.md#新增wanted)。与已修NC-26～29分开，留下一轮修复。

**10-04 第九批修复：NC-26～29 四项已修、声明场景验证，未发版。** [NC-26](../bugfix/RR-20261004-NC-26.md) · [NC-27](../bugfix/RR-20261004-NC-27.md) · [NC-28](../bugfix/RR-20261004-NC-28.md) · [NC-29](../bugfix/RR-20261004-NC-29.md) · [28正式回归/红绿](../bugfix/evidence/noncore-bugfix-20261004-09/README.md)。[N04第五批](../review/REVIEW-2026-10-04-noncore-18.md)Redis14新场景通过，无新RR，场景仍部分完成。下方“第四批未修”是历史时点。

**10-04 N04第四批：NC-26～29四个P3已确认、未修，仅Mongo测试替身。** [运行](../review/REVIEW-2026-10-04-noncore-16.md) · [13叶子7fail/6控制](../review/evidence/noncore-review-20261004-16/README.md) · [机制/实施方案](../review/IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-26](REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-26) | P3 BSON.D嵌套路径查询漏匹配、set丢兄弟、unset未执行 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-26.md) |
| [RR-20261004-NC-27](REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-27) | P3 唯一索引建立接受已有重复数据 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-27.md) |
| [RR-20261004-NC-28](REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-28) | P3 bulk非法Type预检前提交合法前缀 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-28.md) |
| [RR-20261004-NC-29](REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-29) | P3 A全库abort擦除B已提交及新集合数据 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-29.md) |

**10-04第八批修复：NC-21～25 5/5已修、声明场景验证，未发版。** [40新增正式叶子/红绿/真实生成消费](../bugfix/evidence/noncore-bugfix-20261004-08/README.md)，十包race/vet、扩展消费者回归通过；17扩展环境skip留项。下方“第三批未修”保留历史。

**10-04 N04第三批：NC-21～25五项已确认、未修。** Lua未知结果重放一P2；mongotest浅复制、重复候选、大整数比较和ReturnAfter身份四P3。[运行/40源文范围](../review/REVIEW-2026-10-04-noncore-14.md) · [机制/实施交接](../review/IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md) · [真实Redis/替身反例](../review/evidence/noncore-review-20261004-14/README.md)。十二叶子6fail/6控制，五根因；场景部分完成。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-21](REVIEW-2026-10-04-noncore-14.md#rr-20261004-nc-21) | P2 RefHMap未知Lua结果重放覆盖另一完成写且返成功 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-21.md) |
| [RR-20261004-NC-22](REVIEW-2026-10-04-noncore-14.md#rr-20261004-nc-22) | P3 mongotest浅复制破坏嵌套abort与读输出隔离 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-22.md) |
| [RR-20261004-NC-23](REVIEW-2026-10-04-noncore-14.md#rr-20261004-nc-23) | P3 _id $in快速候选重复同一文档 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-23.md) |
| [RR-20261004-NC-24](REVIEW-2026-10-04-noncore-14.md#rr-20261004-nc-24) | P3 浮点化整数使不同int64版本相等 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-24.md) |
| [RR-20261004-NC-25](REVIEW-2026-10-04-noncore-14.md#rr-20261004-nc-25) | P3 ReturnAfter重匹配旧filter返回另一文档 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-25.md) |

**10-04第七批修复：NC-16～20 5/5已修、声明场景验证，未发版。** [41正式项/红绿/真实生成消费](../bugfix/evidence/noncore-bugfix-20261004-07/README.md)，十包race/vet通过。下方“第二批未修”保留历史时点。

**10-04 N04第二批：NC-16～20五项已确认、未修。** RefHMap指针根、文本codec、nil父Patch与内部名称碰撞四P2；mongotest分页一P3。[运行/39源文范围](../review/REVIEW-2026-10-04-noncore-12.md) · [机制/实施交接](../review/IMPLEMENTATION-REFHMAP-LAYOUT-PATCH-AND-REDIS-LIFETIME.md) · [真实Redis/分页证据](../review/evidence/noncore-review-20261004-12/README.md)。十失败叶子五根因，场景仍部分完成。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-16](REVIEW-2026-10-04-noncore-12.md#rr-20261004-nc-16) | P2 RefHMap指针根Set成功后Get panic | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-16.md) |
| [RR-20261004-NC-17](REVIEW-2026-10-04-noncore-12.md#rr-20261004-nc-17) | P2 指针TextMarshaler未执行导致编码/解码不一致 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-17.md) |
| [RR-20261004-NC-18](REVIEW-2026-10-04-noncore-12.md#rr-20261004-nc-18) | P2 nil父结构Patch返回成功但更新不可见 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-18.md) |
| [RR-20261004-NC-19](REVIEW-2026-10-04-noncore-12.md#rr-20261004-nc-19) | P2 root/__keys内部名称碰撞改变业务值 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-19.md) |
| [RR-20261004-NC-20](REVIEW-2026-10-04-noncore-12.md#rr-20261004-nc-20) | P3 公开mongotest漏/误应用分页Skip | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-20.md) |

**10-04 第六批修复：NC-13～15 3/3已修、声明场景验证，未发版。** [运行](../review/REVIEW-2026-10-04-noncore-11.md) · [41正式项/红绿/真实消费者](../bugfix/evidence/noncore-bugfix-20261004-06/README.md)。下方“第一批未修”保留历史时点。

**10-04 N04第一批：三个新RR已确认、未修。** [运行/20源文件范围](../review/REVIEW-2026-10-04-noncore-10.md) · [机制/实施方向](../review/IMPLEMENTATION-CACHE-ADMISSION-AND-MIGRATION.md) · [普通反例与专属真实Redis](../review/evidence/noncore-review-20261004-10/README.md)。9普通失败对应3根因，真实Redis追加2失败同属NC-15；7原环境skip已逐项补测通过。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-13](REVIEW-2026-10-04-noncore-10.md#rr-20261004-nc-13) | P2 ReadThrough Get/Delete吞掉fatal一致性裁决 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-13.md) |
| [RR-20261004-NC-14](REVIEW-2026-10-04-noncore-10.md#rr-20261004-nc-14) | P2 Layered交付L1一致性门禁拒绝的回填 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-14.md) |
| [RR-20261004-NC-15](REVIEW-2026-10-04-noncore-10.md#rr-20261004-nc-15) | P3 四Store拒绝旧版本写入却返回成功 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-15.md) |

**10-04 第五批修复：NC-11/12 2/2已修、声明场景验证，未发版。** [NC-11](../bugfix/RR-20261004-NC-11.md) · [NC-12](../bugfix/RR-20261004-NC-12.md) · [15正式项/红绿](../bugfix/evidence/noncore-bugfix-20261004-05/README.md)。下方原N03审查散文保留其修前时点。

**10-04 N03第五批：NC-11/12两个新P2已确认、未修。**[运行/范围](../review/REVIEW-2026-10-04-noncore-08.md) · [反例与控制](../review/evidence/noncore-review-20261004-08/README.md) · [生命周期学习与方向](../review/IMPLEMENTATION-ETCD-SNAPSHOT-WATCH-AND-LIFETIME.md)。N03源文39/39已读、场景部分完成。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-11](REVIEW-2026-10-04-noncore-08.md#rr-20261004-nc-11) | P2 Campaign取消不传递到session LeaseGrant | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-11.md) |
| [RR-20261004-NC-12](REVIEW-2026-10-04-noncore-08.md#rr-20261004-nc-12) | P2 WatchCallback关闭预算被第三方watcher.Close阻塞 | 已修复、声明场景验证，未发版；[记录](../bugfix/RR-20261004-NC-12.md) |

**10-04 第四批修复：NC-08～10 3/3已修、声明场景验证，未发版。**[运行](../review/REVIEW-2026-10-04-noncore-07.md) · [正式红绿/消费](../bugfix/evidence/noncore-bugfix-20261004-04/README.md)。下方N03第四批“未修”是原审查时点。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-08](REVIEW-2026-10-04-noncore-06.md#rr-20261004-nc-08) | JS无handler回包缺少协议envelope | [已修/声明场景验证](../bugfix/RR-20261004-NC-08.md)，未发版 |
| [RR-20261004-NC-09](REVIEW-2026-10-04-noncore-06.md#rr-20261004-nc-09) | callback停止等待无预算 | [已修/声明场景验证](../bugfix/RR-20261004-NC-09.md)，未发版 |
| [RR-20261004-NC-10](REVIEW-2026-10-04-noncore-06.md#rr-20261004-nc-10) | 服务发现绕过配置timeout | [已修/声明场景验证](../bugfix/RR-20261004-NC-10.md)，未发版 |

**10-04 N03 接续审查：NC-08～10 三个新 P2 已确认、未修。**[运行与范围](../review/REVIEW-2026-10-04-noncore-06.md) · [反例/控制](../review/evidence/noncore-review-20261004-06/README.md) · [机制与实施方向](../review/IMPLEMENTATION-MESSAGING-RPC-BUDGET-AND-TERMINAL-OWNERSHIP.md)。本批17项为4失败/13控制，归为3根因；下方NC-05～07修复不含这三项。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-08](REVIEW-2026-10-04-noncore-06.md#rr-20261004-nc-08) | P2 JetStream 无 handler 回包缺少协议 envelope | 已确认、未修 |
| [RR-20261004-NC-09](REVIEW-2026-10-04-noncore-06.md#rr-20261004-nc-09) | P2 Assembly.Close 的 callback 排空绕过 ctx | 已确认、未修 |
| [RR-20261004-NC-10](REVIEW-2026-10-04-noncore-06.md#rr-20261004-nc-10) | P2 服务发现不受配置 call timeout 约束 | 已确认、未修 |

**10-04 第三批修复更新：NC-05～07 3/3 已修、声明场景验证，未发版。**[运行](../review/REVIEW-2026-10-04-noncore-05.md) · [红绿/消费者](../bugfix/evidence/noncore-bugfix-20261004-03/README.md)。下方 N02“未修”为原审查时点。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-05](REVIEW-2026-10-04-noncore-04.md#rr-20261004-nc-05) | P2 限流超额需求占 key | [已修/声明场景验证](../bugfix/RR-20261004-NC-05.md)，未发版 |
| [RR-20261004-NC-06](REVIEW-2026-10-04-noncore-04.md#rr-20261004-nc-06) | P2 Recover report panic 外逃 | [已修/声明场景验证](../bugfix/RR-20261004-NC-06.md)，未发版 |
| [RR-20261004-NC-07](REVIEW-2026-10-04-noncore-04.md#rr-20261004-nc-07) | P2 非法生成路由注册 panic | [已修/声明场景验证](../bugfix/RR-20261004-NC-07.md)，未发版 |

**10-04 N02 接续审查：三个新 P2 已复现、未修。**[运行与范围](../review/REVIEW-2026-10-04-noncore-04.md) · [复跑/正式生成消费者](../review/evidence/noncore-review-20261004-04/README.md) · [机制和实施建议](../review/IMPLEMENTATION-REQUEST-ADMISSION-AND-GENERATED-WEBROUTES.md)。下方四项修复不包含本批三个新问题。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-05](REVIEW-2026-10-04-noncore-04.md#rr-20261004-nc-05) | P2 超 burst 的拒绝请求占用限流 key 名额 | 已复现、未修 |
| [RR-20261004-NC-06](REVIEW-2026-10-04-noncore-04.md#rr-20261004-nc-06) | P2 Recover 的 report panic 外逃 | 已复现、未修 |
| [RR-20261004-NC-07](REVIEW-2026-10-04-noncore-04.md#rr-20261004-nc-07) | P2 非法路由生成成功，注册时 panic | 已复现、未修 |

**10-04 第二批修复更新：RR-20261004-NC-01～04 4/4 已修、声明场景验证，未发版。**[原问题](REVIEW-2026-10-04-noncore-02.md) · [修复运行](../review/REVIEW-2026-10-04-noncore-03.md) · [正式红绿](../bugfix/evidence/noncore-bugfix-20261004-02/README.md)。原14项转绿，正式35项/11包race/vet通过；原审查11个失败与Health契约观察保留，后者尚非RR。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261004-NC-01](REVIEW-2026-10-04-noncore-02.md#rr-20261004-nc-01) | P2 Manager 停止 panic 跳过清理 / 回滚丢失 Start 错误 | [已修/声明场景验证](../bugfix/RR-20261004-NC-01.md)，未发版 |
| [RR-20261004-NC-02](REVIEW-2026-10-04-noncore-02.md#rr-20261004-nc-02) | P2 重复 Start 重复执行 singleton 生命周期 | [已修/声明场景验证](../bugfix/RR-20261004-NC-02.md)，未发版 |
| [RR-20261004-NC-03](REVIEW-2026-10-04-noncore-02.md#rr-20261004-nc-03) | P3 Admin 空 map / 数组内 map 泄漏 schema 引用 | [已修/声明场景验证](../bugfix/RR-20261004-NC-03.md)，未发版 |
| [RR-20261004-NC-04](REVIEW-2026-10-04-noncore-02.md#rr-20261004-nc-04) | P2 Ops 取消 Shutdown 后遗忘活跃 server | [已修/声明场景验证](../bugfix/RR-20261004-NC-04.md)，未发版 |

**10-04 更新：10-03 非三大核心第一批四项 P2 已修、具名场景验证，未发版。**[修复运行](../review/REVIEW-2026-10-04-noncore-01.md) · [红/绿](../bugfix/evidence/noncore-bugfix-20261004-01/README.md) · [原审查](../review/REVIEW-2026-10-03-noncore-01.md) · [后续计划](../review/NONCORE-REVIEW-PLAN-2026-10-03.md)。原 30 场景全部转绿，邻接对照通过；历史原12个失败保留。

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| [RR-20261003-NC-01](REVIEW-2026-10-03-noncore-01.md#rr-20261003-nc-01p2-单-mod-绕过依赖和输入校验) | 单 Mod 跳过缺失依赖、环和输入校验 | [已修/具名场景验证](../bugfix/RR-20261003-NC-01.md)，未发版 |
| [RR-20261003-NC-02](REVIEW-2026-10-03-noncore-01.md#rr-20261003-nc-02p2-clone-的-withtimeout-不生效) | Clone 的 WithTimeout 不生效 | [已修/具名场景验证](../bugfix/RR-20261003-NC-02.md)，未发版 |
| [RR-20261003-NC-03](REVIEW-2026-10-03-noncore-01.md#rr-20261003-nc-03p2-错误响应解码失败掩盖-http-状态和原文) | 非 2xx 坏/异型 body 丢 StatusError | [已修/具名场景验证](../bugfix/RR-20261003-NC-03.md)，未发版 |
| [RR-20261003-NC-04](REVIEW-2026-10-03-noncore-01.md#rr-20261003-nc-04p2-bindjson-接受首个-json-后的垃圾或第二个值) | 尾随垃圾/第二 JSON 仍调用业务 | [已修/具名场景验证](../bugfix/RR-20261003-NC-04.md)，未发版 |

**09-30 Codegen 第六轮修复：RR-CG-12～14 3/3 已修、具名场景已验，未发版。**[运行/消费证据](../review/REVIEW-2026-09-30-codegen-06.md) · [复跑](../bugfix/evidence/codegen-bugfix-20260930-12-14/README.md)。下方第五轮“均未修”为原 review 时点。

| 编号 | 问题 | 当前状态 |
| --- | --- | --- |
| [RR-20260930-CG-12](REVIEW-2026-09-30-codegen-05.md#rr-20260930-12) | P2 false 索引生成无用 strconv 导入 | [已修复/真实消费者验证](../bugfix/RR-20260930-CG-12.md)，未发版 |
| [RR-20260930-CG-13](REVIEW-2026-09-30-codegen-05.md#rr-20260930-13) | P2 bean 与生成函数/import 冲突 | [已修复/拒绝与正常消费者验证](../bugfix/RR-20260930-CG-13.md)，未发版 |
| [RR-20260930-CG-14](REVIEW-2026-09-30-codegen-05.md#rr-20260930-14) | P2 依赖事务丢失自动迁移回写 | [已修复/迁移与隔离验证](../bugfix/RR-20260930-CG-14.md)，未发版 |

**09-30 Codegen 第五轮：三个新 P2，均未修。**[RR-20260930-CG-12 false 索引无用导入、RR-CG-13 bean 命名冲突、RR-CG-14 依赖事务丢失自动迁移](REVIEW-2026-09-30-codegen-05.md)（原编号 12～14 与 A 线撞号，已改为 CG-）。四个隔离反例对应三个根因，四个正常/失败保护控制通过；[运行](../review/REVIEW-2026-09-30-codegen-05.md) · [复现](../review/evidence/codegen-review-20260930-05/README.md)。本轮只写文档，Codegen 尚未整体收敛。

**v1.18.0 已发布（2026-09-30，tag → `4b277176`）**：A 线 RR-20260928-15、RR-20260930-03/11 与 B 线 RR-20260929-01～34、RR-20260930-01/02/04～10 随本版发布；下方已列入本版的历史条目“未发版”指发布前状态；NC-30为其后新增、仍未发版。逐编号状态见 [ARCHIVE-2026-09-30](../review/ARCHIVE-2026-09-30.md)。

**09-30 Codegen 第四轮修复更新：**[RR-06～10](REVIEW-2026-09-30-codegen-03.md) 五项原触发已修并按声明场景验证，逐项记录见 [Attribute](../bugfix/RR-20260930-06.md)、[Event](../bugfix/RR-20260930-07.md)、[Webroute](../bugfix/RR-20260930-08.md)、[Tablegen](../bugfix/RR-20260930-09.md)、[Errcode](../bugfix/RR-20260930-10.md)。旧 manifest 手工判定、正式 HTTP 启动消费、旧客户端兼容仍待处理；未发版。下方“未修”保留第三轮历史时点。

**09-30 Codegen 第三轮：**[RR-04/05](REVIEW-2026-09-30-codegen-02.md) 原触发已[修复](../bugfix/RR-20260930-04.md)/[Nest 修复](../bugfix/RR-20260930-05.md)；[新 RR-06～10](REVIEW-2026-09-30-codegen-03.md) 包含 Attribute、Event、Webroute、Tablegen 退役旧产物四项 P2，Errcode 注释误提取一项 P3，均未修。[运行](../review/REVIEW-2026-09-30-codegen-03.md)。

**09-30 Codegen 第二轮：**[RR-20260930-01/02](REVIEW-2026-09-30-codegen-01.md) 原触发已修复并按声明场景验证，见 [RPC](../bugfix/RR-20260930-01.md) / [Protocol](../bugfix/RR-20260930-02.md)。新增 [RR-20260930-04 Entity、RR-20260930-05 Nest 旧生成物留存](REVIEW-2026-09-30-codegen-02.md)，两项 P2 **未修复**，已有隔离 CLI 反例。[运行](../review/REVIEW-2026-09-30-codegen-02.md)。

[RR-20261004-11](RR-20261004-11.md)：P3 归还撤离超时、租约回到服务后后台撤离仍在销毁实体，`Admit` 照常放行，登录跳过撤离直接在将被销毁的实体上进场（W-2026-10-04-04）（已修复，未发版；[修复记录](../bugfix/RR-20261004-11.md)）。（2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)）
[RR-20261004-10](RR-20261004-10.md)：**P2** playerowner `renew` 逐个“丢失 → 重新认领”每人等一份 `evictBudget`，一批丢失把刷新循环占住超过 Lease；排在后面的续租确认让本地窗口越过 Redis 键 TTL（W-2026-10-04-03）（已修复，未发版；[修复记录](../bugfix/RR-20261004-10.md)）。（2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)）
[RR-20261004-14](RR-20261004-14.md)：P3 playerowner 本地窗口从确认时刻起算（跨间断撤离 ≤5s / 慢 Refresh 越过键减保护带）；认领丢回复后下一轮 Held 续回未扔的副本（W-2026-10-04-07）（已修复，未发版；[修复记录](../bugfix/RR-20261004-14.md)）。（2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)）
[RR-20261004-13](RR-20261004-13.md)：P3 roost 跑的 go 命令（doctor / project deps / generate）超时只杀 go，compile / link / git 孙进程留在 `.roost-deps-*` / `.roost-generate-*` 暂存树，缓冲输出时 Wait 被孙进程管道拖住（W-2026-10-04-06）（已修复，未发版；[修复记录](../bugfix/RR-20261004-13.md)）。
[RR-20261004-12](RR-20261004-12.md)：P3 生成器运行期间 `os.Chdir` 整个进程，并发启动的子进程继承 `.roost-sync-*` 为工作目录（Windows 上暂存目录删不掉，W-2026-10-04-05）（已修复，未发版；[修复记录](../bugfix/RR-20261004-12.md)）。
[RR-20261004-09](RR-20261004-09.md)：**P2** RefHMap 注册表 guard 逐字节比较，无 schema 变化的并发首次创建 / 并发删除也误报 `ErrRefHMapRegistryChanged`；经 Layered 时 L1 留着已被删除的值（NC-30 引入，v1.19.1 回归）（已修复，未发版；[修复记录](../bugfix/RR-20261004-09.md)）。
[RR-20261004-08](RR-20261004-08.md)：P3 NatsMod 连接 drain 超预算（或连接已关闭）后保留已硬关闭的 Assembly，重试永远 `ErrConnectionClosed`（W-2026-10-04-02，RR-20261004-07 同族）（已修复，未发版；[修复记录](../bugfix/RR-20261004-08.md)）。
[RR-20261004-07](RR-20261004-07.md)：P3 NatsMod 停止时 Bus 超过预算后保留 bus / asm 以便重试，但 `Bus.StopWithContext` 第一次就丢了 pool 并缓存超时错误，重试永远失败，Assembly（NATS 连接、RPC 回调池）永不关闭（NC 修复复审发现）（已修复，未发版；[修复记录](../bugfix/RR-20261004-07.md)）。
[RR-20261004-06](RR-20261004-06.md)：**P2** etcd Campaign 在 session 建好后的失败 / 取消分支不再撤销 lease，候选键或领导键残留到 TTL（缺省 60s），其他候选选不上（NC 修复复审发现）（已修复，未发版；[修复记录](../bugfix/RR-20261004-06.md)）。
[RR-20261004-05](RR-20261004-05.md)：P3 mongotest 忽略 `IndexModel.Sparse`，非 sparse 唯一索引把缺字段跳过，而真实 Mongo 当作 null（替身比真实宽松）（NC 修复复审发现）（已修复，未发版；[修复记录](../bugfix/RR-20261004-05.md)）。
[RR-20261004-04](RR-20261004-04.md)：P3 `ReadThroughStore` 的 loader 回填被 L1 以 stale 拒绝时，`Get` 返回 `ErrStaleWrite`（读取因写被拒而失败）（NC 修复复审发现）（已修复，未发版；[修复记录](../bugfix/RR-20261004-04.md)）。
[RR-20261004-03](RR-20261004-03.md)：**P2** RefHMap Patch 只续期根到叶路径上的 hash，兄弟 hash 过期后 `Get` 返回部分记录且 `ok=true`（NC 修复复审发现）（已修复，未发版；[修复记录](../bugfix/RR-20261004-03.md)）。
[RR-20261004-02](RR-20261004-02.md)：**P2** `LayeredStore` 的 L1 过期后（或 ttl≤0 时）仍永久否决权威值；远端写已生效却报 `ErrStaleWrite`（NC 修复复审发现）（已修复，未发版；[修复记录](../bugfix/RR-20261004-02.md)）。
[RR-20261004-01](RR-20261004-01.md)：**P2** `versionedLock.TryLock` 取锁无明确答复（Eval 因 ctx 截止 / 网络错误返回而脚本已在 Redis 执行）时不记 token，实体本进程内不可写直到 LockTTL（缺省 24h）（W-2026-10-04-01，harness 区间核验负对照暴露）（已修复，未发版；[修复记录](../bugfix/RR-20261004-01.md)，T-207）。
[RR-20261001-09](RR-20261001-09.md)：P3 activity 无 Intent 的 legacy Opening 永久占名额；单条坏 Intent 让整组 `AdvanceExpired` 每 tick 失败（W-2026-10-01-04 拍板）（已修复，含10-05残余补修未发版；[修复记录](../bugfix/RR-20261001-09.md)，T-181）。
[RR-20261001-08](RR-20261001-08.md)：P3 chat 无游标最新页在正常容量淘汰后也报 `Gap=true` 并每次打指标（W-2026-10-01-03 拍板）（已修复，未发版；[修复记录](../bugfix/RR-20261001-08.md)）。
[RR-20261001-07](RR-20261001-07.md)：P3 game-demo `Claim` 认领成功但副本扔不掉时，下一轮 `Refresh` 清掉 `interrupted`、stale 副本又被 `Admit` 放行（W-2026-10-01-02 拍板）（已修复，未发版；[修复记录](../bugfix/RR-20261001-07.md)）。（2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)）
[RR-20261001-06](RR-20261001-06.md)：**P2** account 建角 pending slot 没有 owner-only 释放入口，名字被他人拿走后该账号在该区服永久建不了角色（W-2026-10-01-01 拍板）（已修复，未发版；[修复记录](../bugfix/RR-20261001-06.md)，T-182）。
[RR-20261001-05](RR-20261001-05.md)：**P2** activity `applyProgress` 在 ledger 条目被 TTL 删除后永不回收 `PendingRequestIDs`，32 个孤儿后该参与者每个新请求永久 `ErrConflict`，无对账入口（B 线 service 修复复审，RR-20260929-02 修法引入）（已修复，未发版；[修复记录](../bugfix/RR-20261001-05.md)，T-180）。
[RR-20261001-04](RR-20261001-04.md)：P3 v1 manifest 工程有手写 JSON 时生成链路失败于 `untracked table JSON … migrate it explicitly`，无文档无命令说明迁移（B 线 codegen 修复复审 D2）（已修复：错误文本给出恢复步骤 + 参考文档 §9.1，未发版；[修复记录](../bugfix/RR-20261001-04.md)）。
[RR-20261001-03](RR-20261001-03.md)：**P2** schema 已写、`configs/table` 还没有 CSV 的工程在 v1.18.0 上 `generate` / `sync` / `--check` 全部失败——`153cac3d` 让空 CSV 目录也跑 tablegen，v1.17.2 通过（B 线 codegen 修复复审 D1，回归）（已修复，未发版；[修复记录](../bugfix/RR-20261001-03.md)）。
[RR-20261001-02](RR-20261001-02.md)：P3 `service/mail` `sameSendIntent` 用 `reflect.DeepEqual`，nil 与空切片判为不同，自定义 EnvelopeStore 下同 RequestID 恢复永久 `ErrConflict`（B 线 service 修复复审）（已修复，未发版；[修复记录](../bugfix/RR-20261001-02.md)）。
[RR-20261001-01](RR-20261001-01.md)：**P2** ci.yml 的 Redis job 只设 `REDIS_ADDR`，09-29 service 修复的 Redis 变体（`ROOST_REVIEW_REDIS` / `ROOST_REDIS_TEST_ADDR` / `ROOST_REVIEW3_BACKEND` / `ROOST_REVIEW4_BACKEND`，9 个测试文件）静默 SKIP 或落回 Memory（B 线修复复审发现）（已修复，含残余补修 `ROOST_BUGFIX5_BACKEND`，未发版；[修复记录](../bugfix/RR-20261001-01.md)）。
[RR-20260930-24](RR-20260930-24.md)：P3 game-demo 活动租约丢失后永不重取，`activity: lease not renewed` 每 5s 一条直到停机（B27 第 3 批真实环境暴露）（已修复，未发版；[修复记录](../bugfix/RR-20260930-24.md)）。（2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)）
[RR-20260930-23](RR-20260930-23.md)：**P2** 进程续租中断后重取租约，仍连着的玩家被当作过期副本 Destroy，连接活着却脱离场景，Rebind 在此路径无效（B27 第 3 批真实环境暴露，RR-20260927-23 / RR-20260926-70 后续）（已修复，未发版；[修复记录](../bugfix/RR-20260930-23.md)，T-179）。（2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)）
[RR-20260930-22](RR-20260930-22.md)：**P2** game-demo gift 发放的收件人检查读 `dataengine.database`，而 Player DAO 固定 `db=game`；库名一改所有赠礼都被补偿（B27 第 3 批真实环境暴露）（已修复，未发版；[修复记录](../bugfix/RR-20260930-22.md)）。
[RR-20260930-21](RR-20260930-21.md)：**P2** 提交后释放 Redis 锁失败，`versionedLock` 本地 `acquired` 不清，同一实体在本进程内永久 `versioned lock already acquired`（B27 第 2 批端到端暴露，RR-20260926-46 后续）（已修复，未发版；[修复记录](../bugfix/RR-20260930-21.md)，T-178）。
[RR-20260930-20](RR-20260930-20.md)：**P2** handler 业务失败后 release hook panic，回复丢失业务错误、只剩 `release hook failed after commit`（B27 第 2 批端到端暴露，RR-20260926-53 / 32 后续）（已修复，未发版；[修复记录](../bugfix/RR-20260930-20.md)）。
[RR-20260930-18](RR-20260930-18.md)：P3 game-demo 模板 `Service.Shutdown` 用 `context.Background()` 关场景，停止重载不受 App 停机时限约束（REMAINING §3 N31，维护者 09-30 拍板）（已修复，未发版；[修复记录](../bugfix/RR-20260930-18.md)）。
[RR-20260930-17](RR-20260930-17.md)：P3 生成工程 CI 只跑 `docker compose config --quiet`，RR-20260927-33 那种语法合法、语义错的 tmpfs 抓不到（REMAINING §3 N24）（已修复：生成工程自带 `deploy/docker/compose_check_test.go`，未发版；[修复记录](../bugfix/RR-20260930-17.md)）。
[RR-20260930-16](RR-20260930-16.md)：P4 `add mod` / `add saga` 给 CRLF 的服务配置追加 Mod 段时用 LF，行尾混用（REMAINING §3 N23）（已修复，未发版；[修复记录](../bugfix/RR-20260930-16.md)）。
[RR-20260930-15](RR-20260930-15.md)：P3 Guard `holding` 直接比较实体接口值，值类型且不可比较的实体实现会 panic（REMAINING N29，维护者 09-30 拍板；已修复：契约“实体必须是指针”+ `BuildEntity` / `TryAdd` 入口校验、`entity.ErrEntityNotPointer`，未发版；[修复记录](../bugfix/RR-20260930-15.md)）。
[RR-20260930-14](RR-20260930-14.md)：P3 广播 handler 内 Destroy 后同 ID 重建，`broadcastDispatch` 按 ID 释放、一把锁跨后续目标持有（REMAINING N28；已修复：每目标自己的 Guard 作用域、按实例释放，未发版；[修复记录](../bugfix/RR-20260930-14.md)）。
[RR-20260930-13](RR-20260930-13.md)：P3 `SetEntityVersion` 不做检查改写 StateVersion，同 fence 可回退（REMAINING N26；已修复：拒绝回退并改为返回 error——签名变化，未发版；[修复记录](../bugfix/RR-20260930-13.md)）。
[RR-20260930-12](RR-20260930-12.md)：**P2** 引擎 fence 后带 Remote 批次、无 effect 的 memory handler 仍以 Durability 0 直写权威（REMAINING N21；已修复：fence 后同样拒绝，`ErrNestFenced` + `ErrCommitRejected`，未发版；[修复记录](../bugfix/RR-20260930-12.md)）。
[RR-20260930-19](RR-20260930-19.md)：P3 Remote 其余 Redis 键不带部署前缀——非 authority 兼容装配的 `remote_entity:marks` 缺省键跨部署串租约；锁键 `remote_entity.lock_key` 已可配但缺省不隔离、文档未写（REMAINING §3 N25，维护者 09-30 拍板）（已修复，未发版；[修复记录](../bugfix/RR-20260930-19.md)）。
[RR-20260930-11](RR-20260930-11.md)：P3 `cache` 包 `TestReadThroughStoreCoalescesMisses` 只等到 `loads > 0` 就放行加载器，晚到的 goroutine 再加载一次（`-race -count=400` 每轮约 1% 红，测试假设过强，实现不动）（已修复，v1.18.0；[修复记录](../bugfix/RR-20260930-11.md)）。

[RR-20260930-03](RR-20260930-03.md)：**P2** `cache.AtomicLocalStore` 覆盖写无限追加时钟记录，Remote 快照 L1 缓存随写入次数线性增长（C01 24 小时堆 4.7GB，v1.10.0 起即有）（已修复，v1.18.0；[修复记录](../bugfix/RR-20260930-03.md)）。

**09-30 Codegen 第一轮：新增两个未修 P2。**[RR-20260930-01 servicerpc 删除标记后 `-check` 假通过；RR-20260930-02 protocol 清空定义后旧产物保留](REVIEW-2026-09-30-codegen-01.md)。均有 Core 当前 CLI 隔离复现；[运行](../review/REVIEW-2026-09-30-codegen-01.md) · [原始结果](../review/evidence/codegen-review-20260930-01/README.md)。下方 Service 第十一轮“无新增”是当轮事实，不代表 Codegen 新问题已修复。

**最新第十一轮：RR-34已修/已验，本轮无新增确认缺陷。**[修复与原断言](../bugfix/RR-20260929-34.md) · [本阶段Service完成结论](../review/REVIEW-2026-09-29-services-11.md)。RR-20260929-01～34均沿具名验收关闭；归档/fulfilled/真实外部资源/HA等是单列设计/验证事项，不宣称全框架无bug。下方未实施/FAIL保留历史时点。

**最新第十轮：RR-33已修/原触发及声明场景已验；新[RR-34 P2 Pipeline先缺失后写错误却返回成功](REVIEW-2026-09-29-services-10.md)未实施。**[第八批修复/兼容](../bugfix/SERVICE-BUGFIX-2026-09-29-08.md) · [运行](../review/REVIEW-2026-09-29-services-10.md)。下方“RR-33未修”为历史时点。

**最新第九轮：RR-31/32 2/2已修/声明场景验证；新[RR-33 P2 Mail Cluster多封页CROSSSLOT](REVIEW-2026-09-29-services-09.md)未实施。**[修复与升级](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md) · [service阶段收口](../review/REVIEW-2026-09-29-services-09.md)。原8/8、新正式33叶子、最终17包909事件通过；新Mail反例/Pipeline候选有真实Cluster证据。下方为历史时点。

**最新：第六批已关闭 RR-28/29/30 原触发，继续 review 新开两个 P2。**[RR-31 Activity Cluster 完成聚合但无 dispatch；RR-32 demo 购买 catalog 升级重试覆盖首次 grant](REVIEW-2026-09-29-services-08.md)，均未实施。[三项修复/兼容](../bugfix/SERVICE-BUGFIX-2026-09-29-06.md) · [第八轮运行](../review/REVIEW-2026-09-29-services-08.md)。原15/15转绿、新正式43叶子与17包826事件通过；新3控制通过/5反例失败属两个RR。下方为历史时点。

**第七轮原时点：新增三项，当时均未修复**，[RR-20260929-28 P1：迟到 ghost 退休隐藏新支付待办；RR-29 P2：Rank Cluster 无 tag 准入；RR-30 P2：Platform 空/未闭合 tag 准入](REVIEW-2026-09-29-services-07.md)。真实 Redis/三 master Cluster 6 失败反例、9 正常控制；四包 268 既有测试事件通过。[运行与实施学习](../review/REVIEW-2026-09-29-services-07.md)。原第五批 4/4 修复保持已验收状态，service 尚有三项新开问题。

**最新实施：第五批 4/4 已修复**，[RR-25/26/27、旧 Activity RR-20260914-02 残余](../bugfix/SERVICE-BUGFIX-2026-09-29-05.md)。正式回归、Memory/Redis 与生成消费通过；下方第五轮“均未实施”是原 review 时点。Activity 不再按超时回收未知名额；升级/旧数据恢复边界保留，不能等同生产迁移完成。

**最新：09-29 Service 第五轮**，[问题与实施交接](REVIEW-2026-09-29-services-05.md)。RR-20260929-25 P2 Grouping 非法 Queue panic/伪成功；RR-20260929-26 P2 Rank 加法溢出落库；RR-20260929-27 P3 Chat 时钟偏移漏清理；旧 RR-20260914-02 P2 追加 Opening 回收晚确认的容量/扫描残余。四项均未实施，有可复跑反例。[阶段完成与未验证边界](../review/SERVICE-REVIEW-COMPLETION-2026-09-29.md)。下方历史“未修”以对应 review 时点理解。

[第四批 service 修复](../bugfix/SERVICE-BUGFIX-2026-09-29-04.md)：RR-23/24 和旧 RR-20260910-02 删除残余已修，保留原反例与未验证边界。

**09-29 Service 第四轮**：[问题与交接](REVIEW-2026-09-29-services-04.md)：**RR-20260929-23 P1** 外部发货未知错误清除 pending、允许再发货/结算；**RR-20260929-24 P3** Platform 回包切片共享（仅 Memory）。另补 **RR-20260910-02 P2** 未领取直接删除后重投复活，不另编号。均未修；[Memory/Redis 反例及对照](../review/evidence/service-review-20260929-04/README.md)。旧 RR-19～22 当前源码核对与现有回归通过，新反例独立登记。

**09-29 最新 bugfix**：[RR-20260929-19～22 已实施并验证](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md)：建角持久计划恢复、Mail 取消代次、Directory 身份删除、Profile 输出所有权；794 测试/子测试事件通过，未发版。下方“第三轮新增、未实施”等描述保留原 review 时点，不代表当前状态。

**09-29 第三轮新增、未实施**：[RR-20260929-19..22](REVIEW-2026-09-29-services-03.md)，3 P2、1 P3；[复现材料](../review/evidence/service-review-20260929-03/README.md)。旧 19 项已有回归在 `83c04243` 重跑通过，以下新触发单独登记，不混淆旧修复有效范围。

| 编号 | 优先级 | 当前问题 |
| --- | --- | --- |
| [RR-20260929-19](REVIEW-2026-09-29-services-03.md#rr-20260929-19) | P2 | Account 初始 slot/角色/名字提交未知缺少恢复；Memory 与 Redis |
| [RR-20260929-20](REVIEW-2026-09-29-services-03.md#rr-20260929-20) | P2 | Mail 旧 CancelClaim 解除新一次预约；Memory 与 Redis |
| [RR-20260929-21](REVIEW-2026-09-29-services-03.md#rr-20260929-21) | P2 | Directory Cancel/Release 删除重建 ABA；四后端/操作场景 |
| [RR-20260929-22](REVIEW-2026-09-29-services-03.md#rr-20260929-22) | P3 | Account 返回 Profile 切片改写 MemoryStore，Redis 对照通过 |

过期未结算 mail 占容量、Session 并发释放回调作为设计/契约观察保留，未重复登记功能 bug。

**2026-09-29 修复更新**：上两轮 RR-20260929-01～18 及旧 RR-20260909-02 正常 Finish ABA 共 19 项已实施并通过定向回归，尚未发版。逐项状态、实际验证和旧数据升级限制见 [修复总记录](../bugfix/SERVICE-BUGFIX-2026-09-29.md)。下方“未实施”是原 review 时点的历史结论。

09-29 Service 第二轮：[8 个新增问题与实施方案](REVIEW-2026-09-29-services-02.md)（RR-11..18，6 P2、2 P3）· [复现](REPRO-2026-09-29-services-02.md)。另补旧 [RR-20260909-02 正常 Finish/真实 Redis ABA](REVIEW-2026-09-09-02.md#2026-09-29-normal-finish-aba)，不重复编号。均未在本轮修生产代码。

| 编号 | 优先级 | 结论（本轮未实施） |
| --- | --- | --- |
| [RR-20260929-11](REVIEW-2026-09-29-services-02.md#rr-20260929-11) | P2 | account 已验证身份键碰撞 |
| [RR-20260929-12](REVIEW-2026-09-29-services-02.md#rr-20260929-12) | P2 | account slot 提交未知时误删角色、遗留占位 |
| [RR-20260929-13](REVIEW-2026-09-29-services-02.md#rr-20260929-13) | P2 | global 迁移后旧路由 lease 可续期 （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20260929-14](REVIEW-2026-09-29-services-02.md#rr-20260929-14) | P2 | session Attach 接受伪造释放标记 |
| [RR-20260929-15](REVIEW-2026-09-29-services-02.md#rr-20260929-15) | P2 | activity 非负累加回绕为负 |
| [RR-20260929-16](REVIEW-2026-09-29-services-02.md#rr-20260929-16) | P2 | mail 正文写失败后同请求无法恢复 |
| [RR-20260929-17](REVIEW-2026-09-29-services-02.md#rr-20260929-17) | P3 | global Load 输出引用污染 MemoryStore （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20260929-18](REVIEW-2026-09-29-services-02.md#rr-20260929-18) | P3 | session Finish 重试成功仍留 claim |

09-29 Core 全 service 审查：[问题与实施交接](REVIEW-2026-09-29-services.md) · [9 项反例及源码](REPRO-2026-09-29-services.md)。本轮未修生产代码；RR-09 为源码接线缺口，其他九项动态复现。

| 编号 | 优先级 | 结论（未修复/未实施） |
| --- | --- | --- |
| [RR-20260929-01](REVIEW-2026-09-29-services.md#rr-20260929-01) | P1 | platform 迟到发货覆盖人工结算终态 |
| [RR-20260929-02](REVIEW-2026-09-29-services.md#rr-20260929-02) | P1 | activity 未确认进度证明被 ring 淘汰后重复计入 |
| [RR-20260929-03](REVIEW-2026-09-29-services.md#rr-20260929-03) | P2 | account 跨服建角补偿释放已有名字 |
| [RR-20260929-04](REVIEW-2026-09-29-services.md#rr-20260929-04) | P2 | mail 过期不可见条目占满邮箱 |
| [RR-20260929-05](REVIEW-2026-09-29-services.md#rr-20260929-05) | P2 | rank 逗号 requestID 不能去重 |
| [RR-20260929-06](REVIEW-2026-09-29-services.md#rr-20260929-06) | P2 | rank 并发 no-op 覆盖去重记录 |
| [RR-20260929-07](REVIEW-2026-09-29-services.md#rr-20260929-07) | P2 | chat 自定义频道键碰撞私聊键 |
| [RR-20260929-08](REVIEW-2026-09-29-services.md#rr-20260929-08) | P2 | activity 最后 ACK 窗口被立即重试关闭 |
| [RR-20260929-09](REVIEW-2026-09-29-services.md#rr-20260929-09) | P2 | session sweep 缺少公开 owner 来源接线 |
| [RR-20260929-10](REVIEW-2026-09-29-services.md#rr-20260929-10) | P3 | rank Around 最大半径与页上限矛盾 |

[RR-20260928-15](RR-20260928-15.md)：P4 生成 TCP / scene 测试未等会话登记（framework-compat 偶发）（已修复，v1.18.0）。

[RR-20260928-14](RR-20260928-14.md)：P3 `codegen/internal/roost` 在 Windows CI 超时（已修复，v1.17.2）。

[RR-20260928-13](RR-20260928-13.md)：P4 CRLF Secret 示例被静默跳过（已修复，v1.17.2）。

[RR-20260928-12](RR-20260928-12.md)：P4 回滚时新进程按旧 unit 的 TimeoutStopSec 被停（已修复，v1.17.2）。

[RR-20260928-11](RR-20260928-11.md)：P2 pipelined 回退 strict 路径时 `WAL.Append` 不等 fsync（已修复，v1.17.2）。

[RR-20260928-10](RR-20260928-10.md)：P3 install.sh 升级失败自动回滚 / rollback.sh 回不到旧 release（已修复，v1.17.2）。

[RR-20260928-09](RR-20260928-09.md)：P2 pipelined + Remote 明确拒绝不回滚、不卸载、Sync 门冻结（已修复，v1.17.2）。

[RR-20260928-08](RR-20260928-08.md)：P4 strict 下 tracker 淘汰后 Overloaded 被误标 `ErrRemotePartRejected`（已修复，v1.17.2）。

[RR-20260928-07](RR-20260928-07.md)：P3 k8s Secret 示例缺 `saga` / `player_access`（已修复，v1.17.2）。

[RR-20260928-06](RR-20260928-06.md)：P3 game-demo 生产示例缺 `game_route` / `activity` / `platform`（已修复，v1.17.2）。

[RR-20260928-05](RR-20260928-05.md)：P2 shell / systemd 安装缺 `configs/data`（已修复，v1.17.2）。

[RR-20260928-04](RR-20260928-04.md)：P3 stats_log 在容器 / systemd 里不落盘也不告警（已修复，v1.17.2）。

[RR-20260928-03](RR-20260928-03.md)：P3 本地已提交、Remote 明确拒绝时回复无哨兵（已修复，v1.17.2）。

[RR-20260928-02](RR-20260928-02.md)：P4 Guard 跨 Manager 同 ID 时 RR-21 误判（已修复，v1.17.2）。

[RR-20260928-01](RR-20260928-01.md)：P4 多 ManagerAccess 下积压 gauge 互相覆盖（已修复，v1.17.2）。

[RR-20260927-35](RR-20260927-35.md)：P4 demo 模板测试期望流名未含 `roost.room` 兼容映射（已修复，v1.17.2）。

[RR-20260927-34](RR-20260927-34.md)：P2 生产镜像缺 `configs/data`，容器启动即失败（已修复，v1.17.2）。

[RR-20260927-33](RR-20260927-33.md)：P2 生成 compose 的 `tmpfs` 被逗号拆开，服务起不来（已修复，v1.17.2）。

[RR-20260927-32](RR-20260927-32.md)：P4 提交前拒绝不带 `ErrCommitRejected`，判别表表述（已修复，v1.17.2）。

[RR-20260927-31](RR-20260927-31.md)：P3 Cast 捕获失败被吞后事务照常提交（已修复，v1.17.2）。

[RR-20260927-30](RR-20260927-30.md)：P4 含装着 func 的接口字段的 Mutex 仍 panic（已修复，v1.17.2）。

[RR-20260927-29](RR-20260927-29.md)：P4 领头方正常完成时不关 `leaderAway`，迟到 `RunLocal` 永久阻塞（已修复，v1.17.2）。

[RR-20260927-28](RR-20260927-28.md)：P4 `RetractSyncSubject` 锁外比对后误注销新登记（已修复，v1.17.2）。

[RR-20260927-27](RR-20260927-27.md)：P4 非 Nest 持锁领头方冷加载死锁（已修复，v1.17.2）。

[RR-20260927-26](RR-20260927-26.md)：P3 `ReleaseCast(旧实例)` 放掉同 ID 新实例的锁（已修复，v1.17.2）。

[RR-20260927-25](RR-20260927-25.md)：P4 不可比较的自定义 Mutex 比较 panic（已修复，v1.17.2）。

[RR-20260927-24](RR-20260927-24.md)：P4 strict 截止回复缺 `ErrRemotePersistenceIndeterminate`（已修复，v1.17.2）。

[RR-20260927-23](RR-20260927-23.md)：P4 demo 场景未接 `OnEntityLoaded → Rebind`（已修复，v1.17.2）。

[RR-20260927-22](RR-20260927-22.md)：P4 `Unregister` 作用在已 forget 的旧 subject 上（已修复，v1.17.2）。

[RR-20260927-21](RR-20260927-21.md)：P4 同 Guard 自我撤销后再建同 ID 空转重排（已修复，v1.17.2）。

[RR-20260927-20](RR-20260927-20.md)：P4 nest 单用例 `-count>1` panic duplicate handler（已修复，v1.17.2）。

[RR-20260927-19](RR-20260927-19.md)：P4 场景重开复制会话失败无计数（已修复，v1.17.2）。

[RR-20260927-18](RR-20260927-18.md)：P3 demo 场景 Manager 未接卸载后重载（已修复，v1.17.2）。

[RR-20260927-17](RR-20260927-17.md)：P3 Remote L2 快照键不带部署前缀（已修复，v1.17.2）。

[RR-20260927-16](RR-20260927-16.md)：P4 saga 收件箱 `markCompleted` 失败静默（已修复，v1.17.2）。

[RR-20260927-15](RR-20260927-15.md)：P3 同 fence 下 Remote 版本向量可回退（已修复，v1.17.2）。

[RR-20260927-14](RR-20260927-14.md)：P4 卸载后重载最坏延迟文档不实（已修复，v1.17.2）。

[RR-20260927-13](RR-20260927-13.md)：P4 卸载后重载 / 共享加载超时参数没接 kit 配置（已修复，v1.17.2）。

[RR-20260927-12](RR-20260927-12.md)：P4 撤销新建实体用 `DestroyReasonCommon`（已修复，v1.17.2）。

[RR-20260927-11](RR-20260927-11.md)：P3 `CreateInScope` 捕获失败被吞掉仍提交（已修复，v1.17.2）。

[RR-20260927-10](RR-20260927-10.md)：P4 `RegisterEntityKindDefs` 出错留半批（已修复，v1.17.2）。

[RR-20260927-09](RR-20260927-09.md)：P3 Remote 托管 + sid 作用域 DAO 提交路径不拒绝（已修复，v1.17.2）。

[RR-20260927-07](RR-20260927-07.md)：P3 嵌套独立事务裸 mutation 绕过 RR-74（已修复，v1.17.2）。

[RR-20260927-06](RR-20260927-06.md)：P3 fence 之后外层自己的事务仍交给 committer（已修复，v1.17.2）。

[RR-20260927-05](RR-20260927-05.md)：P3 player TCP Mod 不声明停机预算（已修复，v1.17.2）。

[RR-20260927-04](RR-20260927-04.md)：P4 doctor 非正时长判定与运行时不一致、建议值口径（已修复，v1.17.2）。

[RR-20260927-03](RR-20260927-03.md)：P4 demo run.sh 不传 redis.db / password，玩家 id 计数键不带前缀（已修复，v1.17.2）。

[RR-20260927-02](RR-20260927-02.md)：P4 生成的 TCP `CloseSessions` 恒为 0（已修复，v1.17.2）。

[RR-20260927-01](RR-20260927-01.md)：P4 Windows 上 RR-80 写失败用例注入不生效（CI 红）（已修复，v1.17.2）。 来源：[OPEN-ITEMS-2026-09-27](../review/OPEN-ITEMS-2026-09-27.md)、[第五轮审计](../review/REVIEW-2026-09-27-audit5.md)。

[RR-20260926-85](RR-20260926-85.md)：P4 RR-79 释放通知删掉重开后被撤销的 Direct 绑定（已修复，v1.17.1）。

[RR-20260926-84](RR-20260926-84.md)：P3（潜在） 收尾阶段调用 RunIsolatedTransaction 被当成消息自己的事务（已修复，v1.17.1）。 第四轮修复后独立审计 [REVIEW-2026-09-27-audit4](../review/REVIEW-2026-09-27-audit4.md)，复现 [REPRO-2026-09-26-08](REPRO-2026-09-26-08.md)。

[RR-20260926-83](RR-20260926-83.md)：P4 entity 包测试不能重复运行（已修复，v1.17.1）。

[RR-20260926-82](RR-20260926-82.md)：P4 dataengine/engine 包测试不能重复运行（已修复，v1.17.1）。

[RR-20260926-81](RR-20260926-81.md)：P3 新建冲突回滚后同 ID 新建拿到 ErrEntityRemoved（RR-48 回归偶发）（已修复，v1.17.1）。

[RR-20260926-80](RR-20260926-80.md)：P4 doctor WARN 只看一份配置、减 Mod 后 sync 不一次收敛（已修复，v1.17.1）。

[RR-20260926-79](RR-20260926-79.md)：P4 Direct 绑定表只在 Unbind 时清理（已修复，v1.17.1）。

[RR-20260926-78](RR-20260926-78.md)：P4 二次撤销后 Group / Direct 订阅永久丢失（已修复，v1.17.1）。

[RR-20260926-77](RR-20260926-77.md)：P4 “是否已提交”判别指引与停机文档不准确（已修复，v1.17.1）。

[RR-20260926-76](RR-20260926-76.md)：P3 嵌套独立事务结果未知不 fence（已修复，v1.17.1）。

[RR-20260926-75](RR-20260926-75.md)：P3（潜在） Remote 消息内嵌套独立事务提交外层批次（已修复，v1.17.1）。

[RR-20260926-74](RR-20260926-74.md)：P3（潜在） 外层回滚覆盖嵌套独立事务已提交结果（已修复，v1.17.1）。

[RR-20260926-73](RR-20260926-73.md)：P3 memory handler Cast 目标摘除后锁超时重排重复修改（已修复，v1.17.1）。 第三轮修复后独立审计 [REVIEW-2026-09-27-audit3](../review/REVIEW-2026-09-27-audit3.md)，复现 [REPRO-2026-09-26-07](REPRO-2026-09-26-07.md)。

[RR-20260926-72](RR-20260926-72.md)：P4 卸载退役中 RegisterAfterRetirement 返回值（已修复，v1.17.1）。

[RR-20260926-71](RR-20260926-71.md)：P3 按 builder.RemotePolicy 判定托管的另外两处（已修复，v1.17.1）。

[RR-20260926-70](RR-20260926-70.md)：P3 兜底 remove 后政策不重新订阅（已修复，v1.17.1）。

[RR-20260926-69](RR-20260926-69.md)：P3 forget 清除重新登记 subject 的通知器（已修复，v1.17.1）。

[RR-20260926-68](RR-20260926-68.md)：P3 ctx 临近到期的推送关闭健康连接（已修复，v1.17.1）。

[RR-20260926-67](RR-20260926-67.md)：P3 handler 内先 Destroy 再新建同 ID 未加锁发布（已修复，v1.17.1）。

[RR-20260926-66](RR-20260926-66.md)：P3 默认停机总时长不足以覆盖真实 game 服务（已修复，v1.17.1）。

[RR-20260926-65](RR-20260926-65.md)：P3 嵌套独立事务已提交后外层重排（已修复，v1.17.1）。

[RR-20260926-64](RR-20260926-64.md)：**P2（潜在）** memory handler 新建实体冲突不回滚却重排（已修复，v1.17.1）。 第二轮修复后独立审计 [REVIEW-2026-09-27-audit2](../review/REVIEW-2026-09-27-audit2.md)，复现 [REPRO-2026-09-26-06](REPRO-2026-09-26-06.md)。

[RR-20260926-63](RR-20260926-63.md)：P3 测试与契约收尾（RR-43 抖动、ack 幂等契约、规范）（已修复，v1.17.1）。

[RR-20260926-62](RR-20260926-62.md)：P3 ErrRemoteFenced 无法区分重载窗口（已修复，v1.17.1）。

[RR-20260926-61](RR-20260926-61.md)：P3 RR-37 快池拒绝时就地执行业务回调（已修复，v1.17.1）。

[RR-20260926-60](RR-20260926-60.md)：P4 RR-45 装配校验绕过（已修复，v1.17.1）。

[RR-20260926-59](RR-20260926-59.md)：P3 拒绝 / 跳过卸载后订阅者停在错误内容（已修复，v1.17.1）。

[RR-20260926-58](RR-20260926-58.md)：P3 混合事务 Remote 拒绝丢弃本地实体兴趣事实（已修复，v1.17.1）。

[RR-20260926-57](RR-20260926-57.md)：P3 GetOrCreate 瞬时 ErrEntityRemoved；flush 死分支（已修复，v1.17.1）。

[RR-20260926-56](RR-20260926-56.md)：P3 syncbus 流名不随 prefix 隔离（已修复，v1.17.1）。

[RR-20260926-55](RR-20260926-55.md)：P3 快速重连 ErrSubjectRetiring（已修复，v1.17.1）。

[RR-20260926-54](RR-20260926-54.md)：P3 共享加载被领头 ctx 截断（已修复，v1.17.1）。

[RR-20260926-53](RR-20260926-53.md)：P3 本地 strict 已提交后错误无哨兵（已修复，v1.17.1）。

[RR-20260926-52](RR-20260926-52.md)：P3 旧连接持续写失败的重同步循环（已修复，v1.17.1）。

[RR-20260926-51](RR-20260926-51.md)：P3 停机预算缩放反例，默认总时长过短（已修复，v1.17.1）。

[RR-20260926-50](RR-20260926-50.md)：P3 WAL terminal 后被跳过步骤重复排入驱逐（已修复，v1.17.1）。

[RR-20260926-49](RR-20260926-49.md)：**P2（潜在）** 已提交事务错误链含锁超时被重新准入（已修复，v1.17.1）。

[RR-20260926-48](RR-20260926-48.md)：**P2** 事务内新建实体交叉创建永久死锁；memory 模式 Create 不加锁（已修复，v1.17.1）。 修复合并后独立审计 [REVIEW-2026-09-26-audit](../review/REVIEW-2026-09-26-audit.md)，复现 [REPRO-2026-09-26-05](REPRO-2026-09-26-05.md)。

[RR-20260926-47](RR-20260926-47.md)：P3 准入判定依赖自定义 Getter 契约（已修复，v1.17.1）。

[RR-20260926-46](RR-20260926-46.md)：P3 已提交但释放失败无已提交哨兵（已修复，v1.17.1）。

[RR-20260926-45](RR-20260926-45.md)：P3 Remote 实体允许 sid 作用域 DAO（已修复，v1.17.1）。

[RR-20260926-44](RR-20260926-44.md)：P3 Prepare 失败 Abort 多占快池续行（已修复，v1.17.1）。

[RR-20260926-43](RR-20260926-43.md)：P3 versionedLock 续期重启窗口（已修复，v1.17.1）。

[RR-20260926-42](RR-20260926-42.md)：P3 dataengine.shutdown_timeout 不生效（已修复，v1.17.1）。

[RR-20260926-41](RR-20260926-41.md)：P3 零填充尾部拒绝打开（已修复，v1.17.1）。

[RR-20260926-40](RR-20260926-40.md)：P2 demo 重连时移出新连接玩家（已修复，v1.17.1）。

[RR-20260926-39](RR-20260926-39.md)：P2 被拒绝隔离的 Remote 实体无重载入口（已修复，v1.17.1）。

[RR-20260926-38](RR-20260926-38.md)：P2 finalizer 与投影器并发发布同一 Remote 事务（已修复，v1.17.1）。

[RR-20260926-37](RR-20260926-37.md)：P2 Remote strict 确认超时后 Sync 冻结、AfterCommit 丢失（已修复，v1.17.1）。

[RR-20260926-36](RR-20260926-36.md)：P2 冷登录等待投影无超时（已修复，v1.17.1）。

[RR-20260926-35](RR-20260926-35.md)：P2 handler 内 CreateInScope 不在提交边界内（已修复，v1.17.1）。

[RR-20260926-34](RR-20260926-34.md)：P2 投影在 Mongo 事务内撞键后继续读，真实 Mongo 下投影卡死（已修复，v1.17.1）。

[RR-20260926-33](RR-20260926-33.md)：P2 WAL fsync 失败后仍信任后续 fsync，checkpoint 越过未落盘数据（已修复，v1.17.1）。 v1.17.0 疑点核实 [REVIEW-2026-09-26-v1170-triage](../review/REVIEW-2026-09-26-v1170-triage.md)，复现 [REPRO-2026-09-26-04](REPRO-2026-09-26-04.md)。

[RR-20260926-32](RR-20260926-32.md)：P2 提交后 release hook panic 仍 Abort（RR-14 根因）（已修复，v1.17.0）。

[RR-20260926-31](RR-20260926-31.md)：P2 demo 闲置交还先释放租约未等投影，跨进程读旧版本（已修复，v1.17.0）。（2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)）

[RR-20260926-30](RR-20260926-30.md)：P2 saga 原生步骤本地 lease fence 过期跳过后，后续投影 fatal、重启起不来（已修复，v1.17.1）。

[RR-20260926-29](RR-20260926-29.md)：P2 RR-17 后 kit/dataengine 6 个真实 Mongo 集成测试夹具失效（已修复，v1.17.0）。

[RR-20260926-28](RR-20260926-28.md)：P2 Durability 0 从未提交的不确定结果永无结论，gate / 额度永久占用（已修复，v1.17.0）。

[RR-20260926-27](RR-20260926-27.md)：P2 快池断言 panic 在删除准入被当作不确定，整个进程 fence（已修复，v1.17.0）。

[RR-20260926-26](RR-20260926-26.md)：P2 快阶段不阻塞的冷缺失被改为 panic（已修复，v1.17.0）。

[RR-20260926-25](RR-20260926-25.md)：**P1** 生成 sender / demo 从不走 Slow，冷实体上的 saga 补偿与 GM 发放固定失败（已修复，v1.17.0）。 复核记录 [REVIEW-2026-09-26-fix-verification](../review/REVIEW-2026-09-26-fix-verification.md)，复现 [REPRO-2026-09-26-03](REPRO-2026-09-26-03.md)；RR-04/05/07/10/11/12/15/17/19/22/24 追加复核残留。

[RR-20260926-24](RR-20260926-24.md)：P2 迁移表缺 `room` / `kit/room` 映射，旧工程升级后编译失败（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-23](RR-20260926-23.md)：P2 CI 假红：FatalSuffix 测试竞态 + Windows 时钟断言（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-22](RR-20260926-22.md)：P2 demo 公会发号 upsert 含范围谓词，首次并发 duplicate key（CI demo lane 间歇红）（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-21](RR-20260926-21.md)：P2 正式装配下 Remote 并行投影未开启，Remote 性能验收未在正式装配上测得（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-20](RR-20260926-20.md)：P2 Durability 0 提交结果未知时按失败回滚并立即释放权限（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-19](RR-20260926-19.md)：P2 租约 fence 失效跳过记录后 Remote 事务悬挂，gate / 锁 / 额度永久占用（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-18](RR-20260926-18.md)：P2 `nestwal.OpenRuntime` 零值 CloseWAL 时 Shutdown 不释放 WAL（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-17](RR-20260926-17.md)：P2 停机不等待外部 Flush / ReplayPass，旧回放交出 WAL 后回退 checkpoint（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-16](RR-20260926-16.md)：P2 async 下 checkpoint 先于段 fsync，断电后 WAL 拒绝打开（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-15](RR-20260926-15.md)：P2 同 SessionID 关闭后立即重开，新会话被传输层静默丢弃（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-14](RR-20260926-14.md)：P2 持久提交后 AfterCommit 失败仍 Abort，远端实体锁外回滚、与持久层分叉（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-13](RR-20260926-13.md)：P2 Remote 提交成功但释放失败时 postRemoteCommit 被跳过，Sync 永久冻结、AfterCommit 丢失（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-12](RR-20260926-12.md)：**P1（建议）** 生成的 `syncbus:` 配置段被忽略，JetStream 静默退回普通 NATS（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-11](RR-20260926-11.md)：**P1** 已提交 Remote 事务发布失败后重放被同实体新 fence 拒绝，WAL 投影永久卡住（已修复；本地验收与限制见对应 bugfix）。

[RR-20260926-10](RR-20260926-10.md)：**P1** 卸载后在投影完成前重载读 Mongo 旧版本，后续事务投影冲突，进程 fence 且重启无法恢复（已修复；本地验收与限制见对应 bugfix）。 上线前复审记录 [REVIEW-2026-09-26-release](../review/REVIEW-2026-09-26-release.md)，复现 [REPRO-2026-09-26-02](REPRO-2026-09-26-02.md)。

[RR-20260926-09](RR-20260926-09.md)：Remote 并行投影缺少失败事务 ID，已修复并验证。

[RR-20260926-08](RR-20260926-08.md)：续行占用时快队列容量口径分叉，已修复并验证。

[RR-20260926-07](RR-20260926-07.md)：WAL 回放边界漏计，已修复并验证。

[RR-20260926-06](RR-20260926-06.md)：快池内调用框架阻塞等待入口没有 fail-fast，应直接 panic（P2，源码判定，已修复并验证）。

[RR-20260926-05](RR-20260926-05.md)：Sync 快照额度在 Push 前对全部会话预扣，RetryLater 后本窗口额度被未尝试会话占满（P2，a22c5a5 回退，已修复并验证）。

[RR-20260926-04](RR-20260926-04.md)：Sync 字节软预算下大对象只能作为窗口首个准入，被持续插队而饿死（P2，8ce21a5 起已有，已修复并验证）。

[RR-20260926-03](RR-20260926-03.md)：Slow 消息快阶段继承慢阶段 local executor，RunLocal 在快池内投递快续行并同步等待，快池饥饿死锁（P2，已修复并验证）。复现附录 [REPRO-2026-09-26](REPRO-2026-09-26.md)。

[RR-20260926-02](RR-20260926-02.md)：Nest 快阶段禁止冷加载和等待在途加载，声明目标由慢阶段预加载。

[RR-20260926-01](RR-20260926-01.md)：Sync policy 失败保留 LastError，所有已执行 Flush 失败统一记账并标记阶段。

[RR-20260925-13](RR-20260925-13.md)：冷快照预算窗口不再随晚醒持续漂移，固定边界推进且不积攒空闲额度。

[RR-20260925-12](RR-20260925-12.md)：现有对象的全量视图替换不再等待冷恢复预算，修前对象/字节/会话额度三场景均可复现。

[RR-20260925-11](RR-20260925-11.md)：补齐通用故障矩阵的 Remote 集成包，按包串行使用隔离环境。

[RR-20260925-10](RR-20260925-10.md)：同 ID 重开会话不再接收旧生命周期订阅，反向索引清理与并发回归通过。

[RR-20260925-08](RR-20260925-08.md)：修复小队列错误占用 worker 执行预算，支持 1024 并发 / 16 等待位。
[RR-20260925-09](RR-20260925-09.md)：Remote 回滚钩子 panic 仍释放 Entity 本地锁，防止卡住后续快 worker。

[RR-20260925-07](RR-20260925-07.md)：Nest 收敛快慢双池，统一显式目标 ID 顺序，慢阶段本地回滚和加载初始化回到快池。

[RR-20260925-06](RR-20260925-06.md)：Remote 获取/确认/释放隔离到慢 worker，业务逻辑和 Guard 留在 cost worker；并发回归与 80 TPS × 10 分钟全量验收通过，90 TPS 长测不达标；21/21 故障矩阵通过。

[RR-20260925-05](RR-20260925-05.md)：Remote 串行投影放大确认与 Nest 排队超时；已修复，30 分钟实测 59.997 TPS、108000 笔零错误，全量一致性与故障矩阵通过。

[RR-20260925-04](RR-20260925-04.md)：Remote 发布阶段重复开启 Mongo 只读事务；按持久 digest 直接重放已提交回执。

[RR-20260925-02](RR-20260925-02.md)：统一 MongoCommitter 默认持久许可校验，删除未发布兼容路径。
[RR-20260925-03](RR-20260925-03.md)：限制全进程慢请求堆栈采样，避免 Remote 依赖阻塞时诊断放大。

[RR-20260925-01](RR-20260925-01.md)：故障测试夹具遗留 JetStream 流耗尽预留容量；补齐关闭时删除本次独有流，清理错误使测试失败。
[RR-20260924-26](RR-20260924-26.md)：Redis 未复制写丢失导致 fence 复用；已接入 Mongo 持久权限，真实故障下 Redis 1→1、权威 fence 1→2，旧写拒绝/新写成功；当前未部署，已删除未发布迁移/弱校验分支。

09-24 Remote 集群验收：[RR-24](RR-20260924-24.md) 选主后客户端路由不刷新；[RR-25](RR-20260924-25.md) Cluster 锁前缀缺少同槽校验。已复现、修复并验证，未发布。

09-24 Remote 第三轮：[RR-23](RR-20260924-23.md) 冷准入脱离取消、多实体重复计算预算。已修复；真实多进程强杀/网络隔离与正式生成业务链路通过，见 [验收](../feature/REFACTOR-2026-09-24-remote.md#第三轮进程故障与正式业务链路)。

09-24 Remote 第二轮：[RR-19](RR-20260924-19.md) 装配启停所有权；[RR-20](RR-20260924-20.md) 旧解锁回复破坏新代状态；[RR-21](RR-20260924-21.md) Lua 锁计数精度；[RR-22](RR-20260924-22.md) 分配失败遗留无 TTL owner。已复现、修复并验证，未发布。

09-24 Remote：[RR-16](RR-20260924-16.md) FlushAll 被终态淘汰破坏；[RR-17](RR-20260924-17.md) 无效提交泄漏 pending 容量；[RR-18](RR-20260924-18.md) 真实 Redis 不能编码 RemoteChecksum。均已复现、修复并验证，未发布；[实施记录](../feature/REFACTOR-2026-09-24-remote.md)。

09-24 DataEngine 批量收尾：[RR-14](RR-20260924-14.md) 事务内 Put 重复键误重试超时；[RR-15](RR-20260924-15.md) 同批重复 ID 覆盖 digest 校验。均已复现、修复并回归，未发布。

09-24 DataEngine 恢复续审：[RR-13](RR-20260924-13.md) 慢发布耗尽 Outbox 失败后的退避窗口，已修复；[修复验证](../bugfix/RR-20260924-13.md)。

09-24 DataEngine 续审：[RR-11](RR-20260924-11.md) 修复重复/并发启停、启动取消及 WAL 回收；[RR-12](RR-20260924-12.md) 修复 Remote 确定版本冲突被无限重试。修前复现、修后 race 与真实依赖回归通过，未发布。

09-24 续审：[W-2026-09-22-02](../bugfix/W-2026-09-22-02.md) 已定位为公会 ID 跨重启复用，发号根因修复，历史冲突 WAL 保留；[RR-20260924-10](RR-20260924-10.md) Outbox 启停竞争导致 panic，已修复并通过 race。

09-24 DataEngine：[RR-20260924-09](RR-20260924-09.md)（P2）等待 Flush/重放/并发停机所有权时忽略截止时间，五场景已复现并修复；[修复与验证](../bugfix/RR-20260924-09.md)，未发布。

09-24 Nest：八项锁/提交/生命周期问题已修复，验证见各修复记录，未发布；[审查与实施记录](../feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md)。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| [RR-20260924-01](RR-20260924-01.md) | P1 | pipelined AfterAdmission 在 Entity 解锁后执行 | 已修复 → [记录](../bugfix/RR-20260924-01.md) |
| [RR-20260924-02](RR-20260924-02.md) | P1 | release hook panic 泄漏组锁和 goroutine group scope | 已修复 → [记录](../bugfix/RR-20260924-02.md) |
| [RR-20260924-03](RR-20260924-03.md) | P1 | 未启用 Sync 时动态 Cast 漏回滚、提前解锁及漏写提交水位 | 已修复 → [记录](../bugfix/RR-20260924-03.md) |
| [RR-20260924-04](RR-20260924-04.md) | P2 | 广播 release hook panic 漏归还 Touch 引用并中断后续实体 | 已修复 → [记录](../bugfix/RR-20260924-04.md) |
| [RR-20260924-05](RR-20260924-05.md) | P1 | pipelined 异步完成可能在 Entity 解锁前执行回调和回复 | 已修复 → [记录](../bugfix/RR-20260924-05.md) |
| [RR-20260924-06](RR-20260924-06.md) | P2 | Ticker 并发 Start/Stop 后仍残留运行循环 | 已修复 → [记录](../bugfix/RR-20260924-06.md) |
| [RR-20260924-07](RR-20260924-07.md) | P1 | pipelined 释放异常漏完成、漏回复或错误地回复成功 | 已修复 → [记录](../bugfix/RR-20260924-07.md) |
| [RR-20260924-08](RR-20260924-08.md) | P2 | pipelined 内联/降级完成把回调延后，吞掉 AfterCommit 失败 | 已修复 → [记录](../bugfix/RR-20260924-08.md) |

09-23：基于 M-18（`967bc69`）审查当前同步块，新确认三项问题，均已在本次工作树修复并通过定向回归与模块 race 检查，未发布。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| [RR-20260923-01](RR-20260923-01.md) | P2 | 换 profile / 撤回退订后再次退订或退役，漏发 ObjectRemove | 已修复 → [记录](../bugfix/RR-20260923-01.md) |
| [RR-20260923-02](RR-20260923-02.md) | P1 | tick 部分交付后 RetryLater，内容基线或帧时钟与客户端分叉 | 已修复 → [记录](../bugfix/RR-20260923-02.md) |
| [RR-20260923-03](RR-20260923-03.md) | P2 | 满容量合法替换对象，因 subject ID 排序误拒并关闭会话 | 已修复 → [记录](../bugfix/RR-20260923-03.md) |

09-23 收尾新增：

- [RR-20260923-04](RR-20260923-04.md)（P1）：在途交付覆盖新的会话或订阅意图；已修复，验收见关联记录。
- [RR-20260923-05](RR-20260923-05.md)（P1）：Stop/Close 等待 Flush 忽略 deadline，并允许旧循环未退出就重启；已修复，验收见关联记录。
- [RR-20260923-06](RR-20260923-06.md)（P2）：Group.AddSubject 部分失败后残留成员状态且无法重试；已修复，验收见关联记录。
- [RR-20260923-07](RR-20260923-07.md)（P2）：不同政策相互撤销订阅，Group 关闭误退役共享实体；已修复，验收见关联记录。

> **09-22 第二轮（分流 wanted）**：维护者复审 room sync 结构提出三条，登记 W-2026-09-22-04 / -05 / -06 并当场分流：
> 全部**非缺陷**，转 ARCH-08（room 同步的分层收敛：envelope sink 的 roomID 键、coordinator 的边界）与 ARCH-09（非房间的实体复制路径与 `syncTopic`），
> 同日维护者拍板：实体同步统一为一个 SyncManager，room / AOI 降为组织方式，ARCH-08 / 09 并入 **[ARCH-10](../bugfix/ARCH-10-sync-manager.md)**（未上线，帧头可改），分三批实施。无新 RR。[报告](REVIEW-2026-09-22-02.md) · [源码判定](REPRO-2026-09-22-02.md)。

> **[遗留清单](CARRYOVER.md)**：`WANTED` 待分流是 0、未修复 RR 是 0，但各 bugfix 记录末尾的"未做"里还有
> 真正的缺口。09-21 第二轮判掉了 A 类 14 条；09-22 用真实环境跑了 B 类的 B1 / B3 / B5（B5 的结果是
> RR-20260922-01，并推翻了 C 类里"3/16 缺 pos_x 已闭环"那一条），B2 / B4 仍未做（2026-09-27：B1 / B2 / B3 / B5 已在 CARRYOVER 标注关闭，B4 由 [OPEN-ITEMS-2026-09-27](../review/OPEN-ITEMS-2026-09-27.md) B38 跟踪）。每轮开始时先看它。

> **2026-09-21：框架已合成一个仓库**（core v1.16.0）。审查这套代码之前先读
> [三仓合一仓：给 review 的交接](../feature/SINGLE_MODULE_MIGRATION.md)——它写明哪些东西只是位置变了、
> 哪些**确实**改了行为、已经查过什么（重复逻辑、层次依赖已全仓扫过）、以及层次依赖该用哪个工具判定
> （**不要用知识图谱**，它会把同名符号连成调用边）。

09-22：维护者要求用本机真实环境把 CARRYOVER 里"没跑过的"跑起来。B1 / B3 / B5 跑了，B2 / B4 未做。新增 **1 个 P1、2 个 P2**——P1 那条**推翻了上一轮的一个闭环**：W-2026-09-20-02 的 3/16 缺 `pos_x` 不是 RR-20260920-06，U-0267 在位、零 subscribe 拒绝，症状照旧。[问题与实施方向](REVIEW-2026-09-22.md) · [独立复现](REPRO-2026-09-22.md)

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260922-01 | P1 | 16 观察者实跑 3–6 个客户端自己的 `pos_x` 永不到达。**09-22 下午根因定到行**：断线玩家的订阅撤不掉（core `entitysync/subscription.go:300-312` `Unsubscribe` 必须向它投递 Leave，投不到就恢复 Active），room 每次 flush 都为死会话生成帧，生成工程 `sceneLane.AdmitBatch` 逐个推、遇错整批放弃，排在死会话之后的观察者从此收不到任何帧、之前的收重复帧。不是 RR-06、不是迟到、不是脏位丢失 → [REVIEW](REVIEW-2026-09-22.md) | 已修复（U-0277，未发版）→ [bugfix](../bugfix/RR-20260922-01.md) |
| RR-20260922-02 | P2 | 故障矩阵脚本合仓后烂了三处：两个格子无测试文件、core 侧四套件因路径不存在被跳过且退出码 0、没有任何 workflow 调用它 | 已修复（U-0276，未发版）→ [bugfix](../bugfix/RR-20260922-02.md) |
| RR-20260922-03 | P2 | `service/mail/redis_integration_test.go` 五个用例没有任何 CI 步骤跑：glob 覆盖不到 `./service/mail`，"no Redis test was skipped"守卫看不见 | 已修复（U-0275，未发版）→ [bugfix](../bugfix/RR-20260922-03.md) |

09-21 第二轮：Wanted 与未修复 RR 都是 0，于是按 skill §2 的第二张表走 `CARRYOVER.md`，审查链路选在两天里挨了四个单元的 `playerowner.go.tmpl`。新增 **1 个 P1、2 个 P2**——前两条是同一处的两种代价：**归还所有权这件事，既不回头确认自己的决定还成立，也没算清它占住刷新循环多久**。[问题与实施方向](REVIEW-2026-09-21-02.md) · [独立复现](REPRO-2026-09-21-02.md)

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260921-03 | P1 | 归还租约的过程中被重新 `Claim`：`handingBack` 被 `confirmClaim` 清掉，而归还流程再也不回头看它——登录方拿到"是你的"，共享表里却已无人拥有该玩家，另一进程可装载第二份副本 | 已修复，未发版（[问题](RR-20260921-03.md) · [修复](../bugfix/RR-20260921-03.md)） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| RR-20260921-04 | P2 | `handBackBudget`(8) × `evictBudget`(5s) = 40s > `Lease`(30s)：一次归还批次能把刷新循环占住到本进程**其余所有**租约过期 | 已修复，未发版（[问题](RR-20260921-04.md) · [修复](../bugfix/RR-20260921-04.md)） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| RR-20260921-05 | P2 | 本仓 13 处 `go:generate` 的产物没有任何 CI 步骤校验，`generate --check` 只跑在生成工程那侧（已漏过一次） | 已修复，未发版（[问题](RR-20260921-05.md) · [修复](../bugfix/RR-20260921-05.md)） |

09-21：只分流合仓期间挂起的两条 Wanted，不审新链路。新增 **2 个 P2**，都出在"信息在一条没人接的返回值/流水线里断掉"：[问题与实施方向](REVIEW-2026-09-21.md) · [独立复现](REPRO-2026-09-21.md)

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260921-01 | P2 | 贡献落进哪个窗口既不返回也不记录，机器人只能对"读的时候恰好是哪个窗口"下断言，跨 300 秒边界必然红 | 已修复（U-0273，core v1.16.1）→ [bugfix](../bugfix/RR-20260921-01.md) |
| RR-20260921-02 | P2 | demo-publish 从未成功过：发布的树自带 `.github/workflows/`，Actions 的 token 不被允许推送 workflow 文件 | 已修复（U-0274，core v1.16.1）→ [bugfix](../bugfix/RR-20260921-02.md) |

09-20 第四轮：复审当天最新的三个单元（U-0267 / U-0268 / U-0269），两条都出在当天自己写的代码上。新增 **1 个 P1、1 个 P2**：[问题与实施方向](REVIEW-2026-09-20-04.md) · [独立复现](REPRO-2026-09-20-04.md)

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260920-11 | P1 | `confirm` 只在 `lastUsed` 为零时盖章，为新工作重新取得的租约带着旧时间戳，下一轮就被当成空闲连同实体一起还掉——还在跑的步骤脚下被抽空 | 已修复（U-0271，codegen v1.15.31）→ [bugfix](../bugfix/RR-20260920-11.md) （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| RR-20260920-12 | P2 | `evictBudget` 是 ctx 超时，而 `EntityManager.Destroy` 等实体锁时不读 ctx；一个忙实体能钉住整轮刷新，后面所有玩家的续租排队 | 已修复（U-0272，codegen v1.15.31）→ [bugfix](../bugfix/RR-20260920-12.md) （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |

09-20 第三轮：无挂起 Wanted，按"最近几个单元碰过的地方"审玩家所有权链（U-0258 / U-0259 / U-0267）与第十八批 Guild。新增 **1 个 P1、1 个 P2**，两条都在同一处：所有权可以结束，而**实体和租约都没有与之对应的终点**。[问题与实施方向](REVIEW-2026-09-20-03.md) · [独立复现](REPRO-2026-09-20-03.md)

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260920-09 | P1 | 租约失而复得后仍用失效期间没重新加载过的常驻 Player 实体；表里也无法分辨中间有没有别人写过 | 已修复（U-0268，codegen v1.15.29）→ [bugfix](../bugfix/RR-20260920-09.md) （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| RR-20260920-10 | P2 | 后台消费者为离线玩家取得的租约永不归还，该玩家此后只能从那一个进程登录 | 已修复（U-0269，codegen v1.15.29）→ [bugfix](../bugfix/RR-20260920-10.md) （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |

09-20 第二轮：不审新代码，只把实现侧挂起的三条 Wanted 查到根因并分流。新增 **1 个 P1、2 个 P2**：[问题与实施方向](REVIEW-2026-09-20-02.md) · [独立复现](REPRO-2026-09-20-02.md)。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260920-06 | P1 | 被拒绝的 subscribe 被丢弃且**永不重试**：兴趣系统在发出变更前就把 pair 标成已订阅，而重发只在 band 变化时发生 | 已修复（U-0267，codegen v1.15.28）→ [bugfix](../bugfix/RR-20260920-06.md) |
| RR-20260920-07 | P2 | `SmallSafeMap` 的 `MarshalBSONValue` 签名不符合驱动接口，方法从未生效，该类型被写成空文档 | 已修复（U-0265，core v1.15.18）→ [bugfix](../bugfix/RR-20260920-07.md) |
| RR-20260920-08 | P2 | `OpTimeout` 不约束每实体写闸的排队，一次远端写的总耗时可以是配置值的几十倍 | 已修复（U-0266，core v1.15.18）→ [bugfix](../bugfix/RR-20260920-08.md) |

09-20：同步到 Core `8c589a6` / Kit `19fb010` / Codegen `9bbac81`，验收 09-19 第二轮六项修复并继续实时同步、remote-managed、玩家所有权与支付恢复链。新增 **4 个 P1、1 个 P2**：[问题与实施方向](REVIEW-2026-09-20.md) · [独立复现](REPRO-2026-09-20.md) · [运行与限制](../review/REVIEW-2026-09-20.md) · [机制交接](../review/IMPLEMENTATION-DELTA-TRANSPORT-AND-LEASE-FENCING.md)。两个活动 Wanted 均已分流。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260920-01 | P1 | Remote snapshot checksum 的完整 uint64 直接写 BSON，高位为 1 时提交及 WAL 恢复失败 | 已修复（U-0261，core v1.15.15）→ [bugfix](../bugfix/RR-20260920-01.md) |
| RR-20260920-02 | P1 | latest-only datagram 覆盖已经提交 dirty 的 room delta，独有字段永久丢失 | 已修复（U-0260，core v1.15.14）→ [bugfix](../bugfix/RR-20260920-02.md) |
| RR-20260920-03 | P1 | player owner 的 GET→EXPIRE/DEL 非原子，旧 owner 可续期或删除新租约 | 已修复（U-0258，core v1.15.13 / codegen v1.15.23）→ [bugfix](../bugfix/RR-20260920-03.md) （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| RR-20260920-04 | P1 | owner 租约失效不 fence resident Player 的写入，允许两个进程同时写 | 已修复（U-0259，codegen v1.15.23）→ [bugfix](../bugfix/RR-20260920-04.md) （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](../feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| RR-20260920-05 | P2 | 不可解码 pending 不隔离，128 个最老 poison 条目可占满重试页 | 已修复（U-0262，core v1.15.17 / kit v1.14.16）→ [bugfix](../bugfix/RR-20260920-05.md) |

09-19 第二轮：重启并同步到 Core `a2e8fa0` / Kit `5116f2a` / Codegen `fde74d1`。新确认 **3 个 P1、1 个 P2**：pending 分页坏项饥饿、loader panic 污染 singleflight、Nest 缺失实体 nil 分派、activity 尝试预算与真实交付断链。[问题与实施方向](REVIEW-2026-09-19-02.md) · [复现记录](REPRO-2026-09-19-02.md) · [运行与限制](../review/REVIEW-2026-09-19-02.md)。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260919-07 | P1 | pending 先按 limit 截页再过滤，最老坏记录可永久饿死后继健康订单 | 已修复（U-0253 去掉逐条读 + U-0256 退休没有订单的条目，core v1.15.10 / kit v1.14.12）→ [bugfix](../bugfix/RR-20260919-07.md) |
| RR-20260919-08 | P1 | 实体加载 panic 不清理 flight，同实体后续请求永久等待 | 已修复（U-0254，core v1.15.10）→ [bugfix](../bugfix/RR-20260919-08.md) |
| RR-20260919-09 | P2 | Nest single/broadcast 对缺失实体 nil 调 Touch，广播中止后续 id | 已修复（U-0255，core v1.15.10）→ [bugfix](../bugfix/RR-20260919-09.md) |
| RR-20260919-10 | P1 | activity sweep 消耗 attempt 却不交付，game 只查两个窗口导致旧奖励义务不可见 | 已修复（U-0257，core v1.15.11 / kit v1.14.13 / codegen v1.15.18）→ [bugfix](../bugfix/RR-20260919-10.md) |

09-19：同步并验收最新修复。RR-20260918-05/07/08/09/10、U-0245 与 ARCH-06 的定向门通过；RR-20260918-06 更正为 **部分修复**（scene 已完成，chat presence 未接线）。K1/生命周期确认 3 个 P1；继续审查 Kit `5116f2a` / Codegen `7297f92` 的 platform 支付链，又确认 3 个恢复与履约 P1，合计 **6 个 P1**：[问题与实施方向](REVIEW-2026-09-19.md) · [复现记录](REPRO-2026-09-19.md) · [运行与限制](../review/REVIEW-2026-09-19.md)。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260919-01 | P1 | 顶层 DAO nested pointer 替换/回滚后，离开字段的对象仍能生成持久化提交 | 已修复（U-0249，codegen v1.15.15）→ [bugfix](../bugfix/RR-20260919-01.md) |
| RR-20260919-02 | P1 | 同一个 nested 指针占两个 map key 时，修改只持久化最后绑定的 key | 已修复（U-0252，codegen v1.15.16）→ [bugfix](../bugfix/RR-20260919-02.md) |
| RR-20260919-03 | P1 | 会话关闭订阅者 panic 穿出生命周期 goroutine，终止 game 进程 | 已修复（U-0247，codegen v1.15.15）→ [bugfix](../bugfix/RR-20260919-03.md) |
| RR-20260919-04 | P1 | 已持久化订单与 pending ZSet 分两次写，失败后后台无法枚举 | 已修复（U-0253，core v1.15.9 / kit v1.14.11 / codegen v1.15.16）→ [bugfix](../bugfix/RR-20260919-04.md) |
| RR-20260919-05 | P1 | 一条不可读订单使 pending 整页失败，健康订单被饿死 | 已修复（U-0248，codegen v1.15.15）；分页残余（RR-20260919-07）随 U-0253 一并消失——索引进了存储，读的时候不再逐条查订单 → [bugfix](../bugfix/RR-20260919-05.md) |
| RR-20260919-06 | P1 | 未履约付费 grant 固定 30 天后被拒绝删除，订单仍显示 delivered | 已修复（U-0251，codegen v1.15.15）→ [bugfix](../bugfix/RR-20260919-06.md) |

W-2026-09-18-11 已转 ARCH-07，不计功能 RR；配置所有权方案见[运行期配置所有权](../review/IMPLEMENTATION-RUNTIME-CONFIG-OWNERSHIP.md)。

09-18 第三轮：用户已修改，RR-20260918-03/04 原根因分别以 10/3 个独立叶子验证通过；新增 Wanted 已全部分流。确认 **3 个 P1、3 个 P2**，均未修复：[问题与实施方向](REVIEW-2026-09-18-03.md) · [复现记录](REPRO-2026-09-18-03.md) · [运行与限制](../review/REVIEW-2026-09-18-03.md)。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260918-05 | P1 | 邮件去重账本固定 31 天，合法的更长 `send_ttl` 让旧邮件再次发奖 | 已修复（U-0241，core v1.15.8 / kit v1.14.9 / codegen v1.15.11）→ [bugfix](../bugfix/RR-20260918-05.md) |
| RR-20260918-06 | P2 | 会话关闭没有生命周期事件，scene/presence 无后续流量时保留离线成员 | 已修复（U-0243 + 09-19 残余补修：chat presence 也订阅同一个源，codegen v1.15.15）→ [bugfix](../bugfix/RR-20260918-06.md) |
| RR-20260918-07 | P2 | syncTopic 裸同包常量被静默生成成常量名字符串 | 已修复（U-0239，core v1.15.8 / kit v1.14.9 / codegen v1.15.11）→ [bugfix](../bugfix/RR-20260918-07.md) |
| RR-20260918-08 | P2 | 单观察者 AOI block 数无预算，合法配置可登记 40,401 格 | 已修复（U-0240，core v1.15.8 / kit v1.14.9 / codegen v1.15.11）→ [bugfix](../bugfix/RR-20260918-08.md) |
| RR-20260918-09 | P1 | 怪物运行期 ID 进程本地发号，多 game 实例碰撞 | 已修复（U-0242，codegen v1.15.11；启动修复见 U-0244 / v1.15.12）→ [bugfix](../bugfix/RR-20260918-09.md) |
| RR-20260918-10 | P1 | 顶层 DAO map 替换/回滚后游离 nested 值仍可生成持久化 mutation | 已修复（U-0238，core v1.15.8 / kit v1.14.9 / codegen v1.15.11）→ [bugfix](../bugfix/RR-20260918-10.md) |

W-02 转 ARCH-06；W-06/07/08/09 按已实现边界或文档契约收敛，不计 RR。Wanted 原文保留历史，当前无活动待判项。RR-03/U-0236 与 RR-04/U-0237 的本轮独立验收通过不扩大到上述相邻根因。

09-18 第二轮：用户已修改，本轮恢复验收。**8项旧RR原触发通过，新增2项问题未修复**：[回滚通知归属 P2 / 副本账本清理后重复发奖 P1](REVIEW-2026-09-18-02.md) · [完整复现](REPRO-2026-09-18-02.md) · [逐项验收与限制](../review/REVIEW-2026-09-18-02.md)。旧作者修复标记与历史时点保留，最新结论以下表为准。

| 编号 | 本轮独立状态 |
| --- | --- |
| RR-20260918-03 | P2 新确认：nested map/slice撤销后通知未恢复，pointer旧值未解绑；实际后续commit漏失 |
| RR-20260918-04 | P1 新确认：4小时后另一笔领取清理账本，旧成功Run再次发奖 |
| RR-20260917-04 | 原三恢复路径通过，包括真实30秒owner tick；需配置PendingOrders |
| RR-20260917-05 | 原四通知场景通过；新回滚/替换问题另见RR-0918-03 |
| RR-20260917-06 | 新生成消费编译与属性行为测试通过；非所有profile全面验收 |
| RR-20260917-07 | 原三订阅场景通过；未跑真实broker完成链 |
| RR-20260917-08 | 原五场景经真实Nest通过；新保留期问题另见RR-0918-04 |
| RR-20260917-09 | 原两开帧场景通过；未验TCP交付 |
| RR-20260918-01 | 三种sync生成消费编译通过 |
| RR-20260918-02 | 配置水位的三入口阻挡/推进通过，消费与对照共14叶子通过 |

09-18 第二轮新增两项，实现侧状态：

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260918-03 | P2 | nested child 的通知归属在替换 / 回滚之后与字段内容不一致，恢复出来的 child 后续修改漏出持久化链 | 已修复（U-0236，未发版）→ [bugfix](../bugfix/RR-20260918-03.md) |
| RR-20260918-04 | P1 | 副本领取账本清理后旧的成功 Run 再发一次奖励（U-0226 的保留期论证不成立） | 已修复（U-0237，未发版）→ [bugfix](../bugfix/RR-20260918-04.md) |

09-18：用户声明没有修复，全部旧 RR 跳过修复验收，原状态不变。新增 [RR-20260918-01/02](REVIEW-2026-09-18.md)：sync=true 生成配置与 Core 不兼容；房间内部 coordinator 缺 durable 水位接线。Wanted-05 已分流；公开手动接入八场景通过，不能等同自动/生产端到端通过。[完整复现](REPRO-2026-09-18.md)。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260918-01 | P2 | sync=true 实体生成物与 Core 配置不兼容 | 已修复（U-0229，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260918-01.md) |
| RR-20260918-02 | P2 | RoomBroadcaster 隐藏的 coordinator 无法接入持久化水位 | 已修复（U-0233，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260918-02.md) |

09-17 第三轮：[新增 feature 与 Wanted 审查](REVIEW-2026-09-17-03.md) · [完整复现](REPRO-2026-09-17-03.md)。新增五项，**09-18 已全部修复（core v1.15.7 / kit v1.14.8 / codegen v1.15.10）**：

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260917-08 | P1 | dungeon 重放/failed/expired 仍派发经验奖励 | 已修复（U-0226，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260917-08.md) |
| RR-20260917-05 | P2 | nested DAO 第二层 child 变更不通知父级（Wanted-02） | 已修复（U-0232，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260917-05.md) |
| RR-20260917-06 | P2 | attribute feature 缺可使用的公共运行时契约（Wanted-03） | 已修复（U-0230，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260917-06.md) |
| RR-20260917-07 | P2 | 默认 Saga Assembly 不订阅原生 Nest 完成效果（Wanted-04） | 已修复（U-0231，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260917-07.md) |
| RR-20260917-09 | P3 | battle demo 宽限期不触发开帧 | 已修复（U-0228，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260917-09.md) |

U-0224 原 BSON 三项、U-0225 所在 Saga 现有 race 测试通过；不代表通知与接线契约已收敛。Wanted-05 保留观察并修正“没有公开路径”的前提，暂不登记 RR。

09-17 第二轮：[验收与新问题](REVIEW-2026-09-17-02.md)。RR-20260916-06/07、RR-20260917-02/03 原触发独立通过；RR-20260917-01 新键隔离通过，但历史键迁移四场景失败，升级仍待收敛。新增 **RR-20260917-04，P2，Kit platform 后台订单重试候选固定为空**——已修复（U-0234，core v1.15.7 / kit v1.14.8 / codegen v1.15.10）→ [bugfix](../bugfix/RR-20260917-04.md)。Wanted-01 转 [ARCH-05 实施交接](../review/IMPLEMENTATION-SERVICE-PLACEMENT-AND-RECOVERY.md)，不另计功能 RR。以下历史时点与作者修复标记保留，最新独立结论以此轮为准。

09-17：用户声明未修复，本轮跳过旧问题复核，原状态不变。新增 [三个匹配问题](REVIEW-2026-09-17.md)，附 [最小复现](REPRO-2026-09-17.md)。34 个新场景 20 通过、14 个行为失败，归为以下三个根因。

| 编号 | 等级 | 问题 | 状态 |
| --- | --- | --- | --- |
| RR-20260917-01 | P2 | 合法 Queue 键碰撞，跨队列读取与成组 | 已修复（U-0221，core v1.15.6）→ [bugfix](../bugfix/RR-20260917-01.md) |
| RR-20260917-02 | P2 | ScoreWindow 距离及窗口增长溢出 | 已修复（U-0222，core v1.15.6）→ [bugfix](../bugfix/RR-20260917-02.md) |
| RR-20260917-03 | P3 | 内存 Store 输入/输出切片共享污染状态 | 已修复（U-0223，core v1.15.6）→ [bugfix](../bugfix/RR-20260917-03.md) |

09-16 第五轮：上轮 RR-04 原四叶子通过，RR-05 按删除无效入口的方案验收；WAL Windows 原 20 场景全过。新增 [RR-20260916-06/07](REVIEW-2026-09-16-05.md)：manager 最后启动时 Stop 漏清理、首次启动期间 Register 接纳未启动对象。两项均 P2 未修复；此前“未修复清单为空”为当时记录，不适用于本轮。[交接落实情况](../review/REVIEW-2026-09-16-05.md)。

09-16 第四轮：[统一实施交接](REVIEW-2026-09-16-04.md) 新增 RR-20260916-04/05（均 P2）：Recover 删除/同位置替换绕过校验、匹配策略注入无效。七项新 bugfix 已定向验收；原触发通过、Windows 未验证分支和迁移限制分别记录。此前 Kit 职责问题作为 ARCH-01..04 实施清单，不计入功能 bug 数。

09-16 第三轮：新增 [RR-20260916-03](REVIEW-2026-09-16-03.md)，P2，JetStream 同一 bus/topic 多个本地订阅竞争持久消费者，普通消息分摊、分片无法重组。真实 server 35 场景，32 通过/3 失败；旧问题按用户声明跳过核验。

09-16 第二轮：新增 [RR-20260916-02](REVIEW-2026-09-16-02.md)，P2，不同 Prefix 共用 Stream/topic/SID 时持久消费者身份冲突。32 场景 31 通过、1 失败，Stop/在途 Subscribe 列观察项。用户声明未修复，本轮跳过旧核验。

09-16：新增 [RR-20260916-01](REVIEW-2026-09-16.md)，P2，journal 写入/发布结果不确定后继续写入导致重复序号或新追加不可恢复。28 新场景 26 通过、2 失败；用户未修复，旧问题跳过核验。

09-15 第八轮：新增 [RR-20260915-08/09](REVIEW-2026-09-15-08.md)，P2，Import/Restore 与 journal 脱节、Recover 过期快照提交。29 场景 23 通过、6 失败；用户声明没有修复，旧问题跳过核验。

09-15 第七轮：新增 [RR-20260915-06/07](REVIEW-2026-09-15-07.md)，均 P2，History 清理后复用旧 ACK 身份、WAL 半尾恢复后继续追加污染日志。20 场景 16 通过、4 失败；旧修复未验收。

09-15 第六轮：新增 [RR-20260915-05](REVIEW-2026-09-15-06.md)，P2，SetDownstream 未迁移慢消费者回调，替换后剔除不清理 room 订阅。24 场景 22 通过、2 失败；旧修复未验收。

09-15 第五轮：新增 [RR-20260915-04](REVIEW-2026-09-15-05.md)，P2，慢连接剔除后剩余批次失败导致通知丢失，重试后 room 仍保留订阅。21 新场景 19 通过、2 失败；旧修复未验收。

09-15 第四轮：[EntitySync 失败/生命周期审查](REVIEW-2026-09-15-04.md)，17 新场景通过，无新增确定 RR；同分片取消阻塞列观察项，旧修复未验收。

09-15 第三轮：旧修复未核验；新增 [RR-20260915-03](REVIEW-2026-09-15-03.md)，entitysync 新订阅/profile 切换/直接分发绕过持久化水位。

09-15 第二轮：RR-20260914-10..13、RR-20260915-01 原独立 overlay 共 24 场景已通过，[验收记录](../review/REVIEW-2026-09-15-02.md)。RR-10 的原先提交后重发已修复；重叠准备交付的不同触发另记 RR-20260915-02，不将原通过视为整体基线身份已收敛。

09-15：新增 [RR-20260915-01](REVIEW-2026-09-15.md)，满容量对象/组件替换被中间态容量检查拒绝。本轮不独立验收旧修复，保留作者状态标记。

第八轮：用户声明未修复，旧 RR-10..12 跳过复核；新增 [RR-13](REVIEW-2026-09-14-08.md)，LOD 绝对采样点与发送节奏错相导致更新停滞。

第七轮：RR-10 用户声明未修复，本轮跳过复核；新增 [RR-11/12](REVIEW-2026-09-14-07.md)，恢复屏障与单片帧长边界。

第六轮：RR-09 原四叶子通过，用户发现 U-0199 已复核；新增 [RR-20260914-10](REVIEW-2026-09-14-06.md)：P2，同 tick 投影覆盖造成 ACK 基线错配，未修复。

09-14 第五轮：RR-08 的原 4096 旧身份上限测试在 Core `a1245fd` 通过；新 RR-09 为 Bot 消费者应用边界，见[运行](../review/REVIEW-2026-09-14-05.md)。

09-14 第四轮：lockstep RR-04/05/06/07 原十个测试叶子在 Core `30a6b5b` 全过；新资源边界另立 RR-08，超 ReplayHorizon 重传仍是声明限制。[验收与证据](../review/REVIEW-2026-09-14-04.md)。

09-14 第三轮：Activity RR-20260914-02/03 的原始独立复现已在 Kit `7030c5f` 全部通过；保留作者 bugfix 与原报告，验证及未完成边界见[第三轮](../review/REVIEW-2026-09-14-03.md)。本轮新增 lockstep RR-04 至 RR-07。

[09-13 第六轮生命周期观察](REVIEW-2026-09-13-06.md)：三个测试，无新增确认 RR。

[实现侧待审查候选（Wanted）](WANTED.md)：实现 / bugfix 一侧看到但不该自己拍板的疑点，review 每轮三选一（登记 RR / 判非问题 / 再观察）。09-18 新增 10 条已全部分流，当前无待分流候选。

本目录保存只审查、不修改源码的发现。框架范围为 core、kit、codegen。
历史修复账本仍见 [history/ledger.md](../history/ledger.md)，这里使用独立 RR 编号。

[09-12 第二轮 Mirror 集成观察](REVIEW-2026-09-12-02.md)：已知声明缺口、删除水位与装配边界；无新增确认 RR。

| 编号 | 优先级 | 仓库 | 问题 | 状态 |
| --- | --- | --- | --- | --- |
| RR-20260916-07 | P2 | core/kit | 首个 manager 启动期间 Register 接纳的对象永久未启动 | 已修复（U-0220，core v1.15.6）→ [bugfix](../bugfix/RR-20260916-07.md) |
| RR-20260916-06 | P2 | core/kit | 最后一个 manager 启动期间 Stop 漏清理成功启动的对象 | 已修复（U-0219，core v1.15.6）→ [bugfix](../bugfix/RR-20260916-06.md) |
| RR-20260916-05 | P2 | kit/codegen | Grouping 注入入口未执行，生成契约与实际调用方成组不一致 | 已修复(U-0217,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5,**破坏性** `NewMod(reporter)`)→ [bugfix](../bugfix/RR-20260916-05.md) · [统一交接](REVIEW-2026-09-16-04.md) |
| RR-20260916-04 | P2 | core | Recover 删除 ABA / 同位置 Import 仍接受旧捕获 | 已修复(U-0216,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260916-04.md);原 RR-09 触发已通过 → [统一交接](REVIEW-2026-09-16-04.md) |
| RR-20260916-03 | P2 | core | JetStream 同 bus/topic 多个本地订阅竞争持久消费者,广播变分摊 | U-0209；原真实广播/分片触发独立通过 → [验收](REVIEW-2026-09-16-04.md) · [bugfix](../bugfix/RR-20260916-03.md) |
| RR-20260916-02 | P2 | core | 不同 Prefix 共用 Stream 时持久消费者身份冲突 | U-0210 命名隔离通过；Kit 默认迁移/共享 Stream 限制保留 → [验收](REVIEW-2026-09-16-04.md) · [bugfix](../bugfix/RR-20260916-02.md) |
| RR-20260916-01 | P2 | core | journal 写入/发布结果不确定后继续写入 | U-0212；不确定错误后停止准入两项独立通过，权限对照 Skip → [验收](REVIEW-2026-09-16-04.md) · [bugfix](../bugfix/RR-20260916-01.md) |
| RR-20260915-09 | P2 | core | Recover 将过期捕获提交为更新的 Full | U-0214 原交错通过；删除/Import 新触发另记 RR-04 → [验收](REVIEW-2026-09-16-04.md) · [bugfix](../bugfix/RR-20260915-09.md) |
| RR-20260915-08 | P2 | core | Import/Restore 绕过绑定 journal 的持久化替换 | U-0213；原持久化替换独立通过 → [验收](REVIEW-2026-09-16-04.md) · [bugfix](../bugfix/RR-20260915-08.md) |
| RR-20260915-07 | P2 | core | 忽略 WAL 半条尾记录后续写污染日志 | 已修复(U-0211 + Windows 截断补修,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5;Linux 由 CI linux-quality 持续验收,Windows 以 CI windows-compatibility 为准)→ [验收](REVIEW-2026-09-16-04.md) · [bugfix](../bugfix/RR-20260915-07.md) |
| RR-20260915-06 | P2 | core | 流清理后重建复用旧 ACK 身份 | U-0215；原三种清理触发独立通过 → [验收](REVIEW-2026-09-16-04.md) · [bugfix](../bugfix/RR-20260915-06.md) |
| RR-20260915-05 | P2 | core | room SetDownstream 未迁移慢消费者回调,替换后剔除不清理订阅 | 已修复(U-0208,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260915-05.md) · [第六轮](REVIEW-2026-09-15-06.md) |
| RR-20260915-04 | P2 | core | room 慢连接剔除后剩余批次失败,剔除通知丢失,重试后仍保留订阅 | 已修复(U-0207,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260915-04.md) · [第五轮](REVIEW-2026-09-15-05.md) |
| RR-20260915-03 | P2 | core | entitysync 订阅快照与直接分发绕过持久化水位 | 已修复(U-0206,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260915-03.md) · [第三轮](REVIEW-2026-09-15-03.md) |
| RR-20260915-02 | P2 | core | 重叠准备视图已交付后 Commit stale，后续 delta 仍静默分叉 | 已修复(U-0205,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260915-02.md) · [第二轮](REVIEW-2026-09-15-02.md)，关联 RR-20260914-10 |
| RR-20260915-01 | P2 | core | 满容量对象/组件替换因先创建后删除被误拒 | 已修复(U-0204,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260915-01.md) · [09-15](REVIEW-2026-09-15.md) |
| RR-20260914-13 | P2 | core | LOD 绝对 tick 采样使错相发送的组件持续保留旧值 | 已修复(U-0203,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-13.md) · [第八轮](REVIEW-2026-09-14-08.md) |
| RR-20260914-11 | P2 | core | ForceFull 被旧发送 ACK 取消 | 已修复(U-0201,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-11.md) · [第七轮](REVIEW-2026-09-14-07.md) |
| RR-20260914-12 | P3 | core | 单片重组绕过 MaxFrameBytes | 已修复(U-0202,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-12.md) · [第七轮](REVIEW-2026-09-14-07.md) |
| RR-20260914-10 | P2 | core | 同 tick 投影覆盖使延迟 ACK 绑定错误基线，后续 delta 静默漏对象 | 已修复(U-0200,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-10.md) · [第六轮](REVIEW-2026-09-14-06.md) |
| RR-20260914-09 | P2 | core | LockstepBot 回调失败后继续处理成功但丢失批次尾帧 | 已修复(U-0198,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-09.md) · [第五轮](REVIEW-2026-09-14-05.md) |
| RR-20260914-08 | P2 | core | lockstep 去重身份表在两次 Tick 间没有窗口上限 | 已修复(U-0197,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-08.md) · [第四轮](REVIEW-2026-09-14-04.md) |
| RR-20260914-04 | P2 | core | lockstep 已入帧输入迟到重传再次入帧 | 已修复(U-0193,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-04.md) · [第三轮](REVIEW-2026-09-14-03.md) |
| RR-20260914-05 | P2 | core | lockstep 每 tick 补一帧无法追上帧头 | 已修复(U-0194,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-05.md) · [第三轮](REVIEW-2026-09-14-03.md) |
| RR-20260914-06 | P3 | core | lockstep 座位 -1 与旁观者标记碰撞 | 已修复(U-0195,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-06.md) · [第三轮](REVIEW-2026-09-14-03.md) |
| RR-20260914-07 | P3 | core | lockstep 超协议座位数配置生成不可解码帧 | 已修复(U-0196,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-07.md) · [第三轮](REVIEW-2026-09-14-03.md) |
| RR-20260914-03 | P2 | kit | Activity 派发仅扫描本轮新完成，后续重试与其他完成路径被遗漏 | 已修复(U-0192,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-03.md) · [第二轮](REVIEW-2026-09-14-02.md) |
| RR-20260914-02 | P2 | kit | OpenActivity 与 sweep 交错丢失活动窗口索引 | 已修复(U-0191,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260914-02.md) · [第二轮](REVIEW-2026-09-14-02.md) |
| RR-20260914-01 | P2 | core | Close 尚未排空时 Sync 提前报告持久化成功 | 已修复(U-0190,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)，原交错及错误/取消边界复测通过 → [第二轮](../review/REVIEW-2026-09-14-02.md) · [bugfix](../bugfix/RR-20260914-01.md) · [09-14 原报告](REVIEW-2026-09-14.md) |
| RR-20260913-13 | P2 | core | LeaveShared 回复丢失后普通写重试卡在非法状态迁移 | 已修复（U-0189 同时解决，已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5），三次准入独立通过 → [第十轮](../review/REVIEW-2026-09-13-10.md) |
| RR-20260913-12 | P2 | core | EnterShared 执行后丢回复恢复独占写准入 | 已修复(U-0189,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-12.md) · [第九轮](REVIEW-2026-09-13-09.md) · [第十轮独立通过](../review/REVIEW-2026-09-13-10.md) |
| RR-20260913-09 | P2 | core | Transfer 回复丢失后恢复旧 owner 并允许写准入 | 已修复(U-0188,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-09.md) · [09-13 第三轮](REVIEW-2026-09-13-03.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260913-10 | P3 | core | Redis Lua 大版本浮点舍入破坏顺序 | 已修复(U-0177,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-10.md) · [09-13 第三轮](REVIEW-2026-09-13-03.md) · [第四轮独立验收通过](../review/REVIEW-2026-09-13-04.md) |
| RR-20260913-11 | P3 | core | 大 epoch 转科学计数法并持久化不可读 marker | 已修复(U-0178,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-11.md) · [09-13 第三轮](REVIEW-2026-09-13-03.md) · [第四轮独立验收通过](../review/REVIEW-2026-09-13-04.md) |
| RR-20260913-05 | P2 | core | L2 内容冲突被吞掉，L1/L2 同版本分叉 | 已修复(U-0180 + 第七轮残余补修,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-05.md) · [第二轮](REVIEW-2026-09-13-02.md) · [第七轮残余](REVIEW-2026-09-13-07.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260913-06 | P2 | core | L2 回填绕过同版本冲突检查 | 已修复(U-0181,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-06.md) · [09-13 第二轮](REVIEW-2026-09-13-02.md) · [第七轮适配复现通过](../review/REVIEW-2026-09-13-07.md) |
| RR-20260913-07 | P2 | core | L2 CAS 遗漏 schema/codec 冲突比较 | 已修复(U-0176,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-07.md) · [第二轮](REVIEW-2026-09-13-02.md) · [第四轮独立验收通过](../review/REVIEW-2026-09-13-04.md) |
| RR-20260913-08 | P2 | core | 快照 ExpiresAt 未参与缓存读取准入 | 已修复（U-0175，已发版 core v1.15.3；10-05 残余补修 `7949da08` 随 v1.20.0 发布）；U-0175三个历史残余通过 → [第七轮](../review/REVIEW-2026-09-13-07.md) · [bugfix](../bugfix/RR-20260913-08.md) |
| RR-20260913-01 | P2 | core | Remote 删除无版本屏障，旧删除清新值/旧值复活 | 已修复(U-0187 + 第七轮残余补修,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5；10-05 N05 跨节点 L2 墓碑残余补修 `366058a7` 随 v1.20.2 发布，[复核](REVIEW-2026-09-13.md))→ [bugfix](../bugfix/RR-20260913-01.md) · [09-13](REVIEW-2026-09-13.md) · [第七轮残余](REVIEW-2026-09-13-07.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260913-02 | P2 | core | 旧兴趣释放取消重新订阅 | 已修复(U-0184 + 同刻度播种补修,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-02.md) · [09-13](REVIEW-2026-09-13.md) · [第七轮观察](REVIEW-2026-09-13-07.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260913-03 | P2 | core | Remote payload scope 未与信封身份绑定 | 已修复(U-0179,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-03.md) · [09-13](REVIEW-2026-09-13.md) · [第四轮独立验收通过](../review/REVIEW-2026-09-13-04.md) |
| RR-20260913-04 | P2 | core | 快照加载跟随者取消后不归还等待名额 | 已修复(U-0174,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260913-04.md) · [09-13](REVIEW-2026-09-13.md) · [第四轮独立验收通过](../review/REVIEW-2026-09-13-04.md) |
| RR-20260912-01 | P2 | core | nestwal Shutdown 等待后台重放锁时忽略截止时间 | 已修复(U-0186,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260912-01.md) · [09-12](REVIEW-2026-09-12.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260912-02 | P2 | core | WAL.Sync 成功返回时已准入 ticket 仍未写入 | 已修复(U-0185,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260912-02.md) · [09-12](REVIEW-2026-09-12.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260911-06 | P2 | core | 异步 AfterCommit panic 遗漏回复/释放；饱和回退可崩溃 | 已修复(U-0183,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260911-06.md) · [第四轮](REVIEW-2026-09-11-04.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260911-05 | P3 | kit | 拒绝投递仍修改 MemoryStore 邮箱，计数与条目不一致 | 已修复(U-0182,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260911-05.md) · [第三轮](REVIEW-2026-09-11-03.md) · [第八轮原触发/残余验收通过（含边界）](REVIEW-2026-09-13-08.md) |
| RR-20260911-03 | P2 | core | finalizer 停止后晚到 Close 遗留资源 | 已修复(U-0173,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260911-03.md) · [第二轮](REVIEW-2026-09-11-02.md) · [第三轮独立验收](../review/REVIEW-2026-09-11-03.md) |
| RR-20260911-04 | P2 | core | Nest 停机遗漏延迟同步请求回复 | 已修复(U-0172,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260911-04.md) · [第二轮](REVIEW-2026-09-11-02.md) · [第三轮独立验收](../review/REVIEW-2026-09-11-03.md) |
| RR-20260911-01 | P2 | kit | 墓碑计数淘汰早于信封过期，再铸领取 token | 已修复(U-0171,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260911-01.md) · [2026-09-11](REVIEW-2026-09-11.md) · [第三轮独立验收](../review/REVIEW-2026-09-11-03.md) |
| RR-20260911-02 | P3 | kit | Mailbox 返回的 SettledClaims map 与内存存储共享 | 已修复(U-0170,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260911-02.md) · [2026-09-11](REVIEW-2026-09-11.md) · [第三轮独立验收](../review/REVIEW-2026-09-11-03.md) |
| RR-20260910-03 | P2 | core | 完成事务 tracker 淘汰导致等待者 panic | 已修复(U-0168,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260910-03.md) · [第三轮](REVIEW-2026-09-10-03.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260910-05 | P2 | codegen | category 外包常量遗漏生成 import（M-05） | 已修复(U-0166,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260910-05.md) · [第三轮](REVIEW-2026-09-10-03.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260910-06 | P2 | codegen | 聚合注册固定 entity 导入与业务别名冲突（M-05） | 已修复(U-0167,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260910-06.md) · [第三轮](REVIEW-2026-09-10-03.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260910-04 | P3 | core | Checkpoint 恢复改变完成历史淘汰顺序 | 已修复(U-0169,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260910-04.md) · [第三轮](REVIEW-2026-09-10-03.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260910-02 | P2 | kit | 已领取邮件淘汰后重投生成新发奖 token | 已修复(U-0165,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260910-02.md) · [第二轮](REVIEW-2026-09-10-02.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260910-01 | P3 | core | 极短 IdleTTL 推导出零扫描周期 | 已修复(U-0163,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260910-01.md) · [2026-09-10](REVIEW-2026-09-10.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260909-05 | P2 | kit | Match Enqueue 请求重放未校验 Subject 归属 | 已修复(U-0164,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260909-05.md) · [第四轮](REVIEW-2026-09-09-04.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260909-06 | P2 | codegen | 多 Entity 显式 -output 静默覆盖产物 | 已修复(U-0162,已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5)→ [bugfix](../bugfix/RR-20260909-06.md) · [第四轮](REVIEW-2026-09-09-04.md) · [09-11 原触发独立验收通过](../review/REVIEW-2026-09-11.md) |
| RR-20260908-01 | P2 | kit | Session 忽略幂等账本 Create 的竞争失败 | 已修复（U-0154，kit v1.14.3）→ [bugfix](../bugfix/RR-20260908-01.md) |
| RR-20260908-02 | P2 | core | ReadThrough 取消等待不归还名额 | 已修复（U-0155，core v1.15.2）→ [bugfix](../bugfix/RR-20260908-02.md) |
| RR-20260908-03 | P2 | codegen | 单行 import 包拆分产生非法 Go 语法 | 已修复（U-0156，codegen v1.15.4）→ [bugfix](../bugfix/RR-20260908-03.md) |
| RR-20260909-01 | P2 | core/docs | 当前接入指南的版本与必需参数不一致 | 已修复（U-0157，core v1.15.2）→ [bugfix](../bugfix/RR-20260909-01.md) |
| RR-20260909-02 | P2 | kit → core | Session 清理误删重建 claim（ABA） | 原冲突撤回路径已修（U-0158，已发版）；正常 Finish 新触发仍有残留，09-29 真实 Redis 已复现 → [追加证据](REVIEW-2026-09-09-02.md#2026-09-29-normal-finish-aba) · [原 bugfix](../bugfix/RR-20260909-02.md) |
| RR-20260909-03 | P2 | core | Assembly 停机未完成即遗失 Runtime，重试虚报成功 | 已修复（U-0159，已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5）→ [bugfix](../bugfix/RR-20260909-03.md) · [第三轮](REVIEW-2026-09-09-03.md) |
| RR-20260909-04 | P2 | codegen | 同包多个 Entity 生成重名注册符号，消费者无法编译 | 已修复（U-0160，已发版 core v1.15.3 / kit v1.14.4 / codegen v1.15.5）→ [bugfix](../bugfix/RR-20260909-04.md) · [第三轮](REVIEW-2026-09-09-03.md) |

[问题详情](REVIEW-2026-09-08.md) · [复现附录](REPRO-2026-09-08.md) ·
[学习与验证记录](../review/REVIEW-2026-09-08.md)。

2026-09-09 已在最新提交复现前三项，并完成图谱查询、覆盖检查与过期路径源码补证。
见[最新复核及新增问题](REVIEW-2026-09-09.md)、[架构评估与验证](../review/REVIEW-2026-09-09.md)。

2026-09-09 下午四项全部修复，修改与修改方式见 [bugfix/](../bugfix/README.md)。

第二轮独立验收：前三个原代码问题的旧复现均已通过；Session 新清理竞态单列 RR-20260909-02。
RR-20260909-01 的主要修复已落实，当前版本快照及少量仓数措辞仍待对齐。
见[复核结论](REVIEW-2026-09-09-02.md)与[运行记录](../review/REVIEW-2026-09-09-02.md)。

2026-09-09 晚：第二、三轮的三项已修复（U-0158～U-0160），RR-20260909-01 的文档残留随 U-0161 收尾；见 [bugfix/](../bugfix/README.md)。

第四轮独立验收：RR-20260909-02、03 原 overlay 通过，04 默认多实体真实消费者通过，均为源码 HEAD 验证，未宣称新 tag 发布。新增异归属匹配重放与显式输出覆盖两项，见[第四轮报告](REVIEW-2026-09-09-04.md)、[复现](REPRO-2026-09-09-04.md)及[完整运行记录](../review/REVIEW-2026-09-09-04.md)。历史轮次结论保留，不代表当前状态。

2026-09-10 核对三仓及 bugfix：RR-20260909-05、06 无新修复，保留未修复；新增范围见[本轮记录](../review/REVIEW-2026-09-10.md)。

2026-09-10 晚:四项未修复项全部修复(U-0162～U-0165),修改与修改方式见 [bugfix/](../bugfix/README.md)。

第三轮收尾收到 U-0162～U-0165 并快进三仓：上表四项旧问题保留作者已修复记录，独立验收待下一轮；本轮新增四项尚未修复。基线与到达时间区别见[第三轮收尾记录](../review/REVIEW-2026-09-10-03.md#提交前收到的并发修复)。

2026-09-10 夜:第三轮四项全部修复(U-0166～U-0169),其中 RR-20260910-05、06 是 M-05 引入的回归。见 [bugfix/](../bugfix/README.md)。

2026-09-11：U-0162～U-0169 原触发已独立验收通过；Mail 单次淘汰修复有效，墓碑容量期限与返回副本隔离另列 RR-20260911-01/02。已修复不代表整个模块审完，见[本轮报告](REVIEW-2026-09-11.md)。

2026-09-11:四项全部修复(U-0170～U-0173),其中 RR-20260911-01、02 是 U-0165 的残余与遗漏。见 [bugfix/](../bugfix/README.md)。

2026-09-13:六项已修复(U-0174～U-0179),见 [bugfix/](../bugfix/README.md)。RR-20260913-01/02/05/06/09 与 RR-20260912-01 仍未修复,原因见 [bugfix/README](../bugfix/README.md) 末尾的说明。

2026-09-13 第二轮:再修五项(U-0180～U-0184)并补齐 RR-20260913-08 的权威读取出口。剩余 RR-20260913-01/09、RR-20260912-01/02 四项,原因见 [bugfix/README](../bugfix/README.md) 末尾。

2026-09-13 第三轮:剩余四项全部修复(U-0185～U-0188),见 [bugfix/](../bugfix/README.md)。未修复清单为空。

2026-09-13 第四轮:第七轮复核的三处残余(RR-20260913-01/05 的 L2 路径、RR-20260913-02 的同刻度播种)已补修,记在原 RR 记录末尾;未修复清单仍为空。

2026-09-13 第五轮:RR-20260913-12 已修复(U-0189),Transfer / EnterShared / LeaveShared 共用一套未知结果收尾;未修复清单为空。

2026-09-14:RR-20260913-13 由 U-0189 同一修复覆盖(审查第十轮已独立验收),bugfix 记录见 RR-20260913-12 末尾;仓库测试补了连续三次准入断言。未修复清单为空。

2026-09-14 第二轮:RR-20260914-01 已修复(U-0190,U-0185 的 closed 分支);未修复清单为空。

2026-09-14 第三轮:kit activity 两项已修复(U-0191 窗口条目 opening/确认生命周期、U-0192 Delivering 派发索引);未修复清单为空。

2026-09-14 第四轮:lockstep 四项已修复(U-0193～U-0196);未修复清单为空。

2026-09-14 第五轮:RR-20260914-08 已修复(U-0197,U-0193 引入的资源边界);未修复清单为空。

2026-09-14 第六轮:RR-20260914-09 已修复(U-0198,按失败点分开的可恢复 / terminal 契约);未修复清单为空。

2026-09-15:statesync 四项已修复(U-0200～U-0203);未修复清单为空。

2026-09-15 第二轮:RR-20260915-01 已修复(U-0204);未修复清单为空。

2026-09-15 第三轮:RR-20260915-02 已修复(U-0205,视图在第一次投影时钉住;U-0200 的"幂等收敛"说法作废);未修复清单为空。

2026-09-15 第四轮:RR-20260915-03 已修复(U-0206,门槛跟着被捕获内容的 CommitLSN 走);未修复清单为空。

2026-09-15 第五轮:RR-20260915-04 已修复(U-0207,剔除通知与批次结果分别交接;索引表补了该行);未修复清单为空。

2026-09-15 第六轮:RR-20260915-05 已修复(U-0208,SetDownstream 改为生命周期交接;索引表补了该行);未修复清单为空。

2026-09-16:第七轮起登记但未进表的七项(RR-20260915-06～09、RR-20260916-01～03)全部修复(U-0209～U-0215),表行已补;未修复清单为空。

2026-09-16 晚:第四轮的 RR-20260916-04(Recover 修改代数)、RR-20260916-05(删掉无执行者的 Grouping 注入,kit 破坏性 + codegen 同步)修复,U-0216 / U-0217;ARCH-01..04 按交接分批,第一批见 [bugfix/README.md](../bugfix/README.md) 的 ARCH 表。未修复清单为空。

**10-07 B1 Host / 组合边界：RR-20261007-14～18 已修复，skill 全族及 syncstream 6 包 race 通过，未发布。**

- [RR-20261007-14](RR-20261007-14.md)：调试 Host 丢失可选能力。
- [RR-20261007-15](RR-20261007-15.md)：checkpoint 丢失缺能力错误身份。
- [RR-20261007-16](RR-20261007-16.md)：nil Host 的推进和被动入口 panic。
- [RR-20261007-17](RR-20261007-17.md)：准入缓存无界持有临时程序。
- [RR-20261007-18](RR-20261007-18.md)：合法非伤害效果被组合校验拒绝。
