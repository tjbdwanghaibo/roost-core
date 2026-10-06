# 双文档规格（v1.19.2 → v1.23.0）

维护者要求（2026-10-06）：“希望这次跑完后整理出一个实现和说明的双文档，越详细越好，我自己阅读并且给另一个agent做review，所以需要兼顾人类阅读和agent使用”。

## 位置与范围
- `docs/release/v1.23.0-GUIDE.md`（说明文档）与 `docs/release/v1.23.0-IMPLEMENTATION.md`（实现文档），互相链接；
  大到不便阅读时按主题拆成 `docs/release/v1.23.0/impl-<theme>.md`，两份主文档保留总目录与总表。
- 范围：`v1.19.2..<发版提交>` 的全部改动（v1.20.0 / v1.20.1 / v1.20.2 / v1.21.0 / v1.22.0 / v1.23.0），按主题组织而非按时间；
  每条标明首次发布于哪个版本。

## 条目编号（两份文档共用）
`<主题缩写>-<序号>`，例：APP-1 单实例锁、SAGA-3 Mongo 步骤收件箱、CLK-1 业务时钟单调。每条在两份文档里用同一编号作锚点。

## 主题（建议）
APP（单实例锁、fail-stop、liveness、stop 契约、readyz、退出日志）· OWN（玩家静态绑定、activity 组文件）·
SAGA（U-0280 操作实例收件箱、B1 代际、方向① stepTransition、方向② Mongo 收件箱 + 延迟决定 A、步骤预算、预算大小写）·
DRV（A2 驱动重放契约、NC-100/101、墓碑 WAIT、Close 契约）· DAO（A1 DAO 统一回滚、skill 运行时状态不进事务、buff 投影）·
REM（B2 L2 水位、Mirror 1～6、O4 配额、O-M6-1/3/5/6）· CLK（D-L3 双时钟、单调高水位、activity/mail 回业务钟）·
CFG（严格配置读、B10 规则统一、大小写敏感、cfggen globals 规则、生成配置新键）· SKILL（eval-context 表、O 系列观察）·
NONCORE（N01～N15 修复与 RR-20261005-NC-*、RR-20261006-*）· OPS（metrics 按标签删除、Ops Bearer、CAS 冲突计数、面板）·
TOOL（pretag、source-head-check、冲突标记门禁、mirror-local.sh、示例实跑）。

## 说明文档（GUIDE，写给维护者，也让 agent 能定位）
开头：一页总览（版本时间线、主题地图、行为变化 / 兼容破坏总表、需要业务改代码或配置的清单）。
每条：
1. 一句话结论；
2. 背景：之前是什么问题（现象 + 为什么会发生），对应 RR / U / 决定轮次；
3. 维护者决定（引原话）与选这个方案的理由、没采用的方案；
4. 现在的行为：对业务作者 / 运维可见的变化，配置键（名称、默认值、范围），错误与日志、指标；
5. 兼容与迁移：行为收紧、升级要做什么；
6. 已知限制、待外部验证项（Linux / HA / 跨主机等）；
7. 链接：实现文档同编号条目、feature / bugfix 记录。
结尾：外部验证清单（C 类）、仍待维护者决定的事项（若有）、WANTED 未判项。

## 实现文档（IMPLEMENTATION，写给 review agent，也让人能读懂）
开头：怎么用这份文档 review（建议顺序、先读 roost-coding 规范的哪些契约、本地复跑环境与命令、不能碰的共享资源）。
每条：
1. 提交号（全部相关提交）、首发版本；
2. 改动文件与关键符号，`path:line`（以发版提交为准），一句话说明各自职责；
3. 不变量：要保证什么、在哪一处强制（锁 / 事务 / 单一入口）、哪些守卫测试防回退；
4. 控制流或状态机（必要时用编号步骤或 mermaid）；
5. 失败与不确定结果怎么处理；
6. 测试：用例名与文件、修前红文本（原样）、修后命令与结果、负对照；
7. 性能证据（如有）：基准、样本数、结论；
8. 未验证项与已知风险；
9. **review 检查点**：给 review agent 的具体问题清单（例如“确认所有步骤状态转移只经 stepTransition：看 guard 测试 X 是否覆盖 Y 路径”）。
结尾：全局守卫测试 / 门禁清单；按包的改动索引（包 → 条目编号）。

