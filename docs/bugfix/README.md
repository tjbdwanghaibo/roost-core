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
| RR-20260914-09 | core | LockstepBot 把接收游标当应用游标,回调失败后丢失批次尾帧 | U-0198 | [RR-20260914-09.md](RR-20260914-09.md) |
| RR-20260914-10 | core | statesync 同 tick 重投影覆盖 sent[tick],迟到 ACK 绑错基线 | U-0200 | [RR-20260914-10.md](RR-20260914-10.md) |
| RR-20260914-11 | core | statesync ACK 路径清 forceFull,旧发送的 ACK 取消恢复意图 | U-0201 | [RR-20260914-11.md](RR-20260914-11.md) |
| RR-20260914-12 | core | statesync 单片重组绕过 MaxFrameBytes | U-0202 | [RR-20260914-12.md](RR-20260914-12.md) |
| RR-20260914-13 | core | statesync LOD 按绝对 tick 采样,错相发送的组件永远保留旧值 | U-0203 | [RR-20260914-13.md](RR-20260914-13.md) |
| RR-20260915-01 | core | statesync ApplyDelta 每步用最终存量上限检查暂存 map,满容量替换看 ID 排序决定成败 | U-0204 | [RR-20260915-01.md](RR-20260915-01.md) |
| RR-20260915-02 | core | statesync 重叠准备的不同视图已交付后 Commit 被拒,基线静默分叉(U-0200 残余窗口) | U-0205 | [RR-20260915-02.md](RR-20260915-02.md) |
| RR-20260915-03 | core | entitysync 持久化水位门槛只在 FlushSubject,订阅快照 / profile 切换 / 直接分发绕过 | U-0206 | [RR-20260915-03.md](RR-20260915-03.md) |
| RR-20260915-04 | core | room 慢连接剔除通知绑在批次结果上,剩余批次失败即丢失,room 保留失效订阅 | U-0207 | [RR-20260915-04.md](RR-20260915-04.md) |
| RR-20260915-05 | core | room SetDownstream 只换指针不迁移慢消费者回调,替换后剔除不清理订阅 | U-0208 | [RR-20260915-05.md](RR-20260915-05.md) |
| RR-20260915-06 | core | syncstream 清理后重建的流从序号 1 重来,旧 ACK / Resync 吞掉新内容 | U-0215 | [RR-20260915-06.md](RR-20260915-06.md) |
| RR-20260915-07 | core | syncstream 恢复时忽略的半条 WAL 尾部未截断,续写后下次启动不可读 | U-0211 | [RR-20260915-07.md](RR-20260915-07.md) |
| RR-20260915-08 | core | syncstream 绑定 journal 的 Import / Restore 只换内存不发布 | U-0213 | [RR-20260915-08.md](RR-20260915-08.md) |
| RR-20260915-09 | core | syncstream Recover 把过期捕获提交为更新的 Full | U-0214 | [RR-20260915-09.md](RR-20260915-09.md) |
| RR-20260916-01 | core | syncstream journal 写入 / 发布结果不确定后继续写,重复序号或丢追加 | U-0212 | [RR-20260916-01.md](RR-20260916-01.md) |
| RR-20260916-02 | core | room JetStream 持久消费者身份不含 Prefix,共用 Stream 时撞名 | U-0210 | [RR-20260916-02.md](RR-20260916-02.md) |
| RR-20260916-03 | core | room JetStream 同 topic 多个本地订阅竞争同一消费者,广播变分摊 | U-0209 | [RR-20260916-03.md](RR-20260916-03.md) |
| RR-20260916-04 | core | syncstream Recover 的位置核对识别不出删除 ABA / 同位置 Import,旧捕获仍被提交 | U-0216 | [RR-20260916-04.md](RR-20260916-04.md) |
| RR-20260916-05 | kit + codegen | match 的 Grouping 注入参数无执行者;**破坏性**:`NewMod(reporter)`,codegen 不再生成 `Grouping()` | U-0217 | [RR-20260916-05.md](RR-20260916-05.md) |
| RR-20260916-06 | core | manager：最后一个管理器启动期间 Stop，成功启动者漏清理（交接不在同一把锁下） | U-0219 | [RR-20260916-06.md](RR-20260916-06.md) |
| RR-20260916-07 | core | manager：首个管理器启动期间 Register 被接纳，新对象永不启动也不停止 | U-0220 | [RR-20260916-07.md](RR-20260916-07.md) |
| RR-20260917-01 | core | match：合法 Queue 键碰撞（Mode / Partition 含冒号），跨队列读票与成组 | U-0221 | [RR-20260917-01.md](RR-20260917-01.md) |
| RR-20260917-02 | core | match：ScoreWindow 距离 / 窗口 int64 溢出，远端成组、cap 失效 | U-0222 | [RR-20260917-02.md](RR-20260917-02.md) |
| RR-20260917-03 | core | match：内存 Store 输入 / 返回切片与存储共享 | U-0223 | [RR-20260917-03.md](RR-20260917-03.md) |

