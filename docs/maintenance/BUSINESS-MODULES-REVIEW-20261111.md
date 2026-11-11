# feat/business-modules 分支审查（2026-11-11）

审查基线：`feat/business-modules` **fd0de4cf6e321c032e72afe18cd6cf4670e94b05**；与 main 的共同基线为 `f78495440c52ff8d651a35b4b5eaa5758014815e`。本次差异共 322 个文件，新增 25081 行、删除 2663 行。结束前再次核对远端，分支仍为上述提交。

确认三项 P2 缺陷，均保留三轮实验失败与相邻控制组通过的证据。只提交报告和探针，没有修复运行时代码。报告放在固定分支 `fix/core-game-server-review`；该分支自身的旧运行时代码不是本轮被测对象，复现必须使用以上目标提交。

## RV-20261111-BM-01：中止后的迟到 Begin 留下无法自动收尾的目标占位（P2，未修复）

位置：[BeginRoleTransfer 占位与返回](https://github.com/tjbdwanghaibo/roost-core/blob/fd0de4cf6e321c032e72afe18cd6cf4670e94b05/service/account/transfer.go#L154)、[holdTarget](https://github.com/tjbdwanghaibo/roost-core/blob/fd0de4cf6e321c032e72afe18cd6cf4670e94b05/service/account/transfer.go#L504)、[settleTransfer](https://github.com/tjbdwanghaibo/roost-core/blob/fd0de4cf6e321c032e72afe18cd6cf4670e94b05/service/account/transfer.go#L554)。

确定时序：

1. Begin A 已把 `Role.Transfer` 写入，随后停在目标 `Slots.Create` 实际写入之前。
2. Abort A 写入已中止结果，看到目标 slot 尚不存在，收尾后记 `LastTransfer.Settled=true`，返回成功。
3. 恢复 Begin A，它依据旧 role 创建目标占位，不复核权威结果，返回成功和旧的“正在换服”绑定。
4. 重发 Abort A、执行 SelectRole 均成功，但都因 Settled 为 true 跳过占位清理。新的 Begin B 去同一目标服得到 `ErrTransferInProgress`，虽然 role 上已没有在途换服。

结果：账号的目标服名额被已中止请求占住，普通重试和文档承诺的自动收尾入口无法恢复；同服建角也会被占位阻断。这里没有声称源服角色不能登录。

探针用 channel 固定存储调用交错，不靠 sleep 或概率碰撞。控制组先完成 Begin 再 Abort，不留占位，新的 Begin 成功；延迟组连续三轮失败，race 未报数据竞争。这是跨记录业务竞态，普通 race 检测不能代替该检查。

修复方向：Begin 的占位完成也要与角色当前结论对账；已经中止或换代时条件撤掉本次新留的占位，并返回权威绑定。还需保证“占位刚写完进程就退出”也能被后续恢复发现，不能只增加一次不持久的尾部检查。不能误删已提交的正式 slot 或新一代占位。

关闭判据：覆盖占位 Create 前、后与 Abort/Commit 的交错及中途失败；重复 Abort、SelectRole、下一次 Begin 能收敛，角色与 slot 不产生永久矛盾。

## RV-20261111-BM-02：连续中止会遗忘旧请求，迟到 Begin 可重新启动已取消换服（P2，未修复）

位置：[Abort 覆盖 LastTransfer](https://github.com/tjbdwanghaibo/roost-core/blob/fd0de4cf6e321c032e72afe18cd6cf4670e94b05/service/account/transfer.go#L332)、[standingOf / decideBegin](https://github.com/tjbdwanghaibo/roost-core/blob/fd0de4cf6e321c032e72afe18cd6cf4670e94b05/service/account/transfer.go#L377)。

顺序调用即可复现：`Begin A(epoch=0) → Abort A → Begin B(epoch=0) → Abort B → 原样重放 Begin A(epoch=0)`。最后一步成功，重新写入 Transfer A 和目标占位。

原因：只保存最近一条 LastTransfer；Abort 不改变 BindEpoch。B 的中止覆盖 A 的终态之后，A 被判定为 unknown，而原请求携带的 epoch 仍然合法。本例没有让调用方给新业务复用旧 ID，而是重放原来的完整请求；因此“要求调用方分配唯一 ID”挡不住它。对单次中止的控制组，原样重放 A 正确返回 `ErrTransferStale`；多一次中止的实验组三轮均重启 A。

影响：文档给出的“Begin 结果未知时 Abort 拿确定结论”不能跨下一次中止成立，旧消息可再次占名额、留下换服在途事实；后续按在途事实向前恢复的接线可能继续这个已放弃的意图。没有把它描述成 account 自己会直接搬移实体。

修复方向：引入覆盖每次尝试及中止的持久代际/拒绝依据，或明确有可证明重放期限的终态保留方案；仅保留最后一次且中止不推进任何代际不够。单纯扩大一个淘汰环仍须说明淘汰后如何拒绝旧请求。

关闭判据：两次及更多中止后，任意旧 Begin/Commit 的重放都不能恢复已中止意图；当前新请求和已提交重试仍正确。

## RV-20261111-BM-03：Wallet.Add 的最小整数扣款绕过余额不足检查（P2，未修复）

位置：[wallet_component.go.tmpl:115](https://github.com/tjbdwanghaibo/roost-core/blob/fd0de4cf6e321c032e72afe18cd6cf4670e94b05/modules/wallet/files/game/entities/player/wallet_component.go.tmpl#L115)。

余额检查为 `delta < 0 && coins < -delta`。`delta == math.MinInt64` 时取负仍为 MinInt64，非负余额无法进入不足分支，随后成功写入 `coins + delta`。

在同提交 CLI 正式生成、安装模块的工程中增加仅用于探针的服务端 Nest handler，调用 `WalletComp().Apply(key, delta)`。余额 100 时：

```text
delta=-101                  err=wallet_short  stored=100                   receipts=1
delta=-9223372036854775808   err=<nil>         stored=-9223372036854775708   receipts=2
```

三轮相同；负余额和成功扣款回执一起经生成 Sender / Nest / DAO 路径提交。使用替身 committer，未声称已经写到真实 Mongo。

影响范围：文档公开推荐的服务端 Add/Apply 组合入口。本模块自带 WalletSpend PB 先要求正 Amount，其取负不能产生 MinInt64；所以本轮没有证明现成客户端协议可直接触发。问题仍应在可复用组件边界拒绝，不能要求所有上层调用方知道这个整数特例。

修复方向：对非负余额用不会取负 MinInt64 的比较，或显式拒绝该数值；拒绝时余额和账本均不变。补覆盖 Add 与 Apply，以及最大合法正向加款、普通不足、事务拒绝回滚。

## 通过的验证与边界

Linux、Go 1.27.2、`GOWORK=off`。图谱工具不可用，使用精确源码、rg、正式 CLI 与定向实验。主要审查了模块安装/生命周期、钱包/背包/签到/任务/商店/体力/档案、邮件领取、排行榜提交，以及 account 换服与 DataEngine 交出实体的关键路径；demo 迁移代码做了相关调用点检查，未重新执行完整 game-demo 真服务演练。

| 验证 | 结果 | 证据 |
| --- | --- | --- |
| 原有 account 整包 `go test -race -count=1 ./service/account`（加入探针前） | 通过，MemoryStore | [日志](evidence/business-modules-review-20261111/account-baseline.log) |
| `codegen/internal/roost`，模块与 Runtime 定向筛选（命令见证据说明），count=1 | 通过，包含模块生命周期生成适配层测试 | [日志](evidence/business-modules-review-20261111/codegen-baseline.log) |
| `codegen/internal/dao` 整包 count=1 | 通过 | [日志](evidence/business-modules-review-20261111/dao-baseline.log) |
| DataEngine 交出实体与 checkpoint 定向 race | 10 个顶层测试通过 | [日志](evidence/business-modules-review-20261111/handoff-baseline.log) |
| 新工程 `example.com/businessaudit` 一次安装十二模块；`roost module verify --json` | 206 个模块/联动场景通过，0 skip；build/vet/id check/generate --check 四项通过 | [结果](evidence/business-modules-review-20261111/verify-final.json) |
| 两项 account 缺陷与 wallet 缺陷 | 各三轮实验失败、相邻控制组通过 | [account](evidence/business-modules-review-20261111/account-probes.log)、[wallet](evidence/business-modules-review-20261111/wallet-probes.log) |

首轮生成工程 build 因临时目录未初始化 Git 导致 VCS 状态读取失败；初始化临时工程仓库后重新执行 module verify，上表为最终结果。钱包探针初版直接在 RunLocal 中写 DAO，被事务保护拒绝，已改为生成 Nest handler 后才作为缺陷证据；最终日志对应正式调用链。

`rehome` 业务模块、冷准入与 demo 换服接线尚未完成，分支方案已明确登记，本轮不把“计划未完成”另列为新 bug。现有 wallet/shop 有界回执窗口、活动尽力投递等明确契约，也不算意外缺陷。

未执行全仓全量测试、完整 game-demo 网络验收、真实 Redis/Mongo/NATS 联合故障测试、Unity、跨机和容量测试。DataEngine 真实 Mongo 用例带 integration 标签，本轮未启用，不能把默认未编入当作通过。模块场景使用替身提交器；通过 206 个场景不等于分支无 bug，更不是 3000 在线生产容量结论。没有运行新压测，没有创建 issue/PR。

复现文件和操作见 [证据说明](evidence/business-modules-review-20261111/README.md)。
