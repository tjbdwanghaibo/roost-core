# 活动与首次进服：截止时间及日历窗口评审

**状态：已修复（2026-11-19）。** 修复提交 `355f68d2`（经 `d8a9f709` 合入 main），提交说明逐项对应本文各条发现；现行行为见 CHANGELOG v1.58.0“换服、邮件与活动修复”。以下为评审时的原文。

基线：`main@c32401d8`。审查 `activity` 模块与 `enter` 联动，而非 game-demo 的过渡实现。只做审查，不改生产代码。优先级是处理建议，不是线上事故评级。

## P1 · 创建触发活动截止后，一行仍启用的配置会挡住新角色首次进服

**触发。** 玩家范围、`Trigger=created`、非零 `UntilMilli`。保留已结束的活动配置，新角色在截止之后首次进入。规则本身能通过校验；截止前后不需改表。

**原因。** [首次初始化监听](../../modules/activity/with/enter/game/components/activity/activity_enter.go.tmpl#L11)在同一笔 Player 事务执行活动对账。[激活逻辑](../../modules/activity/files/game/components/activity/engine.go.tmpl#L42)按建角时间算第一期，再由 [window](../../modules/activity/files/game/components/activity/rules.go.tmpl#L165)把关闭时间截到总截止。窗口已空时返回 `ErrRule`；[Reconcile](../../modules/activity/files/game/components/activity/engine.go.tmpl#L112)只跳过 `ErrDenied`，所以这不是“该玩家没有这场活动”，而是整笔首次进入失败。

**证据。** 最小生成工程只装 `enter + activity`；正式配置快照含一条正常活动和一条一秒前已截止的 `created` 活动。走生成的 `Sync_EnterInit`，两次均报 `EnterFirst listener activity.Component.OnEnterFirst: activity: invalid rule`，两次读取均为 `Initialized=false, FirstAt=0`。只禁用旧活动、保留正常活动，同一角色重试即 `First=true, Initialized=true`。未复现账号登录失败；失败的是角色首次初始化事务。

**验收。** 候选玩家已错过整个活动时应只跳过该定义，不吞真正非法的规则；旧角色已冻结的实例仍能结算。修复前可禁用到期定义来恢复首登。

## P2 · 有夏令时的时区，非零业务日界在活动模块与公共日历中不一致

**原因。** [公共日历](../../infra/base/clock/calendar.go#L133)按当地日期直接构造墙钟时刻；[活动的逐期起点](../../modules/activity/files/game/components/activity/rules.go.tmpl#L135)却先在当地零点构造时间，再加经过的毫秒数。相对解锁的 [calendarAnchor](../../modules/activity/files/game/components/activity/rules.go.tmpl#L188)用的是前一种算法，于是甚至可能算出无法被自己的第一期接受的锚点。

**证据。** `America/New_York`，每日 05:00 业务日界，活动开一分钟。普通日（2026-02-08）两边都是 05:00。2026-03-08 公共日界是 05:00 EDT，活动第二期是 06:00 EDT；2026-11-01 公共日界是 05:00 EST，活动第二期是 04:00 EST。让相对解锁恰好把首期排到这两个日界，`calendarAnchor` 均给出正确的 05:00，`window(anchor,1)` 却均返回 `calendar anchor must be the business day boundary`。这里特意选不会消失或重复的当地 05:00，排除了 02:00 本身不存在的歧义。

**验收。** 固定日程和相对解锁使用同一套“业务日期 → 当地墙钟日界”计算，保留普通日、春秋换钟和相对锚点往返三组对照。

## P3 · 末期活动会与实际不存在的下一期做重叠校验

**原因。** [window](../../modules/activity/files/game/components/activity/rules.go.tmpl#L149)只要 `Repeat != none` 就先算理论下一期，再用尚未按截止截短的本期窗口检查重叠；[next](../../modules/activity/files/game/components/activity/rules.go.tmpl#L174)本已能按 `MaxCycles`、`UntilMilli` 判断确无后续期。

**证据。** 纽约 2026-03-08 **零点**开始、每日、持续 24 小时，以下定义都通过表层 `Validate`。无限循环时到次日零点仅 23 小时，报重叠是正确的对照。只改为 `MaxCycles=1`，`next` 已答无下一期，第一期仍报 `overlapping cycles`。另一个对照仅设总截止为开始后 20 小时：实际窗口本应在下一日界前结束，仍被拒。此处日界是零点，与上一项非零日界偏移无关。

**验收。** 按实际期数及截止得到有效窗口，只对确实存在的相邻两期检查重叠；无限循环真正相撞的场景必须继续拒绝。

## 验证范围

以上结果均在隔离生成工程中连续验证三轮。首登使用真实 Nest、生成的 DAO 与正式 handler，提交器为测试替身；日历用例调用生成模块的冻结规则实现和框架业务日历。已有的重连保持创建锚点、零点跨 DST 为 23 小时、真正重叠必须拒绝的对照均通过。没有运行线上服、真实 Mongo 或客户端。共享参与记录的 128 条上限是明确合同；未把它或未明确承诺跨阶段补发的自定义流程计为漏洞或缺陷。
