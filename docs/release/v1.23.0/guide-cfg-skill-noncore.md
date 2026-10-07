# v1.23.0 说明文档（分册）：配置、skill 与非核心 review（CFG / SKILL / NONCORE）

本分册是 v1.23.0 发版双文档的一部分，覆盖 `v1.19.2..5e72ca4d`（最终代码冻结提交）里属于三个主题的全部改动：

- **CFG**：严格配置读取、每个 Mod 声明自己的配置（A4 ①）、配置数据规则统一（`configdata/rules`）与两条配置管线分工、热更结果可见、键大小写敏感、cfggen globals 规则、生成配置与生成 TCP 报错。
- **SKILL**：skill 编译器与 Runtime 的收紧（N09 六批、B3、求值上下文表、O 系列观察），衍生物（Spawn）记录生命周期与停止状态机，召唤物（Summon）改名，Host 取值能力表（B3 ③）。
- **NONCORE**：非核心 review N01～N15 中不属于其他主题的修复与收尾。

配套的实现文档是 [impl-cfg-skill-noncore.md](impl-cfg-skill-noncore.md)，两份文档用同一编号（`CFG-n` / `SKILL-n` / `NONCORE-n`）做锚点，每条说明末尾的“实现”链接直达对应条目。APP、OWN、CLK、OPS、TOOL、SAGA、DRV、DAO、REM 主题在同目录的其他分册里，本分册只引用它们的主题名。

**怎么读**：先看下面的条目总表与“本部分总览”，再按需要跳到条目。每条按同一顺序写：结论 → 背景 → 维护者决定 → 现在的行为 → 兼容与迁移 → 限制 / 外部验证 → 链接。行号、符号一律以最终代码冻结提交 `5e72ca4d` 的源码为准（起草时为 `02c8a10d`，第一次冻结 `e6828e4f` 重核，2026-10-07 按 `5e72ca4d` 再次重核，见实现文档开头的“重核说明”）；历史记录与源码不一致的地方在文末单列。“限制 / 外部验证”只写外部环境项并给出外部验证清单的 E 编号，Windows 一律暂存、不保证正确。与其他分册重复的七条（NONCORE-1、23、24、40、45、50、56）只保留一句话结论，以对方分册为准。

**skill 术语**（维护者第十三轮）：技能施放时生成、之后每个 tick 由技能驱动的东西（飞行物、法术场、光束、位移、环绕等）叫**衍生物（Spawn）**，原名“进程”（process），`4451a0a5` 全量改名（SKILL-25）；效果生成的宿主真实单位（陷阱、宠物、图腾）叫**召唤物（Summon）**，效果 `type: summon`、移除用 `dismiss`，原名 `spawn` / `despawn`，`509c381f` 改名（SKILL-27）；衍生物里只跟着召唤物活的那一种 kind 叫 `minion`，原名 `summon`。本分册正文一律用新名；历史记录、修前红文本与旧提交说明里的旧名原样保留，正文引用到时注“原名”。

## 条目总表

