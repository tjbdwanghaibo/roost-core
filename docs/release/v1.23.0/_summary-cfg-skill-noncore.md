# 汇总用摘要：CFG / SKILL / NONCORE 分册

给汇总者用：把本分册的条目、行为变化、需改动项、守卫、按包索引、未验证项与边界项集中在一处，便于并入两份主文档的总目录与总表。正文见 [说明分册](guide-cfg-skill-noncore.md) 与 [实现分册](impl-cfg-skill-noncore.md)。

- 范围：`v1.19.2..5e72ca4d`（最终代码冻结提交）里属于 CFG / SKILL / NONCORE 的改动；行号以 `5e72ca4d` 为准（起草时 `02c8a10d`，第一次重核 `e6828e4f`，第二次重核见下方“重核”一节）。
- 条目数：102（CFG 16、SKILL 30、NONCORE 56）。两份分册各 102 个锚点，编号一一对应；其中 NONCORE-1、23、24、40、45、50、56 与其他分册重复，只保留索引。2026-10-07 新增 CFG-14～16、SKILL-23～30（11 条），RR-20261006-20 并入 NONCORE-46。
- 术语：衍生物 = Spawn（原名进程 / process），召唤物 = Summon（效果 `summon`、移除 `dismiss`，原名 `spawn` / `despawn`），衍生物 kind `minion`（原名 `summon`）。正文用新名，历史引用注“原名”。
- **仍未闭环：0**（见文末“仍未闭环”）。WANTED 未决数：0。
- 首发版本用 `git tag --contains` 核对；v1.22.0 之后的提交记为“v1.23.0（本版）”。

## 条目总表

