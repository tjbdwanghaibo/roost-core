# 业务模块修复复验与新增提交审查（2026-11-11 续审）

被测 feature：`feat/business-modules` **e4627b597113538e19ad0f4bb3ff2fb2427a9a21**。相对上轮 `fd0de4cf6e321c032e72afe18cd6cf4670e94b05`，变化 118 文件，+10732/-454 行。结束前再次 fetch，feature 仍为上述提交；远端 main 仍为 **f78495440c52ff8d651a35b4b5eaa5758014815e**，本轮没有拉到新的 main 提交，不能把 feature 修复写成已验证合入 main。

结论：上轮 BM-01、BM-02、BM-03 在上述 feature 提交复验通过；新增 BM-04（P1）与 BM-05（P2），均三轮复现并保留控制组。只提交报告及复现证据到 `fix/core-game-server-review`，没有修复运行时、创建 issue 或 PR。该固定分支的旧运行时代码不是被测对象。

## 上轮三项修复

| 编号 | 修复与判断 | 验证 |
| --- | --- | --- |
| RV-20261111-BM-01 | `b0ff6874`：占位写入后重读角色，按角色权威清除无主占位；重复中止也补清理。修复方向合理，原迟到 Begin 交错不再阻塞下一次发起。 | 原探针的 control/delayed 各三轮通过；account 原有整包 race 通过。 |
| RV-20261111-BM-02 | `b0ff6874`：增加 TransferSeq，接受发起/记下中止推进序号；BindEpoch 继续只表示绑定变化。原样旧请求不能在后续中止覆盖 LastTransfer 后复活。 | 原探针 control/second_abort 各三轮通过。仅为后续合法新请求补当前 ExpectSeq，重放的旧请求保持原样。 |
| RV-20261111-BM-03 | `af67d2a3`：负 delta 不再取负比较，对非负 coins 检查相加结果，避免 MinInt64 取负溢出。 | 原正式 Sender/Nest 探针普通不足与 MinInt64 各三轮通过；余额及回执不变。 |

这些关闭结论针对旧报告的具体缺陷，不意味着换服流程已经全部正确。account 只认在途与最近一条结论仍是公开契约；下面 BM-04 是新增 Driver 没有正确使用这一契约。

## RV-20261111-BM-04：旧 Commit 的 Stale 被误判为未提交，解除已经迁出玩家的源服保护（P1，未修复）

