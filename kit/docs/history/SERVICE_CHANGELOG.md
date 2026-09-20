# Changelog

格式遵循 Keep a Changelog，版本号遵循语义化版本。

## [Unreleased]

### Fixed

- **`scripts/gapmap.sh` 收尾不再 `git clean`**（与 roost-core 同一份拷贝）：采样后只还原被改动的已跟踪文件，未跟踪文件原样保留。

### Added

- **gap map 采样器跳过 `*_gen.go`**（B-25）：生成文件是同一模板在每个包的实例，其守卫在模板所在处钉一次即可；采样器现在只统计不采样，并在包级与总计里报告跳过的守卫数。
- **mail 生成传输层的装配拒绝与 Redis 存储参数守卫钉住**（U-0103，C2，B-24 尾项）。nightly gap map 里 `mail` 20 条采样 16 条无覆盖，
  其中 8 条在生成的 `mail_rpc_gen.go`，与其余 11 个服务共用同一模板。钉住：`RegisterHandlers` 缺 bus / service、`NewBusClient` 缺 bus、
  nil 客户端调用、Server 装在只持有客户端的进程（"published but … not"）/ 缺本地实现 / 缺 bus、ClientMod 负超时 / 缺 bus；Redis：nil 客户端、
  空前缀、空 id（单读 / 批读）、存储信封无 id。`wiring_promises_test.go` 两条；回退 16 处守卫 13 红；"缺 handler / handler 数不符"两条
  是防模板漂移的构造性守卫（handler 表是字面量），`NewRedisStores` 的 nil 客户端与下游 `NewRedisEnvelopes` 同文案冗余。
- **account `CreateRole` 提交尾部的三条异常分支钉住**（U-0098，C2）：槽位行在最后一笔写之前消失（`vanished during create`）、分配到的
  player id 已被占用、分配器返回 0——各自报错且**什么都不留下**：同一账号用同一名字重试即成功。`create_role_race_promises_test.go` 两条；
  回退三处守卫各红。
- **platform 回调 / 投递路径的四条订单竞态分支钉住**（U-0097，C2）：插入输给并发者而对方行已消失（`vanished during create`）、
  同一订单号不同载荷（`ErrOrderMismatch`）、认领后标记已投递前行消失、记录失败前行消失——各自以 `ErrConflict` / `ErrOrderMismatch`
  报出，未预留的订单不发货、不计入 accepted。替身嵌入真实内存 store，只在第 N 次 Update 让行消失。`order_race_promises_test.go` 一条；
  回退四处守卫各红（其中摘要比较是跨行 `if`，按文本中和）。
- **mail `Send` 的三条"世界在脚下变了"分支钉住**（U-0096，C2）：账本认领输给并发者但对方的行已消失（`vanished during create`）、
  对方账本行指向不存在的邮件（`names missing mail`）、邮件 id 已被占用——各自以 `ErrConflict` 报出，不留下账本没登记的信封，
  也不计入 accepted。用包装真实内存存储的替身制造竞态。`send_race_promises_test.go` 一条；回退三处守卫各红。
- **gap map 工具**（与 roost-core 同一份拷贝）：`scripts/gapmap/revertsample.py`、`scripts/gapmap.sh`、`nightly-gapmap` 工作流。
  每日对每个有测试的包做承诺回退采样，报告进 job summary，不阻塞。

### Changed（测试质量）

- **global 的迁移 / 租约 / 分页请求守卫钉住**（U-0059，C2）。回退采样 33 条守卫 19 条全绿。`BeginMigration`：目标 sid 非正、
  迁移到"已经在服务它的那个 global"（否则绑定永远停在 Migrating）、路由不存在——并断言拒绝后绑定原样；`ReleaseLease`
  空 incarnation；`LiveGames` limit 非正 / 超页、候选超页。`guards_promises_test.go` 两条；回退五处守卫各红（"incarnation 必填"首轮误判为互掩——回退脚本按文本替换命中的是
  `RenewLease` 里的同名守卫，按行号回退 `ReleaseLease` 的那条后测试变红）。
- **rank 的 `decodeEntry` 严格解码钉住**（U-0060，C2）。字段数不对、value / tie 非十六进制、brief 非 base64——每种都必须
  是错误而**不是零分**：解码成零再写回就是把真实分数换掉。`member_promises_test.go` 一条；回退字段数守卫变红。
- **match 的 `Commit` / `Cancel` 拒绝规则钉住**（U-0057，C2）。回退采样 38 条守卫 21 条全绿。`Commit`：票数与队伍规模不等、
  空票号、同一票号出现两次、票不存在、队列从未有人进入、已配对的票再提交报 `ErrConflict`——并断言所有拒绝之后两张票
  仍在等待（整段是一次 CAS，拒绝不留半改）；`Cancel`：空票号、未知票号。"同一 subject 两张票"那条在 CAS 里的规则经公开
  API 不可达（`Enqueue` 先拒绝），记为纵深防御不钉。`commit_promises_test.go` 两条；回退四处守卫各红。
- **生成的 RPC 胶水三条拒绝在 account 钉住一次，覆盖全部九个服务的同一模板**（U-0058，C2）。`Server.Init`：注册表只有
  远端能力（进程只持有 client）时拒绝并说明"会把每个请求转发给自己"，什么都没有时点名要加 `account.NewMod()`；
  `ClientMod.Init`：`call_timeout` 为负拒绝、为零取默认。`rpc_glue_promises_test.go` 两条；回退两处守卫各红。
- **session 的 `Run.Validate`、`Resource.Validate` 与进入请求校验逐条钉住**（U-0056，C2）。回退采样 40 条守卫 32 条全绿。
  运行：空 id、owner 非正、空 kind、空幂等键、**无截止期（否则持有的资源永远不释放）**、截止期不晚于开始、context 超 64、
  资源超 8、资源缺 kind / id；进入请求：空 kind、**空幂等键（重试会分配第二个运行）**、context 超限。
  `validate_promises_test.go` 两条；回退四处守卫各红。
- **mail 的 `Envelope.Validate` 十二条规则逐条钉住**（U-0054，C2）。脚本回退采样 40 条守卫 32 条全绿，其中信封校验整段无测试：
  空 id、direct 无收件人 / 超 200 人 / 收件人不是玩家、broadcast 带收件人名单、未知 audience、空 / 超长 subject、超长 body /
  attachment、不过期、过期不晚于创建。`validate_promises_test.go` 一条表驱动；回退三处守卫各红。
