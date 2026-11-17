# main v1.50.0 修复复验与增量审查（2026-11-16）

> 2026-11-17 更新：RV-20261116-01 已在 main 修复，原三类故障及对照各三轮 race 复验通过，见 [修复复验](FIX-VERIFICATION-20261117.md)。下文是修前检出的历史结论。

审查对象：main **f2c1ac4434334630569f93ecda49191673965e1a**，v1.50.0 发布提交 `4086b694a`；相对上次审查的 `603dd1394dcf892a25bfaa142454804a723de554` 共 836 文件、+12985/-2356，包含大量 `game/<功能>` → `game/modules/<功能>` 路径调整。收尾再次 fetch，main 未变化。被测源码是独立 main worktree，不是本固定审查分支的旧运行时。

**结论：换服原失败路径修复复验通过；新增一项 P2，room 的提交后连接登记依赖控制器回包，乱序或取消等待都会漏掉当前席位。** 三种失败路径各三轮复现，三组顺序/正常等待对照均通过。仅提交报告与探针至 `fix/core-game-server-review`，不改运行时、不提 issue / PR。

## RV-20261116-01：room 连接登记不随提交可靠、按代际更新（P2）

引入：`58e408ce5`（D24），v1.49.0 起存在；v1.50.0 路径迁移后仍存在。定位：

