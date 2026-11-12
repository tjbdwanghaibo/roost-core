# matchmaking

`roost module add matchmaking` 自动安装 team、matchroom、room，接入托管 match 客户端和模块后台循环。客户端先完成 EnterGame；本模块使用正式 Nest / DAO，不在实体锁内等 RPC，也不引入 Saga。

## 客户端流程

1. `MatchStatus` 返回个人 Sequence；新一轮 `MatchEnqueue{Sequence:当前+1}`，重发仍用原 Sequence。
2. 组队先 TeamCreate、TeamInvite、TeamAccept；队长带 `Team=true` 排队。名单全部成员必须驻留本服、没有别的匹配/换服意图。
3. `MatchStatus` 补查进展。matched 后返回 RoomID / MatchID；客户端用 RoomJoin 进入，RoomReconnect 仍使用 Room 的连接 nonce 和 Epoch。
4. 取消携带当前 Key 调 MatchCancel；取消与成局以 match 的原子 Cancel / Commit 结果为准，可能返回 matched，不能把取消请求发出当作取消成功。
5. cancelled / expired 可 MatchClear；matched 要等 RoomClose 或最后成员离开使房间关闭后才可 Clear。Clear 保留 Sequence，旧请求不能重新排队。每名成员分别清理自己的结果。

没有自动推送，查询是正式恢复入口。客户端建议按 500ms 左右查询；断线不意味着退出队伍、撤销排队或丢弃已经成局的结果。

## 默认匹配规则

一张 match Ticket 代表一支完整队伍；`GroupSize=2` 表示两支队伍，单排是一人队伍。同人数队伍在同 sid 的池子中配对，房间人数是队伍人数的两倍，最多 16 人。一个 2 人队不会被拆成两张票，也不和 1 人队拼局。策略位于 `game/matchmaking/matcher.go`，工程可修改，不把通用 match 服务的张数误当实际人数。

队伍权威和所有 Player 排队意图在 `[Team, []Player]` 严格事务中提交。排队期间 Team.MatchKey 冻结名单；结果写到每个成员，解除冻结只允许对应 Key。单排直接写 Player。默认 500ms 匹配一次，各池最多读 64 票，只接受当前驻留且每个 Player 已经绑定该票的候选。没有跨服房间路由、MMR、跨池扩圈或满载吞吐承诺。

## 未知结果与恢复

- preparing → dispatching 的持久转换选定一次发送。Enqueue 回包丢失只查候选，不无限重发。只有绑定到全部 Player 的 Ticket 才能成局。
- 从未绑定过票的意图在 60 秒后可安全过期：迟到 bind 被终态 / Key 检查拒绝；外部孤票即使晚到也不会被本模块选中，最终由 match TicketTTL 清理。该期限不是已绑定票的等待时限。
- 已绑定票用 Ticket / Match 查询。Commit 成功但开房或本地结果提交失败时，下次查询恢复同一个 MatchID → RoomID，再补齐各队结果。重复提交不重复开房，房间即使已关闭也能确认原移交身份，不重新打开。
- match 的已结束历史默认只保留 10 分钟。**票已绑定而历史已消失时返回 matchmaking_unknown，继续保留意图和名单冻结**。这不是取消或超时自动解锁；需要运营依据存储/业务记录核对。生产必须让查询/恢复在保留期内完成，不能删账本强行重排。
- 数据最多保存当前意图与最后结果，Player 不积累无限请求历史。匹配房映射是持久独立文档，历史清理需与业务留存策略一起制定。

## 联动与验证

rehome 双向拒绝：有匹配状态不能换服，换服意图未清不能排队。已有队伍限制仍由 team + rehome 执行。

生成工程验收包含单排/两队成局、取消与 Commit 的两种先后、掉线补查重连、重复结果、未知 Enqueue、历史到期不解除屏障、队伍冻结与整组回滚、旧序号、活房不能清理和端点注册时序。match 使用正式 MemoryStore，Remote/WAL 提交使用替身；这不是 Redis/Mongo/NATS 联合验收。turn-pvp 配方另跑 TCP/Robot → PB → Controller → Nest 的排队至回合链路，房号映射用 mongotest，真实依赖仍在交接 §6。