- **platform 的 `Order.Validate`、`Credential.Validate`、`Verified.Validate` 逐条钉住**（U-0055，C2）。回退采样 39 条 32 条全绿。
  订单：空 / 超长 order id、玩家 id 非正、空 / 超长 product id、**金额为零或负（"免费送货"守卫）**、无尝试预算；凭证：空
  channel / open id、超长 open id、**空 secret**（被替换实现接受的形状）；校验结果：空 channel / open id。回退三处各红。

首个版本尚未发布。本仓是 roost 框架的通用服务层，从一个生产业务仓的公共服务抽出——
但**不是搬运**：对那些服务的逐文件审计得到 62 项发现，收敛到六个在多个互不相关服务
里各自重现的缺陷模式（见 README）。因此每个包都是重新实现，且每条设计约束都对应一类
已确认的缺陷。

### Added

- **CI 门禁**（`.github/workflows/ci.yml`）。本仓此前没有任何自动化：mail、rank、`integration/`
  三处 Redis 集成测试带 `//go:build integration` 标签并以 `REDIS_ADDR` 门控，而没有任何
  流程设置过这个变量——32 个测试靠"从不运行"保持绿色（C2 类）。新工作流三段：`unit`
  （tidy 一致、vet、`vet -tags integration`、glsvet、`-race`、govulncheck、actionlint）；
  `integration`（真实 Redis 7 服务容器，`-tags integration ./...`，随后**把带 `REDIS_ADDR`
  字样的 skip 判为失败**，再对三处 Redis 套件跑一次 `-race`）；`release-hygiene`（无
  replace、tag 不依赖框架伪版本、模块路径可解析、tag 与主版本号一致）。根目录
  `ci_test.go` 钉住工作流的这四个事实，防止一次改动悄悄删掉门禁。收敛单元 U-0014。

### Fixed

- **`global/activity` 的 ClientMod 漏在上一条修复之外**。手工重生成八个包时漏了第九个带 RPC 传输层的包；
  把 `tool` 指令对齐到 codegen v1.13.5 后 `go generate ./...` 补上了它，`integration/client_mods_test.go`
  现在覆盖全部九个客户端——手工枚举漏包正是"用工具而不是用手"的理由。
- **所有生成的 ClientMod 在真实进程里装配不起来**。八个 `*_rpc_gen.go` 的 `ClientMod.DependsOn` 返回的是
  总线的 capability 名 `bus`，而 app 按 Mod **名字**解析依赖，没有任何 Mod 叫 `bus`：任何把
  `xxx.NewClientMod()` 和 kit 的 nats Mod 装进同一个进程的程序启动即失败——
  `unknown mod dependency "bus"`。本仓自己的集成测试手工 Init/Provide、从不经过 app 的依赖排序，所以
  从未发现；roost-codegen 新的 game 模板第一次真的启动 game 进程时暴露。用修正后的生成器（roost-codegen
  U-0024）重生成八个包，依赖改为发布总线的 `nats` Mod；`integration/client_mods_test.go` 钉住全部八个。
  `go.mod` 的 `tool` 指令仍指向 v1.12.1，下一次 codegen 发布后再对齐。
- **chat / match：后台保留与过期扫描此前从未执行**（B-09）。两个 Server 的 run 钩子遍历的是一个返回硬编码
  `nil` 的方法，且没有任何配置入口：chat 的 `retention_age` 没有执行者，match 的 `ticket_ttl` 只在读写时内联
  兜底。现在由部署提供枚举：chat 读 `chat.prune_channels`（`kind:target` 的共享频道列表，Provide 时逐条
  `Resolve`，解析不了就启动失败）并可用 `Mod.WithPruneChannels` 提供动态集合（如在线玩家的私聊频道）；
  match 读 `match.sweep_queues`（`mode:group_size[:partition]`，Init 时按 `Queue.Validate` 校验）进
  `Config.SweepQueues`，Store 通过 `SweepQueues()` 暴露给 run 钩子。两处未配置时在启动时 `slog.Warn`
  一次，不再沉默。测试用可调 tick 真的驱动 run 循环并断言 `Prune` / `Sweep` 被调用；把 provider 换回
  硬编码 `nil` 两条测试都红。收敛单元 U-0022。
- **account：`CreateRole` 在角色记录之后的两处失败不再留下半成品**（B-08）。此前 `Names.Commit` 失败
  直接返回错误但角色已插入——名字 claim 到期后别的账号可占用同名；`Slots.Update` 失败同样保留角色，
  客户端重试得到 `ErrRoleLimit`。现在提交点是最后的槽位写入，之前的一切在失败时全部撤销：版本校验
  删除角色记录、按 claim 取消或按 owner 释放名字、释放槽位，撤销失败计 `rollback.failed`。
  `create_role_tail_test.go` 用"首次 Commit 丢失"与"首次 Update 丢失"两个替身证明：同一账号立刻
  重试以同名成功，另一账号随后被 `ErrNameTaken` 拒绝。收敛单元 U-0021。
- **global/activity：两条兜底承诺补上测试**。六条注释承诺逐项临时回退，四条已有测试变红；"有界扫描先完成
  截止最早的活动"与"已完成但仍在窗口里（dispatch 创建失败）的活动由扫描补建投递并出窗"两条全绿——
  新增 `sweep_promises_test.go`（键序与截止序故意相反；`dispatchesFailingOnce` 让完成通知的投递创建丢一次）。
  "扫描不会完成 pending 活动"回退后仍绿是因为 `completeExpired` 在 CAS 内再次校验状态，属双重保险而非洞。
  servicemetrics、servicemods 全读：分支全部有测试。收敛单元 U-0020，B-07 的 12 包至此全部过一遍。
- **global：重试的 `CompleteMigration` 被计为 `accepted`**。同 epoch 的重复完成按注释是幂等回放，
  但上报走的是 accepted，迁移速率指标会高于实际迁移数。现在回放计 `replayed:complete_migration`。
  对 global 九条注释承诺逐项临时回退：七条已有测试变红；两条全绿——"非持有者的拒绝不泄露当前
  incarnation"与上面这条——补测试；"incarnation 在 CAS 重试间只铸一次"原回退编译失败无结论，
  用先输一次 CAS 的替身直接验证为一次。顺带修正 `AcquireLease` 注释里"同 incarnation 可重取"
  的承诺：该方法不接收 token，从未有过这条路径。收敛单元 U-0019。