## 写作要求
- 中文，按 roost-coding“写给人阅读”的风格：先结论后细节，表格 + 短段落，不堆砌形容词。
- 每个事实可追溯：提交号、path:line、文档链接；不确定的明确写“未验证 / 推断”。
- 不重复粘贴大段代码，引用位置即可；关键签名可贴一两行。
- 以发版提交的源码为准核对行号；历史记录与源码冲突时以源码为准并注明。

## 汇总者待办（来自分册报告）
- 分册 1（APP/OWN/CLK/OPS/TOOL，`f1d0e563`，39 条）。可能无人覆盖：C9 `TestNetworkCodegenTestsRunInSomeWorkflow`（codegen 联网用例门）、B9 activity 窗口条目统一入口 + account 建角判定表（`bd6df5e5`）——确认归属，缺则补进合适分册。
- 文档与源码不一致，需修文档（以源码为准）：
  1. v1.20.0 CHANGELOG / App 锁方案 §13 obs34 说 gift debit MaxAttempts 改在生成的 definition.go；v1.20.1（054fdd66）起改为配置 `saga.steps.gift_item.debit.max_attempts: 15`（`codegen/internal/roost/demo.go:209`）——在方案文档加更正注（CHANGELOG 历史段不改，加脚注式更正可选）。
  2. App 锁方案 §3.6 “最多 MaxPageSize 个 sid” → `app.SingletonLiveMaxSIDs = 200`（`app/singleton.go:93`）。
  3. D-L3 方案 §3.2 `playerowner.go.tmpl` 豁免用途改为“驻留与闲置卸载”。
  5. roost-coding 规范加例外：短临界区、Close 不等在途的（kit RedisMod，go-redis）可用 sync.Mutex。
- 发版后清理（不在冻结期改代码）：`app/app.go:550-552` stopModsReverse 的注释错放在 startupCleanupTimeout 上方。
- 分册 2（SAGA/DRV/DAO/REM，`bba7f73d`，35 条）报告的不一致，需修文档（源码 02c8a10d 为准；不改代码，代码注释类列入发版后清理）：
  1. “L1 写入只经 admitLocked” 字面不成立（refresh / adoptSharedLocked / loadForRefresh / 无版本 Delete 直接 setL1Locked，均在 publishMu 下、值来自 L2/权威）——B2 §2、DECISIONS 第九轮措辞改准；代码注释 `entity/remote_snapshot.go:131`、`remoteentity/snapshot_client.go:35` 列入发版后清理；“无守卫测试防新绕行”写进 WANTED 候选。
  2. `entity/remote_mirror.go:78` After 注释过时（实际返回 ErrRemoteSnapshotStale）——发版后清理。
  3. MIRROR-M6-OBSERVATIONS §3 墓碑 WAIT 标签只有 skipped（无 redirected/unsupported）。
  4. MIRROR-M6-OBSERVATIONS §2 补 `stopped` 结果与 `interest_refresh_sent_total`。
  5. USER_GUIDE:343 Mirror 第 4 步“未发版”→v1.21.0；T-259 A2“未发版”→v1.20.2。
  6. durable 名 `sync_remote_entity_snapshot.live_…` 经 sanitizeSyncName 变 `_live_`（推断，文档注明）。
  7. L2 落后上界与配额：生成模板 snapshot_l2_ttl=10m → 约 10m30s；配额生成模板 6250——USER_GUIDE / B2 §7 写清“core 缺省 vs 生成模板”。
  8. DECISIONS-PENDING :13 / :210 W-2026-10-06-02 已由 d05a04a1（RR-20261006-10）修复。
  9. CHANGELOG [v1.21.0] O-S5-1 终止错误 5→4（ErrDefinitionMissing 已移出，5a3c4a60）：加更正；SAGA-DIRECTION 同改。
  10. SAGA-DIRECTION 方向① 签名改为 `saga/step_transition.go:53 stepTransition(before, after Record, t transition)`。
  11. NC-250 记录 abandonedOperation → openOperation + stepTransition；U-0280 / B1 文档 reserveInTransaction / commandIncarnation 位置 → `saga/step_operation_inbox.go:162/:413`（加更正段）。
  12. O-M6-5 “10 次 × 1s” 是每个索引的预算，最坏 N×10s——记录与 CHANGELOG 写清。
  13. 小项：81659082 提交信息 T 号笔误（不改历史）；测试名 TestOnlyScriptCommandsOptOutOfTheDriverRetry 名不副实（发版后清理）；两套延迟数字注明来源轮次。