位置：[rehome_driver.go.tmpl:306–319](https://github.com/tjbdwanghaibo/roost-core/blob/e4627b597113538e19ad0f4bb3ff2fb2427a9a21/modules/rehome/files/game/rehome/rehome_driver.go.tmpl#L306)。`conclude` 丢弃 Commit/Abort 的 RoleBinding，只按错误码判结论：Commit 的 `ErrTransferStale` 一律当作未提交，Abort 成功一律当作未提交。

可复现时序：

1. A：源服 1000 已交出实体，account 将玩家提交到 1001；提交应答丢失，源服重试尚未得到结论。
2. 角色在 1001 发起另一笔到 1002 的换服 B，随后中止 B。绑定仍为 1001，但 LastTransfer 现在只保存 B。
3. 源服重试 Commit A。account 已不认识这个旧 ID，返回 `ErrTransferStale` **及当前绑定 1001 / epoch=1**。
4. Driver 忽略绑定，把 A 当成中止，执行 `Settle(Aborted)`；LocalResidency 撤掉交出屏障、删除迁出记录，随后清理源服意图。

实际断言输出：

```text
control:     authority sid=1001 epoch=1; held=true  tracked=true  stale-session-admit=rehome_elsewhere
later_abort: authority sid=1001 epoch=1; held=false tracked=false stale-session-admit=<nil>
```

后者意味着 account 已承认目标服为属主，但源服恢复了可写状态；携带换服前 claims 的进入请求也能被缺省 Driver 放行。迟到的握手/进入是其中一种来源。即使不重连，误判后的 `cancelIntent` 也会尝试在源服写 Player。demo 自有 Residency 的 Aborted 同样会撤屏障；本轮动态复现的是正式 Driver + LocalResidency，没有声称已用两个真实进程证明双写 Mongo。

第二笔可以中止在交出之前，不需要违规人工 Commit。默认玩家冷却会限制紧接着再换服，但服务端 Operator 请求可以绕过冷却，且应用可以调整冷却；依赖默认冷却不能代替属主判定。account 文档已明确“旧结论被覆盖后判断是否提交必须看 ServerID/BindEpoch”，Driver 和 Directory 注释应一起对齐。

修复方向：按返回的当前绑定、绑定代数和仍在途的换服判断本进程是否还能服务该角色；不能用单个错误码推断任意历史 ID 的终态。仍归其他服时保持源服保护；回到本服也要按回迁代数与当前在途状态处理，不能直接当作旧请求从未提交。覆盖 Commit 响应丢失、Abort 响应丢失、后续 Begin 尚在途/已中止/已提交、迁回本服这些交错。

探针采用真实 `account.Service` + MemoryStore，使用模块原有夹具的 Profile、会话关闭、实体交出替身。三轮实验失败、三轮控制通过，`-race` 未报告数据竞争。问题是跨步骤状态判断错误。

## RV-20261111-BM-05：demo 换服前的公会检查通过后可失效，仍带公会提交换服（P2，未修复）

位置：[rehome_checks.go.tmpl:33–34](https://github.com/tjbdwanghaibo/roost-core/blob/e4627b597113538e19ad0f4bb3ff2fb2427a9a21/demo/internal/service/game/rehome_checks.go.tmpl#L33)，相关 [PlayerOwners.Admit](https://github.com/tjbdwanghaibo/roost-core/blob/e4627b597113538e19ad0f4bb3ff2fb2427a9a21/demo/internal/service/game/playerowner.go.tmpl#L352) 与 [Driver.carry](https://github.com/tjbdwanghaibo/roost-core/blob/e4627b597113538e19ad0f4bb3ff2fb2427a9a21/modules/rehome/files/game/rehome/rehome_driver.go.tmpl#L257)。

demo 声明一期禁止公会成员换服，因为联盟成员的 HomeSID 与投递路由尚未迁移。但它只在 Prepare 事务里检查一次 PlayerDao.GuildID。此后客户端仍连着，Admit/AdmitMessage 不检查迁出标记，JoinGuild 仍可发起联盟 RPC；成员通知也可以经真实 ShowGuild 事务更新该字段。后台离开与提交不再验证这个条件。

在同版本正式生成的 game-demo 中，用正式 Controller Driver、Profile 适配器、demo 注册的前置检查、真实 Nest/DAO、PlayerOwners 和 account.Service 复现：

1. GuildID=0 时 Prepare 成功并写下换服意图。
2. 固定在 account Begin 成功、回到驱动器之前，确认 `AdmitMessage(MsgJoinGuild)` 仍放行；投递一条 MemberJoined 通知，经正式 `guildNotices.apply → ShowGuild` 把 GuildID 写成 7。
3. 驱动器照常离开并 Commit，最后 **GuildID=7，account 当前绑定=1001**。

对照：同一通知在 Prepare 之前完成，申请正确返回 `ErrRehomeRefused`。对照与实验各三轮，实验均失败，race 无数据竞争。通知的业务处理、玩家事务和账号改绑都是真实实现；通知发送端、网络、提交器、实体交出是替身。没有启动真实联盟进程，也没有验证实际跨服公会投递结果。

影响：一期声称的“先退公会再换服”限制并不能维持到提交，可能留下仍指向旧服的成员关系。GuildID 本身又是允许通知丢失/乱序的展示副本，更不能作为跨服务成员关系的唯一权威证据。这不是要求二期新增自动迁移公会功能，而是现有拒绝规则不能保证自身承诺。队伍采用类似一次性检查，应纳入修复验收；本轮没有单独复现队伍变更。

修复方向：在建立迁出状态时阻止新的相关业务发起，并处理已经准入的操作、在途联盟 RPC 和通知；排空后再用可靠的成员事实决定中止还是继续。不能只在 carry 前再读一次同样可变化的展示字段，也不能只拒绝通知、让权威成员关系已经变化却永远不更新副本。增加“检查后加入/入会 RPC 应答迟到/通知迟到”的确定性交错测试。

## 验证与范围

Linux / Go 1.27.2，`GOWORK=off`。图谱工具不可用，使用精确源码、rg、同提交 CLI 与定向实验。本轮检查新增 room 席位/连接代数、turn 持久定时器、模块实体与计时器安装、enter 前置检查、ConnectionID 透传、rehome 驱动与 demo 所有权/Saga 准入衔接，以及 account/wallet 修复。

| 检查 | 结果 |
| --- | --- |
| account 原有整包 race，count=1 | 通过，默认内存测试；未启用外部 Redis 验收环境 |
| 两项 account 旧探针、wallet 旧探针，race count=3 | 控制组与旧缺陷实验全部通过 |
| roost 模块与 demo 生成测试，count=1 | 通过；实际筛选式见证据说明 |
| 自定义 module 的 game 工程，安装全部 15 模块后 module verify | 298 个模块/联动顶层场景通过，0 skip；build/vet/id check/generate --check 四项成功 |
| 生成工程 rehome、room、turn 的定向 race，count=1 | 通过，未包含新增缺陷探针 |
| Gateway ConnectionID 转发测试，race count=3 | 通过，使用本机测试监听端口 |
| demo 既有迁出准入、旧 claims 拒绝、公会通知及公会/队伍前置规则测试，race count=1 | 4 个顶层测试通过 |
| BM-04、BM-05 新探针，race count=3 | 各三轮实验失败、三轮控制通过；作为未修复证据保留 |

没有执行全仓全量测试、真实两 game + account + 联盟的联合故障演练、kill -9 恢复、真实 Mongo/WAL 交出、Unity、跨机或容量测试。模块 verify 使用替身提交器；本轮不是上线容量验收，没有新压测，没有改动历史压测材料。room/turn 等暂未发现新的可确认缺陷，不等于证明所有组合无缺陷。

命令、探针与原始日志见 [证据目录](evidence/business-modules-followup-20261111/README.md)。上轮记录保留为历史基线，状态以上述复验为准。