- **platform：`HandleCallback` 首次路径丢掉 `Receipt.Replayed`**。`AttemptDelivery` 的契约要求"我发了货"
  与"货已被别人发过"可区分（`Replayed`），当一次慢投递被过了退避期的重试超车时它确实置位——但
  `HandleCallback` 为首次回调构造 Receipt 时只抄了 `Order` 和 `Delivered`，回调驱动的投递从来看不到
  这个信号（指标 `conflict:deliver` 仍在，所以只有 Receipt 这一层丢）。对 platform 五条注释承诺逐项
  临时回退：两条已有测试变红；三条全绿——"记录的是投递方的错误原因而非常量"、"被超车的提交不移动
  送达时间并计为 conflict"、"领取时预算已耗尽要落成 exhausted 而非原地拒绝"。三条测试补上
  （`race_test.go` 的 `blockingDeliverer` 制造超车），其中第二条顺带暴露了上面的丢字段。收敛单元 U-0018。
- **rank：`TestIntegrationConcurrentAddsAgainstRealRedis` 在 v1.5.1 的 tag CI 上偶发红**
  （`submit lost 8 compare-and-swaps for owner 1`）。八个 writer 对同一 owner 用 CAS 累加，
  `Submit` 按契约在 8 次失败后返回 `ErrConflict`（哨兵注释写明"重试即可"），测试却把它当作失败。
  现在测试按契约以调用方的方式有界重试冲突，其它任何错误仍立即失败；断言不变。
- **session：`Enter` 在账本写失败时的承诺补上测试**。对 session 六条注释承诺逐项临时回退：四条
  已有测试变红；"releaseClaim 的 run id 守卫"回退后无测试变红——但它与版本校验删除等价，不是洞
  （只影响 `claim.release_not_ours` 计数）；"账本 `Create` 失败时 `Enter` 返回错误而非成功"回退后
  全绿——这是洞：调用方会拿到一个账本不认识的 run，重试无法回放。新增
  `TestEnterReportsALostLedgerWriteInsteadOfSuccess`（账本首次 Create 失败的替身）钉住：返回错误、
  run 仍在且 open、同 request id 重试被 `ErrAlreadyRunning` 拒绝、不计 accepted。收敛单元 U-0017。
- **directory：计数器在 CAS 重试下虚高**。`Reserve` / `Commit` 在 `versionstore.Update` 的回调
  里上报 `accepted` / `replayed` / `refused`，而 kit 的契约明说 Mutate 可能被调用多次（每次输掉
  compare-and-set 都重读重放）。内存后端从不重试，所以现有指标测试全绿；换成一个"先输一次 CAS"
  的存储替身，一次预留报 2 个 accept。回调现在只做决定，Update 返回后按结果上报恰好一次。
  收敛单元 U-0016（C3）；`contention_test.go` 的 `contendedStore` 可复用于其它包。

- **rank：CAS 耗尽的冲突错误没有错误码**。`types.go` 声明了 `CodeConflict = 540108`，但从未
  用 `errcode.Define` 接上；`Submit` 输掉全部 8 次 compare-and-swap 时返回的是一个普通的
  `errors.New`，于是生成的 RPC 信封把它报成 `CodeInternal` / "server error"——恰恰是那个哨兵
  自己的注释承诺"与存储故障可区分"的失败模式。本模块其余九个服务都把 `CodeConflict` 与带码
  哨兵配对，rank 是唯一的例外。现在 `ErrConflict = errcode.Define(CodeConflict, ...)`，
  `ErrConflictSentinel` 保留为它的别名（Deprecated），`errors.Is` 调用方不受影响。
  同时补上 `errcode_test.go` 的配对表（`segmentAllocated` 7 → 8）：那张表是手写的，它漏掉的
  正是这一项，所以"每个哨兵都带码"的测试对"有码无哨兵"看不见；并把
  `TestAroundCentresOnTheOwnerAndIsBounded` 名字里承诺却没断言的"居中"补成断言。
  收敛单元 U-0004，见 roost-core `docs/history/ledger.md`。
- **account：超长 profile 被报成 `CodeConflict`**。`UpdateProfile` 对超过 `MaxProfileBytes` 的载荷返回
  `ErrConflict`（560113），客户端会把它当"重试即可"；而本包为此定义、配对、测过"带码"的
  `ErrRangeInvalid`（560112）从没有任何生产路径返回过它。现在超长 profile 返回 `ErrRangeInvalid`。
  **线上可见的变化**：这一种失败的错误码从 560113 变为 560112。原测试只断言 `err != nil`，
  这正是错码能通过的原因；现在断言具体哨兵。
- **account：`TestSelectRoleDoesNotPersistWhenSigningFails` 是空测试**。它用不存在的 player id 0
  触发失败，失败发生在角色查找、签名从未被调用，然后检查一个无关角色的版本没变——无论
  "先签名后落库"还是反过来它都绿（把 `SelectRole` 的两步临时倒过来，它仍然通过）。改为直接在
  store 里种一个 id 为 0 的角色：core 拒绝签这个 id，于是签名真的失败，断言该角色版本与
  登录时间戳都未变。倒序时它变红。收敛单元 U-0005。
- **mail：投递失败后的重试从不重新投递**。`Send` 在投递失败时把错误返回给调用方，注释承诺
  "重试是一次会重新尝试投递的回放"——但回放路径只查账本、取回信封、直接返回成功，从不再
  调用投递。于是一次 fanout 失败的广播被永久记为"已发送"，此后每次重试都答 OK；直投邮件
  对某个收件人失败（如信箱已满）也一样，重试永远到不了那个人。现在 `SentRecord` 带
  `delivered_at_unix`：投递成功才盖戳，回放时未盖戳就先投递（按信箱幂等，设计上允许重复
  尝试）再答复。旧账本记录没有该字段，读作未投递，下次回放多做一次幂等投递后盖戳。
  **行为变化**：直投部分失败时 `Send` 现在连信封一起返回错误（此前返回空信封）；直投失败
  新增 `refused:send:delivery_failed` 计数。
- **mail：翻页游标邮件被删后列表提前结束**。`NextCursor` 只是上一页最后一封的 id，下一页按
  id 相等定位；那封邮件在两页之间被删除后找不到，返回空页且无游标，客户端以为看完了——正是
  本包要消灭的"短页 + 完整计数"缺陷，只是从翻页这一侧进来。现在游标编码为
  `<delivered_at>|<mail_id>` 的位置，下一页从"严格在此之后"处恢复，与那封邮件是否还在无关。
  旧的纯 id 游标仍按相等定位（只影响升级瞬间客户端手里的游标）。
  同时把 `TestAnOversizedLimitIsClamped` 的"不超过上限"收紧为"恰好等于上限"：信箱里的邮件
  多于一页时，少于上限就是短页，`>` 放它过去。收敛单元 U-0006。
