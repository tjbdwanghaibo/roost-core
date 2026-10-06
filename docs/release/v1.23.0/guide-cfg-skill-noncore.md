# v1.23.0 说明文档（分册）：配置、skill 与非核心 review（CFG / SKILL / NONCORE）

本分册是 v1.23.0 发版双文档的一部分，覆盖 `v1.19.2..02c8a10d` 里属于三个主题的全部改动：

- **CFG**：严格配置读取、配置数据规则统一（`configdata/rules`）、热更结果可见、键大小写敏感、cfggen globals 规则、生成配置与生成 TCP 报错。
- **SKILL**：skill 编译器与 Runtime 的收紧（N09 六批、B3、求值上下文表、O 系列观察）。
- **NONCORE**：非核心 review N01～N15 中不属于其他主题的修复与收尾。

配套的实现文档是 [impl-cfg-skill-noncore.md](impl-cfg-skill-noncore.md)，两份文档用同一编号（`CFG-n` / `SKILL-n` / `NONCORE-n`）做锚点，每条说明末尾的“实现”链接直达对应条目。APP、OWN、CLK、OPS、TOOL、SAGA、DRV、DAO、REM 主题在同目录的其他分册里，本分册只引用它们的主题名。

**怎么读**：先看下面的条目总表与“本部分总览”，再按需要跳到条目。每条按同一顺序写：结论 → 背景 → 维护者决定 → 现在的行为 → 兼容与迁移 → 限制 / 外部验证 → 链接。行号、符号一律以发版提交 `02c8a10d` 的源码为准；历史记录与源码不一致的地方在文末单列。

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
| [SKILL-1](#skill-1) | 施法失败只走一个终态入口（NC-110～112） | v1.20.1 | 收紧：终态 cast 的输入被拒；手动 Release 失败不可重试 | 否 |
| [SKILL-2](#skill-2) | Combatant 副本不共享 map（NC-113） | v1.20.1 | 改副本不再改实体 | 误用者改用 `InitCombatant` |
| [SKILL-3](#skill-3) | skillsync 三条下发路径同一可见性（NC-114 / 115） | v1.20.1 | wire 追加字段；可见性变化时 remove 不下发 | 可见性变化时重发快照 |
| [SKILL-4](#skill-4) | Applier 被拒包不改 epoch（NC-116） | v1.20.1 | 无 | 否 |
| [SKILL-5](#skill-5) | 提交前失败的 cast 有界回收（NC-117） | v1.20.1 | 无 | 否 |
| [SKILL-6](#skill-6) | wire 字段名逐字匹配（NC-150） | v1.20.2 | 收紧：非规范大小写 Parse 失败 | 修正技能 JSON |
| [SKILL-7](#skill-7) | 拒绝 Runtime 不派发的 phase 事件（NC-151） | v1.20.2 | 收紧；非零 `timeout_ticks` 给 warning | 修正定义；loadtest 断言 `Warnings == 0` |
| [SKILL-8](#skill-8) | 作者写的 tick 非负（NC-152） | v1.20.2 | 收紧 | 修正负值 |
| [SKILL-9](#skill-9) | 表现缓存等待者、skillcompose 诊断（NC-153 / 154） | v1.20.2 | 无 | 否 |
| [SKILL-10](#skill-10) | 编译器只接受 Runtime / Host 执行的范围（NC-210、212～215） | v1.20.2 | 收紧 | 修正定义与 catalog |
| [SKILL-11](#skill-11) | 移交后 area finish 只结束本进程（NC-211） | v1.20.2 | 放宽（不再报错） | 否 |
| [SKILL-12](#skill-12) | `NegotiateSchema` 拒绝空区间（NC-216） | v1.20.2 | 收紧 | 否（无生产调用方） |
| [SKILL-13](#skill-13) | lower fail-fast 与 phase 事件表单一来源（B3） | v1.20.2 | 正常定义不变；引入一处回归（v1.21.0 修） | 否 |
| [SKILL-14](#skill-14) | 按 Runtime 求值上下文收紧，修 B3 回归（NC-220～224） | v1.21.0 | 收紧；NC-223 恢复 | 修正定义；v1.20.1 的相关 checkpoint 先排空 |
| [SKILL-15](#skill-15) | 求值上下文表（第五轮，NC-280～283） | v1.21.0 | 收紧 + 放宽；诊断码变化 | 修正定义；按诊断码匹配的工具更新 |
| [SKILL-16](#skill-16) | O33 漂移格子编译期拒绝；O34～O36 写文档 | v1.21.0 | **破坏**：以前能编译的定义启动失败 | 按作者文档改写 |
| [SKILL-17](#skill-17) | summon 进程拒绝 `duration_ticks` 与 area 成员字段（O22） | v1.23.0（本版） | 收紧 | 删掉这些字段 |
| [SKILL-18](#skill-18) | checkpoint 字节确定（O7） | v1.23.0（本版） | 字节变、格式不变 | 不要跨版本比较字节 |
| [SKILL-19](#skill-19) | result 分支诊断文案（O29）；O15～O17 / O27 / O28 写文档 | v1.23.0（本版） | 只改文案 | 按文案匹配的工具更新 |
| [SKILL-20](#skill-20) | null 默认值实体状态可以 set（RR-20261006-02） | v1.23.0（本版） | 放宽；事件 Before 的缺省值类型变化 | 否 |
| [SKILL-21](#skill-21) | checkpoint 恢复拒绝 `phase_timeout`（RR-20261006-03） | v1.23.0（本版） | 只拒绝不会出现的任务 | 否 |
| [SKILL-22](#skill-22) | 示例 `statusbridge` 能运行 | v1.23.0（本版） | 只改示例 | 否 |
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
| [NONCORE-20](#noncore-20) | `activity.game_sids` 启动校验（RR-20261005-01） | v1.20.1（v1.20.2 被 C4 取代） | 已取代 | 否 |
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
| [NONCORE-46](#noncore-46) | N13 遍历与 TaskPool 等（NC-180～185） | v1.20.2 | 行为变化 | 否 |
| [NONCORE-47](#noncore-47) | 遍历回调仓库级契约（C7） | v1.20.2 | 契约成文 | 否 |
| [NONCORE-48](#noncore-48) | container / goroutine 零调用方 API（NC-267～269） | v1.21.0 | 无 | 否 |
| [NONCORE-49](#noncore-49) | Mongo URI 口令脱敏（NC-191） | v1.20.2 | 无 | 否 |
| [NONCORE-50](#noncore-50) | 启动失败先收回 Service（NC-193） | v1.20.2 | 契约补充 | `Shutdown` 须容忍部分初始化 |
| [NONCORE-51](#noncore-51) | N15 脚本与门禁（NC-200～208）、A5 | v1.20.2 | 收紧：glsvet 对没检查到的输入退出 2 | 已有工程 `.gitignore` 补 `/data/wal/` |
| [NONCORE-52](#noncore-52) | 停机三步（NC-170～174） | v1.20.2 | 收紧 | 否 |
| [NONCORE-53](#noncore-53) | Mongo Mod 停止收敛（NC-260） | v1.21.0 | 重复 Close 返回 nil | 否 |
| [NONCORE-54](#noncore-54) | nest 用例隔离（A2 / A3）；W-2026-10-06-01 | v1.23.0（本版） | 只改测试 | 否 |
| [NONCORE-55](#noncore-55) | Nest 重排抖动（U-0279） | v1.20.1 | 平均重排延迟 5ms → 7.5ms | 否 |
| [NONCORE-56](#noncore-56) | 租约修复 RR-20261004-10 / 11 / 14（同版被静态绑定取代） | v1.20.0 | 代码已删 | 否 |

共 91 条：CFG 13 条、SKILL 22 条、NONCORE 56 条。

## 本部分总览

### 版本时间线

| 版本 | tag 指向 | 日期 | 本部分首发的条目 |
| --- | --- | --- | --- |
| v1.20.0 | `999dc672` | 2026-10-05 | NONCORE-9、10、13、14、27、28（主体）、32、56 |
| v1.20.1 | `be7407ab` | 2026-10-05 | CFG-6；SKILL-1～5；NONCORE-2、3、4、6、7、11、12（NC-102）、16～20、25、26、28（补修）、29、30、34、39、55 |
| v1.20.2 | `c85d4565` | 2026-10-06 | CFG-1～5；SKILL-6～13；NONCORE-15、35、42、43、46、47、49～52 |
| v1.21.0 | `4881f2b7` | 2026-10-06 | CFG-7、8、9；SKILL-14、15、16；NONCORE-1、21、22、23、31、36、37、40、41、44、48、53 |
| v1.22.0 | `9bf690fb` | 2026-10-06 | CFG-10 |
| v1.23.0（本版） | 发版提交 | — | CFG-11、12、13；SKILL-17～22；NONCORE-5、8、12（RR-20261006-08）、24、33、38、45、54 |

首发版本用 `git tag --contains <提交>` 取最早的 tag 核对；v1.22.0 之后的提交都随 v1.23.0 发布。

### 主题地图

```text
CFG   严格读取： CFG-1 → CFG-2 → CFG-3 / CFG-4 ；CFG-5（同批）
      数据规则： CFG-6 → CFG-7（B10）+ CFG-8（C2）→ CFG-9（被取代）→ CFG-10（大小写）→ CFG-11（globals）
      生成配置： CFG-12、CFG-13
SKILL 终态与同步：SKILL-1～5、SKILL-9、SKILL-12
      编译期收紧：SKILL-6～8、SKILL-10、SKILL-11 → SKILL-13（B3）→ SKILL-14 → SKILL-15（表）→ SKILL-16（O33）→ SKILL-17～19
      收尾小修：SKILL-20、21、22
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
| SKILL-6～8、10、14～17 | 以前能编译的 skill 定义在游戏服启动期 `CompileAll` 失败 | 按错误路径与作者文档改写；仓内 fixture、示例与生成骨架都不受影响 |
| NONCORE-35、37 | `ActionRunner` / `MissionRunner` 回调里的变更不再立即生效、不再返回 `ErrReentrantMutation` | 按延后语义写：回调里返回 nil，执行错误经 OnError；要清场先 `EndCurMission` 再 `EndAll` |

**收紧**（写错的配置 / 用法从“静默读错”变为报错）：CFG-1～4（类型错误的配置）、CFG-6（悬空 ref）、SKILL-1（终态 cast 的输入）、NONCORE-1（Ops 端口被占）、NONCORE-6（JetStream 截获的轻量 RPC）、NONCORE-17（缺摘要的旧 saga 记录重投）、NONCORE-25（非字面量 errcode）、NONCORE-42（failurelog 结果未知）、NONCORE-43（loadtest 无样本）、NONCORE-51（glsvet 输入不存在）。

**生成器 Core 下限**：v1.20.2（CFG-4 生成代码用 `app.ConfigReader`），v1.21.0（CFG-7 生成 loader 用 `configdata.FieldRule`）。v1.23.0 不再上调（CFG-12 的新键在旧 kit 上被 viper 忽略）。**已生成工程一律不迁移**（维护者决定），模板变化在 `roost generate` / `roost project sync` 后生效。

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
11. skill checkpoint：不要拿新旧版本各自写出的字节互相比对（SKILL-18）；v1.20.1 上保存了“以局部变量为实体的读取”类技能的 checkpoint，升级前排空（SKILL-14）。
12. 同一目录不要有两个在写的 skillsync 文件 outbox（NONCORE-33）。

## CFG：配置读取、配置数据规则与生成配置

本主题回答三个问题：框架读配置时写错类型会怎样（CFG-1～CFG-5）；配置数据（`configs/data/*.json`）的规则在哪一层检查、热更失败能不能看见（CFG-6～CFG-11）；生成工程的配置与生成 TCP 的报错（CFG-12、CFG-13）。

主题内的先后关系：

```text
NC-190（布尔 / 时长）──► A4 严格读取（app + kit；同批 C1 生产校验只查有读取方的键）──► A4 留项：kit/redis、生成代码
NC-75（tablegen ref + -check）──► B10 规则统一到 configdata/rules（运行时强制）+ C2 热更可见
                                   └► 发版前审查：大小写变体取最后一个（v1.21.0）──► 大小写敏感（v1.22.0，取代前者）
                                   └► cfggen globals 规则（v1.23.0）
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
| 其他读取点 | `kit/mods.Duration` / `RequiredDuration`、saga 步骤预算时长改用 `app.ConfigDuration` |
| `redis.cluster_addrs` | `kit/mods.RedisClusterAddrs` 接受逗号串或 YAML 列表并去空白 |

错误示例（来自修前红用例的断言）：`ValidateServiceConfig = <nil>; want an error naming singleton.enabled`——修后返回点名该键的错误。

**兼容与迁移**：行为收紧。生成的配置都写 `true` / `false` 与带单位的时长，生成 game-demo 20 份配置逐份通过。升级前用新版本启动一次即可发现写错的键。

**限制 / 外部验证**：真实 Redis Cluster 上用 YAML 列表写 `cluster_addrs` 起服未验证（本机无 Cluster），见外部验证 E08。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-190.md) · [问题](../../bug/RR-20261005-NC-190.md)

<a id="cfg-2"></a>
### CFG-2 框架配置一律严格读取，启动校验覆盖全部类型化的键（A4）

**结论**：app 与 kit 里的布尔、时长、整数配置全部改为严格读取；`ValidateServiceConfig` 在任何 Mod Init 之前按三份登记表检查，一次报全。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-2)

**背景**：配置没有 schema，靠宽松 getter 加事后清单校验（RR-20260926-12、NC-190、NC-192、N02 O2）。NC-190 只修了开关与少数时长，kit 里还有约 80 处宽松读取：`nest.worker_num: 8k` 读成 0（取默认）、`dataengine.wal.queue_capacity: 1.5` 被截断等。

**维护者决定**（[DECISIONS-PENDING 第二轮](../../review/DECISIONS-PENDING-2026-10-05.md)，A4 行原文）：“按推荐：先 ②（逐个改严格读取），① schema 作为后续重构”。可选方案是 ① 每个 Mod 声明配置 schema、校验 / 生成器 / doctor 共用；② 逐个 Mod 改用严格读取。选 ② 的理由：改动可以分批做、立刻堵住静默读错；① 涉及全部 Mod 的声明形态，列为下个大版本。

**现在的行为**：

- 新增 `app.ConfigInt` / `ConfigInt64`（接受 YAML 整数、没有小数部分的浮点数如 `1e3`、十进制字符串；拒绝小数、带后缀、时长、布尔值）与汇总错误的 `app.ConfigReader`（Mod Init 一次读几十个键，读完统一 `Err()`，运维一次看到全部写错的键）。
- `ValidateServiceConfig` 按 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys` 逐个严格读取（发版提交上分别为 17 / 99 / 79 个键，另有 syncbus 三段 `syncbus` / `room` / `sync` 的同名字段与按后缀登记的 `<service>.call_timeout`）。A4 实施时是 16 / 95 / 77 个，之后 C6、`ops.admin_timeout`、B2 / O4 / Mirror 第 5 步 / O-M6-3 各自登记了新键（见实现文档）。
- kit 的 dataengine、remoteentity、saga、nats、nest、syncbus、mongo、ops、etcd、statslog、platform、activity 与 app 自身改用 `ConfigReader`，错误前缀沿用各 Mod 原有的（如 `dataengine mod: …`）。
- 唯一保留的宽松读取是 `sid`（启动校验先严格检查它是 int32 范围内的正整数）。

**兼容与迁移**：行为收紧。`true` / `false` / `1` / `0` / `"true"`、带单位的时长、整数与十进制字符串照常接受。以前被静默读错的写法现在启动失败。删除了 `remote_entity.sync_retry_queue_cap` 的非负检查（没有任何代码读它）。

**限制**：配置 schema（A4 ①）留到下个大版本；三份登记表加源码扫描守卫是过渡形态，登记集中在 app。

**链接**：[A4 方案](../../feature/REFACTOR-2026-10-05-strict-config-reads.md) · [验证记录](../../bugfix/RR-20261005-NC-192.md)（“A4 严格读取的验证”一节）

<a id="cfg-3"></a>
### CFG-3 A4 留项：kit/redis 的三个整数键严格读取

**结论**：`redis.db` / `redis.pool_size` / `redis.min_idle_conns` 由 Redis Mod 与单实例锁的 `SingletonStore` 严格读取，`8k`、`1.5`、`10s` 在 Init 时点名报错。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-3)

**背景**：A4 实施时 kit/redis 由 A2 的 agent 在改，这三处 `GetInt` 留作例外，由启动校验兜住，守卫按“文件 + 键”放行。

**决定**：A4 留项，第二轮决定的延续（DECISIONS-PENDING A4 行“未做”一栏划掉、写明 `5df60765`）。

**现在的行为**：未设置或 ≤ 0 的 `pool_size` / `min_idle_conns` 仍取驱动默认值；错误形如 `redis mod: config: redis.db …`。守卫删掉了这三项放行。

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

**链接**：[A4 方案“A4 留项：生成进工程的代码”](../../feature/REFACTOR-2026-10-05-strict-config-reads.md) · 提交 `914a725f`

<a id="cfg-5"></a>
### CFG-5 `env: production` 只校验有读取方的设置（C1 / RR-20261005-NC-192）

**结论**：生产校验删掉九组没有任何代码读取的要求，生成的生产示例与 k8s Secret 示例打开 `env: production` 可以启动。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#cfg-5)

**背景**：`validateProductionServiceConfig` 按服务名要求 `player.login_auth_required`、`player.login_secret`、`player_protocol.rate_limit.enabled`、`save_load.wal.*`、`instance.*`、`account.ops_token`、`account.redis_required`、`global` / `match_group.redis_required` 为真。这些键来自开源初版的服务形态，全仓没有读取方。生成的生产配置不写它们，打开 `env: production` 后 game / account / global 起不来；写上又让人以为限流、鉴权、WAL 已经打开。

**维护者决定**（DECISIONS-PENDING 第二轮 C1 行原文）：“随 A4 按方案 1”。方案 1 = 删掉无效要求并写明校验范围，之后接入真实限流 / 鉴权开关时再加回。未采用：方案 2（换成真实生效的键，需要先在生成接入层装配按请求限流与鉴权）、方案 3（只改措辞，误导仍在）。

**现在的行为**：保留 ops 端点不绑公网（或声明 `ops.allow_public_addr`）、game / instance / account / match_group / global 必须写 `redis.addr`、account / platform 的密钥不是空的或 `dev-` 开头、admin_gateway 令牌。另有 CLK 主题的 `time.logic_offset` 生产必须为 0（见 CLK 部分）。USER_GUIDE §10 写明校验范围。

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

**维护者决定**（DECISIONS-PENDING 第四轮 B10 行原文）：“按推荐：configdata 新增能看到原始字段是否出现的接口，必填在加载 / 热更时检查（A）；规则统一由运行时加载层强制，生成期检查只作提前反馈。维护者附加原则：**config 使用要容易，手写整理代码尽量少，结构简单易懂**”。未采用方案 B（生成的校验器重读原始 JSON：有替换窗口、每表 25～30 行胶水）。tablegen 与 cfggen 两种标签方言合一评估后列为后续（改写所有已有工程的 schema，收益只是少一个解析函数）。

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

**链接**：[B10 / C2 方案与实施](../../feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) · [NC-75 后续](../../bugfix/RR-20261005-NC-75.md)

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

**限制**：生成的 game-demo 没有运维 Rollback 入口，也没有会失败的 AfterApply 监听者，`stage=apply` 撤回与运维 Rollback 只由单测覆盖，未在真实进程里触发。

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

> 其他分册对应：REM-13 写了同一项（按分工本条为主）。

**结论**：新生成工程的开发配置、生产示例与 k8s Secret 示例带上 B2 / O4 / O-M6-3 / Mirror 第 5 步新增的五个 `remote_entity` 键，取值等于 core / kit 缺省，并附中文注释。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#cfg-12)

**背景**：这些键只在 USER_GUIDE 里，运维要去文档里找键名才知道能调。

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

与其他主题的边界：**skill Runtime 状态不进事务（B4）** 与 **buff 属性投影 `CombatComponent.ProjectAttributes`（N09 O2）** 在 DAO 部分；本部分只写示例 `statusbridge` 的修复（SKILL-22）。文件 outbox 遗留临时文件（RR-20261006-04）在 NONCORE（见 NONCORE-33）。

**对业务作者最重要的一句**：v1.20.2 与 v1.21.0 两次收紧了编译器，以前能编译的定义可能在游戏服启动期 `CompileAll` 失败（错误带 JSON 路径与诊断码）。升级前用新版本跑一次 CompileAll；按[作者文档](../../skill/skill-casting-and-combat.md)“引用在哪里能读”的改写对照修改。所有收紧都只拒绝“以前要么每次施法失败、要么静默不执行、要么结果随机”的定义；正常定义的 gameplay digest 不变。

<a id="skill-1"></a>
### SKILL-1 施法失败只走一个终态入口（RR-20261005-NC-110～112）

**结论**：启动失败、Cancel / Interrupt / Release 出错、排程失败都经 `failCastLocked` 收尾：撤掉本 cast 的全部排程、停进程、释放 policy 槽位。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-1)

**背景**：8 个终止点各自手写收尾，失败路径漏掉的步骤各不相同：启动失败不撤任务却复用 ID（旧任务落到下一个拿到同一 ID 的 cast 上，失败启动后的 checkpoint 也恢复不了，NC-110）；回调出错直接返回，cast 停在半终止、施法者永久 `ErrCasterBusy`（NC-111）；排程失败不释放 policy 槽位，下一次激活对失败 cast 执行 toggle-off（NC-112）。来源：[N09 第一批](../../review/REVIEW-2026-10-05-n09-batch1.md)。

**决定**：bugfix。未采用“启动失败不复用 ID”：ID 进入随机键派生，改复用规则会改变所有后续 cast 的随机序列与回放。

**现在的行为**：

- 对 failed / finished / 已取消等终态 cast 调 `Cancel` / `Interrupt` / `Release` 返回 `ErrCastInputRejected`（之前会执行它的回调）。
- 手动 `Release` 在 charge 重新进施法窗口后付费失败：cast 进 failed，不能再重试（与 auto release 一致；之前停在 preparing、占住施法者）。
- 排程失败的 toggle / hold / charge 释放槽位，下一次激活开始新 cast。

**兼容**：行为收紧（见上）。checkpoint 格式与 wire 不变，只是少了失效任务。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-110.md) · [NC-111](../../bugfix/RR-20261005-NC-111.md) · [NC-112](../../bugfix/RR-20261005-NC-112.md)

<a id="skill-2"></a>
### SKILL-2 combatcomponent 的 Combatant 副本不再共享 map（RR-20261005-NC-113）

**结论**：`Combatant()` 返回、`InitCombatant` 存入的 `ElementMultipliersBP` 都是拷贝。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-2)

**背景**：`combat.Combatant` 唯一的引用类型字段是这张 map，值拷贝与直接赋值都共享它；改副本或改共用的配置模板会在事务、逆操作与脏标记之外改掉权威战斗状态。

**兼容**：公开 API、BSON / JSON 不变；依赖“改副本就能改实体”的误用不再生效，应改用 `InitCombatant`。每次 `Combatant()` 多一次小 map 分配。

**链接**：[bugfix 记录](../../bugfix/RR-20261005-NC-113.md)

<a id="skill-3"></a>
### SKILL-3 skillsync 三条下发路径共用同一可见性规则（RR-20261005-NC-114 / NC-115）

**结论**：presentation reset、state 快照、增量三条路径都按同一 `VisibilityPolicy` 过滤；cast / process remove 带上归属实体。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#skill-3)

**背景**：可见性在快照、增量、presentation reset 三处各写一套：reset 完全不过滤，不可见施法者的持续表现、目标与坐标发给所有 observer（NC-114）；ability 快照按空 handle、增量按具体 handle 问 `FieldVisible`；cast / process remove 不带归属实体，persistent remove 不看 `Binding`（NC-115）。来源：[N09 第二批](../../review/REVIEW-2026-10-05-n09-batch2.md)。

**决定**：bugfix。reset 复用现有 `FilterPresentation`（自定义策略无需改动）；未采用“在 `VisibilityPolicy` 接口上加 `FilterPresentationReset`”（破坏所有自定义策略的编译、同一规则写两遍）。

**现在的行为**：

- wire：`StateMutation` 的 cast / process remove 多出 `caster` / `owner` 字段（`omitempty`，只追加；旧客户端不读）。
- 行为收紧：observer 可见性在 upsert 与 remove 之间变化时以 remove 时为准，“先可见后不可见”的实体其 remove 不再下发——**业务应在可见性变化时重发快照**。
- presentation reset 可能因策略报错而失败（fail-closed，与 state 快照一致）。

**限制**：没有接 kit syncstream / NATS 端到端（外部验证随 E04）。自定义策略若依赖 `PresentationEvent.Sequence`，reset 里的还原事件取 `LatestPresentationSequence`。

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

**兼容**：checkpoint 格式不变。旧版本写出的、已超上限的 checkpoint 在旧版本同样恢复不了，修复不自动迁移。

**限制**：只用 MemoryHost；生产 Host 的 checkpoint 与世界成对恢复没有在真实持久化上演练。

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

**限制**：没有接真实客户端资源加载器；skillcompose 仓内无正式调用方。

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
### SKILL-11 移交后的 area 回调 `finish` 只结束本 area 进程（RR-20261005-NC-211）

**结论**：施法先结束、spawn 出的 area 进程已移交时，回调里的 `finish` 只停止本 area 进程；施法仍存活时照旧结束施法。首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#skill-11)

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

**维护者决定**（DECISIONS-PENDING 第二轮 B3 行原文）：“lower 查找失败一律报错：做”；实施状态栏：“① …… ② phase 事件派发表单一来源（代价小，一并做）。③ 下个大版本，④ 保持 B”。待决定表里的选项：① lower fail-fast ② 事件派发表单一来源 ③ Host 取值约束做成随环境下发的能力表（改环境格式与 authority digest）④ NC-151 / NC-213 走方向 A 还是保持 B。

**现在的行为**：类型检查之外的第二道防线；正常定义的输出与 gameplay / presentation digest 不变。产物不完整、IR 形状未知等编译器自身不变量仍 panic（不是名字查找）。

**兼容**：对正常定义无变化。**v1.20.2 引入了一处回归**：以局部变量为实体的非缓存型 `read_attribute` 编译失败（`LOWER_UNRESOLVED at $`），v1.21.0 修复（NC-223，见 SKILL-14）。

**链接**：[B3 方案与实施](../../feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md)

<a id="skill-14"></a>
### SKILL-14 编译器按 Runtime 求值上下文收紧，修复 B3 回归（RR-20261005-NC-220～224）

**结论**：缓存型快照点、被动、`process` / `on` 的位置、进程字段里的施法引用在编译期按 Runtime 的实际求值上下文检查；B3 引入的局部变量 `read_attribute` 回归修复。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#skill-14)

**背景**（[N09 第五批](../../review/REVIEW-2026-10-06-n09-batch5.md)）：

| 编号 | 问题 | 现在 |
| --- | --- | --- |
| NC-220 | cast_start / phase_start / process_start 的实体读 `$local.*`（采样点上还没有局部变量）；process_start 写在 spawn 进程回调以外 | `ATTRIBUTE_SNAPSHOT_INVALID` |
| NC-221 | 被动 `proc_policy.max_depth: 0` 永不触发；被动的 `input_schema` 为 position / direction 永远填不上 | `SHAPE_INVALID` / `INPUT_UNAVAILABLE at $.input_schema` |
| NC-222 | 非 spawn 效果上的 `process` / `on` 被静默丢弃 | `SHAPE_INVALID` |
| NC-223 | B3 回归：以局部变量为实体的 current / each_tick / on_hit / on_event 读取编译失败 | 恢复可编译，digest 与 v1.20.1 相同 |
| NC-224 | spawn 进程每一步重新求值的字段读 `$input` / `$memory` / `$local`，移交后 `ErrProgramInvariant` | `INPUT_UNAVAILABLE`（方向 A：编译期拒绝） |

**维护者决定**（DECISIONS-PENDING 第五轮）：“NC-224 方向 B：不做：维持编译期拒绝（进程启动时不冻结施法输入）”。方向 B 是“进程启动时把施法输入冻结进 `ProcessInstance`”，可以让 path 投射物（`points: $input.path`）真正可用，但属于语义扩展。

**兼容**：行为收紧；NC-223 是恢复。若有部署在 v1.20.1 上保存了含 NC-223 那类技能的 checkpoint，升级后这些 cast 的 Program 因 digest 不同查不到，需要先排空。

**链接**：[第五批记录](../../review/REVIEW-2026-10-06-n09-batch5.md) · [NC-220](../../bugfix/RR-20261005-NC-220.md) · [NC-221](../../bugfix/RR-20261005-NC-221.md) · [NC-222](../../bugfix/RR-20261005-NC-222.md) · [NC-223](../../bugfix/RR-20261005-NC-223.md) · [NC-224](../../bugfix/RR-20261005-NC-224.md)

<a id="skill-15"></a>
### SKILL-15 求值上下文表：编译期与 Runtime 共用“每个上下文能读哪些引用”（第五轮，含 NC-280～283）

**结论**：`skill/eval_contexts.go` 列出 8 个求值上下文 × 24 类引用（另有 3 个快照点的位置表）；类型检查的作用域由表生成，Runtime 求值查同一张表，表外引用返回新的 `skill.ErrReferenceOutOfContext`（点名上下文与表项）。做表时发现并修复 NC-280～283。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#skill-15)

**背景**：编译期只有一套作用域（施法作用域 + 回调作用域），Runtime 至少有五种求值上下文，二者之间没有对照表；第五批之前同一形态已出五次（NC-211、NC-220、NC-221、NC-223、NC-224）。

**维护者决定**（DECISIONS-PENDING 第五轮“skill 求值上下文表”行原文）：“做：一张表写明每种求值上下文（施法流程 / cast_start / phase_start / 进程启动 / 移交后每步）可用的引用，编译期与 Runtime 都查这一张（N09 第五批方向判断）”。

**现在的行为**：

- 8 个上下文：施法流程（cast_flow）、memory 默认值、cast_start 采样、phase_start 采样、process_start 采样、进程每一步（process_step）、进程回调（process_callback）、状态默认值（state_default）。比决定列的五种多三个，审表时发现它们是独立求值点。完整表见[方案 §3](../../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md)。
- 新拒绝：memory 默认值读另一个 memory（NC-280，此前编译结果随 map 顺序）；状态默认值读 `$input` / `$memory`（NC-281）；cast_start / phase_start 读取可缺省的实体（NC-282）。
- 新放行：`$primary_target.position`、`$lifecycle_entity.position`、`$event.*.position`、`$input.target.position` 等投射（NC-283，此前每次求值失败或 B3 起编译失败）。
- 诊断码变化：进程回调里的施法引用由 `REFERENCE_UNKNOWN` 改为 `INPUT_UNAVAILABLE`（点名表项）；回调里的 cast_start / phase_start 读取改报 `ATTRIBUTE_SNAPSHOT_INVALID`（拒绝集合不变）。

**兼容**：既有定义 digest 不变（`row` 不进 digest）；不改线格式、checkpoint、生成形状。按诊断码字符串匹配的工具要更新。

**链接**：[方案](../../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md) · [第六批记录](../../review/REVIEW-2026-10-06-n09-batch6.md) · [NC-280](../../bugfix/RR-20261005-NC-280.md) · [NC-281](../../bugfix/RR-20261005-NC-281.md) · [NC-282](../../bugfix/RR-20261005-NC-282.md) · [NC-283](../../bugfix/RR-20261005-NC-283.md)

<a id="skill-16"></a>
### SKILL-16 漂移格子编译期拒绝（O33）；O34～O36 写进作者文档

**结论**：进程字段与状态默认值里会随移交 / 读写位置变值的施法引用（`$primary_target`、`$ability.self`、`$cast.*` 除 `$cast.mode`、cast_start / phase_start 读取），以及 memory 默认值里的 phase_start 读取，共 21 格从“可用但漂移”改为编译期拒绝，诊断给出替代写法。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#skill-16)

**背景**：第五批 O33：这些写法能编译、运行不报错，但主目标移交后变成 lifecycle 实体、施法状态变零值、快照退化为 current。求值上下文表起初把它们标成第三种格子“漂移”，保持现状、写明语义。

**维护者决定**（DECISIONS-PENDING 第七轮 O33 行原文）：“漂移格子改为编译期拒绝：进程字段 / 状态默认值里使用会漂移的施法引用（`$primary_target`、`$cast.*`、`$ability.self`、进程字段里的 cast_start / phase_start 读取等）直接报错，提示改用进程自己的引用”；O34～O36：“按推荐：保持现状并写进作者文档”。memory 默认值的 phase_start 由实施者判断“不合理，拒绝”（名字承诺“phase 开始时的值”，实际是 Activate 时的值，与 cast_start 相同）。

**现在的行为**：诊断示例（节选）：`reference "$primary_target" is not available in evaluation context process_step: …；改用施法流程里求一次的 spawn position（如 $input.target.position）…（evaluation context table row $primary_target)`。快照诊断多了 `row <point>`。O34（costs / windup 里的 phase_start 取求值那一刻的值）、O35（进程回调里 `self_ability` 过滤比较 handle 0）、O36（进程回调里 `$caster` 编译期拒绝，改写 `$owner`）保持行为、写进作者文档。

**兼容与迁移**：**新拒绝**，以前能编译的这类定义启动期失败，按[作者文档改写对照](../../skill/skill-casting-and-combat.md)修改；接受集合其余部分与 gameplay digest 不变。仓内 fixture、示例、`roost add skill` 骨架、game-demo 的 `fireball.json.tmpl` 都不用漂移格子。

**文档与源码差异**：作者文档该节标题仍写“（O33，未发版）”，实际随 v1.21.0 发布（`f6043e44` 是 `v1.21.0` 的祖先）。另：DECISIONS-PENDING 的标注提交 `2a7d2a65` 的提交说明写“实施状态（a6e75488）”，`a6e75488` 是 rebase 前的提交号、不在 main 历史上，正确的是 `f6043e44`（DECISIONS-PENDING 表内已写对）。

**链接**：[方案 §8](../../feature/SKILL-EVAL-CONTEXT-TABLE-2026-10-06.md) · [作者文档](../../skill/skill-casting-and-combat.md)

<a id="skill-17"></a>
### SKILL-17 summon 进程不再接受 `duration_ticks` 与 area 成员字段（O22）

**结论**：summon 进程上写非 0 的 `duration_ticks`、或 `area` / `interval_ticks` / `emit_leave_on_stop`，编译期报 `MOTION_INVALID`。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-17)

**背景**：summon 的寿命一直是 spawn 效果的 `duration_ticks`，进程自己的 `duration_ticks` 从不被读（负数也能编译）；写了 `area` 的 summon 编译通过、启动即 `ErrProgramInvariant`（同一处提前返回造成）。

**维护者决定**（DECISIONS-PENDING 第十二轮“skill 剩余观察”行原文）：“O22 编译期拒绝；O7 排序；O29 改文案；O15/O16/O17/O27/O28 保持并写作者文档；其余保持”。

**兼容与迁移**：已有定义在 summon 进程上写了这些字段的，升级后编译失败；删掉即可，行为不变（area 那种本来运行即失败）。

**链接**：[第十二轮记录 §1.1](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)

<a id="skill-18"></a>
### SKILL-18 同一状态的 checkpoint 字节确定（O7）

**结论**：`ActivePolicies`、`ProcLedger`、`RootEventCounts`、`AbilityByProgram` 改为按键排序写出；格式与版本不变，新旧版本写出的 checkpoint 双向可读。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-18)

**背景**：这四个列表按 map 迭代顺序写出，两次 Checkpoint 同一状态的字节与 Checksum 不同。

**决定**：同 SKILL-17（第十二轮 O7 排序）。

**兼容**：恢复本来与列表顺序无关；依赖“同一状态两次 checkpoint 字节相同”的比对从本版起成立，但**不能拿新旧版本各自写出的字节互相比对**。

**链接**：[第十二轮记录 §1.2](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)

<a id="skill-19"></a>
### SKILL-19 result 分支诊断文案（O29）；O15 / O16 / O17 / O27 / O28 写进作者文档

**结论**：effect result 分支与 status 实例消费流程的诊断改为 “cannot suspend (wait, repeat with interval_ticks) or start a process with on callbacks”（规则不变，spawn 加不带回调的进程一直可以）；五条既定语义写进作者文档。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-19)

**写进作者文档的五条**（行为不变）：area 的 `$event.enter_count` 恒为 1（O15）；`max_reflects: N` 是碰撞预算、实际反弹 N−1 次，`max_pierces` 同理（O16）；Host 拿到的 numeric 快照里未绑定字段是启动时的值、运动每步重新求值（O17）；restore 的 `on_blocked` 与 profile 策略不同时必然 `policy_rejected`（O27）；process_start 读取的实体按启动事件求值（O28）。

**兼容**：只改诊断文案；按旧文案字符串匹配的工具要更新。

**链接**：[作者文档“进程、运动与 temporal 的既定语义”](../../skill/skill-casting-and-combat.md) · [第十二轮记录 §1.3～1.4](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md)

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

**兼容**：`RuntimeCheckpointVersion` 不变。`git log -S '&phaseTimeoutTask{'` 显示自 V1.9（`67c9df54`）起除恢复外没有任何代码构造这个任务，已发布版本的 checkpoint 不含它。

**链接**：[bugfix 记录](../../bugfix/RR-20261006-03.md)

<a id="skill-22"></a>
### SKILL-22 示例 `skill/examples/statusbridge` 能运行了

**结论**：A1（回滚统一走 DAO）之后，战斗组件的改动必须在事务里，这个示例一运行即 panic（`persistence mutation outside transaction`），而 build / vet / 测试全绿。第十二轮把整段演示包进 `nest.RunDetachedTransaction`，并改用 `ProjectAttributes`（删掉手写的 `syncArmor`）。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#skill-22)

**背景与后续**：示例目录是独立模块，根模块的 `go build ./...` 不包含它。发版前验证为此加了根包门禁 `TestExamplesRun`（TOOL 部分），roost-coding 也加了“示例要实跑，不能只编译”。

**兼容**：只改示例。

**链接**：[第十二轮记录 §1.5](../../feature/ROUND12-SKILL-CFGGEN-2026-10-06.md) · [发版前验证记录](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md)

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
| pretag / source-head-check / 冲突标记门禁 / 示例实跑门禁 / mirror-local.sh | TOOL |

**边界项**：下面几条与上表主题相邻，本部分完整写出，并在[汇总文件](_summary-cfg-skill-noncore.md)里列为“边界项”，供汇总者去重：NONCORE-1（N01 留项，近 APP / OPS）、NONCORE-15（NC-130 / 131，近 REM）、NONCORE-23（NC-250，近 SAGA）、NONCORE-32（DAO nocoll，近 DAO）、NONCORE-40（D-L1 / D-L2 定时器，近 CLK）、NONCORE-50（NC-193，近 APP）、NONCORE-51（N15 脚本，近 TOOL）、NONCORE-52（NC-170～174，近 APP）、NONCORE-53（NC-260，近 DRV）、NONCORE-55（U-0279，核心线修复）、NONCORE-56（已被静态绑定取代的租约修复，近 OWN）。

**对业务 / 运维最重要的几条**：

- actionflow 回调语义变了（NONCORE-35、NONCORE-37）：`ActionRunner` / `MissionRunner` 回调里的变更延后到回调返回后执行，不再返回 `ErrReentrantMutation`。**破坏性**。
- 遍历回调成为仓库级契约（NONCORE-47）：框架交出的 `Range` 回调里可以读写同一容器、返回 false 立即停止。
- account 换名建角会释放未 admitted 的计划（NONCORE-22）；activity 坏窗口条目有了运维修复入口（NONCORE-21）。
- 生成工程需要手工处理的：`.gitignore` 补 `/data/wal/`（NONCORE-51，NC-206）；已生成工程不自动迁移，其余模板变化 `roost project sync` 后生效。
- roost CLI 中断（Ctrl-C）会先回滚再以同一信号退出（NONCORE-31）；codegen 联网用例需 `ROOST_NETWORK_TESTS=1`。

### N01 app / lifecycle / manager / health / admin

<a id="noncore-1"></a>
#### NONCORE-1 N01 留项：Ops 同步 bind、停机 hook 受预算、停机期 fail-stop 非零退出、Redis / remote_entity Mod 停止收尾、`ops.admin_timeout`（RR-20261005-NC-230～234）

> 其他分册对应：APP-4（NC-232）、APP-7（NC-233 / 234）、APP-8（NC-231）、OPS-3（NC-230 与 `ops.admin_timeout`），以那里为准。

**结论**：五个停机 / 启动边角与 admin 命令期限，首发 v1.21.0（`2c1c7be7`）。边界项（近 APP / OPS）。[实现](impl-cfg-skill-noncore.md#noncore-1)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-230 | Ops 端口被占时 bind 在后台 goroutine 里失败只记日志，进程在没有 `/healthz`、`/readyz` 的情况下继续跑；同机部署的探针可能探到占着端口的另一个进程 | `OpsMod.Start` 同步 `net.Listen`，失败返回 `ops: listen on ops.addr …`，按启动失败收尾。**同机多实例须各配 `ops.addr`** |
| NC-231 | 一个忽略 ctx 的 `service.stopping` / `service.stopped` hook 让停机永远不返回 | hook 在 `shutdown.total_timeout` 内等；stopping 卡住时与 `Service.Shutdown` 不完整相同：不停 Service / Mod、不释放单实例锁 |
| NC-232 | 信号之后才发生的 RuntimeFailure（DataEngine / Remote fatal、失锁）只写 Error、以 0 退出 | 在 `run` 返回时并入错误，进程非零退出；安全动作（围栏、失锁不 Release）不变 |
| NC-233 | Redis Mod 第一次 Close 出错后每次重试都得到 `redis: client is closed` | 第一次 Close 后不论结果都交出连接池，错误只报一次，之后的 Stop 返回 nil |
| NC-234 | remote_entity Mod 停止失败也记 `stopped` | 失败记 Warn `stop incomplete`，停完才记 `stopped` |
| N02 O1 | `/admin/execute` 的命令没有期限 | ctx 带 `ops.admin_timeout`（缺省 10s），到期回 504 并写明结果未知；Ops 写超时 = max(15s, admin_timeout + 5s) |

**决定**：DECISIONS-PENDING 第四轮“留项”行（补齐 15 个单元的单元内留项）。**兼容**：NC-230 是行为收紧；`ops.admin_timeout` 让超过 10s 的配合 ctx 的命令回 504，需调大该值；不配合 ctx 的命令仍可能以传输错误结束（同样是结果未知）。**限制**：外部 E08 / E13 / E21 / E22。**链接**：[n01b 记录](../../review/REVIEW-2026-10-06-n01b.md) · [NC-230](../../bugfix/RR-20261005-NC-230.md) · [NC-231](../../bugfix/RR-20261005-NC-231.md) · [NC-232](../../bugfix/RR-20261005-NC-232.md) · [NC-233](../../bugfix/RR-20261005-NC-233.md) · [NC-234](../../bugfix/RR-20261005-NC-234.md) · [Ops 超时方案](../../feature/OPS-ADMIN-TIMEOUT-2026-10-06.md)

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

**兼容**：手写迁移候选须提供 loader / Id；预校验用目标 schema、旧 version；正常 CAS / 投影等待 / 整聚合重读与 int32 ID 兼容保持。**已有坏 WAL 不自动跳过或删除**。**限制**：真实 Mongo / HA / 持续竞争与生产已有坏 WAL 的恢复未验。**链接**：[NC-31](../../bugfix/RR-20261004-NC-31.md)

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

**兼容**：滚动升级期间旧节点仍可能写回（修复前行为），新节点的下一次删除会收敛。**链接**：[NC-130](../../bugfix/RR-20261005-NC-130.md) · [NC-131](../../bugfix/RR-20261005-NC-131.md) · [RR-20260913-01](../../bugfix/RR-20260913-01.md) · [revn05 记录](../../review/REVIEW-2026-10-05-n05-revn05.md)

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

**兼容**：API / 格式不变；**不自动修坏存量**，诊断按所属窗口清理。之后 B9 把窗口条目读取收成一个入口（NONCORE-21）。**限制**：真实 Mongo 网络 / 未知提交与多进程压力未验证。**链接**：[NC-41](../../bugfix/RR-20261005-NC-41.md) · [NC-42](../../bugfix/RR-20261005-NC-42.md)

<a id="noncore-19"></a>
#### NONCORE-19 account 换名释放已被他人提交的死计划、补偿失败计数；activity sweep 验证确认键（RR-20261005-NC-50 / NC-51、RR-20261001-06 残余）

**结论**：account 换名建角也释放名字已被他人提交的死计划（此前只有同名重试会释放，玩家直接换名永远 `ErrRoleLimit`），补偿失败重新计 `rollback.failed`（NC-50）；activity sweep 对已确认窗口键先验证合法与归属，坏条目跳过、保留、计 `sweep.window_key_malformed`，不再结算别的组的活动（NC-51）。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-19)

**后续**：B9（v1.21.0）在这两处做了复核补修（并发释放只计一次、Delivering 坏条目），见 NONCORE-21。**链接**：[NC-50](../../bugfix/RR-20261005-NC-50.md) · [NC-51](../../bugfix/RR-20261005-NC-51.md) · [RR-20261001-06](../../bugfix/RR-20261001-06.md)

<a id="noncore-20"></a>
#### NONCORE-20 game-demo activity 启动时拒绝注定开不出窗口的 `activity.game_sids`（RR-20261005-01，已被 C4 取代）

**结论**：v1.20.1 的修复：列表里重复的 sid、超出 int32 的值、本服加配置超过 `app.SingletonLiveMaxSIDs` 三种情形在 `startActivity` 任何远端调用之前按键名报错。**v1.20.2 起 `activity.game_sids` 键被 C4 活动组文件取代并删除**（OWN 部分），这段校验随之由 `activity.LoadGroupsFile` 承担。首发 v1.20.1。[实现](impl-cfg-skill-noncore.md#noncore-20)

**链接**：[RR-20261005-01](../../bugfix/RR-20261005-01.md) · [C4 方案](../../feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)

<a id="noncore-21"></a>
#### NONCORE-21 activity 窗口条目统一读入口与运维修复入口；account 建角判定表（B9）

**结论**：activity 读已存窗口条目只走 `readWindowEntries` 一个入口，坏条目在任何列表里都跳过、保留、计数，持有方可用新 Admin 入口清除；account 的 create_role 同名 / 换名、同名被拒后的释放与 `Admin.ResolvePendingCreation` 都查一张“名额状态 × 入口 × 名字状态 → 动作”的表。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#noncore-21)

**背景**：activity 窗口条目校验第 4 次出问题（NC-42、NC-51 链），account 建角判定只在部分入口生效（RR-20261001-06 链）——按 roost-coding“反复出问题要上报方向判断”上报。

**维护者决定**（DECISIONS-PENDING 第四轮 B9 行原文）：“activity 窗口条目统一读入口 + 持有方专用修复入口；account 建角改为‘名额状态 × 名字状态 → 动作’判定表”。

**现在的行为**：

- activity：`PendingActivities`、`DeliveringActivities`、`RetireDelivered` 与后台 sweep 都经 `readWindowEntries`；别的组的键或不合法的键跳过、保留、计数（新增 `sweep.delivering_key_malformed`）；此前 Delivering 里的这种键会让本组 sweep 去写别的组的窗口、自己永远留着，`PendingActivities` 会把它交给 game。`RetireDelivered` 只在本次确实移走时返回 true。新增持有方专用 `Admin.MalformedWindowEntries` / `RemoveMalformedWindowEntry`（**note 必填**，只删确实坏的条目）；窗口记录多两个可选字段 `admin_note` / `admin_action_at_unix`。
- account：`decideCreation`（名额 6 × 入口 4 × 名字 6，144 格表格测试），对外行为不变；两个换名请求并发释放同一个死计划时 `create_role.plan_released` 不再计两次。

**兼容**：`PendingActivities` 不再交出坏键；`RetireDelivered` 返回值语义收紧（仓内唯一调用方不看返回值）。**限制**：真实 Redis / Cluster 未跑（纯窗口逻辑）。**链接**：[B9 / C5 方案](../../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md) · [NC-51 复核补修](../../bugfix/RR-20261005-NC-51.md) · [NC-50 复核补修](../../bugfix/RR-20261005-NC-50.md)

<a id="noncore-22"></a>
#### NONCORE-22 account 换名建角也释放未 admitted 的计划（第五轮、第七轮 O37）

**结论**：计划还没 admitted、名字只被他人 reserved（第五轮）或名字预约已过期无人持有（O37）时，换名请求释放旧 slot 并按新名字建角；已 admitted 的计划不变（仍 `ErrRoleLimit`）。首发 v1.21.0。[实现](impl-cfg-skill-noncore.md#noncore-22)

**背景**：B9 判定表让一处不对称显形：同名请求会释放、换名不释放，玩家要先用旧名字重试一次或等预约过期。

**维护者决定**：第五轮（原文）“做：未 admitted 计划、名字仅被他人 reserved 时，换名请求也释放 slot（判定表 `unadmitted other` 行）”；第七轮 O37（原文）“按推荐：account 判定表 `unadmitted other` 行的 free 格也释放名额并用新名字建角”。

**兼容**：行为变化（以前答 `ErrRoleLimit` 的两格现在建角成功）。**链接**：[B9 方案 §6 / §6.1](../../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md)

<a id="noncore-23"></a>
#### NONCORE-23 saga 定义缺失 fence 时退避中的步骤同样记为放弃（RR-20261005-NC-250）

> 其他分册对应：SAGA-7，以那里为准。

**结论**：协调器因缺少定义版本把记录 fence 到 `ManualRequired` 时，若当前步骤正在重试退避，写放弃关闭的 tombstone、删掉它仍排队的命令；之后才到的成功告警（`saga.completion.late_after_abandon_total`）。截止、人工 Compensate、定义缺失三个出口共用一个判断。首发 v1.21.0。边界项（近 SAGA）。[实现](impl-cfg-skill-noncore.md#noncore-23)

**背景**：此前只在等结果时关闭，退避中较早尝试晚到的成功以 `ErrNotWaiting` 丢弃、不计入告警。SAGA.md 同时补充：被 kill -9 的进程遗留的 Mongo 事务持锁到 `transactionLifetimeLimitSeconds`，Mongo 步骤预算应长于它。随后 saga 方向 ①（`stepTransition`）把所有出口收成一个转移（SAGA 部分）。**链接**：[NC-250](../../bugfix/RR-20261005-NC-250.md) · [n06s5 记录](../../review/REVIEW-2026-10-06-n06s5.md)

<a id="noncore-24"></a>
#### NONCORE-24 global `Bind` 结果未知后用同样参数重试按幂等成功（RR-20261006-05）

> 其他分册对应：OWN-6，以那里为准。

**结论**：`Create` 没建成时读回已存绑定，group 与 globalSID 都一致就返回它（计 `replayed:bind`），不一致才报冲突，错误里带上已存的 group / sid。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-24)

**背景**：A2 之后写结果未知时由调用方重试；写已落库、回复丢失后重试同一个 `Bind` 会被告知“已绑定”（`ErrConflict "already bound"`）。来源：A2 核对调用方时留的观察，收尾第 4 批 A7。**兼容**：放宽（同参重试从冲突变成功）；换 group / globalSID 仍冲突。**限制**：真实 Redis 上的同参重试由发版前验证的真实依赖用例覆盖（TOOL 部分记录）。**链接**：[RR-20261006-05](../../bugfix/RR-20261006-05.md) · [收尾第 4 批](../../bugfix/CLOSING-BATCH-4-2026-10-06.md)

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

**背景**：生成器运行期间 chdir 进 `.roost-sync-*` 暂存树，同进程其他 goroutine 不设 `Dir` 启动的子进程会继承它；Windows 上暂存目录删不掉而残留。**兼容**：两行生成器提示从相对路径变为绝对路径。**限制**：Windows CI 上的两次清理失败是否全由此引起，未在 Windows 上证实（外部 E21～E26 一带）。**链接**：[RR-20261004-12](../../bugfix/RR-20261004-12.md)

<a id="noncore-28"></a>
#### NONCORE-28 roost 跑的 go 命令按进程树取消，Wait 有界；Ctrl-C 后重发信号等其生效（RR-20261004-13 与补修）

**结论**：doctor 的 `go mod verify` / `go list` / `go test` 与 project deps / generate 的 `go get` / `go mod tidy` 超时 / 取消时连同子进程一起结束（Unix 进程组、Windows `taskkill /T`），Wait 最多再等 5 秒。首发 v1.20.0；补修 `cb11be90` 首发 v1.20.1。之后 B6（NONCORE-31）把信号接管收到 CLI 入口。[实现](impl-cfg-skill-noncore.md#noncore-28)

**背景**：超时只杀 go，它起的 compile / link / git 继续以暂存树为工作目录，输出被缓冲时调用还要等它们结束、超时不起作用。补修：重发给自己的信号异步生效，满载时调用方会先跑下去。**链接**：[RR-20261004-13](../../bugfix/RR-20261004-13.md)

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

**链接**：[B6 方案与实施](../../feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md)

<a id="noncore-32"></a>
#### NONCORE-32 DAO 无集合声明 `//roost:dao nocoll`（W-2026-09-18-09 选 A）

**结论**：全部字段 `nopersist` 的内存 DAO 用 `nocoll` 声明，生成物保留读写、undo、回滚快照与同步，不生成集合 / 库名常量和任何 Mongo 读写、迁移、加载路径；game-demo 的 `MonsterDao` 改用它，不再编造 `monsters` 集合。首发 v1.20.0。边界项（近 DAO）。[实现](impl-cfg-skill-noncore.md#noncore-32)

**维护者决定**：10-04 对 WANTED W-2026-09-18-09 选 A。**兼容**：与 `coll=` / `db=` 同时出现、`nocoll=<值>`、含持久字段，以及持久实体或 `remote=managed` 实体使用它时，都在生成期报错并点名。已生成工程把 marker 改成 `nocoll` 后 `roost generate` 即可原地迁移。**链接**：[方案](../../feature/DAO-NO-COLLECTION-2026-10-04.md)

### N09 skill（只列不在 SKILL 的一项）

<a id="noncore-33"></a>
#### NONCORE-33 skillsync 文件 outbox 打开时清理崩溃遗留的临时文件（RR-20261006-04，O6）

**结论**：`NewFileOutboxStore(WithOptions)` 删除名字精确匹配 `outbox-<数字>.tmp` 的普通文件（目录、符号链接、其他名字不动）。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-33)

**背景**：写入中途崩溃留下的临时文件以前永不回收。**决定**：收尾第 3 批 A6。**现在**：同一目录不能有两个在写的 store（另一个 store 的在途临时文件会被删掉，它的替换失败并返回错误，不会静默损坏），写进 `FileOutboxStore` 注释与 `docs/skill/production-readiness.md`。**限制**：Windows 上未实际运行（只做了交叉 vet）。**链接**：[RR-20261006-04](../../bugfix/RR-20261006-04.md)

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

**限制**：Linux / Windows 上的真实插件加载未验（外部 E27，本机 macOS arm64 / Go 1.27.0）。**链接**：[n10b 记录](../../review/REVIEW-2026-10-06-noncore-n10b.md) · [NC-240](../../bugfix/RR-20261005-NC-240.md) … [NC-247](../../bugfix/RR-20261005-NC-247.md)

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

> 其他分册对应：CLK-6，以那里为准。

**结论**：堆按 (End, `Node.Priority`, ID) 排序，priority 数值小的先触发、缺省 0，新增 `Scheduler.NewTimerWithPriority`；到期节点的类型没有 handler 时照旧删除，另打 Warn 并计 `timer.unhandled_dropped_total{kind}`，新增 `Scheduler.ReportUnhandledTypes()`。首发 v1.21.0，**行为变化**。边界项（D-L 系列的时间来源 D-L3 在 CLK）。[实现](impl-cfg-skill-noncore.md#noncore-40)

**背景**（N11 O6 / O7）：同期限顺序由堆形状决定（登记 1..6 按 1、6、5、4、3、2 触发）；World 每次从 DAO 的 map 重建调度器，顺序每次都可能不同。下线一种定时器类型后存量节点到期即无声消失。

**维护者决定**（DECISIONS-PENDING 第六轮原文）：D-L1“同期限定时器缺省按登记顺序（ID 作第二键）；**新增可选 `priority` 字段**用于排序：先比期限，再比 priority，同 priority 按登记顺序”；D-L2“按推荐：未注册类型的到期节点删除时 Warn + 计数，加载时对无 handler 的存量类型告警一次”。

**兼容**：game-demo 模板 `TimerNode` 加 `priority`（旧节点按 0，不迁移），World `OnInitFinish` 调 `ReportUnhandledTypes`；已生成工程 `roost project sync` 后生效，不同步也能编译。**链接**：[方案](../../feature/D-L1-L2-TIMER-ORDER-AND-UNHANDLED-2026-10-06.md)

<a id="noncore-41"></a>
#### NONCORE-41 game-demo 场景寻路系统停止不再清空共享指针（RR-20261005-NC-270）

**结论**：`PathFindSystem.Stop` 只置原子标志，与不持 Scene 锁的并发寻路不再数据竞争。首发 v1.21.0（生成形状）。[实现](impl-cfg-skill-noncore.md#noncore-41)

**链接**：[NC-270](../../bugfix/RR-20261005-NC-270.md)

### N12 metrics / log / failurelog / robot

<a id="noncore-42"></a>
#### NONCORE-42 failurelog 在脚本结果未知时不再走非原子降级（RR-20261005-NC-160）

**结论**：`AppendRaw` / `DeleteRaw` / `Purge` 的 Lua 脚本报错（连接断开、读超时、ctx 到期）时原样返回错误，不再补做 RPUSH / LREM / LLEN+DEL。首发 v1.20.2，**行为收紧**。[实现](impl-cfg-skill-noncore.md#noncore-42)

**背景**：降级会让同一条死信写两份、多删一条同值记录、把清空之后新到的死信删掉（真实 Redis 上复现两份）。降级只留给 Eval 返回 `(nil, nil)` 的无 Lua 适配器。bus 侧表现为 `bus: write dead letter failed`。**限制**：Redis Cluster；`DeadLetter` 报错之后 bus 的完整重投链未做端到端演练。**链接**：[NC-160](../../bugfix/RR-20261005-NC-160.md)

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

> 其他分册对应：OPS-7，以那里为准。

**结论**：序号只增不回收。首发 v1.23.0（本版）。[实现](impl-cfg-skill-noncore.md#noncore-45)

**维护者决定**（第十二轮“robot Stage 序号”行原文）：“只增不回收”。**兼容**：有过缩扩的 staged 运行会用到超过 `Count` 的序号，**自定义 `IdentityProvider` 要覆盖到**。**链接**：[RR-20261006-09](../../bugfix/RR-20261006-09.md)

### N13 container / safemap / goroutine / misc

<a id="noncore-46"></a>
#### NONCORE-46 遍历回调自锁、跨桶停止、FastMap 幻影键、TaskPool 关闭 panic、拓扑排序判环、KeyMap 遍历删除（RR-20261005-NC-180～185 与复审补修）

**结论**：N13 六处，首发 v1.20.2。[实现](impl-cfg-skill-noncore.md#noncore-46)

| 编号 | 以前 | 现在 |
| --- | --- | --- |
| NC-180 | `Bucket.Range` 回调里 `Del` / `Add` / 未命中 `Get` 当场自锁；`EntityManager.Range` 回调里 `Destroy` 卡死全进程的 Add / Destroy | 读锁内复制、锁外回调；实体先持引用（`Touch`）再回调，到达前已摘除的不交出，回调期间被销毁的推迟到引用归还才清理（复审补修 `815c3661`，此前会交出 ID / 分类已清零的实体） |
| NC-181 | `RangeAll` / `RangeWithCursorCnt` 见 false 不跨桶停止；`EntityManager.Range` 写明的提前停止不成立 | false 跨桶停止 |
| NC-182 | FastMap（DAO `map=fast`）遍历回调里改已有键触发扩容重排，交出零值键、漏改原有键，并把 `items.0` 写进提交 | `Set` 改已有键不重排；`Range` 识别表被换掉后到当前表查剩余键；`Clear` 不清零旧数组 |
| NC-183 | `TaskPool.Submit` 与 `Shutdown` 并发 panic “send on closed channel” | 受理与关闭互斥 |
| NC-184 | 拓扑排序遇到未单独注册的依赖误报环，未注册节点还会掩盖真正的环 | 按全部节点判环 |
| NC-185 | `KeyMap.Range` 回调删除当前键漏键、交出零值键 | 每桶先复制再回调 |

**性能**：`EntityManager.Range` 10 万实体一次约 0.86ms → 4.1ms（每实体两次 CAS），唯一正式调用方是 statslog 的分钟级统计；FastMap Set/Get 无显著差异（p=0.937）。**链接**：[NC-180](../../bugfix/RR-20261005-NC-180.md) … [NC-185](../../bugfix/RR-20261005-NC-185.md) · [N13 记录](../../review/REVIEW-2026-10-05-n13.md)

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

**限制**：mongo-driver 自身的错误信息（例如 URI 解析失败）是否带口令没有查。**链接**：[NC-191](../../bugfix/RR-20261005-NC-191.md)

<a id="noncore-50"></a>
#### NONCORE-50 启动失败先收回 Service 已启动的部分（RR-20261005-NC-193）

> 其他分册对应：APP-9，以那里为准。

**结论**：`Service.Init` 返回错误时 App 同样调用 `Shutdown`（限时 5s），再停 Mod、释放单实例锁；Shutdown 没在时限内结束（含不配合 ctx、panic）时不停 Mod、不释放锁，与正常停机一致。首发 v1.20.2。边界项（近 APP）。[实现](impl-cfg-skill-noncore.md#noncore-50)

**兼容**：契约补充——**`Shutdown` 须容忍部分初始化**。**链接**：[NC-193](../../bugfix/RR-20261005-NC-193.md)

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

**限制**：修后 heal / 矩阵在真实共享隔离环境上实跑（会注入故障）、Linux 上 NC-202 的 pid 认领未验（随 E26）。**链接**：[N15 记录](../../review/REVIEW-2026-10-05-n15.md) · [NC-200](../../bugfix/RR-20261005-NC-200.md) … [NC-208](../../bugfix/RR-20261005-NC-208.md)

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
#### NONCORE-54 nest 用例在 `-shuffle` 下互相污染（收尾第 4 批 A2 / A3）与 WANTED W-2026-10-06-01

**结论**：用例隔离问题，不是产品缺陷：`group_lock_test` 在没有 Guard 作用域的 goroutine 里取锁，组迁移重试把同一个 `EntityGuard` 两次放回池，之后两个快 worker 共用一个 Guard、互相解锁。用例改为先建作用域；目标用例失败时放行闸门、停机有上限。首发 v1.23.0（本版，只改测试）。[实现](impl-cfg-skill-noncore.md#noncore-54)

**留给 review 判断**：WANTED W-2026-10-06-01——`releaseDispatchLocks` 的无 Guard 作用域分支会把调用方仍在用的 Guard 放回池。生产调用方（`dispatchLoadedEntities`、`groupTransitionDispatch`）都在 `runNestLogic` 的作用域里，按当前源码不可达；但任何将来的无作用域调用方只要碰上组迁移重试就会污染全进程的 Guard 池。候选修法：只释放 `acquired`、不归还 Guard，或无作用域时直接拒绝。截至 `02c8a10d` **未判**；之后维护者第十三轮要求交给 review 前闭环（DECISIONS-PENDING 末表，状态“进行中”）。**更新**：已转 [RR-20261006-12](../../bug/RR-20261006-12.md) 并修复（`b7471ae4`，未发版）：派发取锁只用 Guard 作用域里的 Guard，没有作用域时取锁前返回错误。

**链接**：[收尾第 4 批](../../bugfix/CLOSING-BATCH-4-2026-10-06.md) · [WANTED](../../bug/WANTED.md)

<a id="noncore-55"></a>
#### NONCORE-55 Nest 暂时性冲突的重新准入加抖动（U-0279）

**结论**：锁超时 / 组变化 / 组迁移待定的消息重排延迟从固定 5ms 改为 5ms 下限加 `[0, 5ms)` 均匀抖动，对称的交叉创建不再靠调度噪声解开。首发 v1.20.1。边界项（核心线 Nest 修复，没有对应主题）。[实现](impl-cfg-skill-noncore.md#noncore-55)

**背景**：固定延迟让同一轮因同一冲突回滚的两条消息（handler 内交叉新建 X / Y，RR-20260926-48）总在同一时刻重新准入；v1.20.0 / v1.19.2 生成工程 `TestGeneratedDataEngineCrossCreateResolvesOnRealWAL` 正常负载下失败率 25%～55%，调用方收到 `ErrLockTimeout`。**兼容**：400 次上限与最短约 2s 的重排窗口不变，平均延迟 5ms → 7.5ms。**链接**：[U-0279](../../bugfix/U-0279-nest-requeue-jitter.md)

<a id="noncore-56"></a>
#### NONCORE-56 game-demo 玩家租约的三处修复（RR-20261004-10 / 11 / 14），同版被静态绑定取代

> 其他分册对应：OWN-1，以那里为准。

**结论**：v1.20.0 开发期间先修了按玩家 Redis 租约的三处缺陷（刷新回合重新认领的时间预算、撤离进行中拒绝准入、租约窗口从设键那次请求起算），随后同一版本里 `f051e24a` 把 PlayerOwners 改为静态绑定、删除了整套租约逻辑（OWN 部分）。**v1.20.0 发布物里这些代码已不存在**（发版提交上模板中 `handBackPassBudget` / `SetNX` 等均无结果）。边界项（近 OWN）。[实现](impl-cfg-skill-noncore.md#noncore-56)

**意义**：这是“反复出问题要上报方向判断”的先例——按玩家租约连续多轮出问题，维护者确认前提不成立后改为静态绑定 + App 单实例锁。**链接**：[RR-20261004-10](../../bugfix/RR-20261004-10.md) · [RR-20261004-11](../../bugfix/RR-20261004-11.md) · [RR-20261004-14](../../bugfix/RR-20261004-14.md) · [静态绑定方案](../../feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md)

## 外部验证（本部分相关）

本机做不了的验证统一在 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)（E01～E28）。与本部分条目有关的：

| 编号 | 内容 | 相关条目 |
| --- | --- | --- |
| E04 | Sync 真实网络（含 skillsync 经 kit syncstream / NATS 的端到端） | SKILL-3、SKILL-4 |
| E05 | 真实网关 / 反向代理后的 HTTP 与 robot 重连 | NONCORE-2～5、NONCORE-43、NONCORE-44 |
| E06 / E07 | NATS JetStream、etcd 多节点 HA | NONCORE-6、NONCORE-7、NONCORE-52 |
| E08 | 多机 Redis Cluster（含 `redis.cluster_addrs` 列表写法起服） | CFG-1、NONCORE-1、NONCORE-42 |
| E09 | account / chat / activity 在 Cluster 与多进程下 | NONCORE-11、NONCORE-19、NONCORE-21、NONCORE-22 |
| E10 / E13 | 异步复制丢写与切主、多主机强杀 | NONCORE-15 |
| E11 | Mongo 跨主机副本集、切主中提交 | NONCORE-16～18 |
| E19 | 物理断电与三进程恢复链路（含 World 定时器提交被拒、日志磁盘写满） | NONCORE-39、NONCORE-44 |
| E21 / E22 | 真实 systemd / k8s 停机 | NONCORE-1 |
| E25 | Windows：CLI 信号与进程树、暂存树、文件 outbox 替换 | NONCORE-27、NONCORE-28、NONCORE-31、NONCORE-33 |
| E26 | Linux 上 CLI 信号、NC-202 pid 认领、修后 heal / 矩阵实跑 | NONCORE-31、NONCORE-51 |
| E27 | hotcode 真实插件加载（Linux / Windows） | NONCORE-36 |

本机未验证、也不在外部清单里的，各条“限制”里写明（例如 CFG-8 的 `stage=apply` 撤回只由单测覆盖、SKILL-5 只用 MemoryHost、NONCORE-54 没有做全 seed 扫描）。

## 仍待决定的事项

- **维护者决定项：无**。[DECISIONS-PENDING](../../review/DECISIONS-PENDING-2026-10-05.md) 文首“当前总状态”：未决事项为零。
- **明确留到下个大版本**（维护者决定，不在本版）：A4 ① 每个 Mod 声明配置 schema（CFG-2 的后续形态）；B3 ③ Host 取值约束做成随环境下发的能力表（SKILL-13）。
- **保持方向 B、不实现的语义**：NC-151 / NC-213 方向 A（phase 计时、recast、chain 间隔 / 重复、modifier 叠层），B3 ④“保持 B”，第十二轮“NC-151 timeout_ticks：保持 warning”；NC-224 方向 B（冻结施法输入）第五轮“不做”。
- **列为后续、未排期**：tablegen 与 cfggen 两种标签方言合一、tablegen 生成期的 ref 数据检查（B10 方案 §2.4 与“未完成 / 后续”）。
- **WANTED 未判**：W-2026-10-06-01（nest `releaseDispatchLocks` 无 Guard 作用域分支，见 NONCORE-54），由核心线 review 判断。**更新**：已转 RR-20261006-12 并修复（`b7471ae4`，未发版）。
- **`02c8a10d` 之后的变化（不在本分册范围，供读者知悉）**：维护者第十三轮决定（DECISIONS-PENDING 末表）——“这次不能有wanted，需要都解决后再给review, review是查问题”，W-2026-10-06-01 等疑点要在交给 review 前闭环（状态“进行中”）；“windows的问题可以暂存，加一个说明 window问题不保证正确”，外部验证 E25 与 E27 的 Windows 部分暂存（`7fec136e`）。本分册里提到 Windows 未验证的条目（NONCORE-27、28、31、33、36）按此理解。

## 文档与源码不一致（以源码为准）

1. A4 方案写 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys` 为 16 / 95 / 77 个键；发版提交上是 17 / 99 / 79。差额来自之后各批登记的新键（`service_metrics.enabled`；`ops.admin_timeout`、`remote_entity.cached_max_staleness`、`remote_entity.mirror.shutdown_timeout`、`remote_entity.snapshot_l2_tombstone_wait_timeout`；`remote_entity.snapshot_interest_per_consumer`、`remote_entity.snapshot_l2_tombstone_wait_replicas`）。方案是实施当时的快照，不算错误，但 review 时以源码为准（CFG-2）。
2. B10 方案 §2.1 的 API 草图写 `rules.Lookup` 是“encoding/json 的键匹配”、§2.2 写规则字段“按 encoding/json 的键匹配”；v1.22.0 起源码是逐字匹配（`configdata/rules/rules.go:276-280`、`configdata/fieldrules.go:91`）。方案文首的状态行没有提到这一变化（CFG-7、CFG-10）。
3. 作者文档 `docs/skill/skill-casting-and-combat.md` 第 80 行标题仍写“（O33，未发版）”；O33 随 v1.21.0 发布（`f6043e44`）。同文 O22 / O29 / O2 标“未发版”在本版发布前是对的，发版时一并改（SKILL-16）。
4. DECISIONS-PENDING 标注提交 `2a7d2a65` 的提交说明写“实施状态（a6e75488）”，`a6e75488` 是 rebase 前的提交号、不在 main 历史上；正确的是 `f6043e44`（DECISIONS-PENDING 表内已写对）（SKILL-16）。
5. NC-220 / NC-224 的 bugfix 记录写修复位置在 `compile_snapshot.go`（`snapshotCapturableWhereRead` / `readsInsideProcessCallbacks`）与 `compile_owned_entity.go`（`validateDetachedProcessFields`）；同一版本（v1.21.0）内求值上下文表把它们收拢到表上，发版提交上这三个函数已不存在（SKILL-14）。记录描述的是当时的提交，不算错误。
6. RR-20261005-01 的回归用例 `TestActivityRefusesACandidateListNoWindowCouldOpenWith` 在发版提交上已不存在：C4 删除了 `activity.game_sids`（NONCORE-20）。