| RR-20260919-01 | codegen | 顶层 nested 指针字段替换 / 回滚后没有解绑离开的对象，游离对象仍能提交该字段 | U-0249 | [RR-20260919-01.md](RR-20260919-01.md) |
| RR-20260919-07 | core+kit | 没有订单的索引条目永远排在页首、占住每一页的槽位 | U-0256 | [RR-20260919-07.md](RR-20260919-07.md) |
| RR-20260919-10 | core+kit+codegen | activity 的 dispatch 没有交付者：sweep 空耗尝试次数、游戏端只能猜两个窗口 | U-0257 | [RR-20260919-10.md](RR-20260919-10.md) |
| RR-20260920-03 | core+codegen | 玩家租约的 `GET` 之后再 `EXPIRE`/`DEL`：旧 owner 会给新 owner 续期或删掉它 | U-0258 | [RR-20260920-03.md](RR-20260920-03.md) |
| RR-20260920-04 | codegen | 租约丢了不阻断本地写入：续租结果被丢弃，实体既不卸载也不停服务 | U-0259 | [RR-20260920-04.md](RR-20260920-04.md) |
| RR-20260920-02 | core | 普通 room delta 走 latest-only 通道，待发帧被下一帧替掉，独有字段永久丢失 | U-0260 | [RR-20260920-02.md](RR-20260920-02.md) |
| RR-20260920-01 | core | snapshot 的 checksum 是完整 64 位散列，BSON 装不下高位为 1 的那一半；失败记录还会成为启动毒丸 | U-0261 | [RR-20260920-01.md](RR-20260920-01.md) |
| RR-20260920-07 | core | `SmallSafeMap` 的 BSON 方法签名不符合驱动接口，从未生效，该类型被写成空文档 | U-0265 | [RR-20260920-07.md](RR-20260920-07.md) |
| RR-20260920-08 | core | `OpTimeout` 在拿到写闸之后才生效，排队没有上界（调用方无 deadline 时无限等） | U-0266 | [RR-20260920-08.md](RR-20260920-08.md) |
| RR-20260920-06 | codegen | 被房间拒绝的 subscribe 只记一条日志就丢掉，那个观察者永久收不到那个 subject | U-0267 | [RR-20260920-06.md](RR-20260920-06.md) |
| RR-20260920-09 | codegen | 租约失而复得后仍用失效期间没重新加载过的常驻 Player 实体；有间断就扔副本 | U-0268 | [RR-20260920-09.md](RR-20260920-09.md) |
| RR-20260920-10 | codegen | 后台为离线玩家取得的租约永不归还，玩家被钉在一个进程上；空闲即归还（先扔副本再还租约） | U-0269 | [RR-20260920-10.md](RR-20260920-10.md) |
| RR-20260920-11 | codegen | 为新工作重新取得的租约带着旧时间戳被当成空闲还掉；拆开续租与认领两种事件，并在归还前先停准入 | U-0271 | [RR-20260920-11.md](RR-20260920-11.md) |
| RR-20260920-12 | codegen | 撤离的 ctx 预算不覆盖它要等的实体锁，一个忙实体钉住整轮刷新；撤离改成跑到底，预算只限制调用方等多久 | U-0272 | [RR-20260920-12.md](RR-20260920-12.md) |
| RR-20260920-05 | core+kit | 读不回来的订单永久占住重试页，健康订单永远进不了 AttemptDelivery | U-0262 | [RR-20260920-05.md](RR-20260920-05.md) |
| RR-20260919-08 | core | 两层共享加载没有 defer 收尾，一次 loader panic 让该实体永久加载不了 | U-0254 | [RR-20260919-08.md](RR-20260919-08.md) |
| RR-20260919-09 | core | 缺失实体 `(nil,nil)` 在 single / broadcast 处被解引用；broadcast 还会中止后续实体 | U-0255 | [RR-20260919-09.md](RR-20260919-09.md) |
| RR-20260919-02 | codegen | 同一个 nested 指针占两个字段 / key 时通知是单槽的，只有最后一个会落库 | U-0252 | [RR-20260919-02.md](RR-20260919-02.md) |
| RR-20260919-03 | codegen | 会话关闭的订阅者 panic 逃出派发 goroutine，整个 game 进程崩溃 | U-0247 | [RR-20260919-03.md](RR-20260919-03.md) |
| RR-20260919-04 | core+kit+codegen | 订单先持久化、待办索引后写；中间崩一次就留下后台永远枚举不到的已付款订单 | U-0253 | [RR-20260919-04.md](RR-20260919-04.md) |
| RR-20260919-05 | codegen | 待发货索引里一条读不出来的订单让整页失败，后面的健康订单永远拿不到重试 | U-0248 | [RR-20260919-05.md](RR-20260919-05.md) |
| RR-20260919-06 | codegen | 未领取的付费 grant 固定 30 天后被拒绝并删除，而订单早已 delivered，形成永久少发货 | U-0251 | [RR-20260919-06.md](RR-20260919-06.md) |