- **chat：幂等键不绑定发送者，撞键会静默丢消息**。去重账本按频道保存 `request_id → seq`，但不记谁用的。
  两个玩家在同一频道各自生成的键撞上（客户端本地计数器、短随机串都会），第二个人的 `Publish`
  拿回**第一个人的消息**且报成功——他的消息被丢了，没有任何信号；角色也能"回放"系统消息的键。
  现在同键只有同一发送者（角色按 id、系统按 origin + actor）才算回放，其他人复用键返回
  `ErrConflict`，并计入 `refused:publish.<kind>:key_reused`。存储格式不变，旧账本记录照常识别。
  同时把 `TestOnlyThePrivilegedEntryPointSendsSystemMessages` 里"缺 actor / 缺键"两处只断言
  `err != nil` 收紧为断言具体哨兵。收敛单元 U-0007。
- **match：Mutate 回调不纯，被放弃的变更会污染内存后端的存储值**。四个 `Update` 回调都先就地
  改 `current`（过期票据处理会原位挪移 `Waiting` 切片），再决定是否保存。kit `versionstore`
  的契约写明回调可能重跑、必须是纯函数，而它的 MemoryStore 直接交出存储值：一次回放式
  `Enqueue`（处理了一张过期票、然后"不保存"）让存储里的 `Waiting` 头部长度不变、底层数组已
  被挪动，尾部重复——`Candidates` 把同一张票给出两次，`Commit` 随即以"重复票据"拒绝，队列
  卡到下一次成功写入。Redis 后端每次重新解码所以看不到，但契约违背是真的。现在每个回调
  先 `clone()` 再改，与 chat 的做法一致。另删掉 `TestTicketIDsAreServerMintedAndDistinct` 里
  一个什么都不做的 `newStoreEach` 助手（注释声称它隔离了单票规则，实际原样返回同一个
  store）。收敛单元 U-0008。

### Added

- **`directory`**：唯一键预留 + 独占归属的两阶段提交原语。一次替掉业务仓里两份各带
  三个缺陷的实现（预留与记录分两个 store 写且补偿错误被丢弃 → 名字被永久烧掉；
  另一条 upsert 路径绕过预留 → 同名可占两次；释放先比 owner 再另起一次往返删除 →
  与重新预留竞争时删掉新持有者）。契约里没有 `Set`，预留必须带过期。
  **注意：同 owner 幂等，因此它不是互斥锁**——需要"只准一次尝试"用
  `versionstore.Create`。
- **`rank`**：榜单。把排名所需的一切编进 sorted-set 的 member，因此读一页只需一次
  往返、排名与分数不可能不一致（旧实现用 2–4 次不同步的读），且只有一个数据结构，
  孤儿成员不可能产生（那是旧实现短页与归档静默截断的成因）。`Limit` 上界不能被 `0`
  绕过；`UpdateAdd` 必须带 requestID 并对记录内的有界环去重（旧实现重投累加 2–20 次）；
  `Tie` 未设时默认为提交时间，于是并列按"谁先到"而非 owner id。
- **`match`**：匹配。整个队列状态放在一个版本化条目里，因此成组提交是一次 CAS——
  旧实现"弹出队首 → 写 match → 逐个翻状态"的三段非原子写，任何中途失败都让那批玩家
  从队列消失但状态仍是 waiting。另修：服务端签发不可猜的 ticket ID、每 subject 一张
  活票、每个操作都要带 subject、超时真正执行（旧实现写了 `ExpiresAt` 但全仓无人读）。
  分组策略留成接口。
- **`account`**：账号与角色目录。`IdentityVerifier` **必填且无默认实现**——旧实现拿
  `{channel, open_id}` 就签发会话令牌，是完整账号接管，而那个缺失看起来像一个默认值。
  `PlayerIDAllocator` 同样必填（旧默认是每进程从 1 开始的计数器，配 `ReplaceOne` 落库
  等于重启后建角色摧毁既有玩家）。创建改仅插入；名字唯一性用 `directory` 的两阶段
  claim；进度字段改 opaque blob；那个能给角色过户与改名的 upsert 入口不存在了。
- **`global`**：跨服路由绑定与游戏服租约。迁移用 **epoch CAS**（旧实现用进程内锁，
  而业务边界文档明写不得如此——多实例下等于没有保证）；租约用 **incarnation 令牌**
  做 fence，续期必须出示（旧实现读-算版本-无条件写，版本字段纯装饰，上一个 incarnation
  的迟到心跳能覆盖活跃租约）。另含**跨服活动协调**：宽限窗从**首个 notify**起算而不是
  从本服自己的 `CloseAt` 起算（时间线属于游戏服）；进度应用是**先在 ledger 里预留再
  应用**，因此"应用了但没记账"这个双计数窗口不存在；拒绝与审计是**同一条代码路径**
  （`refuseNotify` 是本包唯一能造出 notify 拒绝错误的表达式，绕过它就没有 error 可返回
  ——旧实现里"拒绝"是 `return err`、"审计"是旁边另一个调用，于是新增分支就漏审计）；
  结果投递带**不可猜的 ACK 令牌**并用 `crypto/subtle` 做常量时间比较，重试有上界，
  耗尽即计数（那是本包唯一真正丢失的东西：活动完成了而这个服再也收不到结果）。

- **`chat`**：频道消息。`PublishRequest` 里**没有**发送者字段也没有可信字段——旧实现
  的 `Trusted` 布尔来自请求体，客户端自己就能把消息标成系统公告。这里发送者由传输层
  身份决定，系统消息只能经 `PublishSystem` 并出示 `SystemToken`，而 `SystemToken` 的
  `granted` 字段不可导出、只有 `GrantSystem()` 能造，因此**跨包无法伪造一个可信令牌**
  ——零值 `SystemToken{}` 不授权任何东西。另修：幂等键必填（旧实现无幂等键而传输是
  `max_deliver 5`）；私聊频道按**双方有序对**定 key，因此 A→B 与 B→A 是同一条历史；
  游标分页，且保留导致的空洞**明确报告为 `Gap`** 而不是静默少给几条；每频道序号与
  环放在同一个版本化条目里，因此序号不可能与消息内容不一致。