| 编号 | 一句话 | 首发版本 | 行为变化 / 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- |
| [CFG-1](guide-cfg-skill-noncore.md#cfg-1) | 布尔开关与时长严格读取（NC-190） | v1.20.2 | 收紧：`on` / 无单位时长启动即报错 | 写错类型的配置要改 |
| [CFG-2](guide-cfg-skill-noncore.md#cfg-2) | 框架配置一律严格读取，启动校验覆盖全部类型化键（A4） | v1.20.2 | 收紧：类型错误启动即点名报错 | 同上；升级前用新版本启动一次 |
| [CFG-3](guide-cfg-skill-noncore.md#cfg-3) | kit/redis 三个整数键严格读取 | v1.20.2 | 收紧（只影响绕过 App 直接装配 Mod） | 否 |
| [CFG-4](guide-cfg-skill-noncore.md#cfg-4) | 生成的 player TCP / RPC 客户端严格读取 | v1.20.2 | 收紧；生成器 Core 下限升到 v1.20.2 | 已生成工程不迁移 |
| [CFG-5](guide-cfg-skill-noncore.md#cfg-5) | `env: production` 只校验有读取方的设置（C1 / NC-192） | v1.20.2 | 放宽 | 否（上线前自己接入限流 / 鉴权） |
| [CFG-6](guide-cfg-skill-noncore.md#cfg-6) | tablegen `ref` 加载时检查、`-check` 按 schema（NC-75） | v1.20.1 | 收紧：悬空 ref 加载失败 | 修正悬空数据 |
| [CFG-7](guide-cfg-skill-noncore.md#cfg-7) | 配置规则统一由运行时加载层强制（B10） | v1.21.0 | **破坏**：违规数据启动 / reload 失败；生成器 Core 下限 v1.21.0 | 修正数据；重新 generate 才有运行时规则 |
| [CFG-8](guide-cfg-skill-noncore.md#cfg-8) | 热更失败与回滚可见（C2） | v1.21.0 | **破坏**：`configdata.reload.total` 标签变化 | 调整依赖 `reason` / `result="rollback"` 的看板 |
| [CFG-9](guide-cfg-skill-noncore.md#cfg-9) | 规则层大小写变体取最后一个（发版前审查） | v1.21.0（v1.22.0 删除） | 被 CFG-10 取代 | 否 |
| [CFG-10](guide-cfg-skill-noncore.md#cfg-10) | configdata 键大小写敏感 | v1.22.0 | **破坏**：只差大小写的键整次拒绝 | 改成声明的拼写 |
| [CFG-11](guide-cfg-skill-noncore.md#cfg-11) | cfggen globals 支持 required / min / enum | v1.23.0（本版） | 新能力；已有输出不变 | 否（可选） |
| [CFG-12](guide-cfg-skill-noncore.md#cfg-12) | 生成配置写出 `remote_entity` 新键（A8） | v1.23.0（本版） | 只影响新生成工程 | 否 |
| [CFG-13](guide-cfg-skill-noncore.md#cfg-13) | 生成 TCP 越界报错逐条点名（A9） | v1.23.0（本版） | 只改报错文本 | 按旧文本匹配的脚本要更新 |
| [CFG-14](guide-cfg-skill-noncore.md#cfg-14) | 每个 Mod 用“配置结构体 + tag”声明自己的配置，启动检查 / 生成器 / doctor 共用（A4 ①，RR-20261006-38） | v1.23.0（本版） | **破坏**：0 / 负数 / 枚举外的值按声明拒绝；syncbus 旧回退删除；生成器 Core 下限升到 v1.23.0 | 修正配置；自写 Mod 改成声明 + `app.LoadConfig` |
| [CFG-15](guide-cfg-skill-noncore.md#cfg-15) | 业务服务声明自己读的键，doctor 读回进程声明，生成工程同样守住“读配置只经声明”（A4 ① 收尾，RR-20261006-40） | v1.23.0（本版） | 收紧：game-demo 漏写 activity / platform 键启动即拒绝；doctor 新增 `config-reads` | 业务代码直接读 viper 的改成声明 |
| [CFG-16](guide-cfg-skill-noncore.md#cfg-16) | 配置数据保持 tablegen 与 cfggen 两条管线，写明分工（B10，维护者选 A） | v1.23.0（本版） | 只改文档 | 否 |
| [SKILL-1](guide-cfg-skill-noncore.md#skill-1) | 施法失败只走一个终态入口（NC-110～112） | v1.20.1 | 收紧：终态 cast 的输入被拒；手动 Release 失败不可重试 | 否 |
| [SKILL-2](guide-cfg-skill-noncore.md#skill-2) | Combatant 副本不共享 map（NC-113） | v1.20.1 | 改副本不再改实体 | 误用者改用 `InitCombatant` |
| [SKILL-3](guide-cfg-skill-noncore.md#skill-3) | skillsync 三条下发路径同一可见性（NC-114 / 115） | v1.20.1 | wire 追加字段；可见性变化时 remove 不下发 | 可见性变化时重发快照 |
| [SKILL-4](guide-cfg-skill-noncore.md#skill-4) | Applier 被拒包不改 epoch（NC-116） | v1.20.1 | 无 | 否 |
| [SKILL-5](guide-cfg-skill-noncore.md#skill-5) | 提交前失败的 cast 有界回收（NC-117） | v1.20.1 | 无（checkpoint 版本后来升到 7，见 SKILL-24～29） | 否 |
| [SKILL-6](guide-cfg-skill-noncore.md#skill-6) | wire 字段名逐字匹配（NC-150） | v1.20.2 | 收紧：非规范大小写 Parse 失败 | 修正技能 JSON |
| [SKILL-7](guide-cfg-skill-noncore.md#skill-7) | 拒绝 Runtime 不派发的 phase 事件（NC-151） | v1.20.2 | 收紧；非零 `timeout_ticks` 给 warning | 修正定义；loadtest 断言 `Warnings == 0` |
| [SKILL-8](guide-cfg-skill-noncore.md#skill-8) | 作者写的 tick 非负（NC-152） | v1.20.2 | 收紧 | 修正负值 |
| [SKILL-9](guide-cfg-skill-noncore.md#skill-9) | 表现缓存等待者、skillcompose 诊断（NC-153 / 154） | v1.20.2 | 无 | 否 |
| [SKILL-10](guide-cfg-skill-noncore.md#skill-10) | 编译器只接受 Runtime / Host 执行的范围（NC-210、212～215） | v1.20.2 | 收紧 | 修正定义与 catalog |
| [SKILL-11](guide-cfg-skill-noncore.md#skill-11) | 移交后 area finish 只结束本衍生物（原名进程，NC-211） | v1.20.2 | 放宽（不再报错） | 否 |
| [SKILL-12](guide-cfg-skill-noncore.md#skill-12) | `NegotiateSchema` 拒绝空区间（NC-216） | v1.20.2 | 收紧 | 否（无生产调用方） |
| [SKILL-13](guide-cfg-skill-noncore.md#skill-13) | lower fail-fast 与 phase 事件表单一来源（B3） | v1.20.2 | 正常定义不变；引入一处回归（v1.21.0 修） | 否 |
| [SKILL-14](guide-cfg-skill-noncore.md#skill-14) | 按 Runtime 求值上下文收紧，修 B3 回归（NC-220～224） | v1.21.0 | 收紧；NC-223 恢复 | 修正定义；v1.20.1 的相关 checkpoint 先排空 |
| [SKILL-15](guide-cfg-skill-noncore.md#skill-15) | 求值上下文表（第五轮，NC-280～283） | v1.21.0 | 收紧 + 放宽；诊断码变化 | 修正定义；按诊断码匹配的工具更新 |
| [SKILL-16](guide-cfg-skill-noncore.md#skill-16) | O33 漂移格子编译期拒绝；O34～O36 写文档 | v1.21.0 | **破坏**：以前能编译的定义启动失败 | 按作者文档改写 |
| [SKILL-17](guide-cfg-skill-noncore.md#skill-17) | minion 衍生物（原名 summon 进程）拒绝 `duration_ticks` 与 area 成员字段（O22） | v1.23.0（本版） | 收紧 | 删掉这些字段 |
| [SKILL-18](guide-cfg-skill-noncore.md#skill-18) | checkpoint 字节确定（O7） | v1.23.0（本版） | 同一版本内字节确定；本版 checkpoint 版本为 7，旧版本拒绝恢复 | 不要跨版本比较字节 |
| [SKILL-19](guide-cfg-skill-noncore.md#skill-19) | result 分支诊断文案（O29）；O15～O17 / O27 / O28 写文档 | v1.23.0（本版） | 只改文案 | 按文案匹配的工具更新 |
| [SKILL-20](guide-cfg-skill-noncore.md#skill-20) | null 默认值实体状态可以 set（RR-20261006-02） | v1.23.0（本版） | 放宽；事件 Before 的缺省值类型变化 | 否 |
| [SKILL-21](guide-cfg-skill-noncore.md#skill-21) | checkpoint 恢复拒绝 `phase_timeout`（RR-20261006-03） | v1.23.0（本版） | 只拒绝不会出现的任务 | 否 |
| [SKILL-22](guide-cfg-skill-noncore.md#skill-22) | 示例 `statusbridge` 能运行 | v1.23.0（本版） | 只改示例 | 否 |
| [SKILL-23](guide-cfg-skill-noncore.md#skill-23) | 衍生物记录随 cast 一起回收；失败启动不留记录；reset 条目带增量的 PrimaryTarget（RR-20261006-21 / 22 / 23） | v1.23.0（本版） | 行为变化：停不下衍生物的失败启动返回非零 cast ID 并保留 failed cast | 否 |
| [SKILL-24](guide-cfg-skill-noncore.md#skill-24) | 宿主停不下的衍生物由 Runtime 退避重试（RR-20261006-21 后续，RR-20261006-30 / 31） | v1.23.0（本版） | 新状态 `stop_pending`；`Host.StopSpawn` 必须幂等；checkpoint 版本 3 | 客户端认 `stop_pending`；宿主 StopSpawn 幂等 |
| [SKILL-25](guide-cfg-skill-noncore.md#skill-25) | “进程”（process）全量改名为衍生物（Spawn） | v1.23.0（本版） | **破坏**：DSL / wire / Host 接口 / 指标改名，不留别名；checkpoint 版本 4；digest 全变 | 技能 JSON、Host 实现、客户端、告警按对照表改名 |
| [SKILL-26](guide-cfg-skill-noncore.md#skill-26) | 衍生物停止入口统一走一套停止 / 待停止状态机（RR-20261006-32） | v1.23.0（本版） | `Shutdown` / `RemoveProgram` 停不下的衍生物转 `stop_pending` 由 Runtime 重试；被拒的停止回调不再重跑 | 改看 `StateSnapshot` / `RetentionStats` |
| [SKILL-27](guide-cfg-skill-noncore.md#skill-27) | 生成宿主单位的效果改名为召唤物（Summon），`despawn` → `dismiss`，衍生物 kind `summon` → `minion` | v1.23.0（本版） | **破坏**：DSL / Host 契约改名；checkpoint 版本 5；环境与 digest 全变 | 技能 JSON、Host 实现按对照表改名 |
| [SKILL-28](guide-cfg-skill-noncore.md#skill-28) | 衍生物记录按字段分区存放、删掉重复的 owned 表；源文档 digest 逐字段；停止循环判空（RR-20261006-33 / 34） | v1.23.0（本版） | checkpoint 版本 6；全部 `SourceDocumentDigest` 改变 | 保存旧源文档 digest 比对的调用方一次性重算 |
| [SKILL-29](guide-cfg-skill-noncore.md#skill-29) | 待停止到上限不删记录，挪进“已放弃”分区（维护者选 B） | v1.23.0（本版） | 新状态 `abandoned`；指标改名 `skill.spawn.abandoned.total`；checkpoint 版本 7 | 告警规则改名；客户端认 `abandoned` |
| [SKILL-30](guide-cfg-skill-noncore.md#skill-30) | Host 取值能力表随编译环境下发，编译器 / Runtime / Host 共用（B3 ③，RR-20261006-37 / 39） | v1.23.0（本版） | **破坏**：`skill.Host` 必须实现 `HostCapabilities()`；环境格式与 authority digest 变化；表外能力启动 / 注册时拒绝 | Host 声明能力表；重新编译 / 重签 |
| [NONCORE-1](guide-cfg-skill-noncore.md#noncore-1) | N01 留项：Ops 同步 bind、停机 hook 预算、停机期 fail-stop 退出码、Mod 停止收尾、`ops.admin_timeout`（NC-230～234） | v1.21.0 | 收紧：Ops 端口被占启动失败；admin 命令 10s 期限 | 同机多实例各配 `ops.addr`；长命令调大 `ops.admin_timeout` |
| [NONCORE-2](guide-cfg-skill-noncore.md#noncore-2) | HTTP JSON 先编码后写；recover 尊重已开始的响应（NC-80 / 81） | v1.20.1 | 编码失败回 500 | 否 |
| [NONCORE-3](guide-cfg-skill-noncore.md#noncore-3) | RateLimiter 每主体 key 上限（NC-82） | v1.20.1 | 行为变化：每主体默认 256 key | 否 |
| [NONCORE-4](guide-cfg-skill-noncore.md#noncore-4) | 生成 TCP 接入停机可重试（NC-83） | v1.20.1 | 生成形状 | 重新生成 `server_gen*.go` |
| [NONCORE-5](guide-cfg-skill-noncore.md#noncore-5) | 生成 TCP“handler 不配合 ctx”用例（A15） | v1.23.0（本版） | 只加测试 | 否（handler 须配合 ctx） |
| [NONCORE-6](guide-cfg-skill-noncore.md#noncore-6) | Bus JetStream RPC 停止排空 / 拒绝回包 / 截获（NC-90～92） | v1.20.1 | 收紧：截获的轻量 RPC 不执行 | 两端 `nats.rpc.transport` 一致 |
| [NONCORE-7](guide-cfg-skill-noncore.md#noncore-7) | etcd `Resign` 按调用方期限（NC-93） | v1.20.1 | `Resign` 可能返回 ctx 错误 | 否 |
| [NONCORE-8](guide-cfg-skill-noncore.md#noncore-8) | bus SETNX 去重保持、写进契约 | v1.23.0（本版） | 只改注释 | 否 |
| [NONCORE-9](guide-cfg-skill-noncore.md#noncore-9) | 迁移输出 WAL 准入前验证（NC-31） | v1.20.0 | 收紧 | 手写迁移候选须提供 loader / Id |
| [NONCORE-10](guide-cfg-skill-noncore.md#noncore-10) | 生成 DAO 深层嵌套修改进入提交（NC-32） | v1.20.0 | 生成形状 | 重新生成 DAO / nested |
| [NONCORE-11](guide-cfg-skill-noncore.md#noncore-11) | versionstore 退避后重读（NC-52） | v1.20.1 | 伪冲突减少 | 否 |
| [NONCORE-12](guide-cfg-skill-noncore.md#noncore-12) | mongotest 数组唯一键、`$in` 具名切片（NC-102、RR-20261006-08） | v1.20.1 / v1.23.0（本版） | 只影响测试替身 | 否 |
| [NONCORE-13](guide-cfg-skill-noncore.md#noncore-13) | 缓存副本与 interest 身份绑定（NC-33 / 34） | v1.20.0 | 收紧 | 否 |
| [NONCORE-14](guide-cfg-skill-noncore.md#noncore-14) | 权威快照身份与最终最低版本（NC-35 / 36） | v1.20.0 | 收紧 | 否 |
| [NONCORE-15](guide-cfg-skill-noncore.md#noncore-15) | L2 CAS 落败、删除墓碑、Stats 清理（NC-130 / 131） | v1.20.2 | 收紧；L2 键格式增字段 | 否 |
| [NONCORE-16](guide-cfg-skill-noncore.md#noncore-16) | saga 三消费者健康与 Resume 持久代际（NC-37 / 38） | v1.20.1 | 持久格式增量 | 协调器统一升级 |
| [NONCORE-17](guide-cfg-skill-noncore.md#noncore-17) | saga 启动身份与完成路由（NC-39 / 40） | v1.20.1 | 收紧：缺摘要的旧记录重投冲突 | 自定义 Store 保存新字段 |
| [NONCORE-18](guide-cfg-skill-noncore.md#noncore-18) | saga outbox 领取与 activity 恢复校验（NC-41 / 42） | v1.20.1 | 收紧 | 否 |
| [NONCORE-19](guide-cfg-skill-noncore.md#noncore-19) | account 换名释放死计划、activity 确认键校验（NC-50 / 51） | v1.20.1 | 行为变化 | 否 |
| [NONCORE-20](guide-cfg-skill-noncore.md#noncore-20) | `activity.game_sids` 启动校验（RR-20261005-01；C4 后由组文件兑现、回归改名） | v1.20.1（v1.20.2 被 C4 取代） | 已取代 | 否 |
| [NONCORE-21](guide-cfg-skill-noncore.md#noncore-21) | activity 窗口条目统一入口与修复入口；account 判定表（B9） | v1.21.0 | 坏条目不再交出；新 Admin 入口 | 否（运维可用新入口） |
| [NONCORE-22](guide-cfg-skill-noncore.md#noncore-22) | account 换名释放未 admitted 计划（第五轮、O37） | v1.21.0 | 行为变化 | 否 |
| [NONCORE-23](guide-cfg-skill-noncore.md#noncore-23) | saga 定义缺失 fence 时退避中步骤记为放弃（NC-250） | v1.21.0 | 是（迟到成功由丢弃改为 ack + 告警；本版正向的迟到成功改为补偿这一步，见 SAGA-16） | 否 |
| [NONCORE-24](guide-cfg-skill-noncore.md#noncore-24) | global `Bind` 同参重试幂等（RR-20261006-05） | v1.23.0（本版） | 放宽 | 否 |
| [NONCORE-25](guide-cfg-skill-noncore.md#noncore-25) | N07 第一批：快照锁、属性层回滚、attribute 生成器、errcode 扫描（NC-60～63） | v1.20.1 | 收紧：非字面量 errcode 报错 | 常量编号改字面量 |
| [NONCORE-26](guide-cfg-skill-noncore.md#noncore-26) | demo 开关热更说明、加载时重建 Gear（NC-64 / 65） | v1.20.1 | 模板 | `roost project sync` |
| [NONCORE-27](guide-cfg-skill-noncore.md#noncore-27) | 生成器不改进程工作目录（RR-20261004-12） | v1.20.0 | 两行提示变绝对路径 | 否 |
| [NONCORE-28](guide-cfg-skill-noncore.md#noncore-28) | go 命令按进程树取消（RR-20261004-13 与补修） | v1.20.0 / v1.20.1 | 无 | 否 |
| [NONCORE-29](guide-cfg-skill-noncore.md#noncore-29) | N08：中断清理、预览、cfggen 帮助、id 工具（NC-70～73） | v1.20.1 | 无 | 否 |
| [NONCORE-30](guide-cfg-skill-noncore.md#noncore-30) | 运行期目录不算生成输入（NC-74） | v1.20.1 | 无 | 否 |
| [NONCORE-31](guide-cfg-skill-noncore.md#noncore-31) | CLI 入口统一接管信号（B6）；联网用例门（C9） | v1.21.0 | 中断先回滚再以同一信号退出 | 否 |
| [NONCORE-32](guide-cfg-skill-noncore.md#noncore-32) | DAO `//roost:dao nocoll` | v1.20.0 | 新能力 | 可选迁移 |
| [NONCORE-33](guide-cfg-skill-noncore.md#noncore-33) | 文件 outbox 清理崩溃遗留的临时文件（RR-20261006-04） | v1.23.0（本版） | 打开时删除精确匹配的临时文件 | 同一目录不能有两个在写的 store |
| [NONCORE-34](guide-cfg-skill-noncore.md#noncore-34) | N10 第一批（NC-120～123） | v1.20.1 | 行为变化：丢弃排队项发 OnEnded | 否 |
| [NONCORE-35](guide-cfg-skill-noncore.md#noncore-35) | actionflow 回调变更进延后队列（B7） | v1.20.2 | **破坏**：`ActionRunner` 回调语义变化 | 按延后语义改写回调 |
| [NONCORE-36](guide-cfg-skill-noncore.md#noncore-36) | N10 第二批（NC-240～247） | v1.21.0 | 行为变化：ai 回调里切换延后 | 否 |
| [NONCORE-37](guide-cfg-skill-noncore.md#noncore-37) | MissionRunner 延后队列；EndAll 清场文档 | v1.21.0 | **破坏**：`MissionRunner` 回调语义变化 | 按延后语义改写；清场先 `EndCurMission` |
| [NONCORE-38](guide-cfg-skill-noncore.md#noncore-38) | ai O-T3 / O-T4 写文档 | v1.23.0（本版） | 只改注释 | 否 |
| [NONCORE-39](guide-cfg-skill-noncore.md#noncore-39) | N11 第一批：timer / spatial / index（NC-140～147） | v1.20.1 | 行为变化：Tick 期间取消 / 改期立即生效 | 模板 `roost project sync` |
| [NONCORE-40](guide-cfg-skill-noncore.md#noncore-40) | timer 同期限按 priority / 登记顺序；未注册类型可见（D-L1 / D-L2） | v1.21.0 | 行为变化 | 模板 `roost project sync` |
| [NONCORE-41](guide-cfg-skill-noncore.md#noncore-41) | `PathFindSystem.Stop` 数据竞争（NC-270） | v1.21.0 | 生成形状 | 模板 `roost project sync` |
| [NONCORE-42](guide-cfg-skill-noncore.md#noncore-42) | failurelog 结果未知不降级（NC-160） | v1.20.2 | 收紧 | 否 |
| [NONCORE-43](guide-cfg-skill-noncore.md#noncore-43) | robot / statslog / log（NC-161～165） | v1.20.2 | 收紧：loadtest 无样本判失败 | 否 |
| [NONCORE-44](guide-cfg-skill-noncore.md#noncore-44) | robot 会话 / 日志 sink / Prometheus 转义等（NC-261～266） | v1.21.0 | 行为变化 | 否 |
| [NONCORE-45](guide-cfg-skill-noncore.md#noncore-45) | robot Stage 序号只增不回收（RR-20261006-09） | v1.23.0（本版） | 行为变化 | 自定义 `IdentityProvider` 覆盖超出 `Count` 的序号 |
| [NONCORE-46](guide-cfg-skill-noncore.md#noncore-46) | N13 遍历与 TaskPool 等（NC-180～185）；TaskPool 统计不再读到结束数大于提交数（RR-20261006-20） | v1.20.2 / v1.23.0（本版） | 行为变化 | 否 |
| [NONCORE-47](guide-cfg-skill-noncore.md#noncore-47) | 遍历回调仓库级契约（C7） | v1.20.2 | 契约成文 | 否 |
| [NONCORE-48](guide-cfg-skill-noncore.md#noncore-48) | container / goroutine 零调用方 API（NC-267～269） | v1.21.0 | 无 | 否 |
| [NONCORE-49](guide-cfg-skill-noncore.md#noncore-49) | Mongo URI 口令脱敏（NC-191） | v1.20.2 | 无 | 否 |
| [NONCORE-50](guide-cfg-skill-noncore.md#noncore-50) | 启动失败先收回 Service（NC-193） | v1.20.2 | 是（契约收紧） | `Shutdown` 须容忍部分初始化 |
| [NONCORE-51](guide-cfg-skill-noncore.md#noncore-51) | N15 脚本与门禁（NC-200～208）、A5 | v1.20.2 | 收紧：glsvet 对没检查到的输入退出 2 | 已有工程 `.gitignore` 补 `/data/wal/` |
| [NONCORE-52](guide-cfg-skill-noncore.md#noncore-52) | 停机三步（NC-170～174） | v1.20.2 | 收紧 | 否 |
| [NONCORE-53](guide-cfg-skill-noncore.md#noncore-53) | Mongo Mod 停止收敛（NC-260） | v1.21.0 | 重复 Close 返回 nil | 否 |
| [NONCORE-54](guide-cfg-skill-noncore.md#noncore-54) | nest 用例隔离（A2 / A3）；派发取锁要求 Guard 作用域（RR-20261006-12，原 W-2026-10-06-01） | v1.23.0（本版） | 生产行为不变；nest 内部派发函数在没有 Guard 作用域时返回错误 | 否 |
| [NONCORE-55](guide-cfg-skill-noncore.md#noncore-55) | Nest 重排抖动（U-0279） | v1.20.1 | 平均重排延迟 5ms → 7.5ms | 否 |
| [NONCORE-56](guide-cfg-skill-noncore.md#noncore-56) | 租约修复 RR-20261004-10 / 11 / 14（同版被静态绑定取代） | v1.20.0 | 代码已删 | 否 |

## 行为变化、兼容破坏与需要改动的项

### 破坏性（升级后以前能用的会失败）

| 条目 | 首发 | 什么会失败 | 业务 / 运维要做什么 |
| --- | --- | --- | --- |
| CFG-7 | v1.21.0 | 新生成 loader 下违反 required / unique / min / enum / ref 的数据启动或 reload 失败 | 修正数据；生成器 Core 下限 v1.21.0 |
| CFG-8 | v1.21.0 | `configdata.reload.total` 去掉 `reason`、`result` 不再有 `rollback`；新增 `configdata.rollback.total{trigger}` | 改看板 / 告警 |
| CFG-10 | v1.22.0 | 只差大小写的配置键、CSV 表头；手写 `FieldRule.Field` 拼写不符 | 改成声明的拼写 |
| CFG-14 | v1.23.0 | 以前“写 0 或负数静默取默认”的键按声明范围拒绝；字符串枚举对所有服务检查；syncbus 不再回退读 `room.*` / `sync.*`；`app.ConfigReader`、`kit/mods.Duration` / `RedisClusterAddrs` 等删除；生成代码需要 core v1.23.0 | 要缺省就不写该键；自写 Mod 改成配置结构体 + `app.LoadConfig`；打 tag 时 `minimumVersions.Core` 升到 v1.23.0 |
| SKILL-6～8、10、14～17 | v1.20.2 / v1.21.0 / v1.23.0 | 以前能编译的 skill 定义启动期 `CompileAll` 失败 | 按诊断与作者文档改写 |
| SKILL-25、27 | v1.23.0 | 用旧名写的技能 JSON Parse 失败；Host 接口、事件、指标、mutation / 表现 kind 改名；全部 digest 改变 | 按两份对照表改名；skillcompose 契约重签 |
| SKILL-24～29 | v1.23.0 | checkpoint 版本 3 → 7，旧版本 `ErrCheckpointUnsupported` | 排空后升级 |
| SKILL-30 | v1.23.0 | `skill.Host` 不实现 `HostCapabilities()` 编译不过；环境格式与 authority digest 变化；表外能力在准入处拒绝；参考 Host 对表外属性 / 资源报错 | Host 声明能力表；重新编译、重签；旧录制回放重录 |
| NONCORE-35 | v1.20.2 | `ActionRunner` 回调里的变更延后执行，不再返回 `ErrReentrantMutation` | 按延后语义改写回调 |
| NONCORE-37 | v1.21.0 | `MissionRunner` 同上；ai `OnMissionEnd` 里 SetMission 返回 nil 并在结束后启动 | 同上；清场先 `EndCurMission` 再 `EndAll` |

### 行为收紧 / 变化（写错的从静默变为报错，或语义调整）

| 条目 | 首发 | 变化 | 需要改动 |
| --- | --- | --- | --- |
| CFG-1、CFG-2 | v1.20.2 | 布尔 / 时长 / 整数配置严格读取，启动即点名报错 | 修正配置；升级前启动一次检查 |
| CFG-3、CFG-4 | v1.20.2 | kit/redis 三键、生成 TCP / RPC 客户端严格读取；生成器 Core 下限 v1.20.2 | 已生成工程不迁移 |
| CFG-5 | v1.20.2 | 生产校验删掉九组无读取方的要求（放宽） | 上线前自己接入限流 / 鉴权 |
| CFG-6 | v1.20.1 | 悬空 ref 加载失败；ref 声明不合法 `roost generate` 失败 | 修正数据 / schema |
| CFG-12 | v1.23.0 | 新生成配置带五个 `remote_entity` 键（v1.23.0 起由 CFG-14 的声明渲染） | 无（已有工程不回写） |
| CFG-15 | v1.23.0 | game-demo 漏写 `activity.*` / `platform.*` 启动即拒绝；生成的 `world_singleton.go` 对超 int32 的 `sid` 报错；doctor 新增 `config-reads`（用户代码直接读 viper 即 FAIL） | 业务读取改为服务声明 |
| CFG-13 | v1.23.0 | 生成 TCP 报错文本逐条点名 | 按旧文本匹配的脚本更新 |
| SKILL-1 | v1.20.1 | 终态 cast 的输入被拒；手动 Release 失败不可重试 | 无 |
| SKILL-3 | v1.20.1 | wire 追加 `caster` / `owner`；可见性变化时 remove 不下发 | 可见性变化时重发快照 |
| SKILL-7 | v1.20.2 | 非零 `timeout_ticks` 给 warning | game-demo loadtest 断言 `Warnings == 0` |
| SKILL-15、19 | v1.21.0 / v1.23.0 | 诊断码 `REFERENCE_UNKNOWN` → `INPUT_UNAVAILABLE`；O29 文案 | 按诊断码 / 文案匹配的工具更新 |
| SKILL-18 | v1.23.0 | 同一状态两次 checkpoint 字节相同（O7 本身不改格式；本版 checkpoint 版本为 7） | 不跨版本比较字节 |
| SKILL-23 | v1.23.0 | 衍生物停不下的失败启动返回非零 cast ID 并保留 failed cast；起过衍生物的 cast 在衍生物结束后按上限回收 | 无 |
| SKILL-24、26 | v1.23.0 | 新状态 `stop_pending`；`Shutdown` / `RemoveProgram` 停不下的衍生物由 Runtime 重试、不再列在 `OwnedSpawns`；被拒的停止回调不重跑 | 宿主 `StopSpawn` 幂等；客户端认 `stop_pending`；改看 `StateSnapshot` / `RetentionStats` |
| SKILL-28 | v1.23.0 | 全部 `SourceDocumentDigest` 改变（gameplay / presentation digest 不变） | 保存旧源文档 digest 的调用方一次性重算 |
| SKILL-29 | v1.23.0 | 新状态 `abandoned`；指标 `skill.spawn.stop_pending_dropped.total` → `skill.spawn.abandoned.total`（另有 `abandoned_pruned`）；放弃时不发 `spawn_remove` | 告警改名；客户端认 `abandoned` |
| SKILL-20 | v1.23.0 | 事件 Before 的缺省值类型由 null 变为声明类型 | 无 |
| NONCORE-1 | v1.21.0 | Ops 端口被占启动失败；admin 命令 10s 期限回 504；停机期 fail-stop 非零退出 | 同机多实例各配 `ops.addr`；长命令调大 `ops.admin_timeout` |
| NONCORE-3 | v1.20.1 | RateLimiter 每主体默认 256 key | 无 |
| NONCORE-4、10、26、39～41 | 多版本 | 生成形状 / 模板变化 | 重新生成或 `roost project sync` |
| NONCORE-6 | v1.20.1 | JetStream 截获的轻量 RPC 不执行，返回 `bus.ErrRPCCapturedByJetStream` | 两端 `nats.rpc.transport` 一致 |
| NONCORE-7 | v1.20.1 | `Resign` 可能返回 ctx 错误 | 无 |
| NONCORE-9 | v1.20.0 | 手写迁移候选须提供 loader / Id | 补齐 |
| NONCORE-15 | v1.20.2 | L2 写旧版本返回 `ErrStaleWrite`；L2 墓碑（键格式增字段） | 无 |
| NONCORE-16、17 | v1.20.1 | saga 持久格式增量（`incarnation`、`start_digest`）；缺摘要旧记录重投冲突 | 协调器统一升级；自定义 Store 保存新字段 |
| NONCORE-19、21、22 | v1.20.1 / v1.21.0 | account 换名释放计划的范围扩大；activity 坏条目不再交出、新 Admin 修复入口 | 运维可用新入口 |
| NONCORE-24 | v1.23.0 | global `Bind` 同参重试成功（放宽） | 无 |
| NONCORE-25 | v1.20.1 | 非字面量 errcode 报 `file:line` | 常量编号改字面量 |
| NONCORE-31 | v1.21.0 | roost CLI 中断先回滚再以同一信号退出；联网用例需 `ROOST_NETWORK_TESTS=1` | 无 |
| NONCORE-33 | v1.23.0 | 文件 outbox 打开时删除遗留临时文件 | 同一目录不能有两个在写的 store |
| NONCORE-34、36 | v1.20.1 / v1.21.0 | 丢弃排队项发 OnEnded；ai 回调里切换策略延后 | 无 |
| NONCORE-40 | v1.21.0 | timer 同期限按 priority / 登记顺序；未注册类型删除时 Warn + 计数 | 模板 sync |
| NONCORE-42、43 | v1.20.2 | failurelog 结果未知不降级；loadtest 无样本判失败 | 无 |
| NONCORE-44 | v1.21.0 | 新指标 `robot.session.late_response{msg}`、`log.rotate_failures`、`log.write_errors{sink}` | 无 |
| NONCORE-45 | v1.23.0 | robot Stage 序号只增不回收 | 自定义 `IdentityProvider` 覆盖超出 `Count` 的序号 |
| NONCORE-46、47 | v1.20.2 | 遍历回调可以改容器、false 立即停止（契约成文） | 无 |
| NONCORE-46（RR-20261006-20） | v1.23.0 | `TaskPool.GetStats` 保证 `completed + failed ≤ total`（被拒的提交在撤回前可能让 total 瞬时多一） | 无 |
| NONCORE-50 | v1.20.2 | Init 失败也调用 `Shutdown` | `Shutdown` 须容忍部分初始化 |
| NONCORE-51 | v1.20.2 | glsvet 对没检查到的输入退出 2；pretag 在 origin 不可达时失败；生成 `.gitignore` 忽略 `/data/wal/` | 已有工程手工补 `.gitignore` |
| NONCORE-53 | v1.21.0 | Mongo `Client.Close` 对已断开的客户端返回 nil | 无 |

## 新增门禁与守卫测试

| 守卫 | 位置 | 条目 |
| --- | --- | --- |
| 配置只经声明读：`TestFrameworkModsReadConfigOnlyThroughDeclarations`、`TestEveryDeclaredConfigFieldIsRead`、`TestConfigDeclarationGuardsCatchTheirDrift`（取代 A4 ② 的三条正则守卫，那三条随登记表在 v1.23.0 删除） | `app/config_declarations_promises_test.go` | CFG-1～4、CFG-14 |
| `TestEveryKitModLoadsWhatItDeclares`、`TestKitModsRefuseOutOfRangeValuesAtLoadAndAtStartup` | `kit/config_schema_promises_test.go` | CFG-14 |
| `TestKitConfigSchemasMatchKitDeclarations`、`TestGeneratedConfigsMatchDeclarations`、`TestGeneratorConstantsMatchTheDeclaredExamples`、`TestPlayerTCPDeclarationAgreesWithKit`；`go generate ./...` porcelain | `codegen/internal/roost/config_declarations_promises_test.go` | CFG-14（RR-20261006-38） |
| `TestGeneratedProjectsReadConfigOnlyThroughDeclarations`、`TestGeneratedProjectConfigGuardCatchesDrift`、`TestDoctorReadsBusinessDeclarationsFromTheProcess`、`TestServiceDeclarationsAreCheckedAndPrintedWithTheMods`；doctor `config-schema:*` / `config-reads` | `codegen/internal/roost/config_reads_promises_test.go`、`app/config_declarations_promises_test.go` | CFG-15（RR-20261006-40） |
| `TestSharedConfigRulesStayALeaf` + codegen → `configdata/rules` / `internal/configschema` 两个例外 | 根包 `dependency_boundary_test.go` | CFG-7、CFG-14 |
| `TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader` | `codegen/internal/tablegen` | CFG-7 |
| tablegen 运行期门 `tablegen-runtime.sh`（新增，进 `ci.yml`） | `codegen/scripts/` | CFG-6、7、10 |
| cfggen 运行期门 `TestGlobalRulesAreEnforcedOnLoadAndReload` | `codegen/internal/cfggen/testdata/runtime` | CFG-11 |
| `TestGeneratedConfigsPassStrictAndProductionValidation`、`TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys` | `codegen/internal/roost` | CFG-5、CFG-12 |
| 生成 TCP `TestConfigValuesOfTheWrongTypeAreRefusedByName`、`TestAnOutOfBoundsSettingIsRefusedByName`、`TestADispatchTimeoutBoundsTheWaitNotAnUncooperativeHandler` | 生成工程（模板 `render_player_tcp.go`） | CFG-4、CFG-13、NONCORE-5 |
| skill 变异性质测试（编译 ⇒ 可执行、Program 变 digest 变） | `skill/compile_mutation_property_test.go` | SKILL |
| `TestLowerRefusesEveryUnresolvedLookup`、`TestPhaseEventTableIsTheSingleSource` | `skill/` | SKILL-13 |
| 衍生物停止入口登记表 `spawnStopEntries` + `TestEveryStopEntryDefersARefusedStopTheSameWay` + `TestSpawnStopEntriesAreRegistered`（`go/ast`） | `skill/spawn_stop_entries_promises_test.go` | SKILL-24、26 |
| `TestSpawnPartitionsFollowRecordFields` + `TestSpawnPartitionWritesStayInSpawnTable`（`go/types`，`spawnDropSites`） | `skill/spawn_partition_promises_test.go` | SKILL-28、29 |
| `TestSourceDocumentDigestSeesEveryDifference` | `skill/canonical_definition_promises_test.go` | SKILL-28（RR-20261006-33） |
| Host 能力表：`TestCompilerConsultsTheTableForEveryRequirement`、`TestRuntimeAsksHostOnlyForCompiledRequirements`、`TestCompilerReadsHostCapabilitiesOnlyThroughTheTable`、`TestNoHostCapabilitySkipBranch`、`TestHostInterfaceRequiresTheCapabilityTable` | `skill/host_capability_promises_test.go`、`skill/host_capability_required_promises_test.go` | SKILL-30（RR-20261006-37 / 39） |
| `TestTaskPoolStatsCountSubmitBeforeTheTaskCanFinish`、`TestTaskPoolStatsReadFinishedBeforeSubmitted` | `goroutine/task_pool_stats_promises_test.go` | NONCORE-46（RR-20261006-20） |
| 求值上下文表逐格守卫（192 格 + 快照点 15 格 + O33 21 格） | `skill/eval_contexts_table_test.go` | SKILL-15、16 |
| `internal/rangecontract.Check` 共用遍历契约辅助 | container / safemap / entity / 生成 DAO | NONCORE-47 |
| `TestNetworkCodegenTestsRunInSomeWorkflow`；framework-compat 新 job `codegen-network` | 根包 `ci_generated_code_test.go`、`.github/workflows/framework-compat.yml` | NONCORE-31 |
| glsvet 输入守卫（退出码） | `cmd/glsvet/inputs_promises_test.go` | NONCORE-51 |
| 共享隔离环境：全局命令持 `remote-acceptance.lock`；toxic 用例自建代理 | `kit/scripts/integration`、根包守卫 | NONCORE-51 |
| `glsvet -tests ./nest` 无输出 | — | NONCORE-54 |
| `TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool`、`TestDispatchLockingGroupRetryInScopeKeepsGuardUntilScopeEnds`、`TestDispatchEntriesRequireGuardScope` | `nest/dispatch_guard_scope_promises_test.go` | NONCORE-54 |
| `TestAGroupFitsOneLiveQuery`（组上限 ≤ 一次 `App.Live` 上限） | `kit/service/global/activity/groups_live_limit_promises_test.go` | NONCORE-20 |
| hotcode 真实 .so 测试包 | `hotcode/plugintest` | NONCORE-36 |


## 按包的条目索引

| 包 / 目录 | 条目 |
| --- | --- |
| `app` | CFG-1、CFG-2、CFG-5、CFG-14、CFG-15、NONCORE-1、NONCORE-50 |
| `kit`（根）、`kit/mods` | CFG-1、CFG-2、CFG-14 |
| `kit/redis` | CFG-1、CFG-3、CFG-14、NONCORE-1 |
| `kit/saga`、`kit/dataengine` | CFG-1、CFG-2、CFG-14、CFG-15（`StreamSettings` / `EffectSettings`） |
| `kit/internal/configschemagen`、`internal/configschema` | CFG-14、CFG-15 |
| kit 其余 Mod（nest、remoteentity、nats、syncbus、mongo、etcd、ops、statslog、configdata、service/*） | CFG-14（各自的配置声明） |
| `kit/ops`、`kit/remoteentity`、`lifecycle` | NONCORE-1 |
| `kit/configdata` | CFG-8 |
| `kit/mongo` | NONCORE-49、NONCORE-53 |
| `kit/nest` | NONCORE-52 |
| `kit/statslog` | NONCORE-43 |
| `kit/service/account` | NONCORE-19、NONCORE-21、NONCORE-22 |
| `kit/service/global` | NONCORE-20（注释）、NONCORE-24 |
| `kit/service/global/activity` | NONCORE-18、NONCORE-19、NONCORE-20（C4 后的守卫）、NONCORE-21 |
| `kit/scripts/integration`、`scripts`、`cmd/glsvet` | NONCORE-51 |
| `configdata` | CFG-7、CFG-8、CFG-10 |
| `configdata/rules` | CFG-7、CFG-9、CFG-10 |
| `codegen/internal/tablegen` | CFG-6、CFG-7、CFG-10、CFG-16 |
| `codegen/internal/cfggen` | CFG-7、CFG-11、CFG-16 |
| `codegen/internal/roost` | CFG-4、CFG-12、CFG-13、CFG-14、CFG-15、NONCORE-4、NONCORE-5、NONCORE-26、NONCORE-27～31、NONCORE-51（NC-206） |
| `codegen/internal/servicerpc` | CFG-4、CFG-14、NONCORE-27 |
| `codegen/internal/dao`、`codegen/internal/entity` | NONCORE-10、NONCORE-32 |
| `codegen/internal/attribute`、`codegen/internal/errcode` | NONCORE-25、NONCORE-29 |
| `codegen/cmd/*` | NONCORE-31 |
| `skill` | SKILL-1、SKILL-5～8、SKILL-10、SKILL-11、SKILL-13～21、SKILL-23～30 |
| `skill/combatcomponent` | SKILL-2（投影入口属 DAO）、SKILL-30（`HostAdapter` 能力表） |
| `skill/skillsync` | SKILL-3、SKILL-4、SKILL-12、SKILL-23（RR-22）、SKILL-25（改名）、NONCORE-33 |
| `skill/skillcompose` | SKILL-9 |
| `skill/examples` | SKILL-22 |
| `httpserver`、`webroute`、`security`、`gateway` | NONCORE-2、NONCORE-3 |
| `bus` | NONCORE-6、NONCORE-8 |
| `etcd/driver` | NONCORE-7、NONCORE-52 |
| `dataengine/engine` | NONCORE-9 |
| `versionstore` | NONCORE-11 |
| `mongo/mongotest` | NONCORE-12 |
| `mongo/driver` | NONCORE-53 |
| `cache` | NONCORE-13 |
| `entity` | NONCORE-14、NONCORE-15、NONCORE-46、NONCORE-47 |
| `remoteentity` | NONCORE-13、NONCORE-15、NONCORE-52 |
| `saga`、`servicemetrics` | NONCORE-16～18、NONCORE-23（`saga` 用例另见 NONCORE-12） |
| `attribute` | NONCORE-25 |
| `demo/` 模板 | CFG-15（`game/settings`）、NONCORE-20、NONCORE-25、NONCORE-26、NONCORE-39～41、NONCORE-56 |
| `ai` | NONCORE-34、NONCORE-36、NONCORE-38 |
| `actionflow` | NONCORE-34～37 |
| `hotcode` | NONCORE-34、NONCORE-36 |
| `timer` | NONCORE-39、NONCORE-40 |
| `spatial`、`index` | NONCORE-39 |
| `failurelog` | NONCORE-42 |
| `robot/...`、`log`、`metrics` | NONCORE-43、NONCORE-44、NONCORE-45 |
| `container`、`safemap`、`goroutine`、`internal/rangecontract` | NONCORE-46～48（`goroutine` 另有 RR-20261006-20） |
| `manager`、`sync/syncbus/driver`、`sync/syncbus/mirror` | NONCORE-52 |
| `nest` | NONCORE-54（A2 / A3 只改测试；RR-20261006-12 改派发取锁）、NONCORE-55 |
| `.github/workflows` | CFG-6（`ci.yml`）、NONCORE-31（`framework-compat.yml`） |
| 根包测试 | CFG-7、CFG-14（边界）、NONCORE-31（C9） |
| 文档（`docs/USER_GUIDE.md`、`codegen/README.md`、B10 方案） | CFG-16 |

## 未验证与外部验证项

只列外部环境项，编号对应 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)；本机能做的已在冻结提交上做完（见下方“重核”）。2026-10-07 补写的 CFG-14～16、SKILL-23～30、RR-20261006-20 不依赖外部环境，没有新增外部验证项（SKILL-23 的 skillsync 端到端随既有 E04）。

| 项 | 条目 | 外部验证 |
| --- | --- | --- |
| 多机 Redis Cluster 下 `redis.cluster_addrs` YAML 列表起服；failurelog 在 Cluster 下 | CFG-1、NONCORE-42 | E08 |
| skillsync 经真实传输（kit syncstream / NATS）端到端 | SKILL-3、4、23 | E04 |
| HTTP/2、真实网关 / 反向代理、robot 发送缓冲满 | NONCORE-2～5、43、44 | E05 |
| 跨主机 broker 与弱网下的复制丢更新 | NONCORE-13 | E03 / E06 |
| JetStream / etcd 多节点 HA | NONCORE-6、7、52 | E06、E07 |
| Mongo 跨主机副本集 / 切主中提交 / 网络丢回复 | NONCORE-9、16～18 | E11 |
| account / activity / versionstore 在 Cluster 与多进程下 | NONCORE-11、19、21、22 | E09 |
| 复制丢写与切主、多主机强杀 | NONCORE-15 | E10、E13 |
| World 定时器三进程提交被拒、日志磁盘写满 | NONCORE-39、44 | E19 |
| systemd / k8s 停机 | NONCORE-1（以 APP / OPS 为准） | E21、E22 |
| Windows：CLI 信号与进程树、暂存树、文件 outbox 替换——**暂存，不保证正确** | NONCORE-27、28、31、33 | E25 |
| Linux：CLI 信号与进程树、NC-202 pid 认领 | NONCORE-31、51 | E26 |
| hotcode 真实插件加载（Linux；Windows 部分暂存，不保证正确） | NONCORE-34、36 | E27 |

WANTED 未决数：**0**（W-2026-10-06-01 已转 RR-20261006-12 并修复，NONCORE-54）。

## 第二次重核与补写（按最终冻结点 `5e72ca4d`，2026-10-07）

- **引用**：正文（不含修前红文本）524 处 `path:line` 按 `git diff -U0 -M e6828e4f 5e72ca4d` 逐行映射，再核对每处附近的符号 / 测试名，并把正文标识符与 `5e72ca4d` 全仓词表比对。391 处不变；**共改 126 处**：行号漂移、内容不变 54 处；人工改 72 处（CFG-1～5、CFG-7、CFG-12 随 A4 ① 删除或搬移 48 处，SKILL 随两次改名与文件改名 24 处）。随 A4 ① 删除的测试 9 个、符号约 15 个，改为“`e6828e4f` 上的位置 + `5e72ca4d` 上的对应物”；随改名改变的测试 7 个、符号 9 个改为新名并注“原名”。修前红文本里的 135 处 `文件:行` 不改。roost-coding 引用行号随该文件在 72～82 行的改动更新（“配置只经声明读”第 82 行、“反复出问题要上报”第 107 行、验证纪律 125～131 行），作者文档 O33 一节标题由第 80 行移到第 82 行。
- **新增条目（11 条）**：CFG-14（A4 ① + RR-20261006-38 + 配置声明形式选 A）、CFG-15（A4 ① 收尾 + RR-20261006-40）、CFG-16（B10 两条管线选 A）；SKILL-23（RR-20261006-21 / 22 / 23）、SKILL-24（撤除重试 + RR-30 / 31）、SKILL-25（改名 Spawn）、SKILL-26（停止入口统一 + RR-32）、SKILL-27（改名 Summon，含 `dismiss` 与 `minion`）、SKILL-28（分区存放 + RR-33 / 34）、SKILL-29（已放弃分区）、SKILL-30（B3 ③ + RR-37 / 39）。说明分册 SKILL 主题开头加了衍生物生命周期的决定链表（每一步的维护者决定与方向判断）。
- **并入的条目**：RR-20261006-20（TaskPool 统计，`41bdb9e5`，维护者选 A）并入 NONCORE-46，原“按 revleft O9 不改”的说法改为已修复。
- **改掉的过时说法**：SKILL-5 / 13 / 18 / 21 的 checkpoint 版本口径（本版为 7，旧版本一律拒绝；O7 / RR-03 当时不改版本）；“B3 ③ 留到下个大版本”（SKILL-13 说明与实现、说明分册“仍待决定”、本文件“改为说明”一行）与“A4 ① 留到下个大版本”（CFG-2）；CFG-7 的“两种标签方言合一列为后续”改为维护者选 A（CFG-16）；CFG-1～5 的“现在的行为”分成 v1.20.2～v1.22.0 的形态与 v1.23.0 起的形态；CFG-12 的配置段来源改为声明渲染；SKILL-1 / SKILL-3 检查点里“没有单独用例”的路径改为已补测（`5c1f4176`）。
- **术语**：SKILL 全部条目与说明里的“进程 / process”改为衍生物 / Spawn，召唤效果改为 Summon；历史引用（决定原话、修前红文本、旧提交说明）保留旧名并注“原名”。
- **本机验证**：只改文档；根包 `GOWORK=off go test -count=1 -run 'Markdown|Conflict' .`（文档链接与冲突标记门禁）通过。

## 重核（按代码冻结提交 `e6828e4f`）

- **引用**：机器比对全部 `path:line`（正文约 490 处）在 `02c8a10d` 与 `e6828e4f` 上的内容和相邻符号名，再人工看没有符号可比的引用；约 165 个测试名逐个核对。共改 8 处：行号 3 处、NONCORE-54 随重写换 3 处、已删除符号 1 处、已改名测试 1 处。明细：NONCORE-14（`entity/remote_snapshot.go` `:392`→`:396`、`:629`→`:634`）、NONCORE-55（`transientRequeueDelay` 定义 `:411`，原文的 `:399` 是调用点）、NONCORE-54（随 RR-20261006-12 重写，`releaseDispatchLocks` 已删除，改为 `dispatchScopeGuard` / `releaseDispatchEntities` 等现名）、SKILL-2（`undoVitals` 已在 A1 删除，改为 DAO `beginChange`）。其余引用与所述符号一致；修前红文本里的 `文件:行` 是当时原文，不改。测试名逐个在 `e6828e4f` 上核对存在（已删除的 CFG-9 用例与 RR-20261005-01 原回归按“已删除 / 已改名”写明）。
- **新增或并入的条目**：RR-20261006-12（原 W-2026-10-06-01，`b7471ae4`）并入 NONCORE-54 并写全；RR-20261005-01 回归改名（`d5682dc4`，`TestActivityRefusesAGroupNoWindowCouldOpenWith` + 新守卫 `TestAGroupFitsOneLiveQuery`）并入 NONCORE-20；`skill/README.md` 52 个旧链接修复（`d05a04a1`）以 TOOL-5 为准，本分册在 SKILL 主题开头与 NONCORE“只引用”表里引用。条目总数仍为 91。
- **去重**：CFG-12 保留，REM-13 已改为索引条目、以 CFG-12 为准；NONCORE-1、23、24、40、45、50、56 改为只保留编号与一句话结论的索引条目，以 APP-4 / 7 / 8 + OPS-3、SAGA-7、OWN-6、CLK-6、OPS-7、APP-9、OWN-1 为准。
- **本机补跑**（`e6828e4f`）：真实 Mongo 副本集 `TestRealMongoCoordinatorLeaseTakeover`（NONCORE-12）与 `TestRealSagaCrossProcessKillRecovers`（NONCORE-18）通过；临时探针（未入库）在真实副本集上跑 Resume 代际持久（NONCORE-16）、Mongo Mod 在已断开客户端上停止（NONCORE-53），以及 mongo-driver v2.6.0 的 12 种带口令 URI 错误文本不含口令（NONCORE-49）；nest `-shuffle` 有界扫描 230 个 seed + `-race` 30 个 seed 全部通过（NONCORE-54）；GitHub framework-compat 在 `e6828e4f` 上全部通过（run `37461843085`，CFG-12、NONCORE-31）。
- **改为说明、不再列为未验证的**（依据都是已有的维护者决定或源码事实，写在各条）：CFG-6（对象不支持 Ref）、CFG-7（B10 分层）、CFG-8（生成工程无 Rollback / 失败的 AfterApply，单测覆盖）、SKILL-5 / 9 / 10 / 19（第十二轮“其余保持”）、SKILL-13（B3 ③ 当时定为下个大版本；第十三轮改为本版完成，见 SKILL-30）、SKILL-14（按上下文判断是表的设计）、SKILL-15（O36）、NONCORE-15（升级兼容说明）、NONCORE-42（bus 去重契约下死信写失败没有重投链）、NONCORE-46（C8 保留；revleft O9 那一条第十三轮改为修复，RR-20261006-20）、NONCORE-51（发版矩阵已在共享隔离环境实跑）。
- **留给 review 的覆盖缺口**（`e6828e4f` 时写成检查点）：SKILL-1 的 `Interrupt` 停衍生物出错分支、toggle release 回调出错分支、charge enter 失败后衍生物的宿主侧残留；SKILL-3 reset 里的衍生物条目。**已全部由 `5c1f4176` 补测闭环**，查出 RR-20261006-21 / 22 / 23（SKILL-23），之后的决定链见 SKILL-24～29。

## 边界项与其他分册的对应（汇总者去重用）

| 本分册条目 | 内容 | 其他分册 | 处理 |
| --- | --- | --- | --- |
| CFG-1 | NC-190 严格布尔 / 时长 | APP-1 提到 `singleton.enabled` | 本条为主 |
| CFG-12 | 生成配置写出 `remote_entity` 五个新键（A8） | REM-13 同一项 | **本条保留，REM-13 已改为索引条目、以本条为准** |
| NONCORE-1 | NC-230～234、`ops.admin_timeout` | APP-4、APP-7、APP-8、OPS-3 | **以对方为准**，本条只留索引 |
| NONCORE-15 | NC-130 / 131、RR-20260913-01 残余 | REM-1 只在背景里提到 | 本条为主 |
| NONCORE-20 | RR-20261005-01 与 C4 后的回归去向 | OWN-5（组文件） | 本条为主（回归对照在本条） |
| NONCORE-23 | NC-250 | SAGA-7 | **以对方为准**，本条只留索引 |
| NONCORE-24 | RR-20261006-05 global `Bind` | OWN-6 | **以对方为准**，本条只留索引 |
| NONCORE-40 | D-L1 / D-L2 timer | CLK-6 | **以对方为准**，本条只留索引 |
| NONCORE-43 | NC-161～165 | APP-12 背景提到 NC-165 | 本条为主 |
| NONCORE-45 | RR-20261006-09 robot Stage 序号 | OPS-7 | **以对方为准**，本条只留索引 |
| NONCORE-50 | NC-193 | APP-9 | **以对方为准**，本条只留索引 |
| NONCORE-51 | N15 脚本、A5 | TOOL-1（NC-205）、TOOL-7（NC-207、全局命令持锁） | 其余 N15 项本条为主 |
| NONCORE-52 | NC-170～174 | APP-6 背景 | 本条为主 |
| NONCORE-53 | NC-260 | DRV-5 的 Close 口径表 | 本条为主 |
| NONCORE-54 | A2 / A3 与 RR-20261006-12（原 W-2026-10-06-01） | DAO 部分写同提交的 RR-20261006-13 | 本条为主 |
| NONCORE-56 | RR-20261004-10 / 11 / 14 | OWN-1 | **以对方为准**，本条只留索引 |
| NONCORE-32、55 | DAO `nocoll`、U-0279 | 其他分册未收 | 本条为主 |
| （引用） | `skill/README.md` 52 个旧链接修复与文档链接门禁（`d05a04a1`） | TOOL-5 | 以 TOOL-5 为准，本分册只引用 |
| CFG-16 | B10 两条管线分工（`2c01e06d`） | OWN-5 的 groups_file 必填在同一提交 | 各写各的部分，互相只引用提交号 |
| CFG-14 / 15 | 生成器 Core 下限需升到 v1.23.0 | OWN-5（`activity.Config.Groups`）同样要求 | 发版必做项，两边都写，打 tag 时一次改 |
| SKILL-24～29 | skill Runtime 状态不进事务（B4） | DAO 部分 | 本分册写衍生物生命周期，B4 以 DAO 为准 |

其他分册标出“未归入任何分册”的 C9 与 B9，本分册已收：NONCORE-31（B6 + C9）、NONCORE-21（B9）。C6（服务指标默认开启）由 OPS-1 收。收尾第 1 批文档（`88f33776`）里本分册只收了 bus `ReliableStore` 注释（NONCORE-8）与 ai O-T3 / O-T4（NONCORE-38），其余（A10 codegen 文档旧模块路径即 N08 O5、A16 CombatComponent 注释、驱动 README Close 契约、`:lease:*` 迁移说明、L2 落后上界）由对应分册处理或只改文档。

## 文档与源码不一致（已在源文档改正）

1. A4 方案（`docs/feature/REFACTOR-2026-10-05-strict-config-reads.md`）的登记键数 16 / 95 / 77 → 17 / 99 / 79，注明实施当时的数。
2. B10 方案 §2.1 / §2.2 “按 encoding/json 的键匹配” → 逐字匹配（`configdata/rules/rules.go:276-280`、`configdata/fieldrules.go:91`），注明来自 CFG-10（v1.22.0）。
3. `docs/skill/skill-casting-and-combat.md` 第 80 行“（O33，未发版）”→“（O33，v1.21.0）”。
4. `2a7d2a65` 的提交说明引用 rebase 前的 `a6e75488`，实际为 `f6043e44`：提交历史不改，在 DECISIONS-PENDING 第七轮表下加更正注。
5. NC-220 / NC-224 修复记录末尾加后注：记录里的修复函数已在同版被求值上下文表取代，写明现在的位置。
6. RR-20261005-01 的回归用例随 C4 删除：已由 `d5682dc4` 在问题 / 修复记录末节闭环（改名为组文件形态 + 新守卫），本分册 NONCORE-20 写了逐项对照。

第二次重核（`5e72ca4d`）发现的三项，都不需要改源文档：

7. 第十三轮各实施记录、DECISIONS-PENDING 实施状态栏与 CHANGELOG 已有条目里的旧名（`process`、`skill.process.*`、`stop_pending_dropped`、`SpawnCommand` 等）按改名记录 §4“历史记录不改”保留；本分册正文统一新名并注“原名”。
8. A4 ② 方案正文描述的三份登记、`ConfigReader` 与正则守卫已被 A4 ① 删除，方案第 46 行已有实施注；`codegen/internal/roost/manifest.go:67` 注释提到 `app.ConfigReader` 是 v1.20.2 抬 Core 下限的历史原因。
9. 作者文档 `docs/skill/skill-casting-and-combat.md:114`“（O22、O29，未发版）”与 `:187`“（O2，未发版）”指本版内容，随发版改为 v1.23.0（发版必做，不是未闭环项）。

## 发版必做（打 tag 时，不是未闭环项）

- `codegen/internal/roost/manifest.go:83` `minimumVersions.Core`（冻结点为 v1.21.0）与 `.github/workflows/framework-compat.yml` minimum 行、`codegen/ci/framework-release.yaml` 升到 v1.23.0（CFG-14 / 15；OWN-5 同样要求）。
- 作者文档两处“未发版”标题改为 v1.23.0（见上第 9 条）。

## 仍未闭环

**0 项。** 新增条目的“未验证”一栏全部为“无”或指向既有外部验证编号（E04）；review 检查点只留给 review 去查的问题，没有“已知风险待判断”；WANTED 未决数 0；DECISIONS-PENDING 第十三轮与本分册有关的决定全部已实施。