用户复审 / 自查直接发现、没有 RR 编号的修复另记,编号沿用账本单元:

| 编号 | 仓库 | 问题 | 记录 |
| --- | --- | --- | --- |
| U-0270 | codegen | 发布清单里的 codegen 版本停在 v1.15.19，受保护的 framework-release 闸自 v1.15.22 起连红十次，`framework-lock.json` 十个版本没有产出（进度盘点对照 CI 发现） | [U-0270-framework-release-version-drift.md](U-0270-framework-release-version-drift.md) |
| U-0263 | kit | U-0257 加的 owed 索引是一个新键空间却没登记，`TestPerPackageKeyNamespacesDoNotCollide` 红（CI 发现） | [U-0263-activity-owed-namespace.md](U-0263-activity-owed-namespace.md) |
| U-0264 | codegen | 声明的框架版本下限是假的：生成物在 core v1.14.0 / kit v1.13.0 上编译不过（CI 发现） | [U-0264-generator-version-floor.md](U-0264-generator-version-floor.md) |
| U-0199 | core | lockstep `SubmitInput` 先按客户端帧号索引身份环再校验(32 位平台越界 panic + 垃圾帧号分配环) | [U-0199-submit-input-validation-order.md](U-0199-submit-input-validation-order.md) |
| U-0218 | codegen | 托管服务 collaborators 无条件 import 服务包,U-0217 后 match 工程 "imported and not used"(发版验证发现,v1.15.6 补丁) | [U-0218-collaborators-unused-import.md](U-0218-collaborators-unused-import.md) |
| U-0224 | codegen | dao 生成的嵌套 struct 无 BSON 表示，落库 / 回滚快照 / 同步只剩 `{"dirtyhook": {}}`；加 `bson:"-" json:"-"` 并生成 MarshalBSON / UnmarshalBSON（用户复审提出） | [U-0224-dao-nested-bson.md](U-0224-dao-nested-bson.md) |
| U-0250 | codegen | handler 参数名写成 `_` 时生成的 sender 声明并传递空白名，工程编译不过（修 RR-20260919-06 时撞上） | [U-0250-nest-blank-parameter-name.md](U-0250-nest-blank-parameter-name.md) |
| U-0246 | codegen | DAO 字段名小写之后是 Go 关键字（`Type` → `type`），生成物编译不过，错误指向临时文件（加 demo 计时器节点时自查） | [U-0246-dao-keyword-field-names.md](U-0246-dao-keyword-field-names.md) |
| U-0245 | codegen | 新建的 DAO 不接嵌套回调，第一次存盘前的嵌套写入悄悄丢掉（加 demo 嵌套字段时自查） | [U-0245-fresh-dao-nested-wiring.md](U-0245-fresh-dao-nested-wiring.md) |
| U-0244 | codegen | spawner 从 `fctx.RuntimeConfig()` 读 sid，configdata 覆盖该槽位后 sid 为 0，工程起不来（自查，已随 v1.15.11 发出） | [U-0244-spawner-sid-source.md](U-0244-spawner-sid-source.md) |
| U-0243 | codegen | 生成的接入层不通知会话关闭，空闲世界里断线成员永远留着（RR-20260918-06） | [RR-20260918-06.md](RR-20260918-06.md) |
| U-0242 | codegen | 运行期实体 id 由进程本地计数器发号，多实例碰撞（RR-20260918-09） | [RR-20260918-09.md](RR-20260918-09.md) |
| U-0241 | core+codegen | 邮件账本按固定 31 天清理，而 send_ttl 只要求为正数；账本先忘、信封还可领（RR-20260918-05） | [RR-20260918-05.md](RR-20260918-05.md) |
| U-0240 | core | `spatial.InterestConfig` 对单个观察者订阅的格数没有上界，合法配置可登记 40,401 格（RR-20260918-08） | [RR-20260918-08.md](RR-20260918-08.md) |
| U-0239 | codegen | `syncTopic` 的裸标识符被静默当成字面量，实体订阅到常量的名字（RR-20260918-07） | [RR-20260918-07.md](RR-20260918-07.md) |
| U-0238 | codegen | 顶层 DAO 容器换掉成员后不解绑旧值，游离对象能以原 key 写回持久化补丁（RR-20260918-10） | [RR-20260918-10.md](RR-20260918-10.md) |
| U-0237 | codegen | 清关奖励账本按"别的服务会忘掉 run"收界，而 session 的 run 没有存储 TTL；清理后重放旧 run 再发一次（RR-20260918-04） | [RR-20260918-04.md](RR-20260918-04.md) |
| U-0236 | codegen | 嵌套 child 的通知归属散写在各 setter 分支，undo 不搬 callback、`*Child` 不解绑旧值；回滚后恢复的 child 漏出持久化链（RR-20260918-03） | [RR-20260918-03.md](RR-20260918-03.md) |
| U-0235 | codegen | attribute 的 runtime.go 文件头自造标记，doctor 判成应用自有文件、project-templates 整项失败（自查发现，已随 v1.15.10 发出） | [U-0235-attribute-runtime-header.md](U-0235-attribute-runtime-header.md) |
| U-0234 | kit | platform 后台重试的候选来源固定为空，可恢复的发货失败长期挂起（RR-20260917-04） | [RR-20260917-04.md](RR-20260917-04.md) |
| U-0233 | core | RoomBroadcaster 私有持有 coordinator，房间层没有接入持久化水位的入口（RR-20260918-02） | [RR-20260918-02.md](RR-20260918-02.md) |
| U-0232 | codegen | 嵌套 DAO 的第二层 child 变更不通知父级，改动进不了 patch（RR-20260917-05，Wanted-02 转入） | [RR-20260917-05.md](RR-20260917-05.md) |
| U-0231 | core | 默认 Saga Assembly 不订阅原生 Nest 完成效果，saga 永远 waiting（RR-20260917-07，Wanted-04 转入） | [RR-20260917-07.md](RR-20260917-07.md) |
| U-0230 | core+codegen | attribute feature 只有生成器、没有运行时契约，生成物引用七个无人提供的类型（RR-20260917-06，Wanted-03 转入） | [RR-20260917-06.md](RR-20260917-06.md) |
| U-0229 | codegen | sync=true 实体生成物写了 Core 没有的 FlushPolicy / SubjectPackerFactory，整条 feature 编译不过（RR-20260918-01） | [RR-20260918-01.md](RR-20260918-01.md) |
| U-0228 | codegen | battle demo 的启动宽限期没有事件源，无人输入的房间一帧不切（RR-20260917-09） | [RR-20260917-09.md](RR-20260917-09.md) |
| U-0227 | codegen | handler 半的生成文件 import 了只被返回类型用到的包，`imported and not used`（实施 RR-20260917-08 时发现） | [U-0227-nest-return-type-imports.md](U-0227-nest-return-type-imports.md) |
| U-0225 | core | saga 步骤拒绝 / 重试用尽后进补偿的记录版本被加了两次，MongoStore.Apply 只收 expected+1，saga 永远卡在 waiting（game-demo 实跑发现） | [U-0225-saga-compensation-version.md](U-0225-saga-compensation-version.md) |