- **`servicemetrics`**：全仓共享的上报 seam。方法按事件命名而不是字符串键的 `Count`，
  写错事件名是编译错误；`Wrap(nil)` 得到一个安全的空 `Sink`，因此每个调用点都可以
  无条件调用，不存在"某个分支忘了报"。附带一个测试用 `Recorder`——放在生产包而不是
  各包的 `_test.go` 里，因为六份几乎相同的手写替身就是六个可以悄悄停止断言的地方。

  它替掉的不是某一个缺陷，而是第 6 条约束本身曾经失效过：本仓写到第六个包时，实际
  只有 `chat` 有 metrics，其余五个包都违反了写在自己 README 里的约束。这正是"靠人
  记住的约束会失效"的又一次实例，所以现在每个包都有一条"上报点真的被走到"的测试，
  并经变异验证。

- **`mail`**：邮件。核心是**领取的幂等键由服务端生成，且对同一封邮件恒定**——旧实现
  让**客户端**提供 `RequestID`，并且允许"租约过期后可被另一个 requestID 抢占"，于是
  提交响应丢失的调用方只要等过租约、换一个新 id 重试，就会**再拿到一次附件**，而发货
  侧从未见过这个新键。唯一的挡板是三跳之外 game 进程里的去重。这里 token 一次铸出、
  终身不变，重试拿回的是同一个键，所以任何按它去重的发货侧必然收敛；租约过期只意味着
  多一次**投递尝试**，不再意味着多一次**发放**。

  另外三条：列表的上界从"返回多少"改成"**读了多少**"——旧实现夹住返回条数，然后循环
  取 `limit*4` 条信封、每封各读一次状态，直到凑满一页，**循环次数无上界**，一个删除过
  很多邮件的玩家能把一个协议包变成不确定次数的数据库往返；未读数从"每次跑一遍对该玩家
  所有活跃邮件的 `$lookup`/`$unwind`/`$group` 聚合"改成**与状态变更在同一次 CAS 里**
  维护的计数器，因此不可能与它所汇总的状态不一致；`PlayerID` 不再来自请求体。信箱有
  上界，满且无可驱逐项时**拒绝投递**而不是静默丢弃玩家没看过的邮件。

- **`platform`**：渠道边缘。两条最高后果的路径旧实现都错了。会话签发**完全没有鉴权**
  ——`AuthSession` 从请求体取 `player_id` 就签令牌，请求类型里根本没有凭证字段，能碰到
  这个端点的人可以为任意玩家拿到有效会话。这里 playerID 是**验证的输出**而不是输入，
  所以那个有漏洞的调用写不出来；`Verifier` 与 `PlayerResolver` 必填且无默认实现——
  "检查缺席"看起来就像一个默认值，这正是它当初消失的方式。

  充值发货**无状态、无幂等**：回调验签后直接交给 deliverer，`RequestID` 被接收、转发、
  **从未使用**。签名只证明 payload 来自渠道，**不证明这是第一次到达**，所以重放一个抓
  到的回调（签名依然有效）就再发一次货。这里改成**先预留后发货**：订单是仅插入的持久
  记录，第二次到达因此是重放而不是二次发放。发货错误被**记录**而不是换成常量字符串
  （旧实现让"支付发货失败"在服务端不留任何痕迹）；重试有上界，耗尽即计数并置终态。
  同一 order id 带不同内容会被拒绝，而 digest 取的是订单的**语义**不是字节，所以重新
  序列化的重投仍被识别为重放。

- **`session`**：从副本服务里抽出的有界 run 原语。**只抽了原语**——那个服务本身不是抽取
  候选：它的"端口"类型就是 game 进程自己的 manager 结构体，接口没有反转任何依赖，而且
  直接伸进活的场景图。通用的是它做错的那套生命周期，而它错了四处：幂等**失败开放**
  （去重键在 requestID 为空时返回 `""`，而空键让查找**未命中**而不是报错，于是不传 id
  或每次换 id 都能反复 Enter，每次新分配一个 run 和一个场景，且无任何容量核算；更糟的是
  旧 run **变得不可达**——Leave 需要 run id，客户端只持有最后一个，而没有任何索引把
  owner 映射回它的 run，于是那些 run 永远无法 Leave、场景永不销毁）；补偿**不对称**
  （第一个分配步骤的失败路径会释放已占资源，第二个步骤的失败路径直接 return，两个一起
  泄漏——这个不对称本身就是证据：作者知道要补偿，只是漏了一个分支）；截止时间**算了、
  存了、发给客户端了，全仓无一处与时钟比较**；版本字段是装饰（每次保存自增然后无条件写，
  且 store 缺席时保存返回 nil——为一次没发生的写报告成功）。

### 存储后端

- **七个包不需要自己的存储代码。** 它们的状态就是 `versionstore.Store[K,V]`，
  `roost-kit` 已经提供了该契约的生产 Redis 实现。每个包只加一个薄构造器
  （`redis_store.go`）——把 `versionstore.NewRedisStore` 用正确的 key 前缀装起来，
  零存储逻辑。这是第 3.1 节那条系统性缺陷的结构性答案：被替换的实现里每个关注点
  有**四个手写 store**，两个变体满足同一接口而 `Update` 语义不同，于是"配了哪个"
  决定文档写的不变量成不成立。现在只有一个实现，契约里没有无条件写。

- **`mail` 的 `EnvelopeStore` 是唯一真正新写的后端**，因为信封不是版本化状态——
  写一次不再改，读路径是批量。用 Redis 而不是文档库：本仓每个包都已经需要 Redis、
  没有任何包需要 Mongo，所以这不增加基建；而信封的形状正好吻合——`SETNX` 写一次、
  一次 `MGET` 批量读、key TTL 按它自己的截止时间过期。被替换的实现把时间戳存成
  int64 毫秒而不是 date，因此**加不了 TTL 索引**，集合无界增长。
  需要信封活得比过期时间更久（作为发件审计）的调用方自己实现这个契约——三个方法。

  批量读用 `IRedis.MGet` 一次往返。它最初是把 `MGET` 包进一行 Lua 脚本走 `Eval`——
  仅仅因为 `MGet` 不在客户端接口上。代价不是难看，是**测试盲区**：Go 的测试替身无法
  求值脚本，所以脚本文本里的缺陷对整个单测套件不可见。方法上了 core 的接口之后脚本
  就删了——这是把盲区**去掉**，而不是绕着它测。