- 分册 2 推断的风险（写为 review 检查点，并登记 WANTED 候选）：stepTransition 守卫看不到包级 helper 里改写的 ApplyRequest；maxOperationAttempts 4096 在多次 Resume 后可被超过；interest 表满时 release 不留撤销水位；glsvet A1 提示只抓直接调用。
- 维护者 2026-10-06：“这次不能有wanted，需要都解决后再给review, review是查问题”。冻结解除，fixn / fixs / fixr 三路闭环全部疑点（W-2026-10-06-01、glsvet A1 间接调用、stepTransition 盲区、maxOperationAttempts、admitLocked 收拢、interest 表满撤销水位、注释与文档修正）。
  汇总时：① 新冻结提交为准重核三分册全部 path:line（行号会漂）；② 所有“review 检查点”只保留给 review 去查的问题，不得出现“已知风险待判断”；③ 文档总表写明 WANTED 未决数 = 0；④ 新增 RR（-11 起）补进对应分册条目。
- 分册 3（CFG/SKILL/NONCORE，`b549e4df`，91 条）。去重：NONCORE-23↔SAGA-7、24↔OWN-6、40↔CLK-6、45↔OPS-7、50↔APP-9、56↔OWN-1、NONCORE-1↔APP-4/7/8+OPS-3（以对方为准）；CFG-12 与 REM-13 同项二选一（留 CFG-12，REM-13 改为引用）。C9→NONCORE-31、B9→NONCORE-21。
  需修文档：A4 方案键数 16/95/77 → 17/99/79；B10 §2.1/§2.2 “按 encoding/json 匹配”→逐字匹配（`configdata/rules/rules.go:276-280`、`configdata/fieldrules.go:91`）；`docs/skill/skill-casting-and-combat.md:80`“（O33，未发版）”→v1.21.0；`2a7d2a65` 说明里 `a6e75488`→`f6043e44`（加更正，不改历史提交）；NC-220/224 记录加“函数已被求值上下文表取代”的后注。
  **需闭环（不能留给 review）**：RR-20261005-01 的回归 `TestActivityRefusesACandidateListNoWindowCouldOpenWith` 随 C4 删除——核对该承诺在 C4 后是否仍需要：仍需要则补等价回归（组文件形态），不再适用则在 RR 记录写明原因与 C4 提交。
