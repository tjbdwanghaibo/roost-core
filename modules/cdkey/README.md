# cdkey

`roost module add cdkey` 安装 mailbox（连带 wallet），并自动接入 directory Mod。配置表自带 `WELCOME`（每名玩家一次、100 金币）和 `ONE-GIFT`（全服一次、500 金币）。生产项目替换示例码；表在 `configs/schema/cdkey.go`、`configs/table/cdkey.csv`，改表后运行 `roost generate`。码只含 ASCII 大写字母、数字、`-`、`_`，至多 64 字节；请求会去掉两侧空白并统一大写。

客户端发 `CdkeyRedeem{Cdkey}`，成功返回 `MailID`，再用 mailbox 列表及领取入口领附件。奖励不直接进钱包。可信服务端在实体事务外取 `game/controllers/cdkey.FromRegistry`，调用 `Redeem(ctx, playerID, code)`；输入是原始玩家号。

兑换分四步：Player 严格持久事务冻结意图和奖励，directory 永久占用，`mailbox.SendTo` 幂等发件，Player 严格持久事务确认邮件号。跨进程操作都在 controller，Nest handler 只操作实体。任一步结果未知都保留责任；重启后客户端用原码重试，接着原来的事实继续，不换键、不释放占用。已确认的重复兑换不调外部服务，不写任何 DAO 字段，也不受配置下架影响。

`once` 的权威键只有码，`player` 的权威键含玩家号；两者通过 directory 的 Redis 原子版本存储判定。所有 game sid 必须使用同一套共享 Redis、相同项目 namespace。不能给 directory 持久键配置 TTL、删除 `cdkey:*` 占用或复用发行过的码；同一码的模式与奖励不能改，新活动使用新码。Redis 的生产持久与备份策略属于部署责任。

玩家账本最多 128 条，包含成功回执和已受理的在途意图，满额拒绝新码，仍允许旧码恢复。不会淘汰成功回执来换取重复发奖风险。全服码已经被别人占用时，当前玩家的受理意图仍保留：它没有奖励、不会越过共享权威，且计入上限。配置中开始/结束时刻只决定首次受理，0 表示无该侧边界；已有意图按冻结奖励恢复。发件已生成而邮件过期后仍不会换新邮件补发；实际领取期限是邮件本身的期限。

当前恢复由相同兑换请求驱动，没有启动扫描或后台重试。12 个独立场景使用正式生成 PB/controller/Sender/Nest、真实 directory/mail 业务实现和内存存储/替身提交器；含共享唯一性、结果未知、配置下架恢复、严格提交回滚、并发和零写入。变异将全服键错误加入玩家号，先确认可编译，再确认跨玩家场景失败。NATS、Redis、Mongo、WAL、TCP 和跨进程重启尚未在这些离线场景中验证。

本轮验证（macOS，2026-11-12）：`GOWORK=off go test -count=1 -run '^TestModuleCdkeyNoticeInstallAndVerify$' ./codegen/internal/roost` 通过；它从全新自定义 module 工程安装两个模块，实际 `VerifyModules` 得到 cdkey 12 PASS / 0 SKIP、notice 8 PASS / 0 SKIP，以及 build、vet、ID、生成漂移检查通过。在生成工程执行 `GOWORK=off go test -race -count=2 ./game/controllers/cdkey ./game/notice` 通过。测试用例内重建 Player/Nest 并经 `RestorePersisted` 恢复意图，不替代真实数据库重启验收。