- **`integration/`**：跨服务的真实后端测试。九个包在一个活 Redis 上装起来各跑一条
  路径，外加 key 命名空间不冲突的实测——驱动全部九个包，然后 `SCAN` 回读活 keyspace，
  断言 19 个声明的命名空间各自恰好收到写。十条"故意撞前缀"的变异全部变红。

  它存在的理由是本仓做了一个否则无法支撑的声明：七个包不需要存储代码。各包自己的
  测试跑的是进程内替身，只能证明逻辑，对"复用是不是真的"一个字都没说。

  修掉一处我自己的缺陷：`EnvelopeStore` 的 key TTL 原本用 `time.Until` 读墙钟，
  于是它和 Service 对"现在几点"意见不一致——Service 用注入时钟（每个测试、以及任何
  回放/补投任务都是）时，每一封信封写下去就已经过期。两个组件两个时钟，就是一封
  读起来还活着、其实已被逐出的邮件。

### 装配（`app.Mod`）

- **每个服务一个 `app.Mod`**（`global` 注册两个 capability：路由与活动协调不共享状态，
  一个部署可能只要其中之一，合成一个会给活动那半个理由去碰租约存储）。加
  `servicemods/`：九个 capability 名字表（带 `service.` 命名空间，避免与 roost-kit
  的基建 capability 撞键）+ 各 Mod 共用的配置读取。

- Mod 只拥有**基建接线**。凡是配置给不出的东西都是**构造参数、必填、无默认**，
  `Init` 点名拒绝。这不是风格：被替换的实现里 `platform` 的鉴权**根本不存在**，
  而那个缺席看起来就像一个默认值；`account` 的 playerID 分配器默认是每进程从 1 开始
  的计数器，配 upsert 落库等于重启后建角色摧毁既有玩家。**给这些东西一个默认值，
  就是把漏洞装回去。**

- key 前缀**必填且无默认**。默认值在每个部署里都是同一个字符串，于是共用一个 Redis
  的两套部署——staging 挨着生产、两个分片、一个回放环境——会静默共享状态。
  同理 `mail.send_ttl`、`session.request_ttl`、`global.reservation_ttl`、
  `directory.reservation_ttl` 都必填：它们必须超过调用方最长的重试窗口，而那个窗口
  是调用方传输层的性质，库猜不了；猜错了也不报错，只是让重放变成第二次发放。

- 没有一个 Mod 在 `Start` 里起 goroutine。过期票、失效租约、待重投的订单都由调用方
  驱动 Sweep/AttemptDelivery——节奏是部署决策，而且**一个 Mod 悄悄起的后台循环，
  是没人能看见它失败的循环**。

- 端到端验证在 `integration/`：把九个 Mod 按 app 的顺序走一遍 Init→Provide→Start，
  然后把十个 capability 逐个 `Lookup` 出来**真的用一遍**（不是断言非 nil——一个装着
  连不上存储的服务的 registry，是一个能启动、第一个请求就失败的进程）。18 条变异
  验证，其中两条一度是绿的：一条因为测试扫的 key pattern 会捡到上一次运行的遗留
  （改成 before/after 快照整个 keyspace），一条因为 platform 的会话令牌是**自证**的
  ——把 payment secret 接到 session secret 的位置，签发与校验仍然自洽，只有对着配置
  里的值独立验签才抓得到。

### errcode 接线

- **九个包的 error code 此前全部没有接上 sentinel。** 115 个常量声明在那里，
  而任何 RPC 边界上每个错误都会变成 `CodeInternal`——正是本文档记在 rank 名下
  的那条缺陷（"连 board id 为空这种纯客户端错误都返回 CodeInternal"）。
  两处例外要说清：`chat` 与 `global` 各有一张**手写的 `errors.Is` 映射表**，
  我第一次审计时的 grep 没匹配到，先前的汇报因此不准确。

- 改法是把 sentinel 本身变成 `errcode.Define(Code…, 文本, "")`。
  `errcode.ClientError` 用 `errors.As` 穿透任意层 `fmt.Errorf` 找到 code，因此
  **所有调用点零改动**——mail 转换后 43 条测试原样通过。文本放在 `name` 而不是
  `message`，因为 errcode 渲染 "name: message: cause"，两个都填会重复。

  那两张手写表删掉了：一张逐 sentinel 的表是第二份清单，新增的错误会从上面静默
  掉下去。仍然需要函数的唯一理由是**外部 sentinel**——`versionstore.ErrConflict`
  不带本包的 code，CAS 竞争耗尽是调用方能据以重试的真实结果，"server error"
  不是它能据以行动的答案，所以显式映射进本段。

- **每个包的 `CodeStoreFailed` 都删了。** 未分类的错误诚实地报 `errcode.CodeInternal`；
  为任何一个未分类的 bug 回答"存储失败"，是把猜测当成诊断。九个包里这个常量都在，
  而九个包里都没有能产生它的 sentinel。

- `directory` 此前**只有 sentinel、零 code**——"这个名字被占了"这种最日常的拒绝
  一路以 "server error" 到达客户端。现在分配了 530101-530106 段。

- `global` 的段里留下一个**刻意的洞 570111**（原兜底码的位置）。570111 至今不复用
  ——一个曾经意味着"存储失败"的码，改成别的含义比留着空档更坏。

  **后续更正**：那个洞完全是两个服务从一个号段里发号造成的。`global/activity` 拆成
  独立包后带走了自己的 5701xx 号段并重编到 6201xx（见下），两边各自连续。原先写在
  这里的"活动码不向下重编号，因为已发布版本里可观测"这条理由**没有被推翻，而是被
  一个更大的破坏性变更吸收了**：拆包本身就是 `global.ActivityService` →
  `activity.Service`，本来就需要一个大版本，而在一个大版本里改号是可以写进迁移说明
  的；把一个老号悄悄换成新含义不行，因为按码匹配的客户端拿到的是**错的答案而不是
  一个错误**。所以 570111–570124 整段作废，`global` 新增的 `CodeRequestInvalid` 取
  570125 而不是看起来空着的 570111。

- 八条变异验证。其中一条一度是绿的，原因值得记：我用"返回最内层 code"去变异优先级，
  但 `fmt.Errorf` 双 `%w` 返回的是 `Unwrap() []error`，`errors.Unwrap` 对它返回
  nil，所以那个变异是空操作。优先级并非我的代码实现的——**确立它的是 `chat.denied`
  的 wrap 顺序**（`%w: %w`，拒绝在前）。改那个顺序才真的变红。

### 运维面（`admin.go`）

