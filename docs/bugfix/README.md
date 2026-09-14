# Bugfix 记录

`docs/bug/` 是只审查不改码的发现（RR 编号）；本目录记录这些发现是怎样被修掉的：
改了哪几行、为什么这样改、用什么测试证明修前红修后绿、留下了什么没做。
每条一个文件，编号沿用 RR，账本单元编号（U-）指向 [history/ledger.md](../history/ledger.md)。

| 编号 | 仓库 | 问题 | 修复单元 | 记录 |
| --- | --- | --- | --- | --- |
| RR-20260908-01 | kit | Session `Enter` 丢掉幂等账本 Create 的落败结果，两个 owner 都成功 | U-0154（方案 B） | [RR-20260908-01.md](RR-20260908-01.md) |
| RR-20260908-02 | core | ReadThrough 跟随者取消后不归还等待名额 | U-0155 | [RR-20260908-02.md](RR-20260908-02.md) |
| RR-20260908-03 | codegen | `--consolidate` 对单行 import 的混合分流产出非法 Go | U-0156 | [RR-20260908-03.md](RR-20260908-03.md) |
| RR-20260909-01 | core/docs | Quickstart 固定过时 codegen 版本且缺 `-module` | U-0157 | [RR-20260909-01.md](RR-20260909-01.md) |
| RR-20260909-02 | kit | Session 撞 RequestID 的撤回误删同 owner 重新取得的 claim（ABA） | U-0158 | [RR-20260909-02.md](RR-20260909-02.md) |
| RR-20260909-03 | core | Assembly 停机未完成就忘掉 Runtime，重试虚报成功 | U-0159 | [RR-20260909-03.md](RR-20260909-03.md) |
| RR-20260909-04 | codegen | 同包多个 Entity 生成重名注册符号 | U-0160 | [RR-20260909-04.md](RR-20260909-04.md) |
| RR-20260909-05 | kit | Enqueue 幂等重放返回其他 Subject 的 Ticket | U-0164 | [RR-20260909-05.md](RR-20260909-05.md) |
| RR-20260909-06 | codegen | 多 Entity 配合显式 `-output` 静默覆盖生成结果 | U-0162 | [RR-20260909-06.md](RR-20260909-06.md) |
| RR-20260910-01 | core | 极短 IdleTTL 推导出零扫描周期 | U-0163 | [RR-20260910-01.md](RR-20260910-01.md) |
| RR-20260910-02 | kit | 已领取邮件淘汰后重投生成新发奖 token | U-0165 | [RR-20260910-02.md](RR-20260910-02.md) |
| RR-20260910-03 | core | 完成事务的等待者因缓存淘汰读到已删记录而 panic | U-0168 | [RR-20260910-03.md](RR-20260910-03.md) |
| RR-20260910-04 | core | Checkpoint 恢复按 ID 重排完成历史,淘汰顺序分叉 | U-0169 | [RR-20260910-04.md](RR-20260910-04.md) |
| RR-20260910-05 | codegen | category 指向业务包常量时生成物遗漏 import(M-05 引入) | U-0166 | [RR-20260910-05.md](RR-20260910-05.md) |
| RR-20260910-06 | codegen | 聚合注册的固定 entity 导入与业务包别名冲突(M-05 引入) | U-0167 | [RR-20260910-06.md](RR-20260910-06.md) |
| RR-20260911-01 | kit | 墓碑按计数淘汰早于信封过期,再铸领取 token(U-0165 残余) | U-0171 | [RR-20260911-01.md](RR-20260911-01.md) |
| RR-20260911-02 | kit | `Mailbox.clone` 未复制 SettledClaims,快照与存储共享(U-0165 引入) | U-0170 | [RR-20260911-02.md](RR-20260911-02.md) |
| RR-20260911-03 | core | finalizer 停止后晚到的 Close 仍被接受,遗留 entries 与 slot | U-0173 | [RR-20260911-03.md](RR-20260911-03.md) |
| RR-20260911-04 | core | Nest 停机回收延迟消息而不回复等待者 | U-0172 | [RR-20260911-04.md](RR-20260911-04.md) |
| RR-20260913-03 | core | Remote payload 身份未与信封绑定,可写到别的 scope | U-0179 | [RR-20260913-03.md](RR-20260913-03.md) |
| RR-20260913-04 | core | 快照合并加载的等待名额在取消后泄漏 | U-0174 | [RR-20260913-04.md](RR-20260913-04.md) |
| RR-20260913-07 | core | L2 同版本比较遗漏 schema / codec | U-0176 | [RR-20260913-07.md](RR-20260913-07.md) |
| RR-20260913-08 | core | 快照的绝对过期时间未参与读取准入 | U-0175 | [RR-20260913-08.md](RR-20260913-08.md) |
| RR-20260913-10 | core | 快照 Lua 把 uint64 版本转浮点,排序失真 | U-0177 | [RR-20260913-10.md](RR-20260913-10.md) |
| RR-20260913-11 | core | 大 epoch 拼成科学计数法并持久化不可读 marker | U-0178 | [RR-20260913-11.md](RR-20260913-11.md) |
| RR-20260913-05 | core | L2 版本冲突被 IgnoreRemoteError 吞掉,L1/L2 同版本分叉 | U-0180 | [RR-20260913-05.md](RR-20260913-05.md) |
| RR-20260913-06 | core | L2 回填绕过同版本内容检查,覆盖已发布值 | U-0181 | [RR-20260913-06.md](RR-20260913-06.md) |
| RR-20260913-02 | core | 迟到的旧 release 取消更新的 renewal | U-0184 | [RR-20260913-02.md](RR-20260913-02.md) |
| RR-20260911-05 | kit | 被拒绝的投递仍修改 MemoryStore 邮箱(回调未 clone) | U-0182 | [RR-20260911-05.md](RR-20260911-05.md) |
| RR-20260911-06 | core | AfterCommit panic 遗漏回复与释放,饱和回退可崩溃 | U-0183 | [RR-20260911-06.md](RR-20260911-06.md) |
| RR-20260912-02 | core | WAL.Sync 只 fsync 文件,不等队列里已准入的记录 | U-0185 | [RR-20260912-02.md](RR-20260912-02.md) |
| RR-20260912-01 | core | Committer Flush / Shutdown 等 replay 所有权时不可取消 | U-0186 | [RR-20260912-01.md](RR-20260912-01.md) |
| RR-20260913-01 | core | Remote 快照删除无版本屏障,迟到删除清新值 / 旧值复活 | U-0187 | [RR-20260913-01.md](RR-20260913-01.md) |
| RR-20260913-09 | core | Transfer 回复丢失被当成没执行,旧 owner 恢复可写 | U-0188 | [RR-20260913-09.md](RR-20260913-09.md) |
| RR-20260913-12 | core | EnterShared / LeaveShared 回复丢失被当成没执行,恢复旧模式放行独占写 | U-0189 | [RR-20260913-12.md](RR-20260913-12.md) |
| RR-20260913-13 | core | LeaveShared 回复丢失后普通写重试卡在 `shared -> local_owned` 非法迁移 | U-0189(同一修复覆盖) | [RR-20260913-12.md](RR-20260913-12.md#2026-09-13-第十轮rr-20260913-13-由同一修复覆盖仍归-u-0189) |
| RR-20260914-01 | core | Sync 把 Close 发起当成排空完成,关闭进行中提前报告成功(U-0185 引入) | U-0190 | [RR-20260914-01.md](RR-20260914-01.md) |
| RR-20260914-02 | kit | OpenActivity 与 sweep 交错丢失窗口索引(条目无 opening / 确认生命周期) | U-0191 | [RR-20260914-02.md](RR-20260914-02.md) |
| RR-20260914-03 | kit | 派发循环只看本轮新完成,退避重试 / notify 完成 / heal 都失去入口 | U-0192 | [RR-20260914-03.md](RR-20260914-03.md) |
| RR-20260914-04 | core | lockstep 已入帧输入的迟到重传再次入帧(身份只活到目标帧被切) | U-0193 | [RR-20260914-04.md](RR-20260914-04.md) |
| RR-20260914-05 | core | lockstep CatchupBatchFrames=1 净补帧速度为 0,永不切回 live | U-0194 | [RR-20260914-05.md](RR-20260914-05.md) |
| RR-20260914-06 | core | lockstep 座位 -1 与旁观者哨兵碰撞 | U-0195 | [RR-20260914-06.md](RR-20260914-06.md) |
| RR-20260914-07 | core | lockstep 座位数未对齐 wire 的 MaxFrameInputs | U-0196 | [RR-20260914-07.md](RR-20260914-07.md) |
| RR-20260914-08 | core | lockstep 去重身份表准入无上限,只靠 Advance 回收(U-0193 引入) | U-0197 | [RR-20260914-08.md](RR-20260914-08.md) |

重构类改动(不对应任何 RR,不关闭任何 RR)另记,编号 M-:

| 编号 | 仓库 | 改了什么 | 记录 |
| --- | --- | --- | --- |
| M-01 | core | entity kind 注册表改为按 kind 的无锁定长表;含 category 重设计四步方案,仅第一步已实施 | [M-01-entity-kind-registry.md](M-01-entity-kind-registry.md) |
| M-02 | core | category 离开 EntityID,注册表成为唯一权威;ID 那两位降为历史填充,零数据迁移 | [M-02-category-leaves-the-id.md](M-02-category-leaves-the-id.md) |
| M-03 | core | 声明 category 后值即锁序,remote 档强制最先,锁序注册期派生;新增 `ValidateEntityRegistry` | [M-03-category-lock-order.md](M-03-category-lock-order.md) |
| M-04 | core + codegen | **破坏性**:删除 `RemotePolicyCapable`、`GetEntityGroupFunc`、`EntityGroup*`;推荐分类常量;codegen 拒绝 `remote=capable`、脚手架默认 Other | [M-04-drop-capable-and-the-group-hook.md](M-04-drop-capable-and-the-group-hook.md) |
| M-05 | codegen | `category=` 进实体标记并直接进生成物,拆掉运行期查表的隐式前置;生成的聚合注册末尾调 `ValidateEntityRegistry` | [M-05-marker-owns-the-category.md](M-05-marker-owns-the-category.md) |

写法约定：**问题**（一句话）→ **根因**（指向具体行）→ **方案选择**（列出考虑过的方案与取舍）→
**改动**（文件与要点）→ **证明**（红测试名、修前失败文本、修后结果）→ **未做 / 边界**。
前四项已随 core v1.15.2 / kit v1.14.3 / codegen v1.15.4（2026-09-09）发版；RR-20260909-02/03/04 已修复待下次发版（core v1.15.3 / kit v1.14.4 / codegen v1.15.5）。

## 2026-09-13 第三轮:前两轮推后的四项已收敛

| 编号 | 上轮说"要先定的契约" | 这轮定成了什么 |
| --- | --- | --- |
| RR-20260913-01 | 删除水位保留多久、与可重放窗口怎么协调 | `DeleteAtVersion` 与 `Publish` 共用分片锁;墓碑按 `TombstoneTTL`(默认 = 缓存 TTL)收界,更新的快照清墓碑(U-0187) |
| RR-20260913-09 | 所有权"结果不确定"状态 | 失效 marker、独立有界重查权威:未变→恢复,已转移→按成功 fence,查不到→`Recovering` 冻结直到下一次权威读成功(U-0188) |
| RR-20260912-01 | 可取消的锁等待 | `flushMu` / `replayMu` 换成一格信号量,`select` 上 ctx(U-0186) |
| RR-20260912-02 | 有序屏障 / LSN 水位 | Sync 往 appendCh 放 `barrier` 请求,收批即答,答后再 fsync(U-0185) |

未修复清单为空。EnterShared / LeaveShared 的同类失败恢复第九轮登记为 RR-20260913-12,已由 U-0189 收敛(骨架参数化)。

### 第七轮复核后的补修(不新编号,记在原 RR 记录末尾)

| 原 RR | 归属 U | 残余 | 补修 |
| --- | --- | --- | --- |
| RR-20260913-01 | U-0187 | 在途 L2 回填越过墓碑;冷 L1 的旧删除清掉较新的 L2 | `StoreConfig.Superseded` 让三条 L1 写入口共用删除水位;L2 新增 `DeleteAtVersion` 脚本 |
| RR-20260913-05 | U-0180 | 预检查之后的 L2.Set 冲突仍被 IgnoreRemoteError 吞掉 | `ReadThroughOptions.FatalRemoteError` 分类,冲突不降级 |
| RR-20260913-02 | U-0184 | 同一时钟刻度创建的两个 Manager 代际不递增(Windows 实测) | 进程级代际高水位,播种取 `max(now, 已发出+1)` |