重构类改动(不对应任何 RR,不关闭任何 RR)另记,编号 M-:

| 编号 | 仓库 | 改了什么 | 记录 |
| --- | --- | --- | --- |
| M-01 | core | entity kind 注册表改为按 kind 的无锁定长表;含 category 重设计四步方案,仅第一步已实施 | [M-01-entity-kind-registry.md](M-01-entity-kind-registry.md) |
| M-02 | core | category 离开 EntityID,注册表成为唯一权威;ID 那两位降为历史填充,零数据迁移 | [M-02-category-leaves-the-id.md](M-02-category-leaves-the-id.md) |
| M-03 | core | 声明 category 后值即锁序,remote 档强制最先,锁序注册期派生;新增 `ValidateEntityRegistry` | [M-03-category-lock-order.md](M-03-category-lock-order.md) |
| M-04 | core + codegen | **破坏性**:删除 `RemotePolicyCapable`、`GetEntityGroupFunc`、`EntityGroup*`;推荐分类常量;codegen 拒绝 `remote=capable`、脚手架默认 Other | [M-04-drop-capable-and-the-group-hook.md](M-04-drop-capable-and-the-group-hook.md) |
| M-06 | core + kit（core v1.15.3 / kit v1.14.4 已发） | match 领域实现（类型、errcode 段、Store 状态机、Redis store、Grouping）与 `servicemetrics` 契约下沉 core：`roost-core/service/match`、`roost-core/servicemetrics`；kit 侧改别名包的步骤写在记录里 | [M-06-match-domain-into-core.md](M-06-match-domain-into-core.md) |
| M-07 | core + kit（core v1.15.4 / kit v1.14.5 已发） | mail 领域实现（信封 / 邮箱状态机 / 三段式领取 / Redis stores / errcode 段）下沉 `roost-core/service/mail`；`Mail` RPC 接口、生成传输与 Mod 留 kit | [M-07-mail-domain-into-core.md](M-07-mail-domain-into-core.md) |
| M-08 | core + kit（core v1.15.4 / kit v1.14.5 已发） | session 领域实现（幂等 Enter / 归属 / run 状态机 / Admin 操作面 / Redis stores）下沉 `roost-core/service/session`；`Session` RPC 接口、生成传输与 Mod 留 kit | [M-08-session-domain-into-core.md](M-08-session-domain-into-core.md) |
| M-09 | core + kit（core v1.15.4 / kit v1.14.5 已发） | manager 生命周期引擎（稳定拓扑序 `Order`、`Engine`：只回滚已成功者 / 关停中止启动 / Stop 幂等逐个报错）下沉 `roost-core/manager`；`ManagerMod`（Mod 名、capability 登记）留 kit 改为包装 | [M-09-manager-engine-into-core.md](M-09-manager-engine-into-core.md) |
| M-10 | codegen + kit（codegen v1.15.7 / kit v1.14.5 已发） | `servicerpc` 生成传输拆成 `<iface>_rpc_gen.go`（只依赖 core）与 `<iface>_rpc_assembly_gen.go`（Server / OwnerCapabilities / ClientMod，依赖 kit mods）；kit 全部 RPC 接口已重生成 | [M-10-servicerpc-split.md](M-10-servicerpc-split.md) |
| M-11 | codegen + core + kit（core v1.15.5 / kit v1.14.6 / codegen v1.15.8 已发） | `Mail` / `Session` / `Matchmaker` RPC 接口连同传输半（`*_rpc_gen.go`）进 `roost-core/service/*`；codegen `servicerpc` 加 `-emit` / `-out`，kit 从 core 的接口生成装配半 | [M-11-rpc-interfaces-into-core.md](M-11-rpc-interfaces-into-core.md) |
| M-05 | codegen | `category=` 进实体标记并直接进生成物,拆掉运行期查表的隐式前置;生成的聚合注册末尾调 `ValidateEntityRegistry` | [M-05-marker-owns-the-category.md](M-05-marker-owns-the-category.md) |
| M-12 | core+codegen | 生成的同步字段词汇表（ARCH-06，承接 W-2026-09-18-02） | [ARCH-06-sync-field-vocabulary.md](ARCH-06-sync-field-vocabulary.md) |

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
| RR-20260915-07 | U-0211 | Windows 上 `O_APPEND` 句柄的 `Truncate` 被拒(Access is denied),CI windows-compatibility 红 | 截断改为开追加句柄前按路径 `os.Truncate`;Linux 由 CI linux-quality 持续验收 |