- [RoomJoin 第 28–32 行](https://github.com/tjbdwanghaibo/roost-core/blob/f2c1ac4434334630569f93ecda49191673965e1a/modules/room/files/game/controllers/room/room_join.go.tmpl#L28)：仅 `Sync_RoomJoin` 成功返回时 Track；Reconnect 同样处理。
- [Presence.Track 第 133–146 行](https://github.com/tjbdwanghaibo/roost-core/blob/f2c1ac4434334630569f93ecda49191673965e1a/modules/room/files/game/modules/room/room_presence.go.tmpl#L133)：无代际比较，先删同玩家其他连接，再写入返回的 epoch。
- [RoomLeave 第 34–35 行](https://github.com/tjbdwanghaibo/roost-core/blob/f2c1ac4434334630569f93ecda49191673965e1a/modules/room/files/game/controllers/room/room_leave.go.tmpl#L34)：提交后调用不带 epoch/connection 条件的 `Forget`，撤销该玩家的所有登记。

Nest 只串行化房间操作，不保证控制器在收到结果后仍按提交顺序运行；同步等待取消也不撤销已经准入的事务。三条确定性路径：

1. **旧 Join 覆盖新 Reconnect**：玩家已有 epoch=1 的席位；重复 Join 在真实 Nest 提交完成后暂停回包。另一控制器完成 Reconnect 并登记 epoch=3 的新连接。旧 Join 恢复后 Track epoch=1，删掉新连接登记。两条连接都关闭，旧代际 Disconnect 被正确拒绝，Presence 随后删掉旧登记；新席位一直 Connected=true。
2. **旧 Leave 撤掉新 Join**：保留另一名成员，确保房间不因最后一人离开而关闭。Leave 提交后暂停回包；同玩家新连接重新入房并登记新 epoch；旧 Leave 恢复调用 Forget，把新席位登记也删掉。新连接关闭后无人处理。
3. **等待已取消、Join 仍提交成功**：真实 Nest 开始 strict 提交后，用提交器屏障暂停，取消请求的 BaseCtx。真实 Sender 返回 `ErrNestCanceled`，控制器跳过 Track；释放提交屏障，事务返回成功。后续 RoomView 确认席位已经存在且在线，但连接关闭及反复 Sweep 都不能将其置离线。

共同影响：Presence 的 Sweep 只扫描已登记的连接，不扫描 Room DAO；因此不是“最多等 30 秒就能恢复”。席位的真实代际检查防止旧断线伤到新连接，却无法补回丢失的登记。依赖 Connected 做超时托管、离线提示等玩法会读到错误状态，直至显式重连、业务主动 Disconnect 或房间重新装载。本问题不声称产生资产重复或越权。

探针通过当前 CLI 生成的工程运行真实 Controller、生成 Sender、Nest、Room 组件及 Presence；连接层和持久提交器为替身。时序探针只延迟已经成功的 Request 返回，不修改业务；取消探针真实取消 Nest 等待，提交器模拟已经开始且最终成功的持久操作。每条失败都有正常顺序/成功等待对照：

| 路径 | 三轮结果 |
| --- | --- |
| Join → Reconnect，正常回包顺序 | 3 PASS |
| Join → Reconnect，旧回包迟到 | 3 FAIL |
| Leave → Rejoin，正常回包顺序 | 3 PASS |
| Leave → Rejoin，旧回包迟到 | 3 FAIL |
| Join，正常等待提交 | 3 PASS |
| Join，取消等待后提交成功 | 3 FAIL |

实验组稳定得到 `Connected=true`、`tracked=1`，唯一剩余登记是观察者，不是已经断开的玩家。日志没有 race 告警；这是逻辑时序问题。

建议修复契约：让连接登记/撤销由成功提交这一事实驱动，不能仅由控制器是否收到成功回包驱动；提交后的更新要使用房间席位代际保证单调，撤销也应限定对应代际/连接，不能把新席位删掉。仅给 Track 加 epoch 比较不能覆盖 Leave 的无条件撤销或取消等待路径。补以上三个回归，并保留原有“关闭先于登记”“旧断线不影响新连接”用例。

## 上次 RV-20261114-01 的修复判断

`3a062e642` 的改法与原问题吻合：Copy 结果未知时返回可能写入的文档清单；Driver 最多重试三次，成功后继续迁服，持续未知且 account 中止时先删目标副本，再撤源服保护。account 已在别处提交的分支不会误按中止删除目标数据。

- **原审查探针原样复跑三轮全部通过**：丢一次提交应答后 `copy_calls=2`，玩家最终绑定目标服，原来的“已中止但留目标副本”断言不再失败。
- core 新增 `TestCopyHandedOffEntityLostCommitReplyListsWhatMayHaveBeenWritten`，race 三轮通过：事务已提交、返回 unknown 清单；重试识别 already_present；按清单删除；确定冲突不伪造清单。
- rehome 三个新增场景各 race 三轮通过：丢回包后重试成功、连续未知后中止清理、连续未知但已在别处提交时补搬并收尾。

这是原失败路径的关闭，**不是全部换服故障窗口关闭**。当前 main 的 [KNOWN-LIMITS 搬档说明](https://github.com/tjbdwanghaibo/roost-core/blob/f2c1ac4434334630569f93ecda49191673965e1a/docs/maintenance/KNOWN-LIMITS.md#L529) 仍声明：三次失败后，迟到的提交若晚于删副本才落地，仍可能留下目标档；未知期间重启的自动恢复依赖 D8。Q4 的 committed 后源副本崩溃窗口、D7/D8 也仍列为遗留。本轮未重新执行这些真实故障演练，不把已声明窗口另算新发现。

## 覆盖与验证

源码重点覆盖：WAL 独立刷盘的水位、等待者、轮转与关闭；Nest 显式异步完成的注册与接线；后台 checkpoint 的成功前缀、错误传播与生命周期；Remote 空认领的比较删除及结果未知；CellWorld 按来源订阅/解码、清理与关闭；账号区服上报、chat 同频道写排队；manager 启停依赖和 rehome / room 业务。以上除 room 外未确认新缺陷，不表示全部路径已穷尽。代码图谱工具不可用，使用提交差异、精确源码和 rg。

12 个相关 Go 包的完整默认 race 测试通过，另七项 codegen manager 定向测试通过；正式生成工程的 10 个模块及 1 项联动共 248 场景、零跳过，build / vet / ID / 生成物检查全部通过。验证统计、命令误写和一次探针写入干扰的说明见证据 README，均保留原始日志。Linux amd64、Go 1.27.2，GOWORK=off；生成 CLI 和运行依赖均来自本次 main。默认包测试没有启用 integration 标签；生成场景是离线替身验收，不能当作真实 Mongo/NATS/Redis HA、TCP/Gate、跨机和 Unity 联合验收。

未执行全仓测试、完整模块/13 类玩法矩阵、真实进程故障与容量长稳；没有删除旧压测材料或执行新压测。本报告不作 3000 在线、单服 10 万角色的容量结论，也不据测试通过给出无条件上线许可。

[复现方式、验证统计与日志](evidence/main-review-20261116/README.md)。