- fixr 已合 main（`155b9f91`、`6b38cc11`）：用了 RR-20261006-11（interest 表满撤销水位）。**撞号**：origin/fixn（`0bca07d7`，待维护者批准合并）也用了 RR-20261006-11（nest Guard）与 -12（glsvet）→ 合并时把 fixn 的 -11→-12、-12→-13（文件名、索引、CHANGELOG、§7、WANTED、DECISIONS、测试注释里的编号）。
- 待闭环：fixr 的溢出水位到期只靠推理（registry 直读 time.Now）→ 注入时钟补测试；JetStream 对 interest handler 出错是否重投未查（与修复无关，但不留未验证）；A1 盲区（组件普通字段存可变状态不登记 undo）待维护者选 A（glsvet + //roost:transient）/ B（规范约定）。
- 发版文档同步：impl-saga-drv-dao-rem.md:2182/2189 A1 描述（glsvet 已跟进一层 helper）、guide 里 W-01 归属；roost-coding :39 补 helper 跟进说明。
- fixs 已合（`5ca32611`、`ca029401`）：RR-20261006-14（stepTransition 改为 Engine 方法并自写库 + go/types 守卫）、RR-20261006-15（P2，claim outcome + 有界查询）；SAGA-2/SAGA-8 检查点已在 impl 分册标关闭。
- fixr2 已合（`d5682dc4`、`d6f5edd3`）：溢出水位到期实测、interest handler 出错 = Ack（符合设计，MIRROR-STEP-4 §6.10）、RR-20261005-01 回归改名重写于 C4 `277e1252` + 新守卫 TestAGroupFitsOneLiveQuery。汇总时 REM-5 一带“未核对”文字补结论链接。
- 分册 1 重核完（`ac913a60`，41 条，TOOL-8 平台支持 / TOOL-9 不留 WANTED）。报出 6 项本机可做未做：①～④真实进程演练 → `wt-drill`；⑤ OpenActivity 不核对组 + ⑥ dispatcher/bus_rpc_pending DeleteSeries → `wt-oa`。两路完成后分册 1 的 APP-1/7/8/9、OWN-2/3/5、OPS-2 要更新，相关代码行号再核一次。
- 共享文件小改（rv2/rv3 完成后我来改）：App 锁方案 §3.6 `app/app.go:125`→`:130`；D-L3 §3.2 `gift_saga.go.tmpl:353`→`:360`；DECISIONS 第十三轮“不留 WANTED”行状态改为已闭环（列提交）。
- oa 已合（`055a15d6`、`543d4287`）：RR-20261006-17（OpenActivity 按组核对 expected）、-18（派发器序列随销毁删除）、-19（bus method 标签上界 257）。汇总时 OWN-5 / OPS-2 更新（guide-app…:628、impl-app…:1046、_summary-app…:161）。
- 待收尾小项（下一批合并做）：① `activity.groups_file` 未设时不核对是为旧工程兼容——按“线上未部署不做兼容”改为必填（协调器启动即拒绝缺失），删掉不核对分支；② `slow_reroute.total` 的删除补一条触发冷目标改道的用例。
- rv3 已合（`a3002263`）。维护者：NONCORE-46 选 A（修，`wt-tpstat`）；B10 两条管线待维护者在澄清后选 A/B。

## 恢复后进展（2026-10-06 续）
- rv2 已合（`65bef66e`，分册 2 共 39 条，未闭环 0）。drill 合入后复核 SAGA-1 `nats/driver/jetstream.go:96-97` 与 DRV-5 引用。
- tail 已合（`2c01e06d`、`363be382`）：groups_file 必填（破坏性）、slow_reroute 补测、B10 分工文档。汇总时改 OWN-5（guide ~450-460、迁移表第 10 行 ~102、impl 709-742）、OPS-2（guide ~628、impl ~1046）、CFG-7（guide-cfg ~1453“列为后续”→维护者选 A 不做）、核 CFG-11（~403）；兼容破坏总表加“groups_file 必填”。对照表注意：tablegen 也有单例 `//roost:object`。
- 跟进：`activity.New` 在 `Config.Groups == nil` 时仍不核对（只剩直接构造的测试路径）→ 改为必填，消除不核对分支（`wt-grpnew`）。
- grpnew 已合（`061cb538`、`068bf5d4`）：`activity.New` 要求 Groups。**发版必做**：生成的 game 测试用了 `activity.Config.Groups`（v1.22.0 没有）→ `codegen/internal/roost/manifest.go` `minimumVersions.Core` 与 `.github/workflows/framework-compat.yml` minimum 行提到 v1.23.0（打 tag 时一起改）。OWN-5 补“New 也要求组”。
- sktest 已合（`5c1f4176`）：RR-20261006-21/22/23（skill 进程记录生命周期）。分册 3 的 SKILL-1、SKILL-3、SKILL-5 按 `5c1f4176` 更新。待维护者：宿主停进程失败时 Runtime 不重试（A 保持+文档 / B 重试）。