## ARCH：Kit 职责边界收敛（来源 `docs/bug/REVIEW-2026-09-16-04.md` §7,不占 U / M 编号,不进覆盖矩阵）

目标:**Core 核心实现,Kit 装配与使用便利,Codegen 代码生成**。Core 不反向依赖 Kit;同一职责只保留一份实现;迁移不顺便改行为。

| 任务 | 内容 | 状态 |
| --- | --- | --- |
| ARCH-04 | 文档与生成器对齐:kit README 自述与组件表按当前目录;codegen 不再生成旧职责下的接口引用 | **第一步已做（2026-09-16）**:kit README 改为"装配层"自述,删掉 13 行已迁入 core 的包（nestwal / remote_entity / syncstream / replication / lockstep / gateway / spatial / ai / actionflow / versionstore / servicerpc / mongo/mongotest / robot）并加"已在 roost-core"说明;codegen 随 U-0217 删掉 `Grouping()` collaborator。**生成器部分已做（M-10，2026-09-16）**:servicerpc 生成传输拆成 transport（只依赖 core）/ assembly（依赖 kit mods）两半,kit 全部 RPC 接口已重生成,为 RPC 接口随领域包进 core 铺路。**RPC 接口 + 传输半进 core 已做（M-11，core v1.15.5 / kit v1.14.6 / codegen v1.15.8）**:kit 从 core 的接口生成装配半。kit README 第 3～4 节逐包核对见 kit 提交（2026-09-16）。**ARCH-04 完成** |
| ARCH-01 | 通用服务领域实现下沉 core:session（Enter 幂等 / 归属 / 会话状态机）、mail（CommitClaim）、match（票据状态、Grouping 与 ScoreWindow） | **match 的 core 半已做（M-06，2026-09-16）**:`roost-core/service/match` + `roost-core/servicemetrics`,领域测试随迁,边界测试绿。kit 半已随 kit v1.14.4 完成（别名包 + 删重复实现,kit 全套绿）。**mail / session 的 core 半已做（M-07 / M-08，2026-09-16）**：`roost-core/service/mail`、`roost-core/service/session`，领域测试随迁，边界测试绿；kit 半已随 kit v1.14.5 完成（别名包 + 删重复 + 精简 harness，kit 全套绿）。**ARCH-01 完成**：三个领域的实现、RPC 接口与传输半都在 core（M-06～M-08、M-11），kit 只剩 Mod / 装配半 / 别名。原计划:列出纯契约（`Queue` / `Subject` / `Ticket` / `Match` / `Store` 接口 / `Grouping`）、实现（`queue_store.go`）、Mod / 配置（`match_mod.go`、`redis_store.go`）、生成文件（`matchmaker_rpc_gen.go`）与外部依赖（`versionstore`、`servicemetrics`）；`servicemetrics.Reporter` 契约需先在 core 安放。Kit 侧以类型别名过渡,持久化 key / JSON 字段 / 错误码 / RPC 方法名不变。验收:kit 全套 + codegen 生成工程编译 + `go list -deps` 证明 core 不依赖 kit |
| ARCH-02 | manager 生命周期引擎（排序 / 状态 / 失败回滚 / 停止协调）迁 core | **core 半已做（M-09，2026-09-16）**：`roost-core/manager`（`Order` + `Engine`），24 条测试随迁，语义原样（不用 `TopologicalSortCache` / `lifecycle.ManagerGroup`）；kit `ManagerMod` 已随 kit v1.14.5 改为引擎包装（公开方法集不变，sentinel 同指针）。**ARCH-02 完成**。原计划：单独一批:保留启动失败仅回滚成功者、依赖错误诊断、关闭交接与稳定顺序;不换成语义不同的 `TopologicalSortCache` |
| ARCH-03 | 已正确的装配（dataengine / saga 的 Mod 调 core `Assemble` 并转发生命周期）作为迁移样板 | 无需改动,作为 ARCH-01 / 02 的形状参照 |

## 2026-09-19 那两条后来也修了

RR-20260919-02 与 RR-20260919-04 在同一天的第二轮里收敛：前者定了"唯一父所有权"并让越界的绑定
当场 panic（U-0252），后者把索引搬进存储、与值同一个 Lua 写（U-0253）。当时写下的"要先定什么"
就是这两个决定，记录在各自的方案一节里。

## 2026-09-19 第二轮那一条也修了

RR-20260919-10 在同一天收敛（U-0257）：owed 索引 + `OwedDispatches` RPC + `AttemptDispatch` 交给取走
payload 的一方 + sweep 收回本分。当时写下的"要先在 kit 加一个按 gameSID 的待交付 RPC"就是这次做的事。