三个服务有**真正的死路** —— 自动路径已经放弃、而在此之前没有任何代码路径能改变它的
终态。不是补齐对称性:

- **`platform`**：订单尝试耗尽 = 玩家付了钱、货永远不发。`AttemptDelivery` 正确地拒绝
  它（否则重试预算就不是预算），唯一痕迹是一行 `slog.Error` —— 不是工作队列，也活不过
  日志轮转。新增 `ReopenDelivery`（重回重试队列，**只允许 exhausted**）与
  `SettleOutOfBand`（已退款/人工发货）。

  `DeliverySettled` 是**第三个终态**而不是复用 `DeliveryDelivered`：一次把人工退款算成
  已发货的对账，会报出本服务并没有完成的履约。

  加这个状态时自己引入并抓到一个 bug：`AttemptDelivery` 会让 settled 落进"不到期"分支
  并报 `ErrDeliveryHeld`（"a delivery is in flight: order o1 until 0"），而重试钩子把它
  当成正常 —— **一笔已退款的订单会被每 tick 重试到永远**。现在它有自己的
  `ErrOrderSettled`。

- **`global/activity`**：dispatch 尝试耗尽 = 某个 game 服永远收不到活动结果，两条自动
  路径都拒绝它。新增 `ReopenDispatch`。

  **ACK 令牌在重开时不换**，这是这里最容易反过来做错的一条：game 可能已经收到并应用了
  结果、只是确认丢了，之后 dispatch 耗尽；重开会再投一次，而 game 唯一能去重的键就是
  令牌。换新令牌 = 恰好坑掉那些做对了事的调用方。这是 mail 的 claim token 那条规则。

- **`session`**：Releaser 永远不可能成功的资源（副本被带外删了）与暂时故障完全无法
  区分，sweep 永远重试。新增 `ForceRelease` —— 它**不调用** Releaser，因为运维之所以
  在这里就是因为它不可能成功；调了再忽略错误会让"尝试过并已完成"看起来成立。

  后果比看起来严重，而且从 `Run.Live` 上看不出来：`Enter` → claim 被占 →
  `resolveClaim` → run 不 live → 释放资源失败 → **claim 永不释放**。claim 是故意在清理
  成功后才释放的（对的），代价是一个永远无法成功的释放 = 一个**永远进不去的玩家**。
  规划时我把这条说反了（"owner 不会被挡"）——挡住 Enter 的是 claim，不是 Live。

三条共同的约束：**不上总线**（都没有 `//roost:rpc`；集成测试双向钉住"公开 capability
不满足 `Admin`、owner-only capability 必须满足"）、**note 必填无默认**（没有记录理由的
干预无法复核）、**不做枚举**。

不做枚举是对规划时一个说法的更正："找不到耗尽的订单"听起来像问题，其实**支付渠道手里
有权威清单**，运维真正问的是"渠道说收了钱的这些单，我们发货了吗"——`Service.Order`
已经按 id 回答了。在这里建索引是重复一份本服务不拥有的事实来源，而且必须写在设置终态
的那次 CAS 之外，于是它可以和它索引的记录不一致。

**核实后不是死路的两条**（也是我规划时说错的）：`global` 卡在 migrating 用现成的
`AbortMigration` 就能救回（epoch 从 `Resolve` 拿，而且它本来就在跨进程接口上）；
`mail` 满邮箱是有文档的上界、`chat` 超期未裁剪是存储成本，都不是"没有任何路可走"。

三个包共 22 条变异验证。其中**六条一开始是绿的**，全部是测试自身的问题，值得记：
"重开清空退避"那行其实是**冗余的**（耗尽路径已经把它置零了），所以删掉它什么都不变 ——
改成钉住"重开后立刻到期"这个性质，设一个未来时间的变异才会红；`Reopens`/`ForcedReleases`
的累加在只干预一次的测试里看不出来（补了跨多次干预的断言）；而"顺手调一次 Releaser
并忽略错误"这条**连续两次是绿的** —— 第一次因为 fake 只数成功不数调用，第二次因为我
在干预**之后**才读基线，那次多出来的调用已经被算进基线里了。

### 跨进程传输层（`servicerpc` 生成器）

- **九个服务各有一个手写接口 + 一份生成的传输层**（`directory` 故意没有：它是被
  `account` 嵌入的原语，不是服务）。生成的东西：线上类型、handler 注册、打字的
  client、`Server`、`ClientMod`、capability 包装。从**接口本身**生成，所以漂移是
  结构上不可能，而不是被检测到。

- Mod 发布的不再是具体类型，而是 `OwnerCapabilities(service)` 返回的两个
  capability：调用方查的接口名，以及 `Server` 用来判断"本进程是不是拥有者"的
  owner-only 名（公开名 + `.local`）。

- 接口刻意比进程内 API 小，每个省略都有理由（对照表见 README）。三处值得单记：

  - `platform.ValidateSession` **没有 ctx**。这不是签名疏漏——它不做任何 I/O，只用
    本进程已有的密钥重算一个 MAC。生成器要求首参是 `context.Context`，于是这个方法
    自动落在接口外，而这个"限制"恰好是对的：一个什么都不碰的方法没有理由是一次往返。
  - `chat.ChannelRef` 的 key 字段是**未导出的**（故意的：ref 只能来自 `Resolve`），
    所以它根本过不了总线——任何 codec 都会静默丢掉 key，对面拿到的 ref 指向空。
    生成器按"未导出字段"这条规则点名拒绝，并指出是 `ref.key`。
  - `chat.PublishSystem` **在**接口里。关于信任的判断没有任何一部分上线：请求不带
    令牌，令牌由拥有者进程的 `SystemAuthenticator` 从 handler 自己 ctx 里的传输身份
    铸出。总线情形因此 **fail closed**——总线不带可背书的调用方身份时签不出令牌，
    直接 `ErrSystemDenied`。缺一块拼图产生的是拒绝而不是许可，这正是被删掉的
    `Trusted` bool 搞反的那件事。

- 每个服务手写一个 `run(ctx)` 钩子，"没有周期性工作"也要显式写出来。写下来之后
  `match`/`session`/`platform`/`chat`/`global/activity` 五个答"有"，
  `rank`/`account`/`global` 三个答"没有"，而且答"没有"的理由各不相同（README 有表）。

