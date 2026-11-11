# friends：同进程在线好友

`roost module add friends` 自动接 Player DAO、组件、Nest handler、PB 协议与 TCP 接入。没有其他业务模块依赖。初始三个名单为空；每个名单分别最多 64 人，可在生成工程的 `FriendsLimit` 调整规则。

客户端 `FriendsChange(PeerID, Action)` 的操作者只取已认证会话，不能伪造。`Action`：1 申请、2 同意、3 双向删除、4 拉黑、5 取消拉黑、6 拒绝申请。`FriendsList` 只查询自己的好友、收到的申请与黑名单，结果排序且是值拷贝；没有其他玩家档案与名字、在线推送或聊天功能。

## 一致性与边界

客户端修改只允许**同一个 game 进程里双方都有已认证 TCP 会话**，由本进程的 `ActiveSessions` 核验；没有本地会话就拒绝，客户端不能通过填写 sid 冒充同服。断线后可以读取自己的名单，但不能向离线玩家申请或修改关系。一期不支持跨服或离线好友操作，不会为远端玩家创建本地镜像。

两个 Player 由正式 Nest 多实体 handler 同时取得执行权，使用同一笔 `rollback=undo durability=strict` 事务。DataEngine/实体 owner 仍须是工程的正式接线，不能配置允许两个进程写同一实体的自制 getter。会话检查之后遇到迁移/交出等 owner 拒绝，由 Nest 返回失败；该检查不是分布式锁。

服务端调用 `FriendsChangeSender.MultiSync_FriendsChange(ctx, actorEntityID, peerEntityID, action)`；调用者必须已经确认两名玩家属于本服。组件 `FriendsComp().Change(peer, action)` 只能在同一事务同时锁住两边时调用，不能拿解锁实体或远端视图来调用。没有跨服事务、Saga 或邮件降级路径。

- 申请写对方 Incoming；相同申请重发不写入，不占第二格。双方任一把对方拉黑就拒绝。
- 同意要求自己收到对方申请；先检查双方好友容量，再同时写两边好友并清双方之间的申请。已是双向好友时重发零写入。
- 删除同时移除两边好友和双方之间的申请；拉黑还在操作者的单向黑名单里记对方。
- 取消拉黑只删除黑名单，不恢复旧好友或旧申请；拒绝只删除自己收到的该申请。
- 任意检查失败不写；提交失败双方一起回滚。存储只保留有界当前名单，没有永久关系历史、TTL 或申请通知队列。

## 验证

`roost module verify friends` 固定 12 条，无子测试：申请幂等、接受与交叉申请清理、无申请拒绝、对称删除、拉黑与双向申请拒绝、取消拉黑、拒绝申请、三类名单边界、两实体提交回滚、非法/缺失目标零写入，以及客户端进程边界拒绝和认证操作者/本人查询。

场景使用实际 Nest、生成 DAO/Sender/PB/controller 和替身提交器/在线会话能力；未做 TCP 网络、真实 Mongo/WAL、跨进程或长稳验收。专项安装与可编译变异在 `codegen/internal/roost/module_naming_friends_test.go`。