| 编号 | 一句话 | 首发版本 | 行为变化 / 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- |
| [CFG-1](#cfg-1) | 布尔开关与时长严格读取（NC-190） | v1.20.2 | 收紧：`on` / 无单位时长启动即报错 | 写错类型的配置要改 |
| [CFG-2](#cfg-2) | 框架配置一律严格读取，启动校验覆盖全部类型化键（A4） | v1.20.2 | 收紧：类型错误启动即点名报错 | 同上；升级前用新版本启动一次 |
| [CFG-3](#cfg-3) | kit/redis 三个整数键严格读取 | v1.20.2 | 收紧（只影响绕过 App 直接装配 Mod） | 否 |
| [CFG-4](#cfg-4) | 生成的 player TCP / RPC 客户端严格读取 | v1.20.2 | 收紧；生成器 Core 下限升到 v1.20.2 | 已生成工程不迁移 |
| [CFG-5](#cfg-5) | `env: production` 只校验有读取方的设置（C1 / NC-192） | v1.20.2 | 放宽 | 否（上线前自己接入限流 / 鉴权） |
| [CFG-6](#cfg-6) | tablegen `ref` 加载时检查、`-check` 按 schema（NC-75） | v1.20.1 | 收紧：悬空 ref 加载失败 | 修正悬空数据 |
| [CFG-7](#cfg-7) | 配置规则统一由运行时加载层强制（B10） | v1.21.0 | **破坏**：违规数据启动 / reload 失败；生成器 Core 下限 v1.21.0 | 修正数据；重新 generate 才有运行时规则 |
| [CFG-8](#cfg-8) | 热更失败与回滚可见（C2） | v1.21.0 | **破坏**：`configdata.reload.total` 标签变化 | 调整依赖 `reason` / `result="rollback"` 的看板 |
| [CFG-9](#cfg-9) | 规则层大小写变体取最后一个（发版前审查） | v1.21.0（v1.22.0 删除） | 被 CFG-10 取代 | 否 |
| [CFG-10](#cfg-10) | configdata 键大小写敏感 | v1.22.0 | **破坏**：只差大小写的键整次拒绝 | 改成声明的拼写 |
| [CFG-11](#cfg-11) | cfggen globals 支持 required / min / enum | v1.23.0（本版） | 新能力；已有输出不变 | 否（可选） |
| [CFG-12](#cfg-12) | 生成配置写出 `remote_entity` 新键（A8） | v1.23.0（本版） | 只影响新生成工程 | 否 |
| [CFG-13](#cfg-13) | 生成 TCP 越界报错逐条点名（A9） | v1.23.0（本版） | 只改报错文本 | 按旧文本匹配的脚本要更新 |
| [CFG-14](#cfg-14) | 每个 Mod 用“配置结构体 + tag”声明自己的配置，启动检查 / 生成器 / doctor 共用（A4 ①，RR-20261006-38） | v1.23.0（本版） | **破坏**：0 / 负数 / 枚举外的值按声明拒绝；syncbus 旧回退删除；生成器 Core 下限升到 v1.23.0 | 修正配置；自写 Mod 改成声明 + `app.LoadConfig` |
| [CFG-15](#cfg-15) | 业务服务声明自己读的键，doctor 读回进程声明，生成工程同样守住“读配置只经声明”（A4 ① 收尾，RR-20261006-40） | v1.23.0（本版） | 收紧：game-demo 漏写 activity / platform 键启动即拒绝；doctor 新增 `config-reads` | 业务代码直接读 viper 的改成声明 |
| [CFG-16](#cfg-16) | 配置数据保持 tablegen 与 cfggen 两条管线，写明分工（B10，维护者选 A） | v1.23.0（本版） | 只改文档 | 否 |
| [SKILL-1](#skill-1) | 施法失败只走一个终态入口（NC-110～112） | v1.20.1 | 收紧：终态 cast 的输入被拒；手动 Release 失败不可重试 | 否 |
| [SKILL-2](#skill-2) | Combatant 副本不共享 map（NC-113） | v1.20.1 | 改副本不再改实体 | 误用者改用 `InitCombatant` |
| [SKILL-3](#skill-3) | skillsync 三条下发路径同一可见性（NC-114 / 115） | v1.20.1 | wire 追加字段；可见性变化时 remove 不下发 | 可见性变化时重发快照 |
| [SKILL-4](#skill-4) | Applier 被拒包不改 epoch（NC-116） | v1.20.1 | 无 | 否 |
| [SKILL-5](#skill-5) | 提交前失败的 cast 有界回收（NC-117） | v1.20.1 | 无（checkpoint 版本后来升到 7，见 SKILL-24～29） | 否 |
| [SKILL-6](#skill-6) | wire 字段名逐字匹配（NC-150） | v1.20.2 | 收紧：非规范大小写 Parse 失败 | 修正技能 JSON |
| [SKILL-7](#skill-7) | 拒绝 Runtime 不派发的 phase 事件（NC-151） | v1.20.2 | 收紧；非零 `timeout_ticks` 给 warning | 修正定义；loadtest 断言 `Warnings == 0` |
| [SKILL-8](#skill-8) | 作者写的 tick 非负（NC-152） | v1.20.2 | 收紧 | 修正负值 |
| [SKILL-9](#skill-9) | 表现缓存等待者、skillcompose 诊断（NC-153 / 154） | v1.20.2 | 无 | 否 |
| [SKILL-10](#skill-10) | 编译器只接受 Runtime / Host 执行的范围（NC-210、212～215） | v1.20.2 | 收紧 | 修正定义与 catalog |
| [SKILL-11](#skill-11) | 移交后 area finish 只结束本衍生物（原名进程，NC-211） | v1.20.2 | 放宽（不再报错） | 否 |
| [SKILL-12](#skill-12) | `NegotiateSchema` 拒绝空区间（NC-216） | v1.20.2 | 收紧 | 否（无生产调用方） |
| [SKILL-13](#skill-13) | lower fail-fast 与 phase 事件表单一来源（B3） | v1.20.2 | 正常定义不变；引入一处回归（v1.21.0 修） | 否 |
| [SKILL-14](#skill-14) | 按 Runtime 求值上下文收紧，修 B3 回归（NC-220～224） | v1.21.0 | 收紧；NC-223 恢复 | 修正定义；v1.20.1 的相关 checkpoint 先排空 |
| [SKILL-15](#skill-15) | 求值上下文表（第五轮，NC-280～283） | v1.21.0 | 收紧 + 放宽；诊断码变化 | 修正定义；按诊断码匹配的工具更新 |
| [SKILL-16](#skill-16) | O33 漂移格子编译期拒绝；O34～O36 写文档 | v1.21.0 | **破坏**：以前能编译的定义启动失败 | 按作者文档改写 |
| [SKILL-17](#skill-17) | minion 衍生物（原名 summon 进程）拒绝 `duration_ticks` 与 area 成员字段（O22） | v1.23.0（本版） | 收紧 | 删掉这些字段 |
| [SKILL-18](#skill-18) | checkpoint 字节确定（O7） | v1.23.0（本版） | 同一版本内字节确定；本版 checkpoint 版本为 7，旧版本拒绝恢复 | 不要跨版本比较字节 |
| [SKILL-19](#skill-19) | result 分支诊断文案（O29）；O15～O17 / O27 / O28 写文档 | v1.23.0（本版） | 只改文案 | 按文案匹配的工具更新 |
| [SKILL-20](#skill-20) | null 默认值实体状态可以 set（RR-20261006-02） | v1.23.0（本版） | 放宽；事件 Before 的缺省值类型变化 | 否 |
| [SKILL-21](#skill-21) | checkpoint 恢复拒绝 `phase_timeout`（RR-20261006-03） | v1.23.0（本版） | 只拒绝不会出现的任务 | 否 |
| [SKILL-22](#skill-22) | 示例 `statusbridge` 能运行 | v1.23.0（本版） | 只改示例 | 否 |
| [SKILL-23](#skill-23) | 衍生物记录随 cast 一起回收；失败启动不留记录；reset 条目带增量的 PrimaryTarget（RR-20261006-21 / 22 / 23） | v1.23.0（本版） | 行为变化：停不下衍生物的失败启动返回非零 cast ID 并保留 failed cast | 否 |
| [SKILL-24](#skill-24) | 宿主停不下的衍生物由 Runtime 退避重试（RR-20261006-21 后续，RR-20261006-30 / 31） | v1.23.0（本版） | 新状态 `stop_pending`；`Host.StopSpawn` 必须幂等；checkpoint 版本 3 | 客户端认 `stop_pending`；宿主 StopSpawn 幂等 |
| [SKILL-25](#skill-25) | “进程”（process）全量改名为衍生物（Spawn） | v1.23.0（本版） | **破坏**：DSL / wire / Host 接口 / 指标改名，不留别名；checkpoint 版本 4；digest 全变 | 技能 JSON、Host 实现、客户端、告警按对照表改名 |
| [SKILL-26](#skill-26) | 衍生物停止入口统一走一套停止 / 待停止状态机（RR-20261006-32） | v1.23.0（本版） | `Shutdown` / `RemoveProgram` 停不下的衍生物转 `stop_pending` 由 Runtime 重试；被拒的停止回调不再重跑 | 改看 `StateSnapshot` / `RetentionStats` |
| [SKILL-27](#skill-27) | 生成宿主单位的效果改名为召唤物（Summon），`despawn` → `dismiss`，衍生物 kind `summon` → `minion` | v1.23.0（本版） | **破坏**：DSL / Host 契约改名；checkpoint 版本 5；环境与 digest 全变 | 技能 JSON、Host 实现按对照表改名 |
| [SKILL-28](#skill-28) | 衍生物记录按字段分区存放、删掉重复的 owned 表；源文档 digest 逐字段；停止循环判空（RR-20261006-33 / 34） | v1.23.0（本版） | checkpoint 版本 6；全部 `SourceDocumentDigest` 改变 | 保存旧源文档 digest 比对的调用方一次性重算 |
| [SKILL-29](#skill-29) | 待停止到上限不删记录，挪进“已放弃”分区（维护者选 B） | v1.23.0（本版） | 新状态 `abandoned`；指标改名 `skill.spawn.abandoned.total`；checkpoint 版本 7 | 告警规则改名；客户端认 `abandoned` |
| [SKILL-30](#skill-30) | Host 取值能力表随编译环境下发，编译器 / Runtime / Host 共用（B3 ③，RR-20261006-37 / 39） | v1.23.0（本版） | **破坏**：`skill.Host` 必须实现 `HostCapabilities()`；环境格式与 authority digest 变化；表外能力启动 / 注册时拒绝 | Host 声明能力表；重新编译 / 重签 |
| [NONCORE-1](#noncore-1) | N01 留项：Ops 同步 bind、停机 hook 预算、停机期 fail-stop 退出码、Mod 停止收尾、`ops.admin_timeout`（NC-230～234） | v1.21.0 | 收紧：Ops 端口被占启动失败；admin 命令 10s 期限 | 同机多实例各配 `ops.addr`；长命令调大 `ops.admin_timeout` |
| [NONCORE-2](#noncore-2) | HTTP JSON 先编码后写；recover 尊重已开始的响应（NC-80 / 81） | v1.20.1 | 编码失败回 500 | 否 |
| [NONCORE-3](#noncore-3) | RateLimiter 每主体 key 上限（NC-82） | v1.20.1 | 行为变化：每主体默认 256 key | 否 |
| [NONCORE-4](#noncore-4) | 生成 TCP 接入停机可重试（NC-83） | v1.20.1 | 生成形状 | 重新生成 `server_gen*.go` |
| [NONCORE-5](#noncore-5) | 生成 TCP“handler 不配合 ctx”用例（A15） | v1.23.0（本版） | 只加测试 | 否（handler 须配合 ctx） |
| [NONCORE-6](#noncore-6) | Bus JetStream RPC 停止排空 / 拒绝回包 / 截获（NC-90～92） | v1.20.1 | 收紧：截获的轻量 RPC 不执行 | 两端 `nats.rpc.transport` 一致 |
| [NONCORE-7](#noncore-7) | etcd `Resign` 按调用方期限（NC-93） | v1.20.1 | `Resign` 可能返回 ctx 错误 | 否 |
| [NONCORE-8](#noncore-8) | bus SETNX 去重保持、写进契约 | v1.23.0（本版） | 只改注释 | 否 |
| [NONCORE-9](#noncore-9) | 迁移输出 WAL 准入前验证（NC-31） | v1.20.0 | 收紧 | 手写迁移候选须提供 loader / Id |
| [NONCORE-10](#noncore-10) | 生成 DAO 深层嵌套修改进入提交（NC-32） | v1.20.0 | 生成形状 | 重新生成 DAO / nested |
| [NONCORE-11](#noncore-11) | versionstore 退避后重读（NC-52） | v1.20.1 | 伪冲突减少 | 否 |
| [NONCORE-12](#noncore-12) | mongotest 数组唯一键、`$in` 具名切片（NC-102、RR-20261006-08） | v1.20.1 / v1.23.0（本版） | 只影响测试替身 | 否 |
| [NONCORE-13](#noncore-13) | 缓存副本与 interest 身份绑定（NC-33 / 34） | v1.20.0 | 收紧 | 否 |
| [NONCORE-14](#noncore-14) | 权威快照身份与最终最低版本（NC-35 / 36） | v1.20.0 | 收紧 | 否 |
| [NONCORE-15](#noncore-15) | L2 CAS 落败、删除墓碑、Stats 清理（NC-130 / 131） | v1.20.2 | 收紧；L2 键格式增字段 | 否 |
| [NONCORE-16](#noncore-16) | saga 三消费者健康与 Resume 持久代际（NC-37 / 38） | v1.20.1 | 持久格式增量 | 协调器统一升级 |
| [NONCORE-17](#noncore-17) | saga 启动身份与完成路由（NC-39 / 40） | v1.20.1 | 收紧：缺摘要的旧记录重投冲突 | 自定义 Store 保存新字段 |
| [NONCORE-18](#noncore-18) | saga outbox 领取与 activity 恢复校验（NC-41 / 42） | v1.20.1 | 收紧 | 否 |
| [NONCORE-19](#noncore-19) | account 换名释放死计划、activity 确认键校验（NC-50 / 51） | v1.20.1 | 行为变化 | 否 |
| [NONCORE-20](#noncore-20) | `activity.game_sids` 启动校验（RR-20261005-01；C4 后由组文件兑现、回归改名） | v1.20.1（v1.20.2 被 C4 取代） | 已取代 | 否 |
| [NONCORE-21](#noncore-21) | activity 窗口条目统一入口与修复入口；account 判定表（B9） | v1.21.0 | 坏条目不再交出；新 Admin 入口 | 否（运维可用新入口） |
| [NONCORE-22](#noncore-22) | account 换名释放未 admitted 计划（第五轮、O37） | v1.21.0 | 行为变化 | 否 |
| [NONCORE-23](#noncore-23) | saga 定义缺失 fence 时退避中步骤记为放弃（NC-250） | v1.21.0 | 迟到成功告警 | 否 |
| [NONCORE-24](#noncore-24) | global `Bind` 同参重试幂等（RR-20261006-05） | v1.23.0（本版） | 放宽 | 否 |
| [NONCORE-25](#noncore-25) | N07 第一批：快照锁、属性层回滚、attribute 生成器、errcode 扫描（NC-60～63） | v1.20.1 | 收紧：非字面量 errcode 报错 | 常量编号改字面量 |
| [NONCORE-26](#noncore-26) | demo 开关热更说明、加载时重建 Gear（NC-64 / 65） | v1.20.1 | 模板 | `roost project sync` |
| [NONCORE-27](#noncore-27) | 生成器不改进程工作目录（RR-20261004-12） | v1.20.0 | 两行提示变绝对路径 | 否 |
| [NONCORE-28](#noncore-28) | go 命令按进程树取消（RR-20261004-13 与补修） | v1.20.0 / v1.20.1 | 无 | 否 |
| [NONCORE-29](#noncore-29) | N08：中断清理、预览、cfggen 帮助、id 工具（NC-70～73） | v1.20.1 | 无 | 否 |
| [NONCORE-30](#noncore-30) | 运行期目录不算生成输入（NC-74） | v1.20.1 | 无 | 否 |
| [NONCORE-31](#noncore-31) | CLI 入口统一接管信号（B6）；联网用例门（C9） | v1.21.0 | 中断先回滚再以同一信号退出 | 否 |
| [NONCORE-32](#noncore-32) | DAO `//roost:dao nocoll` | v1.20.0 | 新能力 | 可选迁移 |
| [NONCORE-33](#noncore-33) | 文件 outbox 清理崩溃遗留的临时文件（RR-20261006-04） | v1.23.0（本版） | 打开时删除精确匹配的临时文件 | 同一目录不能有两个在写的 store |
| [NONCORE-34](#noncore-34) | N10 第一批（NC-120～123） | v1.20.1 | 行为变化：丢弃排队项发 OnEnded | 否 |
| [NONCORE-35](#noncore-35) | actionflow 回调变更进延后队列（B7） | v1.20.2 | **破坏**：`ActionRunner` 回调语义变化 | 按延后语义改写回调 |
| [NONCORE-36](#noncore-36) | N10 第二批（NC-240～247） | v1.21.0 | 行为变化：ai 回调里切换延后 | 否 |
| [NONCORE-37](#noncore-37) | MissionRunner 延后队列；EndAll 清场文档 | v1.21.0 | **破坏**：`MissionRunner` 回调语义变化 | 按延后语义改写；清场先 `EndCurMission` |
| [NONCORE-38](#noncore-38) | ai O-T3 / O-T4 写文档 | v1.23.0（本版） | 只改注释 | 否 |
| [NONCORE-39](#noncore-39) | N11 第一批：timer / spatial / index（NC-140～147） | v1.20.1 | 行为变化：Tick 期间取消 / 改期立即生效 | 模板 `roost project sync` |
| [NONCORE-40](#noncore-40) | timer 同期限按 priority / 登记顺序；未注册类型可见（D-L1 / D-L2） | v1.21.0 | 行为变化 | 模板 `roost project sync` |
| [NONCORE-41](#noncore-41) | `PathFindSystem.Stop` 数据竞争（NC-270） | v1.21.0 | 生成形状 | 模板 `roost project sync` |
| [NONCORE-42](#noncore-42) | failurelog 结果未知不降级（NC-160） | v1.20.2 | 收紧 | 否 |
| [NONCORE-43](#noncore-43) | robot / statslog / log（NC-161～165） | v1.20.2 | 收紧：loadtest 无样本判失败 | 否 |
| [NONCORE-44](#noncore-44) | robot 会话 / 日志 sink / Prometheus 转义等（NC-261～266） | v1.21.0 | 行为变化 | 否 |
| [NONCORE-45](#noncore-45) | robot Stage 序号只增不回收（RR-20261006-09） | v1.23.0（本版） | 行为变化 | 自定义 `IdentityProvider` 覆盖超出 `Count` 的序号 |
| [NONCORE-46](#noncore-46) | N13 遍历与 TaskPool 等（NC-180～185）；TaskPool 统计不再读到结束数大于提交数（RR-20261006-20） | v1.20.2 / v1.23.0（本版） | 行为变化 | 否 |
| [NONCORE-47](#noncore-47) | 遍历回调仓库级契约（C7） | v1.20.2 | 契约成文 | 否 |
| [NONCORE-48](#noncore-48) | container / goroutine 零调用方 API（NC-267～269） | v1.21.0 | 无 | 否 |
| [NONCORE-49](#noncore-49) | Mongo URI 口令脱敏（NC-191） | v1.20.2 | 无 | 否 |
| [NONCORE-50](#noncore-50) | 启动失败先收回 Service（NC-193） | v1.20.2 | 契约补充 | `Shutdown` 须容忍部分初始化 |
| [NONCORE-51](#noncore-51) | N15 脚本与门禁（NC-200～208）、A5 | v1.20.2 | 收紧：glsvet 对没检查到的输入退出 2 | 已有工程 `.gitignore` 补 `/data/wal/` |
| [NONCORE-52](#noncore-52) | 停机三步（NC-170～174） | v1.20.2 | 收紧 | 否 |
| [NONCORE-53](#noncore-53) | Mongo Mod 停止收敛（NC-260） | v1.21.0 | 重复 Close 返回 nil | 否 |
| [NONCORE-54](#noncore-54) | nest 用例隔离（A2 / A3）；派发取锁要求 Guard 作用域（RR-20261006-12，原 W-2026-10-06-01） | v1.23.0（本版） | 生产行为不变；nest 内部派发函数在没有 Guard 作用域时返回错误 | 否 |
| [NONCORE-55](#noncore-55) | Nest 重排抖动（U-0279） | v1.20.1 | 平均重排延迟 5ms → 7.5ms | 否 |
| [NONCORE-56](#noncore-56) | 租约修复 RR-20261004-10 / 11 / 14（同版被静态绑定取代） | v1.20.0 | 代码已删 | 否 |

共 102 条：CFG 16 条、SKILL 30 条、NONCORE 56 条（2026-10-07 按 `5e72ca4d` 补写 CFG-14～16、SKILL-23～30 共 11 条，RR-20261006-20 并入 NONCORE-46）。

## 本部分总览

### 版本时间线

| 版本 | tag 指向 | 日期 | 本部分首发的条目 |
| --- | --- | --- | --- |
| v1.20.0 | `999dc672` | 2026-10-05 | NONCORE-9、10、13、14、27、28（主体）、32、56 |
| v1.20.1 | `be7407ab` | 2026-10-05 | CFG-6；SKILL-1～5；NONCORE-2、3、4、6、7、11、12（NC-102）、16～20、25、26、28（补修）、29、30、34、39、55 |
| v1.20.2 | `c85d4565` | 2026-10-06 | CFG-1～5；SKILL-6～13；NONCORE-15、35、42、43、46、47、49～52 |
| v1.21.0 | `4881f2b7` | 2026-10-06 | CFG-7、8、9；SKILL-14、15、16；NONCORE-1、21、22、23、31、36、37、40、41、44、48、53 |
| v1.22.0 | `9bf690fb` | 2026-10-06 | CFG-10 |
| v1.23.0（本版） | 发版提交 | — | CFG-11～16；SKILL-17～30；NONCORE-5、8、12（RR-20261006-08）、24、33、38、45、46（RR-20261006-20）、54 |

首发版本用 `git tag --contains <提交>` 取最早的 tag 核对；v1.22.0 之后的提交都随 v1.23.0 发布。

### 主题地图

```text
CFG   严格读取： CFG-1 → CFG-2 → CFG-3 / CFG-4 ；CFG-5（同批）→ CFG-14（A4 ① 声明取代登记表）→ CFG-15（业务声明、doctor）
      数据规则： CFG-6 → CFG-7（B10）+ CFG-8（C2）→ CFG-9（被取代）→ CFG-10（大小写）→ CFG-11（globals）→ CFG-16（两条管线分工）
      生成配置： CFG-12、CFG-13（v1.23.0 起配置段由 CFG-14 的声明渲染）
SKILL 终态与同步：SKILL-1～5、SKILL-9、SKILL-12
      编译期收紧：SKILL-6～8、SKILL-10、SKILL-11 → SKILL-13（B3 ①②）→ SKILL-14 → SKILL-15（表）→ SKILL-16（O33）→ SKILL-17～19 → SKILL-30（B3 ③ 能力表）
      收尾小修：SKILL-20、21、22
      衍生物生命周期（第十三轮决定链）：SKILL-23 → SKILL-24 → SKILL-25（改名 Spawn）→ SKILL-26 → SKILL-27（改名 Summon）→ SKILL-28 → SKILL-29
NONCORE 按单元：N01 → 1；N02 → 2～5；N03 → 6～8；N04 → 9～12；N05 → 13～15；N06 → 16～24；N07 → 25～26；
      N08 → 27～32；N09 → 33；N10 → 34～38；N11 → 39～41；N12 → 42～45；N13 → 46～48；N14 → 49～50；N15 → 51；
      跨单元 / 核心线 → 52～56
```

### 行为变化与兼容破坏（本部分）

**破坏性**（以前能用的东西升级后失败，需要改）：

| 条目 | 什么会失败 | 怎么改 |
| --- | --- | --- |
| CFG-7 | 违反 required / unique / min / enum / ref 的配置数据启动或 reload 失败（新生成的 loader；旧 loader 不查 required） | 修正数据 |
| CFG-8 | 依赖 `configdata.reload.total{reason}` 或 `result="rollback"` 的看板 / 告警 | 改查 `configdata_rollback_total{trigger}` 与 `result=ok\|failed` |
| CFG-10 | 键只差大小写的配置数据、CSV 表头；手写的 `FieldRule{Field: "Level"}` | 改成声明的拼写 |
| CFG-14 | 以前“写 0 或负数静默取默认”的键按声明的范围拒绝（如 `dataengine.outbox.workers: 0`）；字符串枚举对所有服务、所有值检查；syncbus 不再回退读 `room.*` / `sync.*`；`app.ConfigReader`、`kit/mods.Duration` / `RedisClusterAddrs` 等读取 API 删除 | 要缺省就不写这个键；syncbus 配置写在 `syncbus.*`；自写 Mod 改成配置结构体 + `app.LoadConfig` |
| CFG-15 | 新生成 game-demo 的 game 配置漏写 `activity.*` / `platform.*` 任一键在启动检查时拒绝；用户工程里直接读 viper 的代码让 `roost project doctor` 的 `config-reads` FAIL | 补齐键；业务读取改为服务声明 + `app.LoadConfig` |
| SKILL-6～8、10、14～17 | 以前能编译的 skill 定义在游戏服启动期 `CompileAll` 失败 | 按错误路径与作者文档改写；仓内 fixture、示例与生成骨架都不受影响 |
| SKILL-25、27 | 用旧名写的技能 JSON 在 Parse 时失败（`unknown field "process"`、`unsupported effect "spawn"`）；Host 实现的方法名、事件名、指标名、客户端看到的 mutation / 表现 kind 全部改名；旧 checkpoint、旧回放记录、按旧 digest 签的 skillcompose 契约不再适用 | 按 [Spawn 对照](../../feature/REFACTOR-2026-10-06-skill-process-to-spawn.md) 与 [Summon 对照](../../feature/REFACTOR-2026-10-07-skill-summon-rename.md) 改名；checkpoint 排空后升级；契约重签 |
| SKILL-24～29 | checkpoint 版本 3 → 7，旧版本得到 `ErrCheckpointUnsupported` | 排空后升级（线上未部署，不做兼容） |
| SKILL-30 | 业务自己的 `skill.Host` 实现不写 `HostCapabilities()` 编译不过；环境格式变化（`CompileEnvironment.Host`，删 `Motion.EnabledSlots` / `HostFeatures`）、所有 authority digest 改变；Program 需要 Host 表外的能力时在启动 / 注册 / 恢复处被拒；配置了 catalog 的 MemoryHost 与 HostAdapter 对表外属性 / 资源报错（原来读成 0） | Host 声明能力表（嵌入 `*skill.MemoryHost` 的自动得到）；重新编译、重签；旧录制回放重录 |
| NONCORE-35、37 | `ActionRunner` / `MissionRunner` 回调里的变更不再立即生效、不再返回 `ErrReentrantMutation` | 按延后语义写：回调里返回 nil，执行错误经 OnError；要清场先 `EndCurMission` 再 `EndAll` |

**收紧**（写错的配置 / 用法从“静默读错”变为报错）：CFG-1～4（类型错误的配置）、CFG-14 / 15（声明外的值、漏写的必填键）、CFG-6（悬空 ref）、SKILL-1（终态 cast 的输入）、NONCORE-1（Ops 端口被占）、NONCORE-6（JetStream 截获的轻量 RPC）、NONCORE-17（缺摘要的旧 saga 记录重投）、NONCORE-25（非字面量 errcode）、NONCORE-42（failurelog 结果未知）、NONCORE-43（loadtest 无样本）、NONCORE-51（glsvet 输入不存在）。

**生成器 Core 下限**：v1.20.2（CFG-4 生成代码用 `app.ConfigReader`，已被 CFG-14 删除），v1.21.0（CFG-7 生成 loader 用 `configdata.FieldRule`），**v1.23.0**（CFG-14 / 15：生成的 player TCP 接入层、RPC 客户端 Mod 与 game-demo 用 `app.LoadConfig` / `app.SchemaOf` / `kitsaga.StreamSettings` / `kitdataengine.EffectSettings`；OWN-5 的 `activity.Config.Groups` 同样要求 v1.23.0）。冻结提交 `5e72ca4d` 上 `codegen/internal/roost/manifest.go:83` 的 `minimumVersions.Core` 仍是 `v1.21.0`，按交接规格“发版必做”在打 v1.23.0 tag 时与 `.github/workflows/framework-compat.yml` 的 minimum 行一起改。**已生成工程一律不迁移**（维护者决定），模板变化在 `roost generate` / `roost project sync` 后生效。

**放宽**：CFG-5（生产校验）、SKILL-11、SKILL-15（投射引用）、SKILL-20、NONCORE-24（Bind 同参重试）。

### 需要业务改代码或配置的清单

1. 升级前用新版本启动一次每个服务，处理所有点名报错的配置键（CFG-1～4）。
2. 跑一次全部 skill 定义的 `CompileAll`，按诊断码与 JSON 路径改写（SKILL-6～8、10、14～17）；按诊断码 / 文案字符串匹配的工具要更新（SKILL-15、19）。
3. 配置数据：修正违反规则与只差大小写的键（CFG-7、CFG-10）；想要运行时规则的工程重新 `roost generate`。
4. 看板：`configdata.reload.total` 的标签（CFG-8）。
5. actionflow / ai 的回调代码按延后语义复核（NONCORE-35、37）。
6. 同机多实例各配 `ops.addr`；超过 10s 的 admin 命令调大 `ops.admin_timeout`（NONCORE-1）。
7. 两端 `nats.rpc.transport` 一致（NONCORE-6）。
8. 已有工程的 `.gitignore` 手工补 `/data/wal/`（NONCORE-51，NC-206）。
9. 用常量编号的 errcode 改成字面量（NONCORE-25）。
10. 自定义 robot `IdentityProvider` 覆盖超过 `Count` 的序号（NONCORE-45）。
11. skill checkpoint：本版 checkpoint 版本为 7，旧版本写出的一律拒绝，升级前排空（SKILL-24～29）；不要拿新旧版本各自写出的字节互相比对（SKILL-18）。
12. 同一目录不要有两个在写的 skillsync 文件 outbox（NONCORE-33）。
13. 配置：自写 Mod 与业务服务读配置改成“配置结构体 + tag”声明 + `app.LoadConfig`；用 `<bin> <service> --check-config` 检查真实生产配置，`--print-config` 查看全部声明（CFG-14、15）。
14. skill 改名：技能 JSON（`process` → `spawn`、`$process` → `$spawn`、效果 `spawn` → `summon`、`despawn` → `dismiss`、kind `summon` → `minion` 等）、Host 实现（`StepProcess` → `StepSpawn`、`PreviewOwnedSpawn` → `PreviewOwnedSummon` 等）、客户端（`process_*` → `spawn_*` 的 mutation / 表现 kind）与告警（`skill.process.*` → `skill.spawn.*`）按对照表改（SKILL-25、27）。
15. skill Host：实现 `HostCapabilities()` 声明能力表，测试里用 `skill.CheckHostCapabilities` 核对声明与行为一致；宿主 `StopSpawn` 必须幂等（SKILL-24、30）。
16. 客户端 / 渲染器按衍生物状态分支时认 `stop_pending` 与 `abandoned`；告警把 `skill.spawn.stop_pending_dropped.total` 换成 `skill.spawn.abandoned.total`（SKILL-24、29）。

## CFG：配置读取、配置数据规则与生成配置

本主题回答四个问题：框架读配置时写错类型会怎样（CFG-1～CFG-5）、每个键由谁声明（CFG-14、CFG-15）；配置数据（`configs/data/*.json`）的规则在哪一层检查、热更失败能不能看见、两条配置管线怎么分工（CFG-6～CFG-11、CFG-16）；生成工程的配置与生成 TCP 的报错（CFG-12、CFG-13）。

主题内的先后关系：

```text
NC-190（布尔 / 时长）──► A4 ② 严格读取（app + kit；同批 C1 生产校验只查有读取方的键）──► A4 留项：kit/redis、生成代码
                       └► A4 ① 每个 Mod 声明配置（v1.23.0，取代三张登记表与 ConfigReader）──► 收尾：业务服务声明、doctor 读回进程声明
NC-75（tablegen ref + -check）──► B10 规则统一到 configdata/rules（运行时强制）+ C2 热更可见
                                   └► 发版前审查：大小写变体取最后一个（v1.21.0）──► 大小写敏感（v1.22.0，取代前者）
                                   └► cfggen globals 规则（v1.23.0）
                                   └► 两条管线保持、写明分工（v1.23.0，维护者选 A）
```

<a id="cfg-1"></a>
### CFG-1 布尔开关与时长严格读取（RR-20261005-NC-190）

> 其他分册对应：APP-1 提到 `singleton.enabled` 的严格布尔（NC-190），本条为主。

**结论**：`singleton.enabled: on`、不带单位的时长（`15`）等写法从“静默读成 false / 15ns”变为启动即报错并点名键。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-1)

**背景**：viper 的 `GetBool` / `GetDuration` 经 `spf13/cast` 宽松转换，失败返回零值不报错，`ToDuration` 给不带单位的数字补 `ns`。N14 review 发现 `singleton.enabled: on` 被读成 false，单实例锁静默关闭；`saga.step_defaults.timeout: 5` 读成 5ns；`redis.cluster_addrs` 写成 YAML 列表时 `GetString` 读成空串，Redis Mod 退回 localhost。来源：[N14 review](../../review/REVIEW-2026-10-05-n14.md)。

**决定**：这是 bugfix，没有单独的维护者决定；随后的方向判断（“还剩约 80 处宽松读取”）交给维护者，形成 A4（见 CFG-2）。

**现在的行为**：

| 项 | 行为 |
| --- | --- |
| 新 API | `app.ConfigBool` / `app.ConfigDuration`：设置了却不是合法值时报错并点名键，未设置返回零值 |
| 布尔接受 | YAML 布尔、`strconv.ParseBool` 认的字符串（`"true"`、`True`）、整数 0 / 1 |
| 时长接受 | `time.ParseDuration` 认的字符串、`time.Duration`、0；拒绝不带单位的非零数字 |
| 单实例锁 | `singleton.*` 读取错误先报，不管 `enabled` 读成了什么（`on` 不再让校验提前返回） |
| 其他读取点 | `kit/mods.Duration` / `RequiredDuration`、saga 步骤预算时长改用 `app.ConfigDuration`（v1.23.0 起这些读取函数删除，键由各 Mod 的声明读，见 CFG-14） |
| `redis.cluster_addrs` | `kit/mods.RedisClusterAddrs` 接受逗号串或 YAML 列表并去空白（v1.23.0 起是 `kit/redis.ClusterConfig` 的 `[]string` 字段，同样接受两种写法） |

**v1.23.0 起**（CFG-14，A4 ①）：严格解析规则不变（同一套 `on` / 无单位时长报错），实现移到叶子包 `internal/configschema`，由每个 Mod 的配置声明调用；`app.ConfigBool` / `ConfigDuration` 保留为单键解析（工具、测试用）。单实例锁的键由 `singletonConfig` 声明，写错类型由启动检查报出，不管 `enabled` 读成了什么。

错误示例（来自修前红用例的断言）：`ValidateServiceConfig = <nil>; want an error naming singleton.enabled`——修后返回点名该键的错误。

**兼容与迁移**：行为收紧。生成的配置都写 `true` / `false` 与带单位的时长，生成 game-demo 20 份配置逐份通过。升级前用新版本启动一次即可发现写错的键。

**限制 / 外部验证**：真实 Redis Cluster 上用 YAML 列表写 `cluster_addrs` 起服未验证（本机无 Cluster），见外部验证 E08。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-190.md) · [问题](../../bug/RR-20261005-NC-190.md)

<a id="cfg-2"></a>
### CFG-2 框架配置一律严格读取，启动校验覆盖全部类型化的键（A4）

**结论**：app 与 kit 里的布尔、时长、整数配置全部改为严格读取；`ValidateServiceConfig` 在任何 Mod Init 之前按三份登记表检查，一次报全。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-2)

**背景**：配置没有 schema，靠宽松 getter 加事后清单校验（RR-20260926-12、NC-190、NC-192、N02 O2）。NC-190 只修了开关与少数时长，kit 里还有约 80 处宽松读取：`nest.worker_num: 8k` 读成 0（取默认）、`dataengine.wal.queue_capacity: 1.5` 被截断等。

**维护者决定**（[DECISIONS-PENDING 第二轮](../../review/DECISIONS-PENDING-2026-10-05.md)，A4 行原文）：“按推荐：先 ②（逐个改严格读取），① schema 作为后续重构”。可选方案是 ① 每个 Mod 声明配置 schema、校验 / 生成器 / doctor 共用；② 逐个 Mod 改用严格读取。选 ② 的理由：改动可以分批做、立刻堵住静默读错；① 涉及全部 Mod 的声明形态，当时列为下个大版本。**第十三轮维护者要求 ① 本版完成**（“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”），已实施，见 CFG-14。

**v1.20.2～v1.22.0 的行为**（v1.23.0 起三张登记表、`ConfigReader` 与正则守卫被 CFG-14 的声明取代，严格解析规则不变）：

- 新增 `app.ConfigInt` / `ConfigInt64`（接受 YAML 整数、没有小数部分的浮点数如 `1e3`、十进制字符串；拒绝小数、带后缀、时长、布尔值）与汇总错误的 `app.ConfigReader`（Mod Init 一次读几十个键，读完统一 `Err()`，运维一次看到全部写错的键）。
- `ValidateServiceConfig` 按 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys` 逐个严格读取（删除前的最后形态 `e6828e4f` 上分别为 17 / 99 / 79 个键，另有 syncbus 三段 `syncbus` / `room` / `sync` 的同名字段与按后缀登记的 `<service>.call_timeout`）。A4 实施时是 16 / 95 / 77 个，之后 C6、`ops.admin_timeout`、B2 / O4 / Mirror 第 5 步 / O-M6-3 各自登记了新键（见实现文档）。
- kit 的 dataengine、remoteentity、saga、nats、nest、syncbus、mongo、ops、etcd、statslog、platform、activity 与 app 自身改用 `ConfigReader`，错误前缀沿用各 Mod 原有的（如 `dataengine mod: …`）。
- 唯一保留的宽松读取是 `sid`（启动校验先严格检查它是 int32 范围内的正整数）。

**兼容与迁移**：行为收紧。`true` / `false` / `1` / `0` / `"true"`、带单位的时长、整数与十进制字符串照常接受。以前被静默读错的写法现在启动失败。删除了 `remote_entity.sync_retry_queue_cap` 的非负检查（没有任何代码读它）。

**后续**：三份登记表加源码扫描守卫是过渡形态，登记集中在 app；第十三轮按维护者要求在本版换成每个 Mod 自己的声明（A4 ①，CFG-14），登记表与 `ConfigReader` 已删除。

**链接**：[A4 方案](../../feature/REFACTOR-2026-10-05-strict-config-reads.md) · [验证记录](../../bugfix/RR-20261005-NC-192.md)（“A4 严格读取的验证”一节）

<a id="cfg-3"></a>
### CFG-3 A4 留项：kit/redis 的三个整数键严格读取

**结论**：`redis.db` / `redis.pool_size` / `redis.min_idle_conns` 由 Redis Mod 与单实例锁的 `SingletonStore` 严格读取，`8k`、`1.5`、`10s` 在 Init 时点名报错。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-3)

**背景**：A4 实施时 kit/redis 由 A2 的 agent 在改，这三处 `GetInt` 留作例外，由启动校验兜住，守卫按“文件 + 键”放行。

**决定**：A4 留项，第二轮决定的延续（DECISIONS-PENDING A4 行“未做”一栏划掉、写明 `5df60765`）。

**现在的行为**：未设置或 ≤ 0 的 `pool_size` / `min_idle_conns` 仍取驱动默认值；错误形如 `redis mod: config: redis.db …`。守卫删掉了这三项放行。v1.23.0 起三个键写在 `kit/redis.Config` 的声明里（`min:"0"`，负数按声明拒绝），Redis Mod 与 `SingletonStore` 共用这一份声明（CFG-14）。

**兼容**：经 App 启动的进程此前已由 `ValidateServiceConfig` 拒绝这些值，行为不变；变化只在绕过 App 直接装配 Mod 的调用方（测试、工具）——从“读成 0”变为报错。

**链接**：[A4 方案“A4 留项：kit/redis”](../../feature/REFACTOR-2026-10-05-strict-config-reads.md)

<a id="cfg-4"></a>
### CFG-4 A4 留项：生成的 player TCP 接入层与 RPC 客户端 Mod 严格读取

**结论**：生成进工程的 `internal/access/player/tcp/server_gen.go` 与 RPC 客户端 Mod（`*_rpc_assembly_gen.go`、`add rpc` 产物）改用 `app.ConfigReader` / `app.ConfigDuration`；生成器 Core 下限随之升到 v1.20.2。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-4)

**背景**：生成工程按生成器的 Core 下限解析 roost-core，A4 时下限是 v1.20.1，没有 `app.ConfigReader`，所以这两份模板留用 viper getter。修前在 game-demo 里：`enabled: on`、`max_payload_bytes: 8k`、`max_handshake_bytes: -1`、`idle_timeout: 90`、`handshake_timeout: soon`、`nest.request_timeout: 7` 六项 Mod Init 都返回 nil（读成 false / 默认 / 90ns / 7ns）。

**决定**：A4 留项（同上），随 v1.20.2 抬生成器下限时实施。

**现在的行为**：

- player TCP：全部读完后 `read.Err()` 一次报出所有写错的键，例如 `player tcp: config: player_access.tcp.enabled must be true or false, got "on"`；`max_handshake_bytes` / `max_payload_bytes` 先按 int 读，负数或超过 16 MiB 点名拒绝，再转 uint32（以前 `GetUint32(-1)` 读成 0、取默认）。
- RPC 客户端：`app.ConfigDuration(cfg, "<service>.call_timeout")`，错误包成 `<pkg> client mod: config: <service>.call_timeout …`。

**兼容与迁移**：经 App 启动的进程在 v1.20.2 core 上此前已被启动校验拒绝，行为不变；变化在直接调用 Mod Init 的路径。新生成代码需要 core ≥ v1.20.2；**已生成工程不迁移**（维护者决定），`roost generate` 后取新模板。

**v1.23.0 起**（CFG-14）：两份模板改成配置结构体声明 + `app.LoadConfig`（player TCP 的 `tcpConfig` 由生成器里的 `playerTCPDeclaration` 渲染，RPC 客户端 Mod 的 `clientModConfig` 写在模板里），新生成代码需要 core ≥ v1.23.0。

**链接**：[A4 方案“A4 留项：生成进工程的代码”](../../feature/REFACTOR-2026-10-05-strict-config-reads.md) · 提交 `914a725f`

<a id="cfg-5"></a>
### CFG-5 `env: production` 只校验有读取方的设置（C1 / RR-20261005-NC-192）

**结论**：生产校验删掉九组没有任何代码读取的要求，生成的生产示例与 k8s Secret 示例打开 `env: production` 可以启动。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-5)

**背景**：`validateProductionServiceConfig` 按服务名要求 `player.login_auth_required`、`player.login_secret`、`player_protocol.rate_limit.enabled`、`save_load.wal.*`、`instance.*`、`account.ops_token`、`account.redis_required`、`global` / `match_group.redis_required` 为真。这些键来自开源初版的服务形态，全仓没有读取方。生成的生产配置不写它们，打开 `env: production` 后 game / account / global 起不来；写上又让人以为限流、鉴权、WAL 已经打开。

**维护者决定**（DECISIONS-PENDING 第二轮 C1 行原文）：“随 A4 按方案 1”。方案 1 = 删掉无效要求并写明校验范围，之后接入真实限流 / 鉴权开关时再加回。未采用：方案 2（换成真实生效的键，需要先在生成接入层装配按请求限流与鉴权）、方案 3（只改措辞，误导仍在）。

**现在的行为**：保留 ops 端点不绑公网（或声明 `ops.allow_public_addr`）、game / instance / account / match_group / global 必须写 `redis.addr`、account / platform 的密钥不是空的或 `dev-` 开头、admin_gateway 令牌。另有 CLK 主题的 `time.logic_offset` 生产必须为 0（见 CLK 部分）。USER_GUIDE §10 写明校验范围。

**v1.23.0 起**（CFG-14）：生产规则跟着键的主人走，不再集中在 app 的 `validateProductionServiceConfig`（已删除）——密钥非空非 `dev-` 是声明上的 `secret` tag，ops 端点不绑公网在 `kit/ops` 配置的 `ValidateConfig`，Redis 地址（`redis.addr` 或 `redis.cluster_addrs`）在 `kit/redis.Config` 的 `ValidateConfig`，`time.logic_offset` 在 App 自己的 `appConfig.ValidateConfig`；`admin_gateway.*` 的生产检查删除（仓内没有任何代码读这个段，同 C1 方案 1 的原则）。

**兼容**：放宽。旧配置里为通过校验写的这些键留着也无影响（本来就不控制任何行为）。注意：生成的游戏服接入层没有按请求限流、只有演示凭据，上线前要自己接入。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-192.md) · [USER_GUIDE](../../USER_GUIDE.md)

<a id="cfg-6"></a>
### CFG-6 tablegen 的 `ref` 在加载时检查，`-check` 按 schema 规则校验 JSON（RR-20261005-NC-75）

**结论**：tablegen schema 里的 `ref:"<table>"` 由生成的 loader 在每次加载 / reload 检查；`tablegen -json <dir> -check` 按 required / unique / min 校验 JSON。首发 v1.20.1；运行时机制已被 CFG-7（B10）整体替换。[实现](impl-cfg-skill-noncore.md#cfg-6)

**背景**：ref 此前只写进 CSV 规则行、哪里都不检查，悬空引用的配置能加载上线；`-check` 只验 JSON 语法。来源：N08 codegen review。

**决定**：bugfix；“运行时 required 存在性”当时交给维护者，后来成为 B10（CFG-7）。

**现在的行为（v1.20.1 时）**：ref 目标必须是同一 schema 的表、字段类型等于其主键类型，否则 `roost generate` 失败；生成的 loader 带逐表手写的 `ValidateTable` 循环。**v1.21.0 起**这段循环被 `Rules: []configdata.FieldRule{...}` 取代（CFG-7），`-check` 改用 `configdata/rules`。

**兼容**：悬空 ref 的数据此后加载 / reload 失败（旧快照保持）；没有 ref 的表生成物不变。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-75.md) · [问题](../../bug/RR-20261005-NC-75.md)

<a id="cfg-7"></a>
### CFG-7 配置规则统一由运行时加载层强制（B10）

**结论**：required / unique / min / enum 在每次 Load / Reload 对原始 JSON 检查（缺列与零值分得清），ref 在全部表加载后检查；违反即整次拒绝、旧快照保持。规则的表示与检查只有一份：叶子包 `configdata/rules`。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#cfg-7)

**背景**：三套检查实现（tablegen 的 `validateRows` / `parseCell`、生成 loader 里的 ref 循环、auto 表的 `validateRefs`），两种语义的 `required`。直接改 `configs/data/*.json` 再 `gm.config.reload` 只经过运行时那一层，required / unique / min 全被绕过——N07 H2e 实测：删掉 spawn 的 `template` 后 reload 被接受，按 template 0 刷怪。

**维护者决定**（DECISIONS-PENDING 第四轮 B10 行原文）：“按推荐：configdata 新增能看到原始字段是否出现的接口，必填在加载 / 热更时检查（A）；规则统一由运行时加载层强制，生成期检查只作提前反馈。维护者附加原则：**config 使用要容易，手写整理代码尽量少，结构简单易懂**”。未采用方案 B（生成的校验器重读原始 JSON：有替换窗口、每表 25～30 行胶水）。tablegen 与 cfggen 能否合成一套：B10 时评估为后续，第十三轮维护者在澄清后**选 A：保持两条管线、规则已统一、写明分工，不合成一套**（`2c01e06d`，见 CFG-16）。

**现在的行为**：

| 项 | 内容 |
| --- | --- |
| 声明 | `configdata.FieldRule`（= `rules.Rule`）挂在 `TableDef.Rules` / `ObjectDef.Rules`；auto 表的 `cfg` 标签解析成同一组规则 |
| required | 统一为“键出现且不为 null”；`required` + `ref` 时值必须是目标表主键（零值不再当“无引用”放过） |
| 新规则 | tablegen 新增 `enum:"a\|b"` 标签；cfggen 新增 `unique` / `min` / `enum`，`required` 不再要求配 `ref` |
| 注册期 | 规则字段必须对应行类型的一个字段；`Min` 只用于数值、`Enum` 只用于字符串 / 整数 / bool、`Ref` 只用于整数 / 字符串；对象不支持 `Unique` / `Ref`。拼错的规则启动即失败 |
| 错误 | `*configdata.RuleError`，例如 `configdata: table spawn row 1 (key 1) field template: required: missing or null` |
| 生成 API | tablegen 每表多一个 `<Type>Table()`（取当前请求钉住的快照里的表，nil 安全） |
| 业务胶水 | game-demo 读 spawn 表 15 行 → 3 行；“防删列读成零值”从无入口 → 0 行（标签即生效） |

**兼容与迁移**：

- **行为收紧**：以前直接改 `configs/data` 再 reload 能绕过的数据，现在启动 / reload 失败。
- 新生成的 loader 用 `[]configdata.FieldRule`，**生成器 Core 下限升到 v1.21.0**。
- **已生成工程不迁移**：旧 loader 在新 core 上照常编译、行为不变（只是不查 required）；重新 `roost generate` 后获得运行时规则。
- `codegen` 层原本不得 import core，这里给根包边界测试加了唯一例外 `configdata/rules`，并加门禁要求该包只依赖标准库。

**链接**：[B10 / C2 方案与实施](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) · [NC-75 后续](../../bugfix/RR-20261005-NC-75.md) · 两条管线分工见 CFG-16

<a id="cfg-8"></a>
### CFG-8 热更失败与回滚可见（C2）

**结论**：契约不变（新的一代在 AfterApply 之前已发布），但每次 Load / Reload / Rollback 恰好报告一次结果并写日志；kit 指标改为低基数标签。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#cfg-8)

**背景**（N07 C-O1 / C-O5 / C-O6 / C-O7）：build / 监听者 Validate 阶段失败的 reload 不经过任何监听者，日志与指标里都没有痕迹；AfterApply 失败被撤回时 metrics 监听者先记一次 `ok`、回滚又记一次 `rollback`；`reason` 标签是 GM payload 里运维自由填写的文本；observability README 写“热更后版本 +1”不对。

**维护者决定**（DECISIONS-PENDING 第四轮 C2 行原文）：“‘需要可见’：按协调者理解——保持新快照即刻可见的语义并写进契约；同时让热更失败 / 回滚可见（日志 + 指标，N07 C-O5）”。

**现在的行为**：

- 契约（configdata 包注释、USER_GUIDE §10）：Reload 先 build + Validate + BeforeApply，再**发布**，再跑 lifecycle emit 与 AfterApply；发布到 AfterApply 结束之间准入的请求读新的一代；AfterApply 失败时撤回，期间准入的请求在整个生命周期里读被撤回的那一代（钉住准入时的快照，不撕裂）。业务不能接受就把检查放进 Validate / BeforeApply。
- `Store.OnReloadOutcome(fn) (unsubscribe)`：每次恰好一次，含阶段（`build` / `validate` / `before_apply` / `apply`）、是否已发布后撤回、候选版本、当前版本、错误。
- 日志：成功 Info `config reload applied`；失败 Warn `config reload failed: the live generation is unchanged`（带 stage、reason、version、error）；撤回 Warn `config reload reverted: …`；运维 Rollback Info `config rolled back`。
- 指标（kit configdata Mod）：`configdata.reload.total{result=ok|failed}`（每次 Load / Reload 一笔）、`configdata.rollback.total{trigger=apply_failed|operator}`、`configdata.version`。`reason` 只进日志。
- 版本号来自单调计数器，失败、撤回与 DryRun 也占号：成功后版本变大但不一定 +1。

**兼容与迁移**：指标标签变化——`configdata.reload.total` 去掉 `reason`，`result` 不再有 `rollback`；按 `sum(rate(configdata_reload_total))` 的看板不受影响，按 `reason` / `result="rollback"` 的要改。v1.23.0 起 game-demo 仪表盘有“配置撤回”面板（OPS 部分）。

**限制**：无外部项。生成的 game-demo 没有运维 Rollback 入口，也没有会失败的 AfterApply 监听者，所以 `stage=apply` 撤回与运维 Rollback 没有真实进程路径可演练；这两条由单测在同一个 configdata `Store` 上覆盖。

**链接**：[B10 / C2 方案](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)

<a id="cfg-9"></a>
### CFG-9 规则层的大小写变体按 encoding/json 取最后一个（发版前审查，已被 CFG-10 取代）

**结论**：v1.21.0 发版前审查发现规则检查的值可能与类型化行不同，修为“同一列几种大小写拼写按原文顺序取最后一个”；v1.22.0 的大小写敏感（CFG-10）删除了这段逻辑。首发 v1.21.0，v1.22.0 起不再存在。[实现](impl-cfg-skill-noncore.md#cfg-9)

**背景**：encoding/json 解进结构体时，几个只差大小写的键落到同一字段按文档顺序最后一个生效（精确拼写并不优先）；`rules.Lookup` 却先取精确键、否则按 map 遍历顺序取变体。`{"level":5,"Level":0}` 的类型化行是 0，规则查到 5，`min=1` 会放过 0；没有精确键时结果随 map 顺序变。

**决定**：发版前审查观察，维护者授权“确认是缺陷的按先红后绿修”。

**v1.21.0 的行为**：`rules.Rows` 对一行里只差大小写的键按原文顺序每组只保留最后一个；手拼 map 的 `Lookup` 取字节序最小的键。**v1.22.0 起**这类行在加载时整次拒绝（CFG-10），选择逻辑删除。

**兼容**：只影响一行里同一列有几种拼写的数据。读者只需知道：v1.21.0 上这类数据能加载、规则按最后一个值查；v1.22.0 起加载失败。

**链接**：[发版前审查观察收尾 §5 与更正](../../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md)

<a id="cfg-10"></a>
### CFG-10 configdata 键大小写敏感

**结论**：数据文件里的键必须与字段的 json 名逐字一致（嵌套对象同样）；只差大小写的键在 Load / Reload / DryRun 整次拒绝、旧快照保持；tablegen 的 CSV 表头同样精确。首发 v1.22.0。[实现](impl-cfg-skill-noncore.md#cfg-10)

**背景**：encoding/json 的键匹配大小写不敏感，`{"Level":1}` 静默填进 `level`；严格模式（`DisallowUnknownFields`）也放过。tablegen 的 CSV 表头只差大小写时被当成未知列跳过，这一列的值静默丢成零值。

**维护者决定**（[下一轮规划](../../review/NEXT-ROUND-PLAN-2026-10-06.md)第 2 项，原话）：“configdata 需要大小写敏感。”原则沿用 B10（使用容易、手写代码少、结构简单）。做法参照 skill NC-150 的 `requireExactFieldNames`（见 SKILL-6）。

**现在的行为**：

- 错误是 `*configdata.RuleError`，`Rule` 为 `case`，点名表、行（1 起，对象为 0）、行主键、应有拼写：

  ```text
  configdata: table monster row 1 (key 1) field level: case: key "Level" must be spelled "level" (keys are case-sensitive)
  configdata: table monster row 1 (key 1) field rewards[1].item_id: case: key "Item_ID" must be spelled "item_id" (keys are case-sensitive)
  ```

- 逐层核对：嵌套结构体、切片 / 数组元素、map 的值；map 的键、`interface{}`、自带 `UnmarshalJSON` 的类型由作者决定，不核对。
- **未声明的键维持原行为**：宽松模式忽略，严格模式由解码报 `unknown field`。
- `FieldRule.Field` 必须逐字是 json 名，否则注册失败。
- CSV：只差大小写的表头报 `header "Level" must be spelled "level" (column names are case-sensitive)`；生成的 `Convert<Type>CSV` 带同一规则的标准库副本 `tablegenCheckHeader`（不抬生成器 Core 下限）。

**兼容与迁移**：**破坏性**——以前能加载的、键只差大小写的配置数据现在启动 / reload 失败，改成声明的拼写即可。生成器写出的数据全是精确键，不受影响。已生成工程不需要重新生成（校验在 core 里），重新生成只多出 CSV 表头检查。性能：每次加载多解析一次载荷，只在加载 / 热更时发生。

**限制**：结构体里同一 json 名在不同嵌入深度重复时按“浅层优先”取类型，与 encoding/json 的歧义丢弃不完全相同（只影响那种结构体的嵌套核对）。

**链接**：[方案](../../feature/CONFIGDATA-CASE-SENSITIVE-KEYS-2026-10-06.md) · [下一轮规划](../../review/NEXT-ROUND-PLAN-2026-10-06.md)

<a id="cfg-11"></a>
### CFG-11 cfggen globals 支持 `required` / `min` / `enum`

**结论**：cfggen 的 globals（单例配置对象）可以写 `required` / `min` / `enum`，生成为 `ObjectDef.Rules`，每次 Load / Reload 用与 tables 相同的 `configdata/rules` 检查。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#cfg-11)

**背景**：B10 时 cfggen 对 globals 上的规则直接报错（`singleton configs do not support … yet`），列为后续；运行时其实早已支持 `ObjectDef.Rules`（tablegen 的 object 在用）。

**维护者决定**（DECISIONS-PENDING 第十二轮，维护者原话“B 类的都按照推荐即可，mongo 的延迟可以分析下”；本行：“支持 required / min / enum，与 tablegen 统一”）。

**现在的行为**：

- globals 允许 `required` / `min` / `enum`；`unique`（单个对象没有可比的行）、`ref`、`index` 仍拒绝。
- 生成形状：`configdata.ObjectDef[WorldCfg]{Name: "world", File: "world.json", Rules: []configdata.FieldRule{{Field: "width", Required: true, Min: "1"}, …}}`。
- 生成期用同一个 `rules.Rule.Validate` 检查规则声明（tables 与 globals 都过）。global 的 struct 不再带规则 `cfg` 标签（对象注册路径不读标签，写了是不生效的第二份）。
- 违反时启动失败、reload 被拒且旧快照不动。

**兼容**：只放开以前报错的写法；已有 meta 的输出逐字不变；业务只在 meta 里写规则、重新生成。

**链接**：[第十二轮 skill 与 cfggen 记录 §1.6](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md) · [B10 方案 §2.4](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)

<a id="cfg-12"></a>
### CFG-12 生成配置写出 `remote_entity` 的新键（收尾第 2 批 A8）

> 其他分册对应：REM-13 是同一项。汇总去重：本条保留，REM-13 改为引用本条。

**结论**：新生成工程的开发配置、生产示例与 k8s Secret 示例带上 B2 / O4 / O-M6-3 / Mirror 第 5 步新增的五个 `remote_entity` 键，取值等于 core / kit 缺省，并附中文注释。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#cfg-12)

**背景**：这些键只在 USER_GUIDE 里，运维要去文档里找键名才知道能调。

> v1.23.0 后续（CFG-14）：生成器的配置段不再是 `catalog.go` 里手写的字符串，改由 kit Mod 的配置声明渲染（`example` 写成生效行、`help` 写成注释）；这五个键的写入值现在来自 `kit/remoteentity/config.go` 声明上的 `example`，取值与下表相同。

**决定**：维护者第十一轮“收尾：盘点全部未完成问题，处理完后统一发一个版本”，协调者盘点项 A8，已授权。

**现在的行为**：

| 键 | 写入值 | 依据 |
| --- | --- | --- |
| `cached_max_staleness` | `30s` | 零值取 `snapshot_cache_ttl`（模板为 30s）；kit 要求设置了的值为正，所以写与它相同的值 |
| `snapshot_interest_per_consumer` | `0` | 0 = `snapshot_interest_subs / 16` |
| `snapshot_l2_tombstone_wait_replicas` | `1` | `DefaultConfig` 为 1 |
| `snapshot_l2_tombstone_wait_timeout` | `50ms` | `DefaultConfig` 为 50ms，上限 1s |
| `mirror.shutdown_timeout` | `5s` | kit `defaultMirrorShutdownTimeout`；只有只读的 `RemoteMirrorMod` 读，手工接入时要调大 `shutdown.total_timeout` 与宽限期 |

连带修正：生产化原来把整串 `replicas: 1` 替换成 `replicas: 3`，会把墓碑 `WAIT` 的副本数也改成 3；现在只替换独占一行、键名恰为 `replicas` 的行。

**兼容**：只影响新生成的工程；已有工程的配置归应用所有，不回写。v1.21.0 / v1.22.0 的 kit 不认识墓碑两键，viper 忽略未知键，生成器 Core 下限不变。

**链接**：[收尾第 2 批记录 A8](../../bugfix/CLOSING-BATCH-2-2026-10-06.md)

<a id="cfg-13"></a>
### CFG-13 生成 TCP 的配置上限逐条点名报错（收尾第 2 批 A9）

**结论**：生成的 `validateConfig` 不再只回一句 `addr, limits and timeouts are outside safe bounds`，而是一次报全每个越界的 `player_access.tcp.<键>`、当前值与范围，受约束的键点出被约束的键与理由。接受 / 拒绝的边界不变。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#cfg-13)

**背景**：N02 观察 O2：运维只能逐行猜哪个键越界。

**决定**：收尾盘点 A9（同 CFG-12）。

**现在的行为**：例：

```text
player tcp: player_access.tcp.max_handshakes = 2000 exceeds player_access.tcp.max_connections = 1000; every handshake runs on an accepted connection
player tcp: player_access.tcp.handshake_timeout = 2m0s is outside (0, 1m0s]
```

约束关系：`max_connections_per_ip`、`max_handshakes` 不能超过 `max_connections`；`login_timeout` 不能超过 `dispatch_timeout`（登录在一次 dispatch 里）。`max_connections` 本身不合法时不再报关系类错误。多条错误用换行分隔（`errors.Join`）。

**兼容**：只改报错文本；按旧文本匹配的脚本要更新。已生成工程需重新生成 `internal/access/player/tcp/server_gen*.go` 才有新文本。

**链接**：[收尾第 2 批记录 A9](../../bugfix/CLOSING-BATCH-2-2026-10-06.md)

<a id="cfg-14"></a>
### CFG-14 每个 Mod 用“配置结构体 + tag”声明自己的配置，启动检查 / 生成器 / doctor 共用一份声明（A4 ①，RR-20261006-38）

**结论**：app、kit 全部 Mod 与生成的 player TCP / RPC 客户端 Mod 都把自己读的键写成一个配置结构体（字段类型就是键的类型，tag 写键名、缺省、范围 / 枚举、必填、密钥、示例、说明），读配置只剩 `app.LoadConfig` 一个入口；App 在任何 Mod Init 之前合并本服务全部声明检查配置、一次报全；生成器的配置段与 `roost project doctor` 用同一份声明。CFG-2 的三张登记表、`app.ConfigReader` 与正则守卫删除。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#cfg-14)

**背景**：一个配置键的知识分散在四处——Mod 的 `Init` 读取（缺省值、范围散在 `if v <= 0` 里）、app 的三张手写清单（17 / 99 / 79 个键）、生成器 `catalog.go` / `framework_services.go` 里手写的配置字符串、doctor（不知道任何键的类型）。新加一个键要改四处，漏一处没有任何东西报错，A8、A4 两批都出过“清单漏键”“模板缺键”。A4 ② 只解决了“读错类型静默”，没有解决“四处各写一遍”。

**维护者决定**：

- 第二轮 A4 行原文“按推荐：先 ②（逐个改严格读取），① schema 作为后续重构”，① 当时列为下个大版本（CFG-2）。
- 第十三轮“A4①：维护者要求本版完成”，原话：“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”；线上未部署，不做旧格式兼容。
- 第十三轮“配置声明形式”：**维护者同意 A：配置结构体 + tag**。另一种形式（Mod 方法返回键表 + 读取句柄，`app.IntKey("…", 2).Min(1)`）未采用：每个键要写一次声明、一次读取，“声明了没读”要靠扫描句柄变量，读出的值没有结构。
- 原则沿用维护者的配置易用原则：手写整理代码尽量少、结构简单、规则只在运行时加载时强制一次。

**现在的行为**：

| 项 | 内容 |
| --- | --- |
| 声明 | `config:"键"`、`default`、`min` / `max`（闭区间）、`enum`（`\|` 分隔，忽略大小写与空白，读出值规范成小写）、`required:"true"`、`secret:"true"`（生产环境非空且不以 `dev-` 开头）、`example`（生成器写成生效行）、`help`（写成注释）；嵌套结构体拼前缀，`config:"x,closed"` 的段与 map 元素出现未声明键报错；跨键规则写成可选方法 `ValidateConfig(production bool) error` |
| Mod 写法 | `func (*Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(modConfig{}) }` + `Init` 里 `app.LoadConfig(cfg, &m.cfg)` 两行；业务 Mod 同样写（[方案 §2.7 前后对比](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)） |
| 启动检查 | App 合并 App 自己（`appConfig`）、本服务全部 Mod 的声明，在任何 Mod Init 之前检查；两个 Mod 声明同一个键时声明必须完全相同，否则启动报错；进程不报“没有任何 Mod 声明的键”（生成配置会带别的进程才注册的 Mod 的键），拼错的键名由 doctor 判断 |
| 错误 | 类型错误文本不变（`config: x must be true or false, got "on"`、`… needs a unit …`）；新增范围 / 枚举 / 必填一律 `config: <键> …` 开头、点名键，`errors.Join` 一次报全 |
| 进程 flag | `--check-config`（加载并检查配置后退出，不启动 Mod，用来检查真实生产配置）、`--print-config`（打印本服务全部声明生成的配置段，含业务 Mod） |
| 生成器 | `kit/internal/configschemagen` 把 kit 各 Mod 的声明快照成 `codegen/internal/roost/kitconfig_gen.go`（`go:generate`）；配置段从快照渲染：有 `example` 的键写成生效行，其余写成带缺省值的注释行，`help` 写成键上方注释 |
| doctor | 新增 `config-schema:<服务>`：开发配置、生产示例、k8s Secret 示例按该服务注册的 Mod 合并声明检查（类型、范围、枚举、必填、生产密钥为 FAIL；框架段里出现没有声明的键为 FAIL） |
| 守卫 | “读了没声明”（框架代码对 viper 的任何读方法即失败）、“声明了没读”（带 `config` tag 的字段必须在本包非测试代码里被读）、“声明与 Init 对得上”（逐键写错误类型调用 `Init` 要点名该键）；生成器侧快照与 kit 当前声明一致、生成的全量工程配置逐份通过声明 |

**实施中发现**：[RR-20261006-38](../../bug/RR-20261006-38.md)（P3）——生成的每个服务配置 `shutdown:` 段写着 `serve_wait_timeout: 5s`，app、kit 没有任何代码读它（运维调它什么都不会发生）。由新守卫 `TestGeneratedConfigsMatchDeclarations` 第一次运行发现，修复删掉这一行（停机顺序与预算由 `shutdown.total_timeout` 统一管）。

**兼容与迁移**（线上未部署，不做旧格式兼容）：

- **行为收紧**：以前“写 0 或负数静默取默认”的键按声明的范围拒绝（例：`dataengine.outbox.workers: 0`），要缺省就不写这个键；文档本来就写“0 取框架默认”的键（`nest.*` 的 worker / 容量、`remote_entity.snapshot_interest_per_consumer` 等）声明成 `min:"0"`，行为不变。
- 字符串枚举（`nats.rpc.transport`、`sync.transport`、`syncbus.transport`、`sync.entity.mode`、`persistence.engine` …）对所有服务、所有值检查。
- syncbus 只读 `syncbus.*` 段，旧的 `room.*` / `sync.*` 同名回退删除；etcd 的 gate 回退删除。
- 生产规则跟着键的主人走（见 CFG-5 的 v1.23.0 注）；`admin_gateway.*` 的生产检查删除。
- 生成的配置文件形状改变（每个框架段带说明注释，未写进 starter 的键以注释列出缺省值）；已生成工程的配置是应用自有文件，不迁移。
- 公开 API：新增 `app.ConfigSchema`、`app.SchemaOf`、`app.LoadConfig`、`app.CheckConfig`、`app.ModConfigSchema`、`App.CheckServiceConfig`、`App.ServiceConfigSchema`；删除 `app.ConfigReader` / `NewConfigReader`、`kit/mods.Duration` / `RequiredDuration` / `KeyPrefix` / `Secret` / `ResolvePersistenceEngine` / `RedisClusterAddrs`。`app.ConfigBool` / `ConfigDuration` / `ConfigInt` / `ConfigInt64` 保留为单键严格解析（工具、测试用）。
- **生成器 Core 下限**：生成的 player TCP 接入层与 RPC 客户端 Mod 调用 `app.LoadConfig` / `app.SchemaOf`，需要 core v1.23.0；`minimumVersions.Core` 与 framework-compat 的 minimum 行在打 tag 时上调（冻结点上仍是 v1.21.0，见“本部分总览”）。

**限制**：无外部项。业务 Mod 的声明只有编译后的进程知道——这一点由 CFG-15 收尾。

**链接**：[A4 ① 方案与实施](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md) · [RR-20261006-38 问题](../../bug/RR-20261006-38.md) · [修复](../../bugfix/RR-20261006-38.md) · [A4 ② 方案](../../feature/REFACTOR-2026-10-05-strict-config-reads.md)

<a id="cfg-15"></a>
### CFG-15 业务服务声明自己读的键，doctor 读回进程声明，生成工程同样守住“读配置只经声明”（A4 ① 收尾，RR-20261006-40）

**结论**：`RegisterServer` 注册的业务服务与 Mod 同等对待——实现 `app.ModConfigSchema` 的服务，它的声明进 App 启动检查、`--check-config` / `--print-config` 与新的 `--print-config-schema`；新生成的 game-demo 由 `game/settings` 声明 game 业务代码读的 `activity.*` / `platform.*` 键；`roost project doctor` 编译一次工程、读回每个服务的进程声明，并新增 `config-reads` 检查；“读了没声明 / 声明了没读”的判定挪到叶子包，app、codegen 用例与 doctor 共用。新生成 game-demo 的 doctor 零 WARN、零 FAIL。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#cfg-15)

**背景**：CFG-14 实施后留下两件事（方案 §6 自己列出），登记为 [RR-20261006-40](../../bug/RR-20261006-40.md)（P3）：新生成 game-demo 的 doctor 有一行无法消除的 WARN（game 配置里的 `activity.groups_file`、`activity.key_prefix`、`platform.key_prefix`、`platform.payment_secret` 由业务代码直接读，没有任何声明，写错类型、拼错、漏写都要到业务代码第一次读才暴露）；守卫只扫 app 与 kit，新生成的 game-demo 有 16 处直接读 viper。

**维护者决定**：v1.23.0 发版前“交给 review 前不留能绕过检查的分支和 WARN”（第十三轮“不留 WANTED”的同一原则）。实施取舍：业务键由**业务服务本身**声明，而不是给生成器加业务 Mod 注册钩子（每个工程多一个文件、bootstrap 形状变化）；doctor 用一次 `go build` 加每个服务一次 `--print-config-schema` 读回进程声明，而不是 `go run . <svc> --check-config`（它只答“值对不对”，答不了“这个键有没有声明”）。

**现在的行为**：

- 业务服务写法与 Mod 相同：`func (*Service) ConfigSchema() app.ConfigSchema { return settings.Schema() }`，错误前缀 `service <名字>:`。
- game-demo：`game/settings` 声明 `activity.key_prefix` / `activity.groups_file`、`platform.key_prefix` / `platform.payment_secret`（全部 `required`，`payment_secret` 另加 `secret`）；`saga.*` / `dataengine.*` 是本进程框架 Mod 的键，经新 API `kitsaga.StreamSettings` / `kitdataengine.EffectSettings` 按 Mod 自己的声明读，不在业务里抄一份缺省值；生成的 `world_singleton.go` 经 `app.ServiceIdentity` + `app.LoadConfig` 读 `sid`。
- doctor：`config-schema:<服务>` 检查业务键的值，框架段里由业务声明的键不再算“本服务没人读”，只有业务声明用到的段里出现没有声明的键为 FAIL；工程能编译但读不出进程声明时为 FAIL。新增 `config-reads`（FAIL）：用户工程里直接读 viper 的业务代码点名。实测新生成 game-demo 全量 doctor（strict）约 19s。

**兼容与迁移**：game 配置漏写 activity / platform 任一键在启动检查时拒绝（以前分别到 activity 启动、controller 构造时才拒绝）；`world_singleton.go` 对超出 int32 的 `sid` 报错（以前截断）；doctor 在能编译的工程里多一次 `go build`；已生成工程是应用自有代码，不迁移。

**限制**：守卫是按名字的语法检查：把非 viper 的值命名成某个 `*viper.Viper` 参数 / 字段的名字并调 `Get*` 会被误报，改名即可（修复记录“未验证 / 风险”唯一一条，属已知误报形态，不是待验证项）。

**链接**：[A4 ① 方案 §7](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md) · [RR-20261006-40 问题](../../bug/RR-20261006-40.md) · [修复](../../bugfix/RR-20261006-40.md) · [codegen README 配置段](../../../codegen/README.md)

<a id="cfg-16"></a>
### CFG-16 配置数据保持 tablegen 与 cfggen 两条管线，写明分工（B10，维护者选 A）

**结论**：配置数据（`configs/data/*.json`）继续有两条生成管线——tablegen（策划 Excel / CSV 表，`roost generate` 默认）与 cfggen（YAML 描述、JSON 数据、含全局单例 globals，可选）；规则早已统一在 `configdata/rules`（CFG-7、CFG-11），不合成一套，只在文档里写明“该用哪条”。首发 v1.23.0（本版，只改文档）。[实现](impl-cfg-skill-noncore.md#cfg-16)

**背景**：B10 §2.4 评估“两种标签方言能否合成一套”时列为后续：合一要改写所有已有工程的 schema，收益只是少一个解析函数。之后交给维护者在澄清后选 A / B。

**维护者决定**（DECISIONS-PENDING 第十三轮“B10 两条配置管线”行）：“维护者选 A：保持 tablegen（策划 Excel / CSV 表，`roost generate` 默认）与 cfggen（YAML 描述、JSON 数据、含全局单例 globals，可选）两条管线，规则已统一（`configdata/rules`），写明分工；不合成一套”。

**现在的行为**：[B10 §2.4](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md#24-tablegen-与-cfggen-能否合成一套) 写明选 A 与理由；[USER_GUIDE 配置数据一节](../../USER_GUIDE.md#配置数据规则热更与可见性) 与 [codegen README](../../../codegen/README.md#配置管线该用哪条) 各加“该用哪条”对照表。两条管线生成的 loader 都走同一个 `configdata` 加载层、同一份 `configdata/rules` 检查。

**兼容**：无代码变化。

**链接**：DECISIONS-PENDING 第十三轮 · 提交 `2c01e06d`（与 OWN-5 的 groups_file 必填同一提交）

## SKILL：skill 编译器、Runtime 与同步

本主题的主线是一个方向判断：**skill 编译器认可的写法与 Runtime / Host 真正会执行的写法各自维护**，N09 六批 review 里同一形状的缺陷出现了二十多次（NC-110～117、NC-150～154、NC-210～224、NC-280～283）。维护者分三步改结构，而不是继续补孤立分支：

| 步骤 | 做了什么 | 条目 |
| --- | --- | --- |
| 第一步：施法终态单一入口 | 失败、取消、释放出错都走 `failCastLocked` | SKILL-1 |
| 第二步：编译期只接受 Runtime 会执行的形状 | 字段名逐字、不派发的 phase 事件拒绝、tick 非负、catalog 名字、Host 都拒绝的取值 | SKILL-6～SKILL-10 |
| 第三步（B3）：lower fail-fast + 事件派发表单一来源 | lower 查不到名字一律 `LOWER_UNRESOLVED`；phase 事件只在 `phaseEventTable` 写一次 | SKILL-13 |
| 第四步（第五轮）：求值上下文表 | 编译期作用域与 Runtime 求值查同一张表 | SKILL-14、SKILL-15 |
| 第五步（第七轮）：漂移格子编译期拒绝 | 表里只剩“可用 / 不可用”两种格子 | SKILL-16 |
| 收尾（第十二轮） | O22 / O7 / O29 与作者文档 | SKILL-17～SKILL-19 |
| 第六步（第十三轮，B3 ③）：Host 取值能力表 | 编译器、Runtime、Host 对“能读什么、支持什么”查同一张表；表外能力在编译期或启动 / 注册时拒绝，不再施法到一半、扣费之后才失败 | SKILL-30 |

同一个方向判断在 Runtime 一侧又出现一次：**衍生物（原名进程）的记录生命周期与停止**。SKILL-1 / SKILL-3 的 review 检查点补测时查出 RR-20261006-21 / 22 / 23，之后两天里同一块接连出问题（RR-30 / 31 / 32 / 34），属于 roost-coding“同一机制反复出缺陷要上报方向判断”的信号。维护者按顺序做了七个决定，每一步都把“各入口各自处理”收成一处：

| 顺序 | 维护者决定（原话或决定行） | 方向判断 | 做了什么 | 条目 |
| --- | --- | --- | --- | --- |
| 0 | SKILL-1 / SKILL-3 检查点补测（交接 sktest） | 记录生命周期补测，暴露“已停记录从不删” | 记录随 cast 回收；失败启动不留记录；reset 条目带增量的 PrimaryTarget | SKILL-23 |
| 1 | “如果是技能本身的问题是不是技能自己处理比较好” | 选 B：Runtime 对自己启动的东西负责到底（不是交给调用方重试） | 宿主停不下的衍生物进 `stop_pending`，退避重试、有上限与告警、进 checkpoint | SKILL-24 |
| 2 | “skill的一个流程叫做进程很奇怪，用更专业的词语” → “Spawn吧” | 名字跟着领域走，不留旧名 | `process` 全量改名为 `spawn`（衍生物） | SKILL-25 |
| 3 | “停止入口统一” | 失败处理收进一个函数，入口只“请求停止”；登记表 + 源码守卫固定 | 唯一停止函数 `requestSpawnStop`，九个入口登记 | SKILL-26 |
| 4 | “第 1 类改 Summon”；kind 改 minion“按照推荐处理”；移除召唤物用 `dismiss`（“用推荐的”） | 两类 spawn 分开命名，消除混读 | 召唤物改名 Summon；`despawn` → `dismiss`；kind `summon` → `minion` | SKILL-27 |
| 5 | 两张表选 A：“如果为了性能可以给spawns分类map，这样也不用扫全部” | 状态只由记录字段表达，分区由字段决定，只有一个函数改字段 | 四个分区、唯一写入口 `setState`、checkpoint 只存一份 | SKILL-28 |
| 6 | 待停止上限在三个候选（A 维持 / B 放弃分区 / C 去掉上限）里选 B，并要求从结构上解决、不再按入口逐个打补丁 | 消除“记录在别人手里时消失”这一类路径 | 到上限不删记录，挪进第五个分区“已放弃”，只在 Advance 末尾清理 | SKILL-29 |

与其他主题的边界：**skill Runtime 状态不进事务（B4）** 与 **buff 属性投影 `CombatComponent.ProjectAttributes`（N09 O2）** 在 DAO 部分；本部分只写示例 `statusbridge` 的修复（SKILL-22）。文件 outbox 遗留临时文件（RR-20261006-04）在 NONCORE（见 NONCORE-33）。

**对业务作者最重要的一句**：v1.20.2 与 v1.21.0 两次收紧了编译器，以前能编译的定义可能在游戏服启动期 `CompileAll` 失败（错误带 JSON 路径与诊断码）。升级前用新版本跑一次 CompileAll；按[作者文档](../../skill/skill-casting-and-combat.md)“引用在哪里能读”的改写对照修改。所有收紧都只拒绝“以前要么每次施法失败、要么静默不执行、要么结果随机”的定义；正常定义的 gameplay digest 不变。**v1.23.0 另有两次改名（SKILL-25 衍生物 Spawn、SKILL-27 召唤物 Summon）与能力表（SKILL-30）**：用旧名写的技能 JSON 与 Host 实现要按对照表改名，所有 digest 都变，checkpoint 版本 7、旧的一律拒绝——这些是名字与格式的变化，不是语义变化。

<a id="skill-1"></a>
### SKILL-1 施法失败只走一个终态入口（RR-20261005-NC-110～112）

**结论**：启动失败、Cancel / Interrupt / Release 出错、排程失败都经 `failCastLocked` 收尾：撤掉本 cast 的全部排程、停衍生物（原名进程）、释放 policy 槽位。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-1)

**背景**：8 个终止点各自手写收尾，失败路径漏掉的步骤各不相同：启动失败不撤任务却复用 ID（旧任务落到下一个拿到同一 ID 的 cast 上，失败启动后的 checkpoint 也恢复不了，NC-110）；回调出错直接返回，cast 停在半终止、施法者永久 `ErrCasterBusy`（NC-111）；排程失败不释放 policy 槽位，下一次激活对失败 cast 执行 toggle-off（NC-112）。来源：[N09 第一批](../../review/REVIEW-2026-10-05-n09-batch1.md)。

**决定**：bugfix。未采用“启动失败不复用 ID”：ID 进入随机键派生，改复用规则会改变所有后续 cast 的随机序列与回放。

**现在的行为**：

- 对 failed / finished / 已取消等终态 cast 调 `Cancel` / `Interrupt` / `Release` 返回 `ErrCastInputRejected`（之前会执行它的回调）。
- 手动 `Release` 在 charge 重新进施法窗口后付费失败：cast 进 failed，不能再重试（与 auto release 一致；之前停在 preparing、占住施法者）。
- 排程失败的 toggle / hold / charge 释放槽位，下一次激活开始新 cast。

**兼容**：行为收紧（见上）。checkpoint 格式与 wire 不变，只是少了失效任务。

**后续**：本条 review 检查点里“没有单独用例”的三条路径（Interrupt 停衍生物出错、toggle release 回调出错、charge enter 失败后衍生物在宿主侧的残留）已由 `5c1f4176` 补测，查出并修复 RR-20261006-21 / 23（SKILL-23）；“失败后停不下的衍生物由谁负责”在 SKILL-24 / 26 定案。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-110.md) · [NC-111](../../bugfix/RR-20261005-NC-111.md) · [NC-112](../../bugfix/RR-20261005-NC-112.md)

<a id="skill-2"></a>
### SKILL-2 combatcomponent 的 Combatant 副本不再共享 map（RR-20261005-NC-113）

**结论**：`Combatant()` 返回、`InitCombatant` 存入的 `ElementMultipliersBP` 都是拷贝。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-2)

**背景**：`combat.Combatant` 唯一的引用类型字段是这张 map，值拷贝与直接赋值都共享它；改副本或改共用的配置模板会在事务、逆操作与脏标记之外改掉权威战斗状态。

**兼容**：公开 API、BSON / JSON 不变；依赖“改副本就能改实体”的误用不再生效，应改用 `InitCombatant`。每次 `Combatant()` 多一次小 map 分配。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-113.md)

<a id="skill-3"></a>
### SKILL-3 skillsync 三条下发路径共用同一可见性规则（RR-20261005-NC-114 / NC-115）

**结论**：presentation reset、state 快照、增量三条路径都按同一 `VisibilityPolicy` 过滤；cast / 衍生物 remove（原名 process remove）带上归属实体。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-3)

**背景**：可见性在快照、增量、presentation reset 三处各写一套：reset 完全不过滤，不可见施法者的持续表现、目标与坐标发给所有 observer（NC-114）；ability 快照按空 handle、增量按具体 handle 问 `FieldVisible`；cast / process（今衍生物）remove 不带归属实体，persistent remove 不看 `Binding`（NC-115）。来源：[N09 第二批](../../review/REVIEW-2026-10-05-n09-batch2.md)。

**决定**：bugfix。reset 复用现有 `FilterPresentation`（自定义策略无需改动）；未采用“在 `VisibilityPolicy` 接口上加 `FilterPresentationReset`”（破坏所有自定义策略的编译、同一规则写两遍）。

**现在的行为**：

- wire：`StateMutation` 的 `cast_remove` / `spawn_remove`（原名 `process_remove`，SKILL-25 改名）多出 `caster` / `owner` 字段（`omitempty`，只追加；旧客户端不读）。
- 行为收紧：observer 可见性在 upsert 与 remove 之间变化时以 remove 时为准，“先可见后不可见”的实体其 remove 不再下发——**业务应在可见性变化时重发快照**。
- presentation reset 可能因策略报错而失败（fail-closed，与 state 快照一致）。

**限制**：没有接 kit syncstream / NATS 端到端（外部验证随 E04）。自定义策略若依赖 `PresentationEvent.Sequence`，reset 里的还原事件取 `LatestPresentationSequence`。

**后续**：本条检查点“reset 里的衍生物条目没有专门用例”已由 `5c1f4176` 补测，查出并修复 RR-20261006-22（reset 用 lifecycle 实体冒充 PrimaryTarget，SKILL-23）。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-114.md) · [NC-115](../../bugfix/RR-20261005-NC-115.md)

<a id="skill-4"></a>
### SKILL-4 skillsync Applier 不再被一个畸形 full 包卡死（RR-20261005-NC-116）

**结论**：开新 epoch 的状态只在准入成功时置位。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-4)

**背景**：`admit` 在 epoch 判定处直接置 `pendingEpoch`，随后 `BaseSequence` 检查拒绝时不复位；之后每个包（包括合法的恢复 full）都返回 `ErrApplyInProgress`。

**兼容**：准入成功的路径不变。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-116.md)

<a id="skill-5"></a>
### SKILL-5 提交前失败的 cast 按 `CompletedCastLimit` 回收（RR-20261005-NC-117）

**结论**：完成队列收所有终态（finished / failed），与 checkpoint 恢复的口径一致。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-5)

**背景**：提交前失败的 cast（commit 付费不足、Cancel / Release 回调失败）永不进完成队列，累计超过上限（默认 2048）后 checkpoint 恢复判 corrupt。

**兼容**：本条修复本身不改 checkpoint 格式。旧版本写出的、已超上限的 checkpoint 在旧版本同样恢复不了，修复不自动迁移。v1.23.0 发版时 checkpoint 版本为 7（SKILL-24～29 先后 3 → 7），旧版本写出的一律 `ErrCheckpointUnsupported`。“完成队列可以合法地长于上限（剩下的全被钉住）”这一口径由 RR-20261006-30 在恢复侧对齐（SKILL-24）。

**限制**：无外部项。生产 Host 的 checkpoint 与世界成对恢复没有正式接线（N09 O3，第十二轮“skill 剩余观察：其余保持”），本条只在 MemoryHost 上成立。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-117.md)

<a id="skill-6"></a>
### SKILL-6 wire 字段名逐字匹配（RR-20261005-NC-150）

**结论**：skill 定义 JSON 的字段名必须与规范 JSON 名逐字一致；`"ID"`、`"Cooldown_Ticks"` 这类写法 Parse 失败。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-6)

**背景**：严格解码依赖 `DisallowUnknownFields`，而 encoding/json 按大小写不敏感匹配；重复键扫描按字节比较。两套规则之间的缝让 `{"id":"a","ID":"b"}` 同时被接受且后者生效、`"Cooldown_Ticks": 0` 被当作 `cooldown_ticks`。来源：[N09 第三批](../../review/REVIEW-2026-10-05-n09-batch3.md)。

**决定**：bugfix。未采用“重复键扫描按大小写折叠”（挡不住单独出现的变体，还误伤 map 里合法的大小写不同的名字）；未采用 `encoding/json/v2`（Go 1.27 仍需 `GOEXPERIMENT=jsonv2`）。configdata 的大小写敏感（CFG-10）后来照这条做。

**现在的行为**：非规范大小写的键（顶层、嵌套、表达式、presentation、`ParseGenerated` 的 rejection）报 `json: unknown field "X" (field names are case-sensitive)`。以名字为键的 map（memory、persistent_state、attribute_overrides、parameter_bindings）只约束值、不约束键。

**兼容**：行为收紧；规范 JSON 不受影响；checkpoint / skillsync 记录里的 `RuntimeValue` JSON 由 Go `Marshal` 产生，本来逐字一致。仓外若有工具用非规范大小写写技能 JSON，升级后 Parse 失败。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-150.md)

<a id="skill-7"></a>
### SKILL-7 编译期拒绝 Runtime 不派发的 phase 事件，`timeout_ticks` 不再豁免 fallthrough（RR-20261005-NC-151）

**结论**：phase 的 `on.recast` / `on.timeout` 编译期报错；enter 落空不再因 `timeout_ticks` 而通过；非零 `timeout_ticks` 给 warning。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-7)

**背景**：编译侧比 Runtime 多了 `recast`、`timeout` 两个事件，Runtime 从不派发；`timeout_ticks` 只被生命期 pass 读来豁免 enter fallthrough，Runtime 没有 phase 计时——这类 tap 技能每次施法 `ErrProgramInvariant`。

**维护者决定**：B3 ④（DECISIONS-PENDING 第二轮 B3 行）：“④ 保持 B”，即保持编译期拒绝（方向 B），不实现 phase 计时与 recast（方向 A）。第十二轮：“NC-151 timeout_ticks：保持 warning”。

**现在的行为**：

| 写法 | 诊断 |
| --- | --- |
| `on.recast`、`on.timeout` | error `CAPABILITY_UNKNOWN`，路径 `$.phases[i].on.<event>` |
| 非零 `timeout_ticks` | warning `CAPABILITY_UNKNOWN`，路径 `$.phases[i].timeout_ticks`（不被执行） |
| enter 可能落空 | error，消息 “end it with finish or goto (the runtime has no phase timeout)” |

**兼容**：带这些形状的定义在启动期 CompileAll 失败。删除了从未执行 recast 分支的 `recast_combo.json` fixture。**注意**：game-demo 的机器人 `cmd/loadtest` 断言 `Warnings == 0`，带 `timeout_ticks > 0` 的定义会让它失败；fireball 与 `roost add skill` 骨架都是 0。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-151.md)

<a id="skill-8"></a>
### SKILL-8 作者写的 tick 非负在 shape pass 集中检查（RR-20261005-NC-152）

**结论**：`cooldown_ticks`、phase `timeout_ticks`、wait `ticks`、repeat `interval_ticks`、chain `hop_interval_ticks`、add_status `duration_ticks` 为负时按字段报 `SHAPE_INVALID`。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-8)

**背景**：非负规则按字段散落在各 pass，五个字段没有规则；负 wait 只被预算 pass 在 `$` 以饱和后的 MaxInt64 报出。

**兼容**：负值定义启动期编译失败；0 与正值不变。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-152.md)

<a id="skill-9"></a>
### SKILL-9 表现缓存与 skillcompose 两处小修（RR-20261005-NC-153 / NC-154）

**结论**：`VisualPlanCache` 的共享加载被创建者取消时，ctx 仍有效的等待者重新加载，不再继承别人的 `context.Canceled`（NC-153）；`skillcompose.ValidateCandidate` 对空 / 重复 source 给出 `PROVENANCE_MISMATCH` 诊断（NC-154）。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-9)

**兼容**：成功路径、真实失败、等待者自身取消的行为不变；skillcompose 只多一条诊断。

**限制**：无外部项。资源加载器由业务客户端提供，用例用可控的假加载器覆盖“创建者取消”与“真实失败”两种时序；skillcompose 仓内无正式调用方。

**链接**：[NC-153](../../bugfix/RR-20261005-NC-153.md) · [NC-154](../../bugfix/RR-20261005-NC-154.md)

<a id="skill-10"></a>
### SKILL-10 编译器只接受 Runtime / Host 实际执行的范围（RR-20261005-NC-210、NC-212～NC-215）

**结论**：memory 名字、catalog 名字、catalog key、只编译不执行的字段、Host 都拒绝的取值，在编译期点名字段报错；新增“编译 ⇒ 可执行”的变异性质测试。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-10)

**背景**（[N09 第四批](../../review/REVIEW-2026-10-05-n09-batch4.md)）：lower 对查不到的名字用 map 零值兜底，指向槽位 / handle 0：

| 编号 | 问题 | 现在 |
| --- | --- | --- |
| NC-210 | set / add / clear_memory 用未声明的名字，落到槽位 0 静默改写别的 memory；add_memory 用在非 int memory | `REFERENCE_UNKNOWN` / `TYPE_MISMATCH` |
| NC-212 | CompileEnvironment 九类 catalog key 为空或重复，按 key 查时有 last-wins 与 first-wins 两种口径 | `CATALOG_DUPLICATE_HANDLE` 到 `…[i].key` |
| NC-213 | chain `allow_repeat` / `hop_interval_ticks`、attribute_modifier `stack_policy` / `max_stacks` 被 lower、进摘要，却从不传给 Host | 只接受默认值，否则 `SHAPE_INVALID`（方向 B） |
| NC-214 | effect、filter、cost 里的 status / attribute / resource 名字不在 catalog 时兜底成 handle 0 | `CAPABILITY_UNKNOWN` |
| NC-215 | modifier 运算 `set`、modifier 时长 ≤ 0、add_status 时长 0、resource 运算 `mul_bp`、cost 字面量为负、attribute_compare `op:"zz"` | `SHAPE_INVALID`（只收紧两个参考 Host 与 Runtime 都拒绝的取值） |

**维护者决定**：NC-213 的方向 A（实现 chain 间隔 / 重复、modifier 叠层）留给维护者，B3 ④ 定为“保持 B”。

**兼容**：行为收紧。业务 catalog 若有重复 key，升级后所有定义都编不出 Program，需要修正 catalog 数据。combatcomponent 的 StatusBridge 若将来支持 0 时长或新运算，要同时放宽这里与 MemoryHost。

**链接**：[NC-210](../../bugfix/RR-20261005-NC-210.md) · [NC-212](../../bugfix/RR-20261005-NC-212.md) · [NC-213](../../bugfix/RR-20261005-NC-213.md) · [NC-214](../../bugfix/RR-20261005-NC-214.md) · [NC-215](../../bugfix/RR-20261005-NC-215.md)

<a id="skill-11"></a>
### SKILL-11 移交后的 area 回调 `finish` 只结束本 area 衍生物（原名进程，RR-20261005-NC-211）

**结论**：施法先结束、召唤效果（原名 spawn 效果）带出的 area 衍生物已移交时，回调里的 `finish` 只停止本 area 衍生物；施法仍存活时照旧结束施法。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-11)

**背景**：编译器允许 area 回调 finish，但不知道施法会不会先结束；移交后 Runtime 对这个合法状态返回 `ErrProgramInvariant`，`Advance` 失败。

**决定**：采用方向 A（Runtime 处理这个合法状态）。未采用“编译期禁止 entity 作用域 area 回调 finish”：会拒绝 `area_membership.json` 这类施法存活时合法的定义。

**兼容**：施法存活时的行为不变；checkpoint 形状不变。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-211.md)

<a id="skill-12"></a>
### SKILL-12 `skillsync.NegotiateSchema` 拒绝空区间（RR-20261005-NC-216）

**结论**：任一边 `Min` 为 0 或 `Min > Max` 时返回 `ErrSchemaNegotiationFailed`；协商结果必须同时被两边 `Contains`。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-12)

**兼容**：仓内无生产调用方；把 `SchemaRange{}` 当“任意版本”传入的调用方现在协商失败（与 `Contains` 口径一致）。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-216.md)

<a id="skill-13"></a>
### SKILL-13 lower 查找失败一律报编译错误；phase 事件派发表单一来源（B3）

**结论**：lower 里每个“名字 → 槽位 / handle”的查找走唯一入口 `resolveName`，查不到返回新诊断码 `LOWER_UNRESOLVED`、不交出 Program；phase 事件名与“Runtime 是否派发”只在 `phaseEventTable` 写一次。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-13)

**背景**：前面的 pass 每漏查一种名字，lower 就静默产出指向槽位 / handle 0 的 Program（NC-210、NC-214 都是这个形状）；少数查找直接 panic；`$memory.` / `$local.` / `$input.` 引用查不到时退成同名 builtin、运行期才 `ErrProgramInvariant`。回归逐表删条目，修前 35 处静默兜底 / 23 处未解析引用照样出 Program / 4 处 panic。

**维护者决定**（DECISIONS-PENDING 第二轮 B3 行原文）：“lower 查找失败一律报错：做”；实施状态栏：“① …… ② phase 事件派发表单一来源（代价小，一并做）。③ 下个大版本，④ 保持 B”。③ 第十三轮改为本版完成（“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”），见 SKILL-30。待决定表里的选项：① lower fail-fast ② 事件派发表单一来源 ③ Host 取值约束做成随环境下发的能力表（改环境格式与 authority digest）④ NC-151 / NC-213 走方向 A 还是保持 B。

**现在的行为**：类型检查之外的第二道防线；正常定义的输出与 gameplay / presentation digest 不变。产物不完整、IR 形状未知等编译器自身不变量仍 panic（不是名字查找）。

**兼容**：对正常定义无变化。**v1.20.2 引入了一处回归**：以局部变量为实体的非缓存型 `read_attribute` 编译失败（`LOWER_UNRESOLVED at $`），v1.21.0 修复（NC-223，见 SKILL-14）。

**链接**：[B3 方案与实施](../../feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md)

<a id="skill-14"></a>
### SKILL-14 编译器按 Runtime 求值上下文收紧，修复 B3 回归（RR-20261005-NC-220～224）

**结论**：缓存型快照点、被动、衍生物定义（原名 `process`）/ `on` 的位置、衍生物字段里的施法引用在编译期按 Runtime 的实际求值上下文检查；B3 引入的局部变量 `read_attribute` 回归修复。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#skill-14)

**背景**（[N09 第五批](../../review/REVIEW-2026-10-06-n09-batch5.md)）：

| 编号 | 问题 | 现在 |
| --- | --- | --- |
| NC-220 | cast_start / phase_start / spawn_start（原名 process_start）的实体读 `$local.*`（采样点上还没有局部变量）；spawn_start 写在衍生物回调以外 | `ATTRIBUTE_SNAPSHOT_INVALID` |
| NC-221 | 被动 `proc_policy.max_depth: 0` 永不触发；被动的 `input_schema` 为 position / direction 永远填不上 | `SHAPE_INVALID` / `INPUT_UNAVAILABLE at $.input_schema` |
| NC-222 | 非召唤效果上的衍生物定义（原名 `process`，今 `spawn`）/ `on` 被静默丢弃 | `SHAPE_INVALID` |
| NC-223 | B3 回归：以局部变量为实体的 current / each_tick / on_hit / on_event 读取编译失败 | 恢复可编译，digest 与 v1.20.1 相同 |
| NC-224 | 衍生物每一步重新求值的字段读 `$input` / `$memory` / `$local`，移交后 `ErrProgramInvariant` | `INPUT_UNAVAILABLE`（方向 A：编译期拒绝） |

**维护者决定**（DECISIONS-PENDING 第五轮）：“NC-224 方向 B：不做：维持编译期拒绝（进程启动时不冻结施法输入）”。方向 B 是“衍生物启动时把施法输入冻结进 `SpawnInstance`（原名 `ProcessInstance`）”，可以让 path 投射物（`points: $input.path`）真正可用，但属于语义扩展。

**兼容**：行为收紧；NC-223 是恢复。若有部署在 v1.20.1 上保存了含 NC-223 那类技能的 checkpoint，升级后这些 cast 的 Program 因 digest 不同查不到，需要先排空。

**链接**：[第五批记录](../../review/REVIEW-2026-10-06-n09-batch5.md) · [NC-220](../../bugfix/RR-20261005-NC-220.md) · [NC-221](../../bugfix/RR-20261005-NC-221.md) · [NC-222](../../bugfix/RR-20261005-NC-222.md) · [NC-223](../../bugfix/RR-20261005-NC-223.md) · [NC-224](../../bugfix/RR-20261005-NC-224.md)

<a id="skill-15"></a>
### SKILL-15 求值上下文表：编译期与 Runtime 共用“每个上下文能读哪些引用”（第五轮，含 NC-280～283）

**结论**：`skill/eval_contexts.go` 列出 8 个求值上下文 × 24 类引用（另有 3 个快照点的位置表）；类型检查的作用域由表生成，Runtime 求值查同一张表，表外引用返回新的 `skill.ErrReferenceOutOfContext`（点名上下文与表项）。做表时发现并修复 NC-280～283。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#skill-15)

**背景**：编译期只有一套作用域（施法作用域 + 回调作用域），Runtime 至少有五种求值上下文，二者之间没有对照表；第五批之前同一形态已出五次（NC-211、NC-220、NC-221、NC-223、NC-224）。

**维护者决定**（DECISIONS-PENDING 第五轮“skill 求值上下文表”行原文，“进程”即今天的衍生物）：“做：一张表写明每种求值上下文（施法流程 / cast_start / phase_start / 进程启动 / 移交后每步）可用的引用，编译期与 Runtime 都查这一张（N09 第五批方向判断）”。

**现在的行为**：

- 8 个上下文：施法流程（cast_flow）、memory 默认值、cast_start 采样、phase_start 采样、spawn_start 采样、衍生物每一步（spawn_step）、衍生物回调（spawn_callback）、状态默认值（state_default）。三个衍生物上下文原名 process_start / process_step / process_callback（SKILL-25 改名）。比决定列的五种多三个，审表时发现它们是独立求值点。完整表见[方案 §3](../../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)。
- 新拒绝：memory 默认值读另一个 memory（NC-280，此前编译结果随 map 顺序）；状态默认值读 `$input` / `$memory`（NC-281）；cast_start / phase_start 读取可缺省的实体（NC-282）。
- 新放行：`$primary_target.position`、`$lifecycle_entity.position`、`$event.*.position`、`$input.target.position` 等投射（NC-283，此前每次求值失败或 B3 起编译失败）。
- 诊断码变化：衍生物回调里的施法引用由 `REFERENCE_UNKNOWN` 改为 `INPUT_UNAVAILABLE`（点名表项）；回调里的 cast_start / phase_start 读取改报 `ATTRIBUTE_SNAPSHOT_INVALID`（拒绝集合不变）。

**兼容**：既有定义 digest 不变（`row` 不进 digest）；不改线格式、checkpoint、生成形状（当时）。按诊断码字符串匹配的工具要更新。v1.23.0 的改名（SKILL-25、27）让上下文名、诊断文案与全部 digest 改变，见那两条。

**链接**：[方案](../../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md) · [第六批记录](../../review/REVIEW-2026-10-06-n09-batch6.md) · [NC-280](../../bugfix/RR-20261005-NC-280.md) · [NC-281](../../bugfix/RR-20261005-NC-281.md) · [NC-282](../../bugfix/RR-20261005-NC-282.md) · [NC-283](../../bugfix/RR-20261005-NC-283.md)

<a id="skill-16"></a>
### SKILL-16 漂移格子编译期拒绝（O33）；O34～O36 写进作者文档

**结论**：衍生物字段（原名进程字段）与状态默认值里会随移交 / 读写位置变值的施法引用（`$primary_target`、`$ability.self`、`$cast.*` 除 `$cast.mode`、cast_start / phase_start 读取），以及 memory 默认值里的 phase_start 读取，共 21 格从“可用但漂移”改为编译期拒绝，诊断给出替代写法。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#skill-16)

**背景**：第五批 O33：这些写法能编译、运行不报错，但主目标移交后变成 lifecycle 实体、施法状态变零值、快照退化为 current。求值上下文表起初把它们标成第三种格子“漂移”，保持现状、写明语义。

**维护者决定**（DECISIONS-PENDING 第七轮 O33 行原文）：“漂移格子改为编译期拒绝：进程字段 / 状态默认值里使用会漂移的施法引用（`$primary_target`、`$cast.*`、`$ability.self`、进程字段里的 cast_start / phase_start 读取等）直接报错，提示改用进程自己的引用”（原话里的“进程”即今天的衍生物）；O34～O36：“按推荐：保持现状并写进作者文档”。memory 默认值的 phase_start 由实施者判断“不合理，拒绝”（名字承诺“phase 开始时的值”，实际是 Activate 时的值，与 cast_start 相同）。

**现在的行为**：诊断示例（节选，`f6043e44` 时的文案）：`reference "$primary_target" is not available in evaluation context process_step: …；改用施法流程里求一次的 spawn position（如 $input.target.position）…（evaluation context table row $primary_target)`。v1.23.0 起上下文名是 `spawn_step`，替代写法说成“施法流程里求一次的召唤 position”（SKILL-25、27）。快照诊断多了 `row <point>`。O34（costs / windup 里的 phase_start 取求值那一刻的值）、O35（衍生物回调里 `self_ability` 过滤比较 handle 0）、O36（衍生物回调里 `$caster` 编译期拒绝，改写 `$owner`）保持行为、写进作者文档。

**兼容与迁移**：**新拒绝**，以前能编译的这类定义启动期失败，按[作者文档改写对照](../../skill/skill-casting-and-combat.md)修改；接受集合其余部分与 gameplay digest 不变。仓内 fixture、示例、`roost add skill` 骨架、game-demo 的 `fireball.json.tmpl` 都不用漂移格子。

**文档与源码差异（已更正）**：作者文档该节标题原写“（O33，未发版）”，实际随 v1.21.0 发布（`f6043e44` 是 `v1.21.0` 的祖先），已改为“（O33，v1.21.0）”。DECISIONS-PENDING 的标注提交 `2a7d2a65` 的提交说明写“实施状态（a6e75488）”，`a6e75488` 是 rebase 前的提交号、不在 main 历史上，正确的是 `f6043e44`（表内一直写对）；提交历史不改，已在 DECISIONS-PENDING 第七轮表下加更正注。

**链接**：[方案 §8](../../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md) · [作者文档](../../skill/skill-casting-and-combat.md)

<a id="skill-17"></a>
### SKILL-17 minion 衍生物（原名 summon 进程）不再接受 `duration_ticks` 与 area 成员字段（O22）

**结论**：kind 为 `minion` 的衍生物（实施时叫 summon 进程，SKILL-27 把 kind 改名为 `minion`）上写非 0 的 `duration_ticks`、或 `area` / `interval_ticks` / `emit_leave_on_stop`，编译期报 `MOTION_INVALID`。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-17)

**背景**：minion 衍生物的寿命一直是召唤效果（原名 spawn 效果）的 `duration_ticks`，衍生物自己的 `duration_ticks` 从不被读（负数也能编译）；写了 `area` 的 minion 编译通过、启动即 `ErrProgramInvariant`（同一处提前返回造成）。

**维护者决定**（DECISIONS-PENDING 第十二轮“skill 剩余观察”行原文）：“O22 编译期拒绝；O7 排序；O29 改文案；O15/O16/O17/O27/O28 保持并写作者文档；其余保持”。

**兼容与迁移**：已有定义在这类衍生物上写了这些字段的，升级后编译失败；删掉即可，行为不变（area 那种本来运行即失败）。

**链接**：[第十二轮记录 §1.1](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)

<a id="skill-18"></a>
### SKILL-18 同一状态的 checkpoint 字节确定（O7）

**结论**：`ActivePolicies`、`ProcLedger`、`RootEventCounts`、`AbilityByProgram` 改为按键排序写出，同一状态两次 Checkpoint 字节相同。首发 v1.23.0（本版）。O7 实施（`229a5aa0`）时格式与版本（2）都不变、与之前写出的 checkpoint 双向可读；之后同版的 SKILL-24～29 把 checkpoint 版本升到 7，**v1.23.0 发布物只恢复版本 7**，旧版本写出的一律 `ErrCheckpointUnsupported`。[实现](impl-cfg-skill-noncore.md#skill-18)

**背景**：这四个列表按 map 迭代顺序写出，两次 Checkpoint 同一状态的字节与 Checksum 不同。

**决定**：同 SKILL-17（第十二轮 O7 排序）。

**兼容**：恢复本来与列表顺序无关；依赖“同一状态两次 checkpoint 字节相同”的比对从本版起成立，但**不能拿新旧版本各自写出的字节互相比对**（版本号也不同）。

**链接**：[第十二轮记录 §1.2](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)

<a id="skill-19"></a>
### SKILL-19 result 分支诊断文案（O29）；O15 / O16 / O17 / O27 / O28 写进作者文档

**结论**：effect result 分支与 status 实例消费流程的诊断改为 “cannot suspend (wait, repeat with interval_ticks) or start a spawn with on callbacks”（实施时文案里是 process，SKILL-25 改名；规则不变，召唤效果带不带回调的衍生物一直可以）；五条既定语义写进作者文档。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-19)

**写进作者文档的五条**（行为不变）：area 的 `$event.enter_count` 恒为 1（O15）；`max_reflects: N` 是碰撞预算、实际反弹 N−1 次，`max_pierces` 同理（O16）；Host 拿到的 numeric 快照里未绑定字段是启动时的值、运动每步重新求值（O17）；restore 的 `on_blocked` 与 profile 策略不同时必然 `policy_rejected`（O27）；spawn_start（原名 process_start）读取的实体按启动事件求值（O28）。

**兼容**：只改诊断文案；按旧文案字符串匹配的工具要更新。

**链接**：[作者文档“衍生物、运动与 temporal 的既定语义”](../../skill/skill-casting-and-combat.md) · [第十二轮记录 §1.3～1.4](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)

<a id="skill-20"></a>
### SKILL-20 默认值为 null 的实体持久状态可以 `modify_state set`（RR-20261006-02）

**结论**：状态默认值缺省时按 state 声明的类型交给 Host（shared 状态同样）；以前第一次 `set` 必报 `ErrRuntimeTypeMismatch`。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-20)

**背景**：O33 实施时发现：Runtime 把 null 默认值按 null 类型交给 Host，MemoryHost 的 `set` 比较前后类型。null 默认值的语义是“还没写过”，状态类型仍是声明类型，所以错在 Runtime 交出的默认值丢了类型。

**决定**：收尾第 3 批 A4（维护者第十一轮“收尾”授权）。未采用“lower 时把 null 默认值改成声明类型”（改 gameplay digest，digest 是私有状态 `StateHandle` 的一部分，已持久的状态会失联）；未采用“只在 MemoryHost 放行”（其他 Host 还会再撞）。

**兼容**：Program、摘要、checkpoint 格式不变。放宽：以前每次失败的 `set` 现在成功；`state_changed` / `state_cleared` 事件与 `StateMutationResult.Before` 在状态不存在时的缺省值类型由 null 变为声明类型（都是 not present）。

**链接**：[bugfix 记录](../../bugfix/RR-20261006-02.md) · [问题](../../bug/RR-20261006-02.md)

<a id="skill-21"></a>
### SKILL-21 checkpoint 恢复拒绝 `phase_timeout` 任务（RR-20261006-03，O20）

**结论**：Runtime 从不调度 `phase_timeout` 任务（phase 计时在编译期拒绝，B3 ④），恢复时出现即 `ErrCheckpointCorrupt`；内部的 `phaseTimeoutTask` 删除。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-21)

**兼容**：本条（`b8fbcee0`）不改 `RuntimeCheckpointVersion`（当时为 2）；v1.23.0 发版时版本为 7（SKILL-24～29），旧版本写出的 checkpoint 先在版本号上被拒。`git log -S '&phaseTimeoutTask{'` 显示自 V1.9（`67c9df54`）起除恢复外没有任何代码构造这个任务，已发布版本的 checkpoint 不含它。

**链接**：[bugfix 记录](../../bugfix/RR-20261006-03.md)

<a id="skill-22"></a>
### SKILL-22 示例 `skill/examples/statusbridge` 能运行了

**结论**：A1（回滚统一走 DAO）之后，战斗组件的改动必须在事务里，这个示例一运行即 panic（`persistence mutation outside transaction`），而 build / vet / 测试全绿。第十二轮把整段演示包进 `nest.RunDetachedTransaction`，并改用 `ProjectAttributes`（删掉手写的 `syncArmor`）。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-22)

**背景与后续**：示例目录是独立模块，根模块的 `go build ./...` 不包含它。发版前验证为此加了根包门禁 `TestExamplesRun`（TOOL 部分），roost-coding 也加了“示例要实跑，不能只编译”。

**兼容**：只改示例。

**链接**：[第十二轮记录 §1.5](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md) · [发版前验证记录](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md)

<a id="skill-23"></a>
### SKILL-23 衍生物记录随 cast 一起回收；失败启动不留记录；reset 条目带增量的 PrimaryTarget（RR-20261006-21 / 22 / 23）

> 决定链第 0 步（见本主题开头的表）。方向判断：这是 SKILL-1 / SKILL-3 review 检查点的补测，查出的是“记录生命周期从来没定义”，后面六步都从这里展开。

**结论**：三处修复，首发 v1.23.0（本版，`5c1f4176`）。①失败启动删 cast、还 ID 之前先删掉它名下已停的衍生物记录；有衍生物停不下来时保留 failed cast、不还 ID（RR-21，P2）。②只有仍在运行的衍生物钉住 cast，已停的记录随 cast 一起回收（RR-23，P2）。③presentation reset 的衍生物条目带上 Runtime 增量里的 `PrimaryTarget`，交给可见性策略的事件与增量同形（RR-22，P3）。[实现](impl-cfg-skill-noncore.md#skill-23)

**背景**（实施时还叫“进程”）：

| 编号 | 以前 | 后果 |
| --- | --- | --- |
| RR-20261006-21 | 失败启动（提交前失败，例如 charge 的 enter 先召唤、再付费失败）只撤排程任务（NC-110），衍生物停了但记录留在 `runtime.spawns`，`CastID` 是被还回去的 ID | 之后每一次 `Checkpoint` 都报 corrupt；下一个拿到同一 ID 的 cast 接走不属于它的记录 |
| RR-20261006-23 | 回收 cast 时把名下**任何**衍生物记录当作引用，而记录在衍生物停止后从不删除 | 起过衍生物的 cast 永不回收，一局里累计超过 `CompletedCastLimit`（默认 2048）后 checkpoint 恢复判 corrupt |
| RR-20261006-22 | `ActivePresentation` 不带 `PrimaryTarget`，reset 用 `Anchor.Target`（lifecycle 实体）代替 | 按 `PrimaryTarget` 决定去留的自定义策略，reset 里放出增量里挡住的衍生物表现（NC-114 的承诺是两条路径同一规则） |

**决定**：bugfix（交接规格“sktest 已合（`5c1f4176`）：RR-20261006-21/22/23”）。未采用：失败启动永不复用 ID（改变 NC-110“未提交失败等于没有施法”）、停衍生物时立即删记录（同一提交里客户端只看到 remove、看不到停止状态）、`PrimaryTarget` 进 wire（旧 Applier 严格解码会拒绝未知字段）。

**现在的行为**：

- 正常失败启动之后不再残留衍生物记录，Start 照旧返回 `(0, 原错误)`、ID 照旧复用；宿主停不下衍生物时返回 `(cast.id, 原错误)` 并保留 failed cast（之前返回 0、记录被下一个 cast 接走）。停不下的衍生物之后怎么办，见 SKILL-24。
- 起过衍生物的 cast 在衍生物结束后按 `CompletedCastLimit` 回收，已停记录同批删除（state mutation 的 `spawn_remove` 与 `cast_remove` 同一笔）。
- `ActivePresentation.PrimaryTarget`（`json:"-"`，不进 wire）按增量来源填：施法条目与仍归施法的衍生物是施法目标，已移交的是 lifecycle 实体。默认 `EntityVisibilityPolicy` 结论不变。

**兼容**：API、checkpoint 格式（当时版本 2）、wire 不变；`ActivePresentation` 多一个不序列化的导出字段。

**限制**：经 kit syncstream / NATS 的端到端随 E04。

**链接**：[RR-21 问题](../../bug/RR-20261006-21.md) · [修复](../../bugfix/RR-20261006-21.md) · [RR-22 修复](../../bugfix/RR-20261006-22.md) · [RR-23 修复](../../bugfix/RR-20261006-23.md)

<a id="skill-24"></a>
### SKILL-24 宿主停不下的衍生物由 Runtime 退避重试（RR-20261006-21 后续，RR-20261006-30 / 31）

> 决定链第 1 步。方向判断：Runtime 对自己启动的东西负责到底（维护者选 B），不交给调用方重试；复用 RR-21 / 23 的记录生命周期，不另建状态表。

**结论**：施法失败时宿主拒绝停止的衍生物，不再“就此不管”，而是标成新状态 `stop_pending`，由 Runtime 按退避重试，有次数上限、告警与内存上限，并写进 checkpoint；实施中另修两处：被钉住的 cast 多于上限时 checkpoint 恢复判 corrupt（RR-30，P3），tick 驱动的停止被拒时 Runtime 的 tick 冻住（RR-31，P2）。首发 v1.23.0（本版，`1ce01e5c`）。[实现](impl-cfg-skill-noncore.md#skill-24)

**背景**：SKILL-23 修完后 RR-21 修复记录的“未验证 / 风险”写着：宿主停不下进程时 Runtime 不会自动重试，进程在宿主侧仍运行。交接规格把它列为待维护者：“宿主停进程失败时 Runtime 不重试（A 保持 + 文档 / B 重试）”。

**维护者决定**（DECISIONS-PENDING 第十三轮“skill 宿主撤除失败”行）：维护者原话“如果是技能本身的问题是不是技能自己处理比较好” → Runtime 记录待撤除并按退避重试，有上限与告警，写入 checkpoint。实施取舍：不建单独的“待撤除”表（两份状态各自同步、各自进 checkpoint）；不每 tick 重试（宿主故障期间被每 tick 打一次）；到上限保留记录让运维看到。

**现在的行为**（名字按 SKILL-25 改名之后；实施时是 `ProcessStopPending`、`skill.process.*`）：

| 项 | 行为 |
| --- | --- |
| 状态 | `SpawnStopPending`（`stop_pending`）：钉住 cast、ID 不复用、不步进、不派发信号、不跑回调；仍占 owned 容量；在 `StateSnapshot().Spawns` 与 presentation reset 里可见，不在 `OwnedSpawns` 里 |
| 重试 | 第一次在 `SpawnStopRetryBackoff`（默认 4）tick 之后，失败一次间隔翻倍，最多 64 倍；成功后进入 cancelled、发 `spawn_stop` 表现，cast 与记录按 SKILL-23 的规则回收 |
| 上限与告警 | 失败的重试到 `SpawnStopRetryLimit`（默认 10，约 1276 tick）后不再自动重试：计 `skill.spawn.stop_retry_exhausted.total`、一条 Warn 日志，记录保留；`RetentionStats` 有 `StopPendingSpawns` / `StopRetryExhaustedSpawns` |
| 内存上限 | 待停止条目最多 `MaxStopPendingSpawns`（默认 256）。实施时超限删记录（`stop_pending_dropped`），SKILL-29 改为挪进“已放弃”分区 |
| 宿主契约 | `Host.StopSpawn` 必须幂等（Runtime 会对同一个衍生物再次请求停止）；`MemoryHost` 本来幂等 |
| 客户端 | 进入待停止一条 `spawn_upsert`（`status: stop_pending`）与一条 `spawn_update` 表现；停掉后 `spawn_upsert`（cancelled）与 `spawn_stop`；cast 回收时 `spawn_remove` |
| RR-30 | 恢复按回收的同一不变量核对完成队列：超过上限时队列里不能有可回收的 cast（放宽：被钉住的 cast 多于上限现在能恢复） |
| RR-31 | 移交后的衍生物到期 / 失效时宿主拒绝停止：那一次 `Advance` 返回错误，之后转待停止、tick 照常前进（修前 `Advance` 一直卡在同一 tick，同一 tick 连打宿主四次） |

**兼容**：新状态值 `stop_pending` 出现在衍生物状态快照、state mutation 与表现事件里，按状态分支的客户端 / 渲染器要认它；`RuntimeOptions` 新增三项、`RuntimeRetentionStats` 新增两个字段（零值取默认）；checkpoint 版本 2 → 3（之后到 7）。

**限制**：无外部依赖，未在真实宿主上演练（参考宿主与测试替身覆盖）。

**链接**：[RR-21 修复“后续”一节](../../bugfix/RR-20261006-21.md#后续runtime-负责重试停止维护者-2026-10-06) · [RR-30 修复](../../bugfix/RR-20261006-30.md) · [RR-31 修复](../../bugfix/RR-20261006-31.md)

<a id="skill-25"></a>
### SKILL-25 “进程”（process）全量改名为衍生物（Spawn）

> 决定链第 2 步。方向判断：名字跟着领域走；线上未部署，不保留旧名、不做兼容别名。

**结论**：skill 包里的 process 一律改名为 spawn（衍生物）：DSL、求值上下文、同步与表现、checkpoint 字段、Host 接口、指标、公开与未导出的 Go 标识符、16 个文件名。行为不变（测试除改名外不改断言）；checkpoint 版本 3 → 4，gameplay / presentation digest 全部改变（digest 输入里有字段名）。首发 v1.23.0（本版，`4451a0a5`）。[实现](impl-cfg-skill-noncore.md#skill-25)

**维护者决定**（DECISIONS-PENDING 第十三轮“skill 进程改名”行）：原话“skill的一个流程叫做进程很奇怪，用更专业的词语”，选 **Spawn（衍生物）**（“Spawn吧”）：`process` 全量改名为 `spawn`，不保留旧名（线上未部署）。`proc`（被动触发、`ProcLedger`）是游戏术语“触发”，不是 process 的缩写，不改。

**主要对照**（完整对照见[重构记录 §3](../../feature/REFACTOR-2026-10-06-skill-process-to-spawn.md)）：

| 类别 | 旧 | 新 |
| --- | --- | --- |
| DSL | effect flow 的 `"process": {…}`、`modify_process`、`$process`、快照点 `process_start`、环境 `process_properties` / `process_kinds` | `"spawn": {…}`、`modify_spawn`、`$spawn`、`spawn_start`、`spawn_properties` / `spawn_kinds` |
| 求值上下文 | `process_start` / `process_step` / `process_callback` | `spawn_start` / `spawn_step` / `spawn_callback` |
| 同步与表现 | `process_upsert` / `process_remove`；`process_start` / `_update` / `_signal` / `_stop`；字段 `process_id` 等 | `spawn_upsert` / `spawn_remove`；`spawn_start` / … ；`spawn_id` 等 |
| Host 接口 | `StepProcess(ProcessStepCommand, ProcessHostState)`、`StopProcess(…)` | `StepSpawn(SpawnStepCommand, SpawnHostState)`、`StopSpawn(…)`（幂等契约不变） |
| 选项 / 统计 | `ProcessStopRetryBackoff`、`MaxStopPendingProcesses`、`MaxOwnedProcesses*`、`StopPendingProcesses` | `SpawnStopRetryBackoff`、`MaxStopPendingSpawns`、`MaxOwnedSpawns*`、`StopPendingSpawns` |
| 指标 | `skill.process.stop_retry_exhausted.total` | `skill.spawn.stop_retry_exhausted.total` |
| checkpoint | `processes`、`owned_processes`、`next_process_id` … | `spawns`、`owned_spawns`、`next_spawn_id` …（版本 4） |

**兼容与迁移（破坏性）**：用旧名写的技能 JSON Parse 失败（`phases: [0].on: [0]: json: unknown field "process"`，TROUBLESHOOTING T-288）；Host 实现、客户端、告警按对照表改名；skillcompose 契约按新 digest 重签；旧回放记录与旧 checkpoint 不再适用（排空后升级）。codegen、demo 模板、kit 不引用这些名字，生成物不变。

**链接**：[重构记录与完整对照](../../feature/REFACTOR-2026-10-06-skill-process-to-spawn.md)

<a id="skill-26"></a>
### SKILL-26 衍生物停止入口统一走一套停止 / 待停止状态机（RR-20261006-32）

> 决定链第 3 步。方向判断：根因在实现结构（失败处理散在入口里、靠错误传播到另一个入口“补一刀”），不是“Runtime 负责到底”这个前提错了；把失败处理收进一个函数，用登记表 + 源码守卫固定，以后新增入口不补就红。

**结论**：衍生物的全部停止入口（施法失败、goto / Cancel / Interrupt / 施法收尾、衍生物启动失败清理、召唤事务提交失败、移交时 lifecycle 已失效、施法期间 lifecycle 消失、移交后到期 / 失效、`RemoveProgram`、`Shutdown`）只“请求停止”，经唯一的 `requestSpawnStop` 进入同一个状态机：宿主停了就结束，拒绝就转 `stop_pending` 由 Runtime 退避重试。实施中修 RR-20261006-32（P3：被拒后同一请求再停一次，cancel 回调跑两遍）。首发 v1.23.0（本版，`3fad5b6e`）。[实现](impl-cfg-skill-noncore.md#skill-26)

**背景**：10-06 一天之内这一块出了 RR-21 / 22 / 23 / 30 / 31，各自在入口上加分支：`failCastLocked` 停完补扫、tick 回收两处补分支、`Shutdown` / `RemoveProgram` 把记录留成 running 交调用方重试（之后 Runtime 不管）、施法里的入口靠错误传到 `failCastLocked` 再停一次。

**维护者决定**（DECISIONS-PENDING 第十三轮“停止入口统一”行）：维护者原话“停止入口统一”：施放失败 / tick 到期 / Shutdown / RemoveProgram 四个入口走同一套停止 / 待停止状态机。

**现在的行为**：

- 处理宿主拒绝的位置从 5 处变成 1 处；直接调用 `terminateSpawn` 的位置从 14 处变成 1 处，调用链固定为 `requestSpawnStop` → `terminateSpawn` → `stopSpawn` → 宿主 `StopSpawn`（源码守卫核对）。
- **`Shutdown` / `RemoveProgram` 的语义**：停不下的衍生物留成 `stop_pending`、写进 checkpoint，之后由 Runtime 在 tick 上接着重试；不在 `Shutdown` 里同步重试（Runtime 按 tick 确定性推进、没有 ctx，墙钟等待会破坏回放并阻塞快池 / 帧线程上的调用方）。调用方仍收到第一个错误；再调一次会立即再请求一次；停不下的衍生物不再出现在 `OwnedSpawns`（之前 running、留在里面）。
- **RR-32**：Interrupt / Cancel / goto / 收尾 / 移交被拒时 cancel 回调只跑一次（之前两次）；启动失败与召唤事务提交失败的清理被拒时不跑 cancel 回调（之前跑了成功路径从不跑的 cancel）；同一请求里宿主只被打一次，之后在第 `SpawnStopRetryBackoff` 个 tick 重试。

**兼容**：API、checkpoint 格式（版本 4）、wire、指标不变；行为变化见上（依赖“失败后从 `OwnedSpawns` 里找到它再调一次”的调用方改看 `StateSnapshot().Spawns` / `RetentionStats()`，或直接再调一次）。文件 `runtime_spawn_stop_retry.go` 改名为 `runtime_spawn_stop.go`。

**链接**：[方案、状态迁移表与验证](../../feature/REFACTOR-2026-10-06-skill-spawn-stop-unified.md) · [RR-32 问题](../../bug/RR-20261006-32.md) · [修复](../../bugfix/RR-20261006-32.md)

<a id="skill-27"></a>
### SKILL-27 生成宿主单位的效果改名为召唤物（Summon）：`despawn` → `dismiss`，衍生物 kind `summon` → `minion`

> 决定链第 4 步。方向判断：SKILL-25 之后 skill 包里有两套 spawn（宿主真实单位 / 技能驱动的衍生物），按“来源 + 语义”逐个判定，把第 1 类改名为 Summon，消除两类混读。

**结论**：效果 `{"type":"spawn","template":…}`（用单位模板在场景里生成归施法者所有的真实单位）改名为 `{"type":"summon",…}`，移除召唤物的 `despawn` 改为 `dismiss`，宿主事务与相关类型、owned 选择、effect result 类型、MemoryHost 事件一并改名；衍生物里只跟着召唤物活的 kind `summon` 改为 `minion`。行为不变；checkpoint 版本 4 → 5（cast 值里召唤结果的类型 `spawn_result` → `summon_result`）；编译环境 digest 与全部 gameplay / presentation digest 改变。首发 v1.23.0（本版，`509c381f`）。[实现](impl-cfg-skill-noncore.md#skill-27)

**维护者决定**（DECISIONS-PENDING 第十三轮“skill 生成宿主实体改名”行）：维护者“第 1 类改 Summon”；技能驱动对象（第 2 类）保持 Spawn（衍生物）；衍生物 `kind: "summon"` 改为 `kind: "minion"`（维护者“按照推荐处理”）。“版本号与命名”行：维护者“用推荐的”——移除召唤物用 `dismiss`（游戏里解散召唤物 / 宠物的通用词，未用生造的 `unsummon`）。

**主要对照**（完整对照见[重构记录 §3](../../feature/REFACTOR-2026-10-07-skill-summon-rename.md)）：

| 类别 | 旧 | 新 |
| --- | --- | --- |
| DSL | 效果 `spawn`、`despawn`；`issue_entity_command` 的 `despawn`；单位模板生命周期策略值 `despawn`；owned 过滤 `spawned_before` / `spawned_after`、排序 `spawn_tick` / `spawn_sequence`；result 类型 `spawn_result` | `summon`、`dismiss`；`dismiss`；`dismiss`；`summoned_before` / `summoned_after`、`summon_tick` / `summon_sequence`；`summon_result` |
| Host 契约 | `PreviewOwnedSpawn(SpawnCommand)`、`CommitOwnedSpawn` / `RollbackOwnedSpawn(OwnedSpawnTransactionID)`、`SpawnEffectResult` | `PreviewOwnedSummon(SummonCommand)`、`CommitOwnedSummon` / `RollbackOwnedSummon(OwnedSummonTransactionID)`、`SummonEffectResult` |
| 环境 | `UnitTemplateCatalogEntry.MaximumSpawnCount` | `MaximumSummonCount` |
| MemoryHost 事件 | `owned_entity_spawned` / `_spawn_rolled_back` / `_despawned` | `owned_entity_summoned` / `_summon_rolled_back` / `_dismissed` |
| 衍生物 kind | `"spawn":{"kind":"summon"}` | `"spawn":{"kind":"minion"}` |

**不改的**（经核对属于衍生物）：`SpawnCommandMeta`、`OwnedSpawns` / `OwnedSpawnSnapshot`、`MaxOwnedSpawns*`、`HasSpawn`、`SpawnTemplate*`。文件 `runtime_owned_spawn.go` 改名为 `runtime_owned_entity.go`。

**兼容与迁移（破坏性）**：旧定义 Parse 失败（`unsupported effect "spawn"`，T-289）；召唤效果上挂 `kind: "summon"` 的衍生物编译报 `MOTION_INVALID`（不是闭合的 motion kind）；Host 实现按对照表改名；环境与 Program digest 改变，契约重签；旧 checkpoint 排空后升级。

**实施中发现**：source document digest 丢掉接口字段的具体类型与 `json:"-"` 字段——登记为 RR-20261006-33，随 SKILL-28 修复。

**链接**：[重构记录与完整对照](../../feature/REFACTOR-2026-10-07-skill-summon-rename.md)

<a id="skill-28"></a>
### SKILL-28 衍生物记录按字段分区存放、删掉重复的 owned 表；源文档 digest 逐字段；停止循环判空（RR-20261006-33 / 34）

> 决定链第 5 步。方向判断：状态只由记录字段（`Status` + `handedOff`）表达，分区由字段决定、只有一个函数改字段并换分区；SKILL-26 的方向判断里留的风险点（`spawns` 与 `ownedSpawns` 两张表要同步）在这里消除。

**结论**：Runtime 不再有 `spawns` 与 `ownedSpawns` 两张同步维护的表，改为一个 `spawnTable` 的四个分区（施放中 / 已移交 / 待停止 / 已停止），每条记录恰好在一个分区，由 `setState` 唯一负责改字段并挪分区；每个 tick 入口只扫自己那一个分区；checkpoint 只存一份记录（版本 5 → 6）。同一提交修 RR-20261006-33（P3：源文档 digest 改为逐字段的规范表示）与 RR-20261006-34（P2：`Shutdown` / `RemoveProgram` 遇到同一轮被待停止上限删掉的记录空指针 panic，SKILL-26 引入、未发版）。首发 v1.23.0（本版，`6826eeb2`）。[实现](impl-cfg-skill-noncore.md#skill-28)

**维护者决定**（DECISIONS-PENDING 第十三轮“skill 衍生物两张表”行）：选 A——“移交给谁”只由记录字段（`Owner` + `handedOff`）表达，不再有重复索引；原话“如果为了性能可以给spawns分类map，这样也不用扫全部” → 按类别**分区**存放，checkpoint 只存一份。

**现在的行为**：

- 行为不变（原有测试除名字外不改断言）；`OwnedSpawns(owner)` 扫已移交分区按 `Owner` 过滤；所有有副作用的遍历仍按 ID 排序。
- checkpoint 版本 6：删掉 `owned_spawns` 与恢复时的逐条比对；恢复额外拒绝未知 `status` 与非 entity 衍生物上的 `handed_off`。
- **RR-33**：`SourceDocumentDigest` 以前是 `json.Marshal(Definition)` 的摘要，接口值不写具体类型、`json:"-"` 字段被跳过——只差效果类型（`set_memory` / `add_memory`）、消耗数量、cast window 表达式、策略 / 输入 / 形状 / 过滤器类型的两个定义摘要相同。改为按反射逐字段写出（接口写具体类型名、`json:"-"` 字段也写）。**全部定义的 `SourceDocumentDigest` 改变**；gameplay / presentation digest 不经过它、不变；框架内没有用它做身份判断。
- **RR-34**：两处停止循环取回记录后判空、跳过（SKILL-29 之后循环取回的是已放弃的记录，判空保留作双重保险）。

**兼容**：checkpoint 版本 6（之后到 7）；保存了旧 `SourceDocumentDigest` 做比对的调用方第一次比对会认为全部源文档变了（一次性）。

**链接**：[分区方案 §1～§10](../../feature/REFACTOR-2026-10-07-skill-spawn-partition.md) · [RR-33 问题](../../bug/RR-20261006-33.md) · [修复](../../bugfix/RR-20261006-33.md) · [RR-34 问题](../../bug/RR-20261006-34.md) · [修复](../../bugfix/RR-20261006-34.md)

<a id="skill-29"></a>
### SKILL-29 待停止到上限不删记录，挪进第五个分区“已放弃”（维护者选 B）

> 决定链第 6 步。方向判断：分区方案 §10 判断根因在“上限删除”这一条——它是这一块唯一“记录在别人手里时消失”的路径（RR-34 就是它）；候选 A 维持判空 / B 放弃分区 / C 去掉上限，维护者选 B，从结构上消除而不是再给新循环判空。

**结论**：待停止条目会超过 `MaxStopPendingSpawns` 时，最早的一条（先选已到重试上限的）不再被删，而是经 `setState` 挪进第五个分区“已放弃”（新状态 `abandoned`）：不再重试、不再推进、不钉住 cast、`Shutdown` / `RemoveProgram` 对它是空操作；已放弃分区有自己的上限 `MaxAbandonedSpawns`（默认 1024），只在 `Advance` 返回前清理。删记录只剩三个登记点。checkpoint 版本 6 → 7。首发 v1.23.0（本版，`f28285ad`）。[实现](impl-cfg-skill-noncore.md#skill-29)

**维护者决定**（DECISIONS-PENDING 第十三轮“待停止上限”行）：“维护者选 B：到 `MaxStopPendingSpawns` 不删记录，改挪进第五个分区‘已放弃’（不再重试、告警），该分区自有上限、只在 tick 末尾统一清理，杜绝循环中途删记录”。

**现在的行为**：

| 时刻 | 客户端看到 | 指标 / 日志 |
| --- | --- | --- |
| 放弃 | `spawn_update` / `spawn_upsert`（`status: abandoned`），不发 `spawn_stop` / `spawn_remove`（宿主那边可能仍在运行） | `skill.spawn.abandoned.total`（替换 `skill.spawn.stop_pending_dropped.total`）；Error 日志 `stop-pending spawn abandoned`，点名衍生物、cast、owner、lifecycle 实体与放弃前的重试次数 |
| 之后（cast 回收、Shutdown 等） | 无；cast 照 RR-23 回收，已放弃的记录不随 cast 删 | 无 |
| `Advance` 末尾超出 `MaxAbandonedSpawns` | `spawn_remove`（Runtime 不再记得它，不代表宿主已停） | `skill.spawn.abandoned_pruned.total`、Warn 日志 |

`RetentionStats` 加 `AbandonedSpawns`。两次 `Advance` 之间（`Shutdown`、`Start` 失败等入口只放弃、不删）已放弃分区可以暂时超限，下一次 `Advance` 末尾清理。

**兼容与迁移**：告警规则把 `stop_pending_dropped` 换成 `abandoned`（另有 `abandoned_pruned`）；客户端按状态分支时认 `abandoned`；checkpoint 版本 7（加 `max_abandoned_spawns`，已放弃的记录必须带 `direct_program`）。公开 API：新增 `SpawnAbandoned`、`RuntimeOptions.MaxAbandonedSpawns`、`RuntimeRetentionStats.AbandonedSpawns`、`MetricSpawnAbandoned`、`MetricSpawnAbandonedPruned`；删除 `MetricSpawnStopPendingDropped`（10-06 改名时引入，未发版）。宿主侧对已放弃单位的清理靠宿主自己的比赛结束 / 程序移除（`RemoveOwnedEntitiesForMatchEnd` / `ByProgram` 照常调用）。

**链接**：[分区方案 §11](../../feature/REFACTOR-2026-10-07-skill-spawn-partition.md#11-待停止上限改为已放弃分区维护者第十三轮待停止上限选-b2026-10-07)

<a id="skill-30"></a>
### SKILL-30 Host 取值能力表随编译环境下发，编译器 / Runtime / Host 共用（B3 ③，RR-20261006-37 / 39）

> 方向判断：本主题开头的“编译器认可的写法与 Runtime / Host 真正会执行的写法各自维护”在 Host 一侧的最后一块——三处各自维护“能读什么、支持什么”的集合（NC-151 / 213 / 214 / 215 是直接先例）；做成一张随环境下发的表，三处只查它。

**结论**：Host 能读 / 支持的取值做成能力表（八列：属性、资源、衍生物 kind、motion 步骤、只交给 Host 的衍生物数值字段、资源 operation、修正 operation、召唤物），随 `CompileEnvironment.Host` 下发、算进 authority digest。编译器按表拒绝（`HOST_CAPABILITY_MISSING`，点名缺的项与源路径）；Runtime 在 Program 第一次 Start / RegisterAbility / ActivatePassive / RestoreRuntime 时按 Host 声明的表准入（`ErrHostCapabilityMissing`，不扣费）；MemoryHost / HostAdapter 声明自己的表并对表外属性 / 资源报错；`CheckHostCapabilities` 按声明逐项调用 Host 核对。收尾把能力表并进 `skill.Host` 接口，删掉“未实现则跳过准入”的分支。首发 v1.23.0（本版，`cd8ed341`、`e999f68e`）。[实现](impl-cfg-skill-noncore.md#skill-30)

**背景**：编译时认为能读的属性、资源、运动步骤，到运行期 Host 可能不支持，结果是施法到一半才报错（往往已经扣了费），或 Host 静默返回 0。修前探针（方案 §7.1）：summon 写在不实现 `OwnedEntityRuntimeHost` 的 Host 上、带 collision 的衍生物跑在不接受碰撞步骤的 Host 上，都是“编译通过、Activate 失败、mana 90（原 100）”；MemoryHost 与 HostAdapter 对 catalog 外的属性 / 资源读成 0。

**维护者决定**：第二轮 B3 行“③ 下个大版本”（Host 取值约束做成随环境下发的能力表，改环境格式与 authority digest）；第十三轮“原下个大版本项”行，维护者原话“还有留到下个版本的几项在本机能完成吗？希望本次能完成了” → 本版完成。收尾按第十三轮“交给 review 前不留能绕过检查的分支”：选“能力表并进 `Host` 接口”而不是“接口不变、入口处没实现就拒绝”（改动更小、Runtime 不需要任何“没实现怎么办”的分支、业务漏写在编译期就报）。

**实施中发现**：

- [RR-20261006-37](../../bug/RR-20261006-37.md)（P3）：motion catalog 的 `enabled_slots` 只在定义写了 steering / offsets 时才查，Runtime 却对每个运动衍生物每步都发这两种步骤——关掉槽位的环境照样编译出运动衍生物，扣费之后才失败。修法：槽位并入能力表的 `motion_step` 列，运动衍生物固定需要 frame / steering / offsets / completion。
- [RR-20261006-39](../../bug/RR-20261006-39.md)（P3）：Host 不声明能力表时 Runtime 跳过准入，包装型 Host（`RecordingHost` / `ReplayHost`）能绕过核对直接施法。修法：`HostCapabilityProvider` 并入 `skill.Host`，包装型 Host 转发被包装者的表并记录这次调用。

**现在的行为**：默认环境（`DefaultCompileEnvironment()`）声明全部能力（`FullHostCapabilityCatalog()`，revision `host-1`），MemoryHost 全部实现——用默认环境与 MemoryHost 的项目编译结果、gameplay / presentation digest、Runtime 行为都不变。业务 Host 至少写两处（[方案 §6](../../feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md)）：`HostCapabilities()` 声明能力（战斗部分交给 `HostAdapter`、自己负责的运动 / 衍生物 / 召唤物合并进来），装配时环境的 Host 段取自 Host 的表；测试里加一行 `skill.CheckHostCapabilities` 核对声明与行为一致。空表是合法声明（什么都不支持，带需求的 Program 一律在准入处被拒并点名）。

**兼容与迁移（破坏性）**：业务自己的 `skill.Host` 实现必须有 `HostCapabilities()`（否则编译不过；嵌入 `*skill.MemoryHost` 的自动得到）；环境格式变化（新增 `Host`、删 `Motion.EnabledSlots` / `HostFeatures`），所有 authority digest 改变，用旧 digest 的 Program / checkpoint / skillcompose 契约需重新编译、重签；配置了 catalog 的 MemoryHost 与 HostAdapter 对表外属性 / 资源从“返回 0”改为报错；环境收窄后原来能编译的定义报 `HOST_CAPABILITY_MISSING`；录制的 `HostRecord` 多一条 `host_capabilities`，旧记录回放报 `ErrReplayMismatch`，重录即可。

**限制**：能力表能核对“Host 接受这个步骤 / 字段”，但 Host 是否真的使用只交给它的数值字段（`spawn_numeric_field`）、非 minion 的衍生物 kind 从接口上观察不到——声明即承诺（方案 §11 写明的设计边界，不是待验证项）；谎报能力的 Host 仍由施法中途的类型断言（召唤物）或 Host 自己的错误（运动步骤）兜底，业务测试里用 `CheckHostCapabilities` 发现。

**链接**：[B3 ③ 方案与实施（含 §12 收尾）](../../feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md) · [RR-37 修复](../../bugfix/RR-20261006-37.md) · [RR-39 修复](../../bugfix/RR-20261006-39.md) · [B3 ①② 方案](../../feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md)

## NONCORE：非核心 review（N01～N15）的修复与收尾

非核心 review 把 core 里 Nest / Sync / DataEngine / Remote 本体以外的代码分成 15 个单元（N01～N15），按场景矩阵审，确认的缺陷按先红后绿修。完成总结见 [NONCORE-REVIEW-COMPLETION](../../review/NONCORE-REVIEW-COMPLETION-2026-10-06.md)，单元状态见 [单元状态](../../review/REMAINING-REVIEW-HANDOFF-2026-10-05.md)。本主题按单元排列；编号里的 NC-xx 是 `RR-2026100x-NC-xx` 的简写。

**不在本主题写、只引用的**（避免重复）：

| 内容 | 所在部分 |
| --- | --- |
| A4 严格配置读取、C1 生产校验（NC-190 / 192）、NC-75 | 本文件 CFG |
| N09 skill 的全部修复（NC-110～117、150～154、210～224、280～283、RR-20261006-02 / 03） | 本文件 SKILL |
| App 单实例锁、fail-stop、liveness（C5）、A3 停机契约骨架、`/readyz`（D1 与第十二轮期限）、退出日志 RR-20261006-07 | APP |
| 静态绑定、活动组文件 C4 | OWN |
| U-0280 / U-0281、B1、saga 方向 ①②、步骤预算与预算大小写（NC-194、RR-20261006-06）、O-S5-* | SAGA |
| A2 驱动不重放写、NC-100 / 101、Close 契约 RR-20261006-10 | DRV |
| A1 DAO 统一回滚、B4、buff 投影 | DAO |
| B2、Mirror 第 1～6 步、O4、O-M6-* | REM |
| D-L3 双时钟、业务时间只许前进 | CLK |
| C6 服务指标、metrics 按标签删除、Ops Bearer、CAS 口径、面板 | OPS |
| pretag / source-head-check / 冲突标记门禁 / 示例实跑门禁 / mirror-local.sh；文档链接门禁与 `skill/README.md` 52 个旧链接修复（`d05a04a1`，TOOL-5） | TOOL |
| glsvet A1 提示跟进同包 helper（RR-20261006-13）、组件字段写入提示（`565f657b`） | DAO |

**边界项**（[汇总文件](_summary-cfg-skill-noncore.md)“边界项”表）：本部分为主、完整写出的是 NONCORE-15（NC-130 / 131，近 REM）、NONCORE-32（DAO nocoll，近 DAO）、NONCORE-51（N15 脚本，近 TOOL）、NONCORE-52（NC-170～174，近 APP）、NONCORE-53（NC-260，近 DRV）、NONCORE-54（RR-20261006-12，核心线 nest）、NONCORE-55（U-0279，核心线修复）；与其他分册重复、只留编号与一句话结论的是 NONCORE-1（APP-4 / 7 / 8、OPS-3）、NONCORE-23（SAGA-7）、NONCORE-24（OWN-6）、NONCORE-40（CLK-6）、NONCORE-45（OPS-7）、NONCORE-50（APP-9）、NONCORE-56（OWN-1）。

**对业务 / 运维最重要的几条**：

- actionflow 回调语义变了（NONCORE-35、NONCORE-37）：`ActionRunner` / `MissionRunner` 回调里的变更延后到回调返回后执行，不再返回 `ErrReentrantMutation`。**破坏性**。
- 遍历回调成为仓库级契约（NONCORE-47）：框架交出的 `Range` 回调里可以读写同一容器、返回 false 立即停止。
- account 换名建角会释放未 admitted 的计划（NONCORE-22）；activity 坏窗口条目有了运维修复入口（NONCORE-21）。
- 生成工程需要手工处理的：`.gitignore` 补 `/data/wal/`（NONCORE-51，NC-206）；已生成工程不自动迁移，其余模板变化 `roost project sync` 后生效。
- roost CLI 中断（Ctrl-C）会先回滚再以同一信号退出（NONCORE-31）；codegen 联网用例需 `ROOST_NETWORK_TESTS=1`。

### N01 app / lifecycle / manager / health / admin

<a id="noncore-1"></a>
#### NONCORE-1 N01 留项：Ops 同步 bind、停机 hook 受预算、停机期 fail-stop 非零退出、Redis / remote_entity Mod 停止收尾、`ops.admin_timeout`（RR-20261005-NC-230～234）

> 与其他分册重复：以对方条目为准——NC-230 与 `ops.admin_timeout` 见 [OPS-3](guide-app-own-clk-ops-tool.md#ops-3)，NC-231 见 [APP-8](guide-app-own-clk-ops-tool.md#app-8)，NC-232 见 [APP-4](guide-app-own-clk-ops-tool.md#app-4)，NC-233 / 234 见 [APP-7](guide-app-own-clk-ops-tool.md#app-7)；本条只保留编号与一句话结论（汇总去重）。

**结论**：五个停机 / 启动边角与 admin 命令期限，首发 v1.21.0（`2c1c7be7`）。边界项（近 APP / OPS）。[实现](impl-cfg-skill-noncore.md#noncore-1)

### N02 httpclient / httpserver / security / gateway / webroute

<a id="noncore-2"></a>
#### NONCORE-2 HTTP JSON 先编码后写状态；recover 尊重已开始的响应（RR-20261005-NC-80 / NC-81）

**结论**：`httpserver.JSON` 编码失败时回 500 固定错误体，不再发出 2xx 空体；响应开始后的 panic 以 `http.ErrAbortHandler` 中止连接。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-2)

**背景**：`JSON` 先 `WriteHeader` 再编码，NaN / Inf、不可编码类型、`MarshalJSON` 错误时客户端收到 2xx 空体——生成 Webroute 与 Ops `/admin/execute` 的调用方把已执行但无结果的请求当成功（NC-80）；recover 中间件在响应已开始后仍追加 500 JSON，客户端收到“完整”的 2xx 拼接体（NC-81）。

**现在的行为**：状态码推迟到 Encoder 唯一一次 Write（`json.Encoder.Encode` 在池化缓冲里编码完再一次写出），成功输出逐字节不变、也不为每个响应另复制一份响应体；响应前 panic 仍回 500 JSON；包装 writer 保留 Flusher（含 `FlushError`）/ ReaderFrom / Unwrap，原 writer 支持时才暴露 Hijacker。**兼容**：无 API 变化。**链接**：[NC-80](../../bugfix/RR-20261005-NC-80.md) · [NC-81](../../bugfix/RR-20261005-NC-81.md)

<a id="noncore-3"></a>
#### NONCORE-3 RateLimiter 每主体 key 上限，满表不再全表扫描（RR-20261005-NC-82）

**结论**：新增 `security.RateLimitConfig.MaxKeysPerOwner`（默认 256，不超过 MaxKeys）；一个主体变化 Action（`gateway.RateLimit` 下即 MessageID）只耗尽自己的名额；满表时陌生 key O(1) 拒绝。首发 v1.20.1，**行为变化**。[实现](impl-cfg-skill-noncore.md#noncore-3)

**背景**：一个玩家换着 MessageID 发包就能占满全表，让其他玩家被限流；满表时每个陌生 key 全表扫描（100k 表约 529µs）。**现在**：闲置 key 由每 SweepInterval 至多一次的周期清扫回收（满表时最多晚一个间隔腾出名额）；`RateLimitStats` 新增 `MaxKeysPerOwner`、`OwnerCapacityRejected`。基准：满表陌生 key 修前 1k 表约 5.7µs、100k 表约 529µs，修后 56～102ns，与表大小无关。**限制**：gateway / RateLimiter 仓内无装配方（N02 O6）。**链接**：[NC-82](../../bugfix/RR-20261005-NC-82.md)

<a id="noncore-4"></a>
#### NONCORE-4 生成的 player TCP 接入停机可重试（RR-20261005-NC-83）

**结论**：`Server.Stop` / `Mod.StopWithContext` 超时后保留 server，用新 ctx 重试会等到连接 goroutine 真实返回；会话关闭订阅者的等待受 ctx 约束。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-4)

**背景**：同一“停机三步”不变量第 N 次被打破（之后形成 A3，见 APP）。**兼容**：已生成工程需重新生成 `internal/access/player/tcp/server_gen*.go`。NC-83 点名的同形候选随后逐个处理（NC-170～174，见 NONCORE-52；bus NC-90）。**链接**：[NC-83](../../bugfix/RR-20261005-NC-83.md)

<a id="noncore-5"></a>
#### NONCORE-5 生成 TCP 加“handler 不配合 ctx”的用例（收尾第 2 批 A15，只加测试）

**结论**：钉住两条既有承诺：`DispatchTimeout` 只界定等待、不界定工作；连接槽与按 IP 计数在 handler 返回、连接结束之后才归还。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-5)

**背景**：N02 review 的“Dispatch handler 不配合 ctx”单独用例。现有 `TestADispatchThatOutlivesItsBudgetAnswersAndFreesTheSlot` 的 handler 在 ctx 结束时返回，钉不住不配合的情形。**现在的行为（不变，写明）**：100ms 预算到点时 handler 拿到的 ctx 以 `DeadlineExceeded` 结束，但 handler 不返回就一直在跑、期间没有答复；放行后答复照常发出、内容是 handler 自己的结果；客户端先断开时，两项计数在 handler 返回前仍各为 1（读循环停在 handler 里看不到断开）。**对业务的含义**：handler 必须配合 ctx，否则名额被占住。**链接**：[收尾第 2 批记录 A15](../../bugfix/CLOSING-BATCH-2-2026-10-06.md)

### N03 bus / nats / servicerpc / etcd

<a id="noncore-6"></a>
#### NONCORE-6 Bus JetStream RPC：停止排空在途 handler、派发池拒绝立即回包、被请求流截获的轻量 RPC 不再执行（RR-20261005-NC-90 / 91 / 92）

**结论**：三处 bus 修复，首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-6)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-90 | 停止只排空 pool，`Stop` 立即返回 nil、NatsMod 随即关连接，回包用随停止取消的 ctx 发布——每次停机在途的可靠 RPC 都丢回包并在 AckWait 后重投再执行 | 先关 JetStream 请求准入、再等在途 handler（超预算返回 ctx 错误并保留，重试继续等）；回包预算为调用方期限；因停止被取消而中断的 handler 交还 broker 重投 |
| NC-91 | 队列满 / 派发器未运行时不回包，调用方等满超时（结果未知），开可靠总线时还把 RPC 请求写进死信 | 回失败 envelope（`remote.1`），死信只收异步消息 |
| NC-92 | 目标服务在 JetStream 模式时，轻量 `Call` / `CallTo` 落进 `<prefix>.rpc.>` 请求流，调用方报 `unsupported rpc response version 0`，请求却被无期限执行 | 服务端拒绝没有 reply subject 的 JetStream RPC 请求；调用端识别 PubAck，得到 `bus.ErrRPCCapturedByJetStream`。**两端 `nats.rpc.transport` 须一致** |

**限制**：外部 E06 / E07（跨主机网络分区下的 JetStream）。**链接**：[NC-90](../../bugfix/RR-20261005-NC-90.md) · [NC-91](../../bugfix/RR-20261005-NC-91.md) · [NC-92](../../bugfix/RR-20261005-NC-92.md) · [N03 记录](../../review/REVIEW-2026-10-05-noncore-n03.md)

<a id="noncore-7"></a>
#### NONCORE-7 etcd 选主 `Resign(ctx)` 按调用方期限返回（RR-20261005-NC-93）

**结论**：lease 撤销由 election 持有、自带 5s 截止；调用方期限先到返回 ctx 错误。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-7)

**背景**：SDK `Session.Close` 用 session TTL（默认 60s）发 Revoke，etcd 无响应时 Resign 阻塞到 TTL。**现在**：`Resign` 可能返回 `context.DeadlineExceeded` / `Canceled`，此时本地领导权已结束（`IsLeader=false`、`LeaderChan` 关闭）；Revoke 失败或超出 5s 时 lease 在 TTL 后过期。**决定**：B5 etcd 选举保留（第二轮“保留（不弃用）”）。**链接**：[NC-93](../../bugfix/RR-20261005-NC-93.md)

<a id="noncore-8"></a>
#### NONCORE-8 bus 的 SETNX 去重保持现状，契约写进 `ReliableStore` 注释（第十二轮）

**结论**：只改注释。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-8)

**背景**：A2 之后核对调用方时提出“bus inbox 的 SETNX 去重在写结果未知时怎样”。**维护者决定**（第十二轮“bus SETNX 去重”行）：“保持，写进 bus 契约”。**现在写明**（按当前源码）：`BeginConsume` 出错进死信、不重投；死信重投用新 MsgID。**链接**：[DECISIONS-PENDING 第十二轮](../../review/DECISIONS-PENDING-2026-10-05.md)

### N04 redis / mongo / cache / migration

<a id="noncore-9"></a>
#### NONCORE-9 迁移输出在 WAL 准入前验证目标装载与身份（RR-20261004-NC-31）

**结论**：DataEngine 迁移复用 Mongo BSON / ID 与目标 `RestorePersisted` 预校验；坏 BSON、字段类型或身份不再先持久提交。首发 v1.20.0。[实现](impl-cfg-skill-noncore.md#noncore-9)

**兼容**：手写迁移候选须提供 loader / Id；预校验用目标 schema、旧 version；正常 CAS / 投影等待 / 整聚合重读与 int32 ID 兼容保持。**已有坏 WAL 不自动跳过或删除**。**限制**：真实副本集 / HA / 持续竞争见外部验证 E11；生产上已有的坏 WAL 按上面的兼容说明处理。**链接**：[NC-31](../../bugfix/RR-20261004-NC-31.md)

<a id="noncore-10"></a>
#### NONCORE-10 生成 DAO 恢复后深层嵌套修改进入持久提交（RR-20261005-NC-32）

**结论**：wire 转换先恢复未绑定数据，父对象到最终位置后再递归接线，避免子对象的脏通知指向按值返回前的临时副本。首发 v1.20.0。[实现](impl-cfg-skill-noncore.md#noncore-10)

**背景**：加载值正确、深层内存更新正确，但最终 DAO 看不到该更新（commit records=0），影响普通冷加载、迁移后重新加载与同一 wire 恢复的回滚 / 同步入口。**兼容**：应用须重新生成关联 DAO / nested 代码；**历史漏写的数据不自动补回**。**链接**：[NC-32](../../bugfix/RR-20261005-NC-32.md)

<a id="noncore-11"></a>
#### NONCORE-11 versionstore 输掉 CAS 后退避再重读（RR-20261005-NC-52）

**结论**：`RedisStore.Update` 输掉 compare-and-set 后只多一次 GET，再用新值重试；尝试次数与退避策略不变。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-11)

**背景**：退避后仍用退避前 CAS 带回的值重试，竞争写落在退避窗口里时每次重试必输，同键并发（如 chat 世界频道）出现伪 `ErrConflict`。真实隔离 Redis：修前 6 写者 0～2/120、8 写者 3～5/160 次 ErrConflict，修后 0。CAS 冲突率在 versionstore 统一计数是另一项（OPS）。**链接**：[NC-52](../../bugfix/RR-20261005-NC-52.md)

<a id="noncore-12"></a>
#### NONCORE-12 mongotest：唯一索引路径遇数组明确拒绝；`$in` 接受具名切片与整型切片（RR-20261005-NC-102、RR-20261006-08）

**结论**：测试替身 `mongo/mongotest` 两处口径对齐真实 Mongo。NC-102 首发 v1.20.1；RR-20261006-08 首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-12)

**背景**：NC-102——替身把数组当一个值比较，放过真实 Mongo 会拒绝的重复、又误报真实 Mongo 接受的写入；现在返回 `ErrUnsupported`。RR-20261006-08（收尾 A14，O-S5-6）——替身只认 `bson.A` / `[]any` / `[]string`，`saga.MongoStore.ClaimDue` 的 `$in []Status` 报 ErrUnsupported，saga 用例为此绕行；现在按 reflect 展开切片 / 数组、元素按底层类型比较（与驱动编码一致），元素恰为 `byte` 的仍按二进制拒绝；saga 用例去掉绕行、改走完整的 `ClaimDue`。**兼容**：只影响测试替身。**链接**：[NC-102](../../bugfix/RR-20261005-NC-102.md) · [RR-20261006-08](../../bugfix/RR-20261006-08.md)

### N05 remoteentity / ownerroute

<a id="noncore-13"></a>
#### NONCORE-13 缓存副本与 Remote interest 写入前绑定消息身份（RR-20261005-NC-33 / NC-34）

**结论**：`cache` 的副本写入在 Set 前核对业务 key / version 与信封一致；Remote interest 在写注册表前核对完整 snapshot key / SID 哈希、ExpiresAt 与 Upsert 操作。首发 v1.20.0。[实现](impl-cfg-skill-noncore.md#noncore-13)

**背景**：真实 Replicator 两个反例：信封 key7/version3 携带 ID8/version3 时键 8 被写成 bad；携带 ID7/version4 时键 7 被写成 version4，之后合法 version3 还被判为 stale（NC-33）；信封 A 的操作能订阅 B（NC-34）。**兼容**：无 VersionOf 与普通 Delete 兼容保持；合法 Generation=0 与旧 release 代际保护保持；**不增加发布权限认证**。**链接**：[NC-33](../../bugfix/RR-20261005-NC-33.md) · [NC-34](../../bugfix/RR-20261005-NC-34.md)

<a id="noncore-14"></a>
#### NONCORE-14 权威快照加载绑定完整请求键、读取重新检查最终 L1 最低版本（RR-20261005-NC-35 / NC-36、RR-20260913-08 残余）

**结论**：异键权威结果明确拒绝、不写其他视图；旧 epoch 结果被缓存准入拒绝后，实际 L1 版本不足返回 `ErrRemoteSnapshotStale`；L2 发布期间跨过 ExpiresAt 时以返回前的当前时间判过期。首发 v1.20.0。[实现](impl-cfg-skill-noncore.md#noncore-14)

**兼容**：API / wire 不变；保留 epoch 防护、不新增重试。之后 B2（v1.20.2）与 Mirror 第 1～3 步（v1.21.0）重写了快照缓存的水位与读出口（REM 部分）。**链接**：[NC-35](../../bugfix/RR-20261005-NC-35.md) · [NC-36](../../bugfix/RR-20261005-NC-36.md) · [RR-20260913-08](../../bugfix/RR-20260913-08.md)

<a id="noncore-15"></a>
#### NONCORE-15 L2 CAS 落败报 `ErrStaleWrite`、版本化删除留墓碑、Stats 先清理过期兴趣（RR-20261005-NC-130 / NC-131、RR-20260913-01 残余）

> 其他分册对应：REM-1 的背景提到 NC-130 / 131，未单列；本条为主。

**结论**：三处共享 L2 / 兴趣表修复，首发 v1.20.2。边界项（近 REM；B2 随后在同一版本把 L2 定为水位权威）。[实现](impl-cfg-skill-noncore.md#noncore-15)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-130 | `remoteSnapshotL2Store.Set` 在 CAS 落败时返回 nil，`Publish` 把旧快照装进 L1；L1 冷的节点收到迟到复制消息或权威加载输给新提交时读到比 L2 旧的版本 | 返回 `cache.ErrStaleWrite`，`Publish` 改取 L2 的较新值 |
| RR-20260913-01 残余 | 另一节点的在途加载或迟到消息能把已删除实体写回 L2，冷节点读到它直到 L2 TTL | 版本化删除在 L2 留只含 `deleted_version`、与 `snapshot_l2_ttl` 同 TTL 的墓碑，不新于它的写一律拒绝，更新的写清墓碑（**L2 键格式增加字段**） |
| NC-131 | 一阵读取把兴趣表读满后，空闲进程的健康一直报 `capacity exhausted` | 表满时 `Manager.Stats` 先清理过期条目再计数 |

**兼容**：从 v1.20.2 之前的版本滚动升级期间，旧节点不认墓碑、仍可能写回（修复前行为），新节点的下一次删除会收敛。**限制**：复制丢写与切主、多主机强杀见外部验证 E10 / E13。**链接**：[NC-130](../../bugfix/RR-20261005-NC-130.md) · [NC-131](../../bugfix/RR-20261005-NC-131.md) · [RR-20260913-01](../../bugfix/RR-20260913-01.md) · [revn05 记录](../../review/REVIEW-2026-10-05-n05-revn05.md)

### N06 service / saga / servicemetrics

<a id="noncore-16"></a>
#### NONCORE-16 saga 三消费者健康检查与 Resume 持久代际（RR-20261005-NC-37 / NC-38）

**结论**：`ConsumersClosed` 纳入原生 Nest 完成消费者，任一必需订阅缺失 / 退出都让 kit 健康项 fail（NC-37）；Mongo 记录增补兼容字段 `incarnation,omitempty`，重载后派发不复用旧命令 / 回执 ID（NC-38）。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-16)

**兼容**：NC-38 是持久格式增量；参与写入的协调器需统一升级（旧 writer 完整 Replace 会丢新字段），不自动修复历史 waiting / 回执。之后 B1（v1.20.2）用这个代际过滤 completion（SAGA 部分）。**链接**：[NC-37](../../bugfix/RR-20261005-NC-37.md) · [NC-38](../../bugfix/RR-20261005-NC-38.md)

<a id="noncore-17"></a>
#### NONCORE-17 saga 启动幂等身份独立持久化、原生完成路由绑定（RR-20261005-NC-39 / NC-40）

**结论**：`StartDigest`（`start_digest,omitempty`）保存规范化原始意图，步骤 Data 与 Resume 截止时间变化不再改变启动身份（NC-39）；原生 saga 完成解码后精确匹配 Topic 与 payload SagaID，异键在 Complete 与回执副作用前 Permanent 拒绝（NC-40）。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-17)

**兼容**：NC-39——旧的已推进、缺摘要的记录不能证明原始身份，重投收紧为冲突，不自动迁移；自定义 Store / 协调 writer 须保存新增字段。NC-40——合法发送与 wire 不变，不提供发布鉴权或自动改路由。**链接**：[NC-39](../../bugfix/RR-20261005-NC-39.md) · [NC-40](../../bugfix/RR-20261005-NC-40.md)

<a id="noncore-18"></a>
#### NONCORE-18 saga outbox 领取遵守并发更新的重试期限；activity 恢复前验证持久计划（RR-20261005-NC-41 / NC-42、RR-20261001-09 残余）

**结论**：原子领取复查 `next_attempt_at` 与 lease，陈旧候选不能绕过另一发布者的 Nack 退避（NC-41）；activity 公开 `Open` 在 Create 前拒绝异键 / 非 pending / 非法 expected 计划，sweep 在访问 Activities 前跳过非法键 / 跨组 opening（NC-42）。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-18)

**兼容**：API / 格式不变；**不自动修坏存量**，诊断按所属窗口清理。之后 B9 把窗口条目读取收成一个入口（NONCORE-21）。**限制**：Mongo 网络丢回复与提交结果未知见外部验证 E11。多进程已在 `e6828e4f` 上实跑：`TestRealSagaCrossProcessKillRecovers`（真实 JetStream + Mongo 副本集，中途 SIGKILL 一个协调器）60 个 saga 全部完成、outbox 排空、无残留租约。**链接**：[NC-41](../../bugfix/RR-20261005-NC-41.md) · [NC-42](../../bugfix/RR-20261005-NC-42.md)

<a id="noncore-19"></a>
#### NONCORE-19 account 换名释放已被他人提交的死计划、补偿失败计数；activity sweep 验证确认键（RR-20261005-NC-50 / NC-51、RR-20261001-06 残余）

**结论**：account 换名建角也释放名字已被他人提交的死计划（此前只有同名重试会释放，玩家直接换名永远 `ErrRoleLimit`），补偿失败重新计 `rollback.failed`（NC-50）；activity sweep 对已确认窗口键先验证合法与归属，坏条目跳过、保留、计 `sweep.window_key_malformed`，不再结算别的组的活动（NC-51）。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-19)

**后续**：B9（v1.21.0）在这两处做了复核补修（并发释放只计一次、Delivering 坏条目），见 NONCORE-21。**链接**：[NC-50](../../bugfix/RR-20261005-NC-50.md) · [NC-51](../../bugfix/RR-20261005-NC-51.md) · [RR-20261001-06](../../bugfix/RR-20261001-06.md)

<a id="noncore-20"></a>
#### NONCORE-20 game-demo activity 启动时拒绝注定开不出窗口的 `activity.game_sids`（RR-20261005-01，已被 C4 取代）

**结论**：v1.20.1 的修复：列表里重复的 sid、超出 int32 的值、本服加配置超过 `app.SingletonLiveMaxSIDs` 三种情形在 `startActivity` 任何远端调用之前按键名报错。**v1.20.2 起 `activity.game_sids` 键被 C4 活动组文件取代并删除**（OWN 部分），这条承诺改由组文件兑现：`kit/service/global/activity.ParseGroups` 加载时拒绝组内重复、非正数 / 超出 int32、一组超过 64 个成员，game-demo 在查协调器能力之前按 `activity.groups_file` 点名拒绝。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-20)

**回归的去向**（`d5682dc4` 复核，v1.23.0）：原回归 `TestActivityRefusesACandidateListNoWindowCouldOpenWith` 随 C4（`277e1252`）删除，承诺仍然需要，回归改名改形为模板用例 `TestActivityRefusesAGroupNoWindowCouldOpenWith`，kit 有同等用例：

| 原子用例 | 现在 |
| --- | --- |
| 重复 sid、超出 int32 | 组文件加载时拒绝（模板与 kit 各有用例；另加“一个 sid 在两个组里”） |
| 候选超过一次 `Live` 查询上限（200） | 一组至多 64 个成员；**新增守卫** `TestAGroupFitsOneLiveQuery`：组上限不得超过 `app.SingletonLiveMaxSIDs`，两个常量改一个不改另一个时先红 |
| 本服与非正数跳过 | 不再适用：本服必须在某个组里，非正数从“跳过”收紧为“拒绝” |
| 恰好一次 Live 上限被接受 | 恰好 64 个成员的组被接受，且全活时能开窗 |

kit 与生成 game-demo 上都做过变异证明这些用例能红（见修复记录末节）。

**链接**：[RR-20261005-01 修复记录](../../bugfix/RR-20261005-01.md)（末节“更正 / 后续”是逐项对照） · [问题记录](../../bug/RR-20261005-01.md) · [C4 方案](../../feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)

<a id="noncore-21"></a>
#### NONCORE-21 activity 窗口条目统一读入口与运维修复入口；account 建角判定表（B9）

**结论**：activity 读已存窗口条目只走 `readWindowEntries` 一个入口，坏条目在任何列表里都跳过、保留、计数，持有方可用新 Admin 入口清除；account 的 create_role 同名 / 换名、同名被拒后的释放与 `Admin.ResolvePendingCreation` 都查一张“名额状态 × 入口 × 名字状态 → 动作”的表。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#noncore-21)

**背景**：activity 窗口条目校验第 4 次出问题（NC-42、NC-51 链），account 建角判定只在部分入口生效（RR-20261001-06 链）——按 roost-coding“反复出问题要上报方向判断”上报。

**维护者决定**（DECISIONS-PENDING 第四轮 B9 行原文）：“activity 窗口条目统一读入口 + 持有方专用修复入口；account 建角改为‘名额状态 × 名字状态 → 动作’判定表”。

**现在的行为**：

- activity：`PendingActivities`、`DeliveringActivities`、`RetireDelivered` 与后台 sweep 都经 `readWindowEntries`；别的组的键或不合法的键跳过、保留、计数（新增 `sweep.delivering_key_malformed`）；此前 Delivering 里的这种键会让本组 sweep 去写别的组的窗口、自己永远留着，`PendingActivities` 会把它交给 game。`RetireDelivered` 只在本次确实移走时返回 true。新增持有方专用 `Admin.MalformedWindowEntries` / `RemoveMalformedWindowEntry`（**note 必填**，只删确实坏的条目）；窗口记录多两个可选字段 `admin_note` / `admin_action_at_unix`。
- account：`decideCreation`（名额 6 × 入口 4 × 名字 6，144 格表格测试），对外行为不变；两个换名请求并发释放同一个死计划时 `create_role.plan_released` 不再计两次。

**兼容**：`PendingActivities` 不再交出坏键；`RetireDelivered` 返回值语义收紧（仓内唯一调用方不看返回值）。**限制**：Redis Cluster 与多进程见外部验证 E09。**链接**：[B9 / C5 方案](../../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md) · [NC-51 复核补修](../../bugfix/RR-20261005-NC-51.md) · [NC-50 复核补修](../../bugfix/RR-20261005-NC-50.md)

<a id="noncore-22"></a>
#### NONCORE-22 account 换名建角也释放未 admitted 的计划（第五轮、第七轮 O37）

**结论**：计划还没 admitted、名字只被他人 reserved（第五轮）或名字预约已过期无人持有（O37）时，换名请求释放旧 slot 并按新名字建角；已 admitted 的计划不变（仍 `ErrRoleLimit`）。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#noncore-22)

**背景**：B9 判定表让一处不对称显形：同名请求会释放、换名不释放，玩家要先用旧名字重试一次或等预约过期。

**维护者决定**：第五轮（原文）“做：未 admitted 计划、名字仅被他人 reserved 时，换名请求也释放 slot（判定表 `unadmitted other` 行）”；第七轮 O37（原文）“按推荐：account 判定表 `unadmitted other` 行的 free 格也释放名额并用新名字建角”。

**兼容**：行为变化（以前答 `ErrRoleLimit` 的两格现在建角成功）。**链接**：[B9 方案 §6 / §6.1](../../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md)

<a id="noncore-23"></a>
#### NONCORE-23 saga 定义缺失 fence 时退避中的步骤同样记为放弃（RR-20261005-NC-250）

> 与其他分册重复：以 [SAGA-7](guide-saga-drv-dao-rem.md#saga-7) 为准，本条只保留编号与一句话结论（汇总去重）。

**结论**：协调器因缺少定义版本把记录 fence 到 `ManualRequired` 时，若当前步骤正在重试退避，写放弃关闭的 tombstone、删掉它仍排队的命令；之后才到的成功告警（`saga.completion.late_after_abandon_total`）。截止、人工 Compensate、定义缺失三个出口共用一个判断。首发 v1.21.0。边界项（近 SAGA）。[实现](impl-cfg-skill-noncore.md#noncore-23)

<a id="noncore-24"></a>
#### NONCORE-24 global `Bind` 结果未知后用同样参数重试按幂等成功（RR-20261006-05）

> 与其他分册重复：以 [OWN-6](guide-app-own-clk-ops-tool.md#own-6) 为准，本条只保留编号与一句话结论（汇总去重）。

**结论**：`Create` 没建成时读回已存绑定，group 与 globalSID 都一致就返回它（计 `replayed:bind`），不一致才报冲突，错误里带上已存的 group / sid。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-24)

### N07 configdata / attribute / event / errcode

<a id="noncore-25"></a>
#### NONCORE-25 attribute 快照锁、属性层随事务回滚、attribute 生成器拒绝不可表示声明、errcode 扫描不再静默跳过（RR-20261005-NC-60～63）

**结论**：N07 第一批四处，首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-25)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-60 | `attribute.Container.Snapshot` 先释放读锁再 CloneProfile，并发 Apply 撕裂快照 | 复制期间持读锁；CloneProfile 不得回调同一容器 |
| NC-61 | game-demo 升级 / 换装改组件内存里的属性层，Nest 只撤回 DAO；失败后下一次成功提交把虚高的 Base 持久化 | 当时用 `RecordUndo` 登记层的副本；**v1.20.2 起被 A1 取代**：属性三层全在 DAO，组件无状态（DAO 部分） |
| NC-62 | float 字段被静默截断（0.15 导出为 0）、`max` 超过 64 到编译期才失败 | float / bool / string 字段、`max` 超过 64、`index+max-1` 超过 AttrID 在生成期报错 |
| NC-63 | errcode 扫描按正则，别名导入 / 非字面量的 `Define` 不进导出表、逃过重复检查 | 按导入名识别 `Define`（含别名 / 点导入），编号、名字、消息不是字面量时报 `file:line`，检查重复 name |

**兼容**：NC-63——用常量编号的业务工程需改为字面量。NC-61 已生成工程需 `roost project sync`，已污染的存量不自动修正。**链接**：[NC-60](../../bugfix/RR-20261005-NC-60.md) · [NC-61](../../bugfix/RR-20261005-NC-61.md) · [NC-62](../../bugfix/RR-20261005-NC-62.md) · [NC-63](../../bugfix/RR-20261005-NC-63.md)

<a id="noncore-26"></a>
#### NONCORE-26 game-demo 开关热更说明指向 reload 实际读的文件；玩家加载时重建 Gear 层（RR-20261005-NC-64 / NC-65）

**结论**：`gm.config.reload` / `gm.flag.set` 的说明改为“改 `configs/data/feature_flag.json`，或改 CSV 后先 `roost generate`”（NC-64）；加载与新建都按穿戴求 Gear、重组后写 `attr_final`（NC-65）。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-26)

**背景**：照旧说明改 CSV 再 reload 得到 `ok` 而开关不变（文件名也不对）；穿着装备的玩家重启或冷卸载后 Final 掉回 Base、复制给客户端的 `attr_final` 为空。**兼容**：不改存储格式；已有工程 `roost project sync` 取模板。**链接**：[NC-64](../../bugfix/RR-20261005-NC-64.md) · [NC-65](../../bugfix/RR-20261005-NC-65.md)

### N08 codegen

<a id="noncore-27"></a>
#### NONCORE-27 `roost generate` / `project sync` 不再改变进程工作目录（RR-20261004-12）

**结论**：生成器拿工程根下的绝对路径，不再 `os.Chdir`；生成物逐字节不变。首发 v1.20.0。[实现](impl-cfg-skill-noncore.md#noncore-27)

**背景**：生成器运行期间 chdir 进 `.roost-sync-*` 暂存树，同进程其他 goroutine 不设 `Dir` 启动的子进程会继承它；Windows 上暂存目录删不掉而残留。**兼容**：两行生成器提示从相对路径变为绝对路径。**限制**：Windows CI 上的两次清理失败是否全由此引起——Windows 暂存，不保证正确（外部验证 E25）。**链接**：[RR-20261004-12](../../bugfix/RR-20261004-12.md)

<a id="noncore-28"></a>
#### NONCORE-28 roost 跑的 go 命令按进程树取消，Wait 有界；Ctrl-C 后重发信号等其生效（RR-20261004-13 与补修）

**结论**：doctor 的 `go mod verify` / `go list` / `go test` 与 project deps / generate 的 `go get` / `go mod tidy` 超时 / 取消时连同子进程一起结束（Unix 进程组、Windows `taskkill /T`），Wait 最多再等 5 秒。首发 v1.20.0；补修 `cb11be90` 首发 v1.20.1。之后 B6（NONCORE-31）把信号接管收到 CLI 入口。[实现](impl-cfg-skill-noncore.md#noncore-28)

**背景**：超时只杀 go，它起的 compile / link / git 继续以暂存树为工作目录，输出被缓冲时调用还要等它们结束、超时不起作用。补修：重发给自己的信号异步生效，满载时调用方会先跑下去。**限制**：Windows 的 `taskkill /T` 分支只做了交叉 vet——Windows 暂存，不保证正确（外部验证 E25）。**链接**：[RR-20261004-13](../../bugfix/RR-20261004-13.md)

<a id="noncore-29"></a>
#### NONCORE-29 中断不留暂存树、预览列出将刷新的配置、cfggen 帮助目录、`roost id` 用生成器口径（RR-20261005-NC-70～73）

**结论**：N08 四处，首发 v1.20.1（`80802200`）。[实现](impl-cfg-skill-noncore.md#noncore-29)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-70 | Ctrl-C 打断 `go get` / `go mod tidy` 时进程死于信号不跑 defer，整份工程副本留在父目录 | 中断时删掉本次命令所在、已登记的暂存树（v1.21.0 起由 B6 的“正常回滚后再重抛”取代登记表） |
| NC-71 | `roost project diff` / `project upgrade --dry-run` 不列出 sync 将刷新的三份应用自有配置 | 预览先对副本做与 sync 相同的 shutdown 块刷新，再渲染 |
| NC-72 | `roost help cfggen` 指向 tablegen 的输出目录 `configs/generated`，照做会重复声明 `RegisterConfigData` | 改为 `-out ./configs/cfg -pkg cfg` |
| NC-73 | `roost id next / check` 与 `roost add errcode` 用正则识别错误码，别名导入的不算占用、注释里的却算 | 复用 errcode 生成器的 AST 扫描 `ScanDefinitions` |

**链接**：[NC-70](../../bugfix/RR-20261005-NC-70.md) · [NC-71](../../bugfix/RR-20261005-NC-71.md) · [NC-72](../../bugfix/RR-20261005-NC-72.md) · [NC-73](../../bugfix/RR-20261005-NC-73.md)

<a id="noncore-30"></a>
#### NONCORE-30 `make dev-run` 运行期间 generate / sync 不再报 inputs changed（RR-20261005-NC-74）

**结论**：`.dev/`（dev-run 日志）与默认 WAL 目录 `data/wal` 不再算应用输入、不再复制进暂存树；复制、输入快照与提交计划共用同一条工程边界。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-30)

**连带**：生成的 `.gitignore` 此前不忽略 `data/wal`，N15 的 NC-206 补上（NONCORE-51）。**链接**：[NC-74](../../bugfix/RR-20261005-NC-74.md)

<a id="noncore-31"></a>
#### NONCORE-31 roost CLI 入口统一接管信号，中断走正常回滚后再死于该信号（B6，含 N08 O2 / O3 / O6）；codegen 联网用例门（C9）

**结论**：第一次 SIGINT / SIGTERM / SIGHUP 只取消命令的 ctx；`project deps / sync / upgrade / new`、`generate`、`doctor` 在复制、生成器、go 命令或提交点前停下，杀掉 go 进程树、回滚 go.mod / manifest、删除 `.roost-*` 暂存树，已开始的提交做完，然后打印 `roost: interrupted by <sig>: …` 并以同一信号结束（退出码不变）；回滚期间再按一次 Ctrl-C 立即退出。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#noncore-31)

**背景**：RR-20261004-12 / 13、cb11be90、NC-70 是同一处反复出问题：信号接管散在 `runCommandTree` 里，进程死于信号不跑 defer。

**维护者决定**（DECISIONS-PENDING 第四轮原文）：B6“CLI 信号统一接管：CLI 入口接管信号 → ctx 取消 → 正常回滚后再重抛”；C9“过时测试开关要测：打开 `publishedDataEngineGeneratorDependencies` 覆盖的用例，放进需要网络的 CI lane 跑”。

**现在的行为**：O2——upgrade / sync 依赖解析失败的错误提示 `roost project deps --root <dir>`；O3——生成器在暂存树里报的错误改指工程路径；C9——过时常量换成 `ROOST_NETWORK_TESTS=1`，framework-compat 新 job `codegen-network` 打开（SKIP 即失败），默认 `go test` 不联网。Windows 不接管信号，行为不变。

**限制**：Windows 暂存，不保证正确（外部验证 E25）；Linux 上的信号与进程树见外部验证 E26。`codegen-network` job 在 `e6828e4f` 上通过（framework-compat run `37461843085`）。

**链接**：[B6 方案与实施](../../feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md)

<a id="noncore-32"></a>
#### NONCORE-32 DAO 无集合声明 `//roost:dao nocoll`（W-2026-09-18-09 选 A）

**结论**：全部字段 `nopersist` 的内存 DAO 用 `nocoll` 声明，生成物保留读写、undo、回滚快照与同步，不生成集合 / 库名常量和任何 Mongo 读写、迁移、加载路径；game-demo 的 `MonsterDao` 改用它，不再编造 `monsters` 集合。首发 v1.20.0。边界项（近 DAO）。[实现](impl-cfg-skill-noncore.md#noncore-32)

**维护者决定**：10-04 对 WANTED W-2026-09-18-09 选 A。**兼容**：与 `coll=` / `db=` 同时出现、`nocoll=<值>`、含持久字段，以及持久实体或 `remote=managed` 实体使用它时，都在生成期报错并点名。已生成工程把 marker 改成 `nocoll` 后 `roost generate` 即可原地迁移。**链接**：[方案](../../feature/DAO-NO-COLLECTION-2026-10-04.md)

### N09 skill（只列不在 SKILL 的一项）

<a id="noncore-33"></a>
#### NONCORE-33 skillsync 文件 outbox 打开时清理崩溃遗留的临时文件（RR-20261006-04，O6）

**结论**：`NewFileOutboxStore(WithOptions)` 删除名字精确匹配 `outbox-<数字>.tmp` 的普通文件（目录、符号链接、其他名字不动）。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-33)

**背景**：写入中途崩溃留下的临时文件以前永不回收。**决定**：收尾第 3 批 A6。**现在**：同一目录不能有两个在写的 store（另一个 store 的在途临时文件会被删掉，它的替换失败并返回错误，不会静默损坏），写进 `FileOutboxStore` 注释与 `docs/skill/production-readiness.md`。**限制**：Windows 上未实际运行（只做了交叉 vet）——Windows 暂存，不保证正确（外部验证 E25）。**链接**：[RR-20261006-04](../../bugfix/RR-20261006-04.md)

### N10 ai / actionflow / featureflag / hotcode

<a id="noncore-34"></a>
#### NONCORE-34 ai 冻结不丢结束通知、actionflow 丢弃排队动作发 OnEnded、启动失败 Cancel 重入不留孤儿、hotcode 补丁点状态整体发布（RR-20261005-NC-120～123）

**结论**：N10 第一批四处，首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-34)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-120 | ai `Freeze` 期间动作 / 任务结束通知被丢，BehaviorStrategy 里等该动作的叶子在 Recover 后永远 Running | `Freeze` 只暂停 Tick，结束照常交给策略 |
| NC-121 | `ClearQueue` / `EndAll` / `ClearMission` 丢弃排队项不发 OnEnded | 每个被丢弃项发一次取消状态的 OnEnded（不调 Cancel、不发切换） |
| NC-122 | 启动失败后 Cancel 里重入装上的动作被清掉，成为孤儿 | 留在当前位，外层返回 `ErrReentrantMutation`（v1.20.2 起被 B7 整体取代，见 NONCORE-35） |
| NC-123 | 并发 Replace / Revert 可永久留下 Patched 与 Meta 互相矛盾的点，`hotcode.list` 误报 | 当前函数、Meta、代数合成一个不可变状态原子替换，写者按点串行 |

**链接**：[NC-120](../../bugfix/RR-20261005-NC-120.md) · [NC-121](../../bugfix/RR-20261005-NC-121.md) · [NC-122](../../bugfix/RR-20261005-NC-122.md) · [NC-123](../../bugfix/RR-20261005-NC-123.md)

<a id="noncore-35"></a>
#### NONCORE-35 actionflow 回调里的变更进延后队列，判定集中在 `submit` 一处（B7）

**结论**：动作的 Start / Tick / Cancel 与 OnQueued / OnTransition / OnEnded 钩子里对 `ActionRunner` 的 Start、Enqueue、Tick、End、EndAll、ClearQueue、ClearMission、Recover 不再立即执行，回调返回后由最外层调用按发起顺序执行；`ActionRunner` 不再返回 `ErrReentrantMutation`。首发 v1.20.2，**破坏性**。[实现](impl-cfg-skill-noncore.md#noncore-35)

**背景**：NC-121 / NC-122、U-0100 都是回调重入的同形问题，靠事后比对“当前位是否被改”的分支补了六处。

**维护者决定**：第二轮 B7“ai / actionflow：保留在 core”；第三轮（原文）“actionflow 回调重入：按推荐（b）——回调里对 runner 的变更进延后命令队列，回调返回后按序执行，判定集中一处”。未采用 (a) 回调期间拒绝重入、(c) 移出 core。

**现在的行为**：回调里的 Start / Enqueue 返回已分配的 ID 与 nil（`Deferring()` 可查），**拿到 ID 的动作一定收到 OnEnded**；直接 Start 失败也发一次 OnEnded（O-A2）；Cancel 重入不再让同一动作被取消两次（O-A3）。新增 `ActionRunnerConfig.MaxDeferredCommands`（默认 64，超出 `ErrDeferredQueueFull`）与 `MaxDeferredSteps`（默认 1024，回调互相触发时截停并返回 `ErrDeferredRunaway`）。O-A1 定义为“EndAll 先结束全部、回调变更随后执行”。

**兼容与迁移**：依赖“回调里 Start 立即生效 / 返回 `ErrReentrantMutation`”的业务代码要改为按延后语义写（回调里返回 nil，执行错误经 OnError）。`ai` 只补注释。**链接**：[方案与实施](../../feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md)

<a id="noncore-36"></a>
#### NONCORE-36 N10 第二批：Parallel 结果已定即停、策略回调里切换延后、B7 两处 panic 路径收敛、插件加载修正与回滚、补丁可见性（RR-20261005-NC-240～247）

**结论**：首发 v1.21.0（`44964553`）。[实现](impl-cfg-skill-noncore.md#noncore-36)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-240 | ai Parallel 在 RequireAll 出现失败 / RequireOne 出现成功后继续 Tick 后面的子节点，先发起没人要的动作再打断 | 结果确定即停止本次 Tick |
| NC-241 | ai Controller 策略回调里的 SetStrategy / Shutdown 立即切换，旧行为树在 Stop 之后接着跑、发起的动作成孤儿 | 延后到回调返回后执行（与 B7 同向）；回调里返回 nil，切换出错经 OnError；回调返回前 `Strategy()` 仍是旧策略 |
| NC-242 | `Update` 的 fn panic 让 runner 永远处在“回调中” | panic 恢复成错误 |
| NC-243 | 替换动作时旧动作 Cancel panic 让新动作不启动、交出的 ID 没有结论 | Cancel panic 只经 OnError 报告，新动作照常启动；直接 Start 返回 (ID, nil) |
| NC-244 | 以 `hotcode.Bundle` 接口变量导出的 PatchBundle 因变量遮蔽一律被拒 | 修正；新增真实 .so 测试包 `hotcode/plugintest` |
| NC-245 | 插件 Apply 失败或 panic 时已替换的点留在半应用状态 | 新增 `Registry.ApplyBundle`，失败时恢复成加载前那一代（不是原函数）；LoadPlugin 改走它，admin revert 与插件加载串行 |
| NC-246 | 同一工厂产生的闭包补丁被误报未打补丁 | `Patched` 由 Replace / Revert 记录 |
| NC-247 | `Resolve[T]` 在签名相同只差命名时回落 fallback，补丁不生效 | 转换后返回补丁；签名不符仍回落，计入新字段 `PointInfo.ResolveMismatches` |

**限制**：Linux 上的真实插件加载见外部验证 E27（本机 macOS arm64 / Go 1.27.0 已跑真实 `.so`）；Windows 部分暂存，不保证正确。**链接**：[n10b 记录](../../review/REVIEW-2026-10-06-noncore-n10b.md) · [NC-240](../../bugfix/RR-20261005-NC-240.md) … [NC-247](../../bugfix/RR-20261005-NC-247.md)

<a id="noncore-37"></a>
#### NONCORE-37 MissionRunner 回调里的变更进延后队列；EndAll 清场顺序写明（第五轮）

**结论**：任务的 Start / Tick / OnActionEnd / End 与 OnState / OnChanged / OnEnded / ClearActions 等钩子里对 MissionRunner 的 StartMission、CancelMission、EndCurMission、Tick、OnActionEnd 延后到最外层调用返回前按发起顺序执行；任何 runner 都不再返回 `ErrReentrantMutation`（保留导出）。`EndAll` / `EndAllAction` 期间回调发起的动作在它返回后启动——**要什么都不再运行，先 `EndCurMission` 再 `EndAll`**。首发 v1.21.0，**破坏性**。[实现](impl-cfg-skill-noncore.md#noncore-37)

**维护者决定**（DECISIONS-PENDING 第五轮原文）：MissionRunner“改为延后队列（与 ActionRunner / B7 一致：回调里的变更回调返回后按序执行）”；EndAll“按推荐：保持现状，在接线说明写清‘要清场先结束当前任务再 EndAll’”。

**现在的行为**：回调里返回 nil，执行时的错误经 OnError 报告（`taskflow: deferred start mission: …`）；新增 `MissionRunner.Deferring()`、`MissionRunnerConfig.MaxDeferredCommands` / `MaxDeferredSteps`。ai 无代码改动：`OnMissionEnd` 里的 SetMission 现在返回 nil 并在结束做完后启动。

**链接**：[方案 §7 / §8](../../feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md)

<a id="noncore-38"></a>
#### NONCORE-38 ai 的 O-T3 / O-T4 保持并写文档（第十二轮）

**结论**：只改注释与 kit/README。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-38)

O-T3：策略 `Init` 里发起的动作会被 `EndActions` 结束；O-T4：`Shutdown` 不调 `EndActions`。**维护者决定**（第十二轮原文）：“N10 O-T3 / O-T4：保持并写文档”。写进 `ai/strategy.go` 的 `Strategy` / `StoppableStrategy`、`Controller.Shutdown` 注释与 kit/README 的 ai 段。**链接**：[DECISIONS-PENDING 第十二轮](../../review/DECISIONS-PENDING-2026-10-05.md)

### N11 spatial / timer / clock / index

<a id="noncore-39"></a>
#### NONCORE-39 World 定时器堆随事务回滚；timer Tick 期间取消 / 改期 / 重入生效；spatial / index 边界（RR-20261005-NC-140～147）

**结论**：N11 第一批，首发 v1.20.1（`23a10f42`）。[实现](impl-cfg-skill-noncore.md#noncore-39)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-140 / 142 | game-demo World 定时器：撤回的武装留在内存、撤回的触发从堆里消失（截止时间丢到重启）；已过期的截止时间报告已武装却没有节点 | 当时向可回滚事务登记逆操作；**v1.20.2 起被 A1 取代**（`timer_next_due` 进 DAO，组件无状态）；过期截止时间武装为下一次 Tick 触发 |
| NC-141 / 147 | Tick 期间对仍在堆里的定时器取消 / 改期，它仍按旧期限触发一次；重入 Tick 改变外层语义 | 立即生效；重入 Tick 只由最外层收尾 |
| NC-143 | `spatial.BlockIndex.BlockRect` 在 int64 上界溢出，矩形为空或倒置 | 修正 |
| NC-144～146 | `index` 在 NaN 值、混合动态类型接口键、零值 `OrderedIndex` 上 panic / 丢写 | 修正 |

**兼容**：模板改动需 `roost project sync`。**链接**：[N11 记录](../../review/REVIEW-2026-10-05-n11.md) · [NC-140](../../bugfix/RR-20261005-NC-140.md) … [NC-147](../../bugfix/RR-20261005-NC-147.md)

<a id="noncore-40"></a>
#### NONCORE-40 timer 同期限按 priority、再按登记顺序触发；未注册类型的节点删除时可见（D-L1 / D-L2）

> 与其他分册重复：以 [CLK-6](guide-app-own-clk-ops-tool.md#clk-6) 为准，本条只保留编号与一句话结论（汇总去重）。

**结论**：堆按 (End, `Node.Priority`, ID) 排序，priority 数值小的先触发、缺省 0，新增 `Scheduler.NewTimerWithPriority`；到期节点的类型没有 handler 时照旧删除，另打 Warn 并计 `timer.unhandled_dropped_total{kind}`，新增 `Scheduler.ReportUnhandledTypes()`。首发 v1.21.0，**行为变化**。边界项（D-L 系列的时间来源 D-L3 在 CLK）。[实现](impl-cfg-skill-noncore.md#noncore-40)

<a id="noncore-41"></a>
#### NONCORE-41 game-demo 场景寻路系统停止不再清空共享指针（RR-20261005-NC-270）

**结论**：`PathFindSystem.Stop` 只置原子标志，与不持 Scene 锁的并发寻路不再数据竞争。首发 v1.21.0（生成形状）。[实现](impl-cfg-skill-noncore.md#noncore-41)

**链接**：[NC-270](../../bugfix/RR-20261005-NC-270.md)

### N12 metrics / log / failurelog / robot

<a id="noncore-42"></a>
#### NONCORE-42 failurelog 在脚本结果未知时不再走非原子降级（RR-20261005-NC-160）

**结论**：`AppendRaw` / `DeleteRaw` / `Purge` 的 Lua 脚本报错（连接断开、读超时、ctx 到期）时原样返回错误，不再补做 RPUSH / LREM / LLEN+DEL。首发 v1.20.2，**行为收紧**。[实现](impl-cfg-skill-noncore.md#noncore-42)

**背景**：降级会让同一条死信写两份、多删一条同值记录、把清空之后新到的死信删掉（真实 Redis 上复现两份）。降级只留给 Eval 返回 `(nil, nil)` 的无 Lua 适配器。bus 侧表现为 `bus: write dead letter failed`。**限制**：Redis Cluster 见外部验证 E08。`DeadLetter` 报错之后没有重投链：按 bus 去重契约（NONCORE-8，第十二轮“保持并写进契约”），消息只留 `bus: write dead letter failed` 错误日志，InboxTTL 内同 MsgID 的投递按重复跳过；本修复只让结果未知也走这条既有分支。**链接**：[NC-160](../../bugfix/RR-20261005-NC-160.md)

<a id="noncore-43"></a>
#### NONCORE-43 robot / statslog / log 五处（RR-20261005-NC-161～165）

> 其他分册对应：NC-165 另见 APP-12 背景。

**结论**：首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#noncore-43)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-161 | loadtest 阈值没有样本时按 0 通过（`-duration` 短于场景耗时的运行退出码 0） | 判违反，`ThresholdResult.reason` 写 `no_samples`；新增 `metrics.HistogramCount` 与可选 `loadtest.Config.SampleCount`（**行为收紧**） |
| NC-162 | 重连后 `EnsurePushCapture` 因标记残留变成空操作，推送被丢 | capture 标记记住注册时的会话，重连后在新会话上注册 |
| NC-163 | websocket 握手不回时永久阻塞 | `DialContext`，`DialTimeout` 覆盖建连与升级握手 |
| NC-164 | 实体全部卸载后 `entity.count_by_kind` / `count_by_category` 停在旧值 | 写回 0 |
| NC-165 | `log.Close` 之后的日志丢失，`log.stdout: false` 时 main 的 `server exit` 无去处 | 关闭文件 sink 后默认 logger 改写控制台输出，只配文件时写 stderr |

**链接**：[NC-161](../../bugfix/RR-20261005-NC-161.md) … [NC-165](../../bugfix/RR-20261005-NC-165.md)

<a id="noncore-44"></a>
#### NONCORE-44 robot 会话 / 日志 sink / Prometheus 转义 / Coalescer / loadtest 停止原因（RR-20261005-NC-261～266）

**结论**：N12 留项，首发 v1.21.0（`36220f34`）。[实现](impl-cfg-skill-noncore.md#noncore-44)

| 编号 | 现在 |
| --- | --- |
| NC-261 | robot `Call` 发送受 ctx 约束（服务端不读时按调用方 ctx 返回，之前等到会话关闭） |
| NC-262 | seq 非 0 而无人等待的包丢弃并计 `robot.session.late_response{msg}`，不再当推送交给 `WaitPush` / 推送 handler |
| NC-263 | 轮转打不开新分片时继续写上一分片、每秒重试一次（计 `log.rotate_failures`）；控制台与文件逐个写，一个出错不影响另一个（计 `log.write_errors{sink}`） |
| NC-264 | Prometheus 标签值只转义 `\\`、`\"`、换行，非法 UTF-8 换成 U+FFFD；之前 `strconv.Quote` 的 `\t` / `\u` / `\x` 让抓取整页失败 |
| NC-265 | robot `Coalescer.Close` 等最后一次 flush |
| NC-266 | loadtest Duration 到期记 `stop_reason=duration`（之前记成 completed） |

**链接**：[revleft 记录](../../review/REVIEW-2026-10-06-revleft.md) · [NC-261](../../bugfix/RR-20261005-NC-261.md) … [NC-266](../../bugfix/RR-20261005-NC-266.md)

<a id="noncore-45"></a>
#### NONCORE-45 robot：Stage 先缩后扩不再复用刚停掉的机器人的序号与 PlayerID（RR-20261006-09，N12 O9）

> 与其他分册重复：以 [OPS-7](guide-app-own-clk-ops-tool.md#ops-7) 为准，本条只保留编号与一句话结论（汇总去重）。

**结论**：序号只增不回收。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-45)

### N13 container / safemap / goroutine / misc

<a id="noncore-46"></a>
#### NONCORE-46 遍历回调自锁、跨桶停止、FastMap 幻影键、TaskPool 关闭 panic、拓扑排序判环、KeyMap 遍历删除（RR-20261005-NC-180～185 与复审补修）；TaskPool 统计口径（RR-20261006-20）

**结论**：N13 六处，首发 v1.20.2；加上 TaskPool 统计不再瞬间读到“结束数大于提交数”（RR-20261006-20，`41bdb9e5`），首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-46)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-180 | `Bucket.Range` 回调里 `Del` / `Add` / 未命中 `Get` 当场自锁；`EntityManager.Range` 回调里 `Destroy` 卡死全进程的 Add / Destroy | 读锁内复制、锁外回调；实体先持引用（`Touch`）再回调，到达前已摘除的不交出，回调期间被销毁的推迟到引用归还才清理（复审补修 `815c3661`，此前会交出 ID / 分类已清零的实体） |
| NC-181 | `RangeAll` / `RangeWithCursorCnt` 见 false 不跨桶停止；`EntityManager.Range` 写明的提前停止不成立 | false 跨桶停止 |
| NC-182 | FastMap（DAO `map=fast`）遍历回调里改已有键触发扩容重排，交出零值键、漏改原有键，并把 `items.0` 写进提交 | `Set` 改已有键不重排；`Range` 识别表被换掉后到当前表查剩余键；`Clear` 不清零旧数组 |
| NC-183 | `TaskPool.Submit` 与 `Shutdown` 并发 panic “send on closed channel” | 受理与关闭互斥 |
| NC-184 | 拓扑排序遇到未单独注册的依赖误报环，未注册节点还会掩盖真正的环 | 按全部节点判环 |
| NC-185 | `KeyMap.Range` 回调删除当前键漏键、交出零值键 | 每桶先复制再回调 |
| RR-20261006-20 | `TaskPool.Submit` 先入队后计 total、`GetStats` 先读 total 后读结束数，两个窗口任一都能让一次读到 `completed + failed > total`（P4，统计口径） | `Submit` 先计数再入队、被拒时撤回；`GetStats` 先读结束数再读 total；保证 `completed + failed ≤ total`（三个计数不是同一时刻的快照，注释写明） |

**RR-20261006-20 的来历与决定**：N13 O9 后半，[revleft](../../review/REVIEW-2026-10-06-revleft.md) §4 曾以“没有确定性红测试、零调用方”不改；第十三轮维护者决定“NONCORE-46 TaskPool 统计：维护者选 A：修，统计取一致快照，保证 completed ≤ total”。实施选“调整计数与读取顺序”而不是“一把锁包住计数与读取”（每个任务的提交与完成都去抢同一把锁，只为一个零调用方的统计接口）。零调用方 API 按 C8 保留。被拒的提交在“先加后撤回”之间会让 total 瞬时多算一个，不破坏这条关系。

**性能**：`EntityManager.Range` 10 万实体一次约 0.86ms → 4.1ms（每实体两次 CAS），唯一正式调用方是 statslog 的分钟级统计；FastMap Set/Get 无显著差异（p=0.937）。**链接**：[NC-180](../../bugfix/RR-20261005-NC-180.md) … [NC-185](../../bugfix/RR-20261005-NC-185.md) · [N13 记录](../../review/REVIEW-2026-10-05-n13.md) · [RR-20261006-20 问题](../../bug/RR-20261006-20.md) · [修复](../../bugfix/RR-20261006-20.md)

<a id="noncore-47"></a>
#### NONCORE-47 遍历回调定为仓库级契约（C7）

**结论**：凡框架交给业务的 `Range` 回调里可以读写同一个容器、返回 false 立即停止；写进 roost-coding、根 README §16 与 `safemap` 包注释；新增共用回归辅助 `internal/rangecontract`，已套 container、safemap、entity 与生成 DAO 三种 map 的 `RangeX`。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#noncore-47)

**维护者决定**（DECISIONS-PENDING 第二轮 C7 行原文）：“遍历回调语义定为仓库级契约：是”。

**契约细节**：回调里 Get / Set / Delete / Clear / 嵌套 Range 不死锁不 panic；false 之后不再调用回调；不交出不存在的条目，同一键至多一次；开始时就在、期间没被删除的键恰好一次；期间删掉的未到达键在快照实现里可能仍交出，回调里新增的键是否交出不承诺。不是遍历的回调（`ShardedSafeMap.Read` / `Compute` 持分片锁调用）不适用。

**补修**（套契约时发现）：`BucketHolder.RangeWithCursorCnt` 回调里再做游标遍历让外层重走同一个桶（NC-181 残余）；`EntityManager.RangeGroupEntities` 回调里销毁同组实体后以 ID 0 交出已清零的实体（NC-180 残余）。

**链接**：[C7 方案](../../feature/C7-RANGE-CALLBACK-CONTRACT-2026-10-06.md) · [roost-coding](../../agent-skills/roost-coding/SKILL.md)

<a id="noncore-48"></a>
#### NONCORE-48 container / goroutine 零调用方 API 三处（RR-20261005-NC-267～269）

**结论**：ObjectPool 重复 Put 不再让两次 Get 拿到同一对象（NC-267）；TopologicalSortCache 复制依赖切片（NC-268）；TaskPool 一次性——先 Shutdown 再 Start 之后仍能停下，Shutdown 后 Start 无效（NC-269）。首发 v1.21.0。仓内零调用方（C8 决定“保留”）。[实现](impl-cfg-skill-noncore.md#noncore-48)

**链接**：[NC-267](../../bugfix/RR-20261005-NC-267.md) · [NC-268](../../bugfix/RR-20261005-NC-268.md) · [NC-269](../../bugfix/RR-20261005-NC-269.md)

### N14 Kit 跨域装配

<a id="noncore-49"></a>
#### NONCORE-49 Mongo Mod 连接日志去掉 URI 里的口令（RR-20261005-NC-191）

**结论**：`mongo mod: connected` 的 `uri` 把 userinfo 口令换成 `***`。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#noncore-49)

**限制**：无。mongo-driver 自身的错误信息已查：第一次重核用 mongo-driver v2.6.0（`e6828e4f` 的依赖）试了 12 种带口令的 URI（解析失败的各种形状、连不上的主机、对隔离环境 Mongo 的认证失败），错误文本都不含口令。**链接**：[NC-191](../../bugfix/RR-20261005-NC-191.md)

<a id="noncore-50"></a>
#### NONCORE-50 启动失败先收回 Service 已启动的部分（RR-20261005-NC-193）

> 与其他分册重复：以 [APP-9](guide-app-own-clk-ops-tool.md#app-9) 为准，本条只保留编号与一句话结论（汇总去重）。

**结论**：`Service.Init` 返回错误时 App 同样调用 `Shutdown`（限时 5s），再停 Mod、释放单实例锁；Shutdown 没在时限内结束（含不配合 ctx、panic）时不停 Mod、不释放锁，与正常停机一致。首发 v1.20.2。边界项（近 APP）。[实现](impl-cfg-skill-noncore.md#noncore-50)

### N15 scripts / cmd 与非 Go 资产

<a id="noncore-51"></a>
#### NONCORE-51 脚本与门禁不再对没核对的东西报通过；隔离环境入口互不干扰（RR-20261005-NC-200～208、A5）

> 其他分册对应：TOOL-1（NC-205 pretag）、TOOL-7（NC-207 与全局命令持锁），其余 N15 项本条为主。

**结论**：N15 九处与 A5 共享环境规则，首发 v1.20.2。边界项（NC-205 pretag 近 TOOL）。[实现](impl-cfg-skill-noncore.md#noncore-51)

| 编号 | 现在 |
| --- | --- |
| NC-200 | `scripts/gapmap.sh` 在跟踪文件有未提交修改时拒绝启动（之前收尾 `git checkout -- .` 会把它们一起丢掉） |
| NC-201 | `redis-cluster-suites.sh` 清掉 `ROOST_DATAENGINE_IT` / `REDIS_ADDR`，只跑 Cluster 准入的用例 |
| NC-202 | toxiproxy pid 文件指向的进程须是 `toxiproxy-server -port <本环境 API 端口>`，否则 down / reset 拒绝而不是杀掉复用该 pid 的进程 |
| NC-203 | `remote-acceptance.lock` 被别的运行持有时，`dataengine-env.sh` 的 up / down / reset / fault / heal / test 与 `scripts/remote-fault.sh` 以 2 拒绝；**A5 复核补修**：全局运维命令运行期间持有锁（持锁者的子进程沿用） |
| NC-204 | glsvet 对不存在的目录、`<dir>/...` 的根不存在或文件解析失败退出 2（之前按 0 个违例放行，**行为收紧**） |
| NC-205 | pretag 在 origin 不可达时失败（之前跳过远端同名 tag 检查） |
| NC-206 | 生成的 `.gitignore` 忽略 `/data/wal/`。**已有工程请手工补一行**，已提交的 WAL 段自行 `git rm --cached` |
| NC-207 | 故障矩阵格日志含 `no tests to run` / `[no test files]` 记 FAIL |
| NC-208 | `redis/driver`、`kit/nats`、`kit/dataengine` 的 toxic 用例自建随机端口代理、只删自己的毒，不再 `/reset` 共享 toxiproxy（`d43aa3ba` 补修 kit/dataengine） |

**维护者决定**（DECISIONS-PENDING 第二轮 A5 行原文）：“按推荐：② 共享，全局运维命令保留锁”。规则写进 `kit/scripts/integration/README.md`、roost-coding 与 roost-bugfix lessons：并行会话各跑 `-run` 选定的用例，故障一律自建代理 / 进程。

**限制**：Linux 上 NC-202 的 pid 认领见外部验证 E26。修后 heal / 矩阵已在真实共享隔离环境上实跑：v1.20.2 / v1.21.0 / v1.22.0 发版矩阵各 21/21（矩阵先 heal、每格前再 heal）。**链接**：[N15 记录](../../review/REVIEW-2026-10-05-n15.md) · [NC-200](../../bugfix/RR-20261005-NC-200.md) … [NC-208](../../bugfix/RR-20261005-NC-208.md)

### 跨单元与核心线相关

<a id="noncore-52"></a>
#### NONCORE-52 停机入口超预算时如实返回 ctx 错误并保留对象，重试继续等到真实排空（RR-20261005-NC-170～174）

> 其他分册对应：APP-6（A3 停机契约骨架，背景提到 NC-170～174），本条为主。

**结论**：五个停机入口按“三步停机”修正，首发 v1.20.2。边界项（A3 停机契约骨架在 APP）。[实现](impl-cfg-skill-noncore.md#noncore-52)

| 编号 | 位置 | 以前 → 现在 |
| --- | --- | --- |
| NC-170 | `manager.Engine` / `kit/manager` | 第一次超时后清空列表、重试报成功，过期 ctx 下继续停依赖 → 保留、重试再等 |
| NC-171 | kit Nest Mod | 卸载后重载的停止句柄提前丢弃、entitysync 在 worker 仍跑时关闭 → 保留到 worker 退出、排空后才关闭 |
| NC-172 | JetStream 同步总线 | `Stop` 不等在途 handler → 新增 `StopWithContext`，`Stop` 等在途 handler；停止后的投递 NAK 交还 broker、拒绝 `Subscribe` |
| NC-173 | etcd `Discovery.Deregister` / `Assembly.Close` | 不在 ctx 内等注册循环、注销失败也关 client → ctx 内等、注销成功才关闭；A3 复核补修：`Close` 只关一次、之后返回第一次的结果 |
| NC-174 | `mirror.Replicator` / remoteentity `Assembly.Stop` | 不等已进入 ApplyReplica 的 handler → 新增 `StopWithContext` 并等待 |

**兼容**：行为收紧；不配合 ctx 的 `StopWithContext` 不会被终止，只是如实超时并保留。**链接**：[证据](../../bugfix/evidence/noncore-bugfix-20261005-stopshape/README.md) · [NC-170](../../bugfix/RR-20261005-NC-170.md) … [NC-174](../../bugfix/RR-20261005-NC-174.md)

<a id="noncore-53"></a>
#### NONCORE-53 Mongo Mod 停止收敛（RR-20261005-NC-260）

> 其他分册对应：DRV-5 的 Close 口径表列了 NC-260，本条为主。

**结论**：`mongo/driver` 的 `Client.Close` 对已经断开的客户端返回 nil，Mod 不再因 mongo-driver 的 “client is disconnected” 永远停不下。首发 v1.21.0。边界项（Close 统一口径 RR-20261006-10 在 DRV）。[实现](impl-cfg-skill-noncore.md#noncore-53)

**兼容**：依赖第二次关闭报错的调用方现在看到 nil。**链接**：[NC-260](../../bugfix/RR-20261005-NC-260.md)

<a id="noncore-54"></a>
#### NONCORE-54 nest 用例在 `-shuffle` 下互相污染（收尾第 4 批 A2 / A3）；派发取锁要求 Guard 作用域（RR-20261006-12，原 W-2026-10-06-01）

**结论**：两步。① 收尾第 4 批（`611d5d72`，只改测试）：`group_lock_test` 在没有 Guard 作用域的 goroutine 里取锁，组迁移重试把同一个 `EntityGuard` 两次放回池，之后两个快 worker 共用一个 Guard、互相解锁；用例改为先建作用域，目标用例失败时放行闸门、停机有上限。② 根因修在产品代码（`b7471ae4`，RR-20261006-12）：nest 派发取锁只用当前 goroutine Guard 作用域里的 Guard，没有作用域时取锁前返回错误；删除了“无作用域时把整个 Guard 归还池”的 `releaseDispatchLocks`，释放只放本次取得的实体锁，Guard 只由作用域结束时归还一次。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-54)

**背景**：A2 查 `-shuffle` 失败时发现，这一分支按当时源码生产不可达（派发入口都在 `runNestLogic` 的作用域里），登记为 WANTED W-2026-10-06-01 留给核心线判断。维护者第十三轮决定交给 review 前不留 WANTED，转为 RR-20261006-12（P3）修复；fixn 合并时与 fixr 撞号，顺延为 -12（`71c8f394`）。

**选这个修法的理由**：没有采用“无作用域时调用方持有、重试不归还”——Guard 上挂的不只本次取得的实体锁（handler 新建实体的锁、被取代的旧实例、解锁后回调），只有完整的作用域释放能收尾，调用方等于要自己再写一遍作用域；而且 handler 里没有作用域时 `Cast` 返回 `ErrCastNoContext`，与快池派发的语义不一致。报错而不 panic：入口本来就返回 error。

**现在的行为**：生产行为不变（入口全在作用域里）。直接调用 nest 内部派发函数而不建作用域的测试会拿到未导出的 `errDispatchWithoutGuardScope`，要像 `runNestLogic` 一样先建作用域。

**兼容**：只改 nest 未导出函数；业务代码不受影响。

**验证**：三条新回归先红后绿（无作用域重试时 Guard 被归还 2 次 → 0 次；无作用域 `singleDispatch` 成功执行 → 返回错误）；第一次重核在 `e6828e4f` 上做了有界 seed 扫描：`-shuffle` 230 个 seed、`-race` 30 个 seed，260 次全部通过。

**链接**：[RR-20261006-12 问题](../../bug/RR-20261006-12.md) · [修复](../../bugfix/RR-20261006-12.md) · [收尾第 4 批](../../bugfix/CLOSING-BATCH-4-2026-10-06.md) · [WANTED（已转 RR）](../../bug/WANTED.md)

<a id="noncore-55"></a>
#### NONCORE-55 Nest 暂时性冲突的重新准入加抖动（U-0279）

**结论**：锁超时 / 组变化 / 组迁移待定的消息重排延迟从固定 5ms 改为 5ms 下限加 `[0, 5ms)` 均匀抖动，对称的交叉创建不再靠调度噪声解开。首发 v1.20.1。边界项（核心线 Nest 修复，没有对应主题）。[实现](impl-cfg-skill-noncore.md#noncore-55)

**背景**：固定延迟让同一轮因同一冲突回滚的两条消息（handler 内交叉新建 X / Y，RR-20260926-48）总在同一时刻重新准入；v1.20.0 / v1.19.2 生成工程 `TestGeneratedDataEngineCrossCreateResolvesOnRealWAL` 正常负载下失败率 25%～55%，调用方收到 `ErrLockTimeout`。**兼容**：400 次上限与最短约 2s 的重排窗口不变，平均延迟 5ms → 7.5ms。**链接**：[U-0279](../../bugfix/U-0279-nest-requeue-jitter.md)

<a id="noncore-56"></a>
#### NONCORE-56 game-demo 玩家租约的三处修复（RR-20261004-10 / 11 / 14），同版被静态绑定取代

> 与其他分册重复：以 [OWN-1](guide-app-own-clk-ops-tool.md#own-1) 为准，本条只保留编号与一句话结论（汇总去重）。

**结论**：v1.20.0 开发期间先修了按玩家 Redis 租约的三处缺陷（刷新回合重新认领的时间预算、撤离进行中拒绝准入、租约窗口从设键那次请求起算），随后同一版本里 `f051e24a` 把 PlayerOwners 改为静态绑定、删除了整套租约逻辑（OWN 部分）。**v1.20.0 发布物里这些代码已不存在**（发版提交上模板中 `handBackPassBudget` / `SetNX` 等均无结果）。边界项（近 OWN）。[实现](impl-cfg-skill-noncore.md#noncore-56)

## 外部验证（本部分相关）

本机做不了的验证统一在 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)（E01～E28）。与本部分条目有关的：

| 编号 | 内容 | 相关条目 |
| --- | --- | --- |
| E03 / E06 | 跨主机 broker 与弱网下的复制丢更新 | NONCORE-13 |
| E04 | Sync 真实网络（含 skillsync 经 kit syncstream / NATS 的端到端） | SKILL-3、SKILL-4 |
| E05 | 真实网关 / 反向代理后的 HTTP（含 HTTP/2）与 robot 重连、发送缓冲满 | NONCORE-2～5、NONCORE-43、NONCORE-44 |
| E06 / E07 | NATS JetStream、etcd 多节点 HA | NONCORE-6、NONCORE-7、NONCORE-52 |
| E08 | 多机 Redis Cluster（含 `redis.cluster_addrs` 列表写法起服） | CFG-1、NONCORE-1（以 APP / OPS 为准）、NONCORE-42 |
| E09 | account / chat / activity 与 versionstore 在 Cluster 与多进程下 | NONCORE-11、NONCORE-19、NONCORE-21、NONCORE-22 |
| E10 / E13 | 异步复制丢写与切主、多主机强杀 | NONCORE-15 |
| E11 | Mongo 跨主机副本集、切主中提交、网络丢回复 | NONCORE-9、NONCORE-16～18 |
| E19 | 物理断电与三进程恢复链路（含 World 定时器提交被拒、日志磁盘写满） | NONCORE-39、NONCORE-44 |
| E21 / E22 | 真实 systemd / k8s 停机 | NONCORE-1（以 APP / OPS 为准） |
| E25 | Windows：CLI 信号与进程树、暂存树、文件 outbox 替换——**暂存，不保证正确** | NONCORE-27、NONCORE-28、NONCORE-31、NONCORE-33 |
| E26 | Linux 上 CLI 信号与进程树、NC-202 pid 认领 | NONCORE-31、NONCORE-51 |
| E27 | hotcode 真实插件加载（Linux；Windows 部分暂存，不保证正确） | NONCORE-34、NONCORE-36 |

本机能做的验证在冻结提交上都已做完或有既有证据，各条“限制”写了结论；2026-10-07 补写的 CFG-14～16、SKILL-23～30、RR-20261006-20 都不依赖外部环境（skill 全部在参考宿主与测试替身上验证，A4 ① 在新生成的 game-demo 与隔离环境上实跑），没有新增外部验证项。第一次重核（`e6828e4f`）补跑的：真实 Mongo 副本集上的 `TestRealMongoCoordinatorLeaseTakeover`（NONCORE-12）、Resume 代际持久（NONCORE-16，临时探针）、`TestRealSagaCrossProcessKillRecovers`（NONCORE-18）、Mongo Mod 在已断开的真实客户端上停止（NONCORE-53，临时探针）；mongo-driver 错误信息不带口令（NONCORE-49，临时探针）；nest `-shuffle` 有界 seed 扫描 260 次（NONCORE-54）；GitHub framework-compat 在 `e6828e4f` 上全部通过（CFG-12、NONCORE-31）。临时探针都没有入库。

## 仍待决定的事项

- **维护者决定项：无**。[DECISIONS-PENDING](../../review/DECISIONS-PENDING-2026-10-05.md) 第十三轮与本分册有关的决定（B10 两条管线选 A、NONCORE-46 选 A、skill 撤除重试、改名 Spawn、停止入口统一、改名 Summon 与 `dismiss`、两张表选 A、待停止上限选 B、原下个大版本项本版完成、配置声明形式选 A）全部已实施。
- **WANTED 未决数：0**。本部分原有的 W-2026-10-06-01（nest 无 Guard 作用域分支）已转 [RR-20261006-12](../../bug/RR-20261006-12.md) 并修复（`b7471ae4`，NONCORE-54）。
- **原“下个大版本”项本版已完成**：第十三轮维护者要求本版完成，A4 ① 每个 Mod 声明配置（CFG-14、15）、B3 ③ Host 取值能力表（SKILL-30）都已实施；本分册不再有“留到下个大版本”的项。
- **保持方向 B、不实现的语义**：NC-151 / NC-213 方向 A（phase 计时、recast、chain 间隔 / 重复、modifier 叠层），B3 ④“保持 B”，第十二轮“NC-151 timeout_ticks：保持 warning”；NC-224 方向 B（冻结施法输入）第五轮“不做”。
- **已定的设计边界**（不是缺陷、不是待验证项）：tablegen 与 cfggen 不合成一套（第十三轮维护者选 A，CFG-16）；tablegen 生成期不做 ref 数据检查（B10 决定规则由运行时加载层强制、生成期检查只作提前反馈，ref 只在加载时查）。
- **发版必做（打 tag 时，不是未闭环项）**：`codegen/internal/roost/manifest.go:83` 的 `minimumVersions.Core` 与 `.github/workflows/framework-compat.yml` 的 minimum 行升到 v1.23.0（CFG-14 / 15，交接规格“发版必做”）；作者文档 `docs/skill/skill-casting-and-combat.md:114`“（O22、O29，未发版）”与 `:187`“（O2，未发版）”两处标题随发版改为 v1.23.0。
- **Windows**：维护者第十三轮“windows的问题可以暂存，加一个说明 window问题不保证正确”（`7fec136e`）。本部分提到 Windows 的条目（NONCORE-27、28、31、33、34、36）一律按“暂存，不保证正确”理解。

## 文档与源码不一致（以源码为准）

1～6 在第一次重核（`e6828e4f`）时已处理：1～5 改的是源文档本身，6 已由 RR-20261005-01 记录末节闭环。7～9 是第二次重核（`5e72ca4d`）的结果，都不需要再改源文档。

1. A4 方案（`docs/feature/REFACTOR-2026-10-05-strict-config-reads.md`）写 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys` 为 16 / 95 / 77 个键；冻结提交上是 17 / 99 / 79，差额来自之后各批登记的新键（`service_metrics.enabled`；`ops.admin_timeout`、`remote_entity.cached_max_staleness`、`remote_entity.mirror.shutdown_timeout`、`remote_entity.snapshot_l2_tombstone_wait_timeout`；`remote_entity.snapshot_interest_per_consumer`、`remote_entity.snapshot_l2_tombstone_wait_replicas`）。**已改**为 17 / 99 / 79 并注明实施当时的数（CFG-2）。
2. B10 方案 §2.1 的 API 草图写 `rules.Lookup` 是“encoding/json 的键匹配”、§2.2 写规则字段“按 encoding/json 的键匹配”；v1.22.0 起源码是逐字匹配（`configdata/rules/rules.go:276-280`、`configdata/fieldrules.go:91`）。**已改**为逐字匹配并注明来自 CFG-10（CFG-7、CFG-10）。
3. 作者文档 `docs/skill/skill-casting-and-combat.md` 第 80 行（`5e72ca4d` 上为第 82 行）标题原写“（O33，未发版）”；O33 随 v1.21.0 发布（`f6043e44`）。**已改**为“（O33，v1.21.0）”。同文 O22 / O29 / O2 标“未发版”指 v1.23.0 本版内容，发版时一并改（SKILL-16）。
4. DECISIONS-PENDING 标注提交 `2a7d2a65` 的提交说明写“实施状态（a6e75488）”，`a6e75488` 是 rebase 前的提交号、不在 main 历史上；正确的是 `f6043e44`（表内一直写对）。提交历史不改，**已在** DECISIONS-PENDING 第七轮表下加更正注（SKILL-16）。
5. NC-220 / NC-224 的 bugfix 记录写修复位置在 `compile_snapshot.go`（`snapshotCapturableWhereRead` / `readsInsideProcessCallbacks`）与 `compile_owned_entity.go`（`validateDetachedProcessFields`）；同一版本（v1.21.0）内求值上下文表把它们收拢到表上，冻结提交上这三个函数已不存在（SKILL-14）。**已在**两份记录末尾加后注，指向现在的位置。
6. RR-20261005-01 的回归用例 `TestActivityRefusesACandidateListNoWindowCouldOpenWith` 随 C4 删除。`d5682dc4` 复核：承诺仍需要，回归改名为组文件形态的 `TestActivityRefusesAGroupNoWindowCouldOpenWith`，另加守卫 `TestAGroupFitsOneLiveQuery`；逐项对照见 NONCORE-20 与该记录末节。
7. 第十三轮各实施记录与 DECISIONS-PENDING 的实施状态栏、CHANGELOG 已有条目里用的是当时的名字（`process` / `skill.process.*` / `stop_pending_dropped` / `SpawnCommand` 等）；按两份改名记录 §4“历史记录不改”的约定保留，查新名用 [Spawn 对照](../../feature/REFACTOR-2026-10-06-skill-process-to-spawn.md) 与 [Summon 对照](../../feature/REFACTOR-2026-10-07-skill-summon-rename.md)。本分册正文已统一为新名，引用旧名处注“原名”。
8. A4 ② 方案（`REFACTOR-2026-10-05-strict-config-reads.md`）正文描述的三份登记表、`ConfigReader` 与正则守卫已被 A4 ① 删除；该方案第 46 行已有“2026-10-07 已实施 A4 ①、三份登记与 `ConfigReader` 删除”的注，CFG-1～5 按“当时的行为 + v1.23.0 起”的形式写。`codegen/internal/roost/manifest.go:67` 的注释里提到 `app.ConfigReader`，说的是 v1.20.2 抬 Core 下限的历史原因，不是现行代码。
9. 作者文档 `docs/skill/skill-casting-and-combat.md:114`“编译期收紧与诊断文案（O22、O29，未发版）”与 `:187`“属性投影（O2，未发版）”指本版内容，随发版改为 v1.23.0（见“仍待决定的事项”里的发版必做）。