- **`global` 拆成 `global/` + `global/activity/`（破坏性）。** 触发它的是生成器新增的
  "一个包只能有一个 `//roost:rpc` 接口"规则：生成文件在包级别声明十几个固定名字，
  两份就是每个都声明两次。而这条规则只是把一件早就成立的事说出来——`app.Service`
  每进程一个，所以 `global` 那两个 capability 从来就是两个独立部署的东西。
  两者**不共享任何类型、任何 store**（拆的时候确认过：活动侧一个 routing 符号都没用）。
  变化：`global.ActivityService` → `activity.Service`、`ActivityKey` → `activity.Key`
  等去掉 stutter 的重命名；配置段 `global:` → `activity:`；错误码 5701xx → 6201xx；
  各自一个 Mod，于是"Mod 叫什么"和"它发布什么"重新变成一件事。

- **两条新的包级拒绝规则**（都由真实事故推出来，见 roost-codegen CHANGELOG）：生成名
  与包内已有声明撞名（`account` 的 `type Server struct` 撞生成的进程壳 `Server`，
  已重命名为 `GameServer`）；一个包两个被标记的接口。

### 构建与生成

- **go 指令 1.25.0 → 1.27.0**，与 core / kit / codegen 和 `go.work` 统一。取 1.27.0
  而不是最新的 1.27.1：补丁级的 go 指令什么都买不到，还会让停在 1.27.0 的工具链去下载
  一个新的。`go mod tidy` 顺带纠正了两处旧标注——`spf13/viper` 与 `roost-core` 从
  `// indirect` 改为直接依赖，各 Mod 的 `Init(cfg *viper.Viper)` 与 core 的
  `app`/`errcode` 一直在直接用它们。

- **`roost-codegen` v1.12.0 接成 tool 依赖**（`tool
  github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc`），于是 `GOWORK=off` 发布态下
  `go generate ./...` 也能跑——此前只有 workspace 里能重新生成，提交的生成物是唯一
  可信来源，而"生成物是否与接口一致"在发布态无法验证。九个包现在都报 `up to date`。

  代价很小：go.sum 多两行（本仓只依赖 `gopkg.in/yaml.v3`，不成环、不牵扯别的东西）。

  接进来时先撞到一件事：`go mod tidy` 把本仓的 go 指令从 1.25.0 顶到 **1.26.5**（
  roost-codegen v1.12.0 的 go 指令），而且**手工按回去不管用**——下次 tidy 又顶回来。
  也就是说一个生成器的 go 指令是每个消费方都要满足的下限。**已按"五仓统一到最新"
  处理**：core / kit / codegen / service / skill 与 `go.work` 全部改为 `go 1.27.0`，
  于是 tidy 稳定不再动它。这条代价记在 roost-codegen 的 CHANGELOG 里：1.25.x /
  1.26.x 两条 consumer lane 因此没了。

  工具依赖钉在 **v1.12.1**。v1.12.0 是不能用的：它的模板在 owner-only 名下注册
  capability 包装器，所以 `GOWORK=off go generate` 会**静默把五个服务的进程级修复
  改回去**（实测过，它把 mail 改回 `Value: wrapped` 并报"generated"）。现在发布态
  `go generate` 报九个 `up to date`，发布态 `-check` 面对一个手改过的生成文件会
  `STALE` + 非零退出。



### 集成测试

- **补上了缺失的那条测试:每个拥有者进程真的能 `Serve`。** 它的缺席藏了一个五个包
  都中的进程级致命 bug。

  拥有者 Mod 发布的是 capability **包装器**（这是消费方绑不到实现类型的原因），
  `Server.Service()` 把这个值交给手写的 `run` 钩子——而钩子**必须**断言具体类型，
  因为它要调的正是那些**刻意不上总线**的拥有者专属方法。于是
  `match`/`platform`/`chat`/`global/activity` 的 `Serve` 直接失败、进程起不来;
  `session` 那条是**写在 ticker 里的裸断言**,30 秒后 panic,而且只在真的配了
  owner 的部署里 panic。

  没有测试抓到,因为**没有测试调用过 `Serve`**——只测了 `Init`,而 `Init` 恰好是
  好的那半。修法在生成器侧（owner-only 名下放未包装的实现，见 roost-codegen
  CHANGELOG），这里补两条断言:`TestEveryServerServesInTheOwningProcess`（九个包
  都能 Serve）与 `TestTheOwnerOnlyCapabilityHoldsTheImplementation`（直接钉住修法，
  对那四个今天不需要具体类型的包也成立）。两个方向的变异都验证变红。

  `session` 的断言同时**从 ticker 里提到循环之前**。这是这件事真正的教训:惰性断言
  把一个接线错误从启动挪到了生产流量里。提上来之后,那条变异从抓四个变成抓五个。

- `global/activity` 缺了兄弟包都有的错误码测试。缺的两条恰好包括**外部 sentinel 映射**
  （`versionstore.ErrConflict` → `CodeConflict`）——而那段 `errors.Is` 分支是拆包时
  **新写的**,拆之前它借用 global 的 `Error`,所以从来没有被任何测试碰过。这个包每次
  存储调用都走 versionstore,不是冷路径。

- **默认跳过。** 没有 `REDIS_ADDR` 时 `integration/` 全部 `t.Skip`，本轮因此抓到一件
  事：`session`/`rank`/`match`/`platform` 四个包里断言**具体类型**的集成子测试
  （`app.Lookup[*session.Service]` 之类）长期"绿着"，真的连上 Redis 那天同时红掉。
  它们现在全部改成查接口，并新增
  `TestEveryOwningModPublishesTheInterfaceAndNotTheImplementation`——**九个包**一起断言
  "接口能查到、具体类型查不到"。反向变异（把 `OwnerCapabilities` 换成
  `Value: Coordinator(service)` 这种调用点转换）确认变红：Go 存进 any 的动态类型仍是
  `*Service`，所以调用点转换达不到这个效果，必须有包装类型。

- `global/activity` 的六个 key 命名空间此前**从未被覆盖**（它们挂在 global 前缀下，
  而没有任何测试驱动它们）。现在纳入 keyspace 走查，并且为了让六个都真的被写到，
  测试把活动驱动到完成：dispatch 记录只在聚合结束后存在，audit 只在一次 notify 被拒
  之后存在。逐个命名空间改名的六次变异全部变红。

### 依赖的框架能力

本仓依赖 `roost-kit` 的三个包，它们是为本仓补的前置：`versionstore`（版本化状态契约，
**没有无条件写**，因此非 CAS 实现不可能存在）、`servicerpc`（服务间 RPC 客户端 +
按 key 亲和的 picker）、`mongo/mongotest`（求值 filter 的内存 MongoDB）。
