# naming：全服唯一名字

`roost module add naming` 自动装 profile、Directory Mod 和 Redis 依赖。Directory 是当前进程访问共享 Redis 的机制，不是独立的 RPC 服务；所有目录 I/O 都在 controller 协调器里，不能在 Nest 业务 worker 中调用。新 game 工程会生成按项目隔离的目录前缀与 60 秒预留配置。生产环境的所有 game sid 必须共用同一套目录前缀与 Redis；不同游戏不能共用前缀。

名字先经 profile 校验：合法 UTF-8、首尾去空白、1–16 个字符；目录的 `NormalizeLower` 再决定大小写不敏感的唯一键，模块使用 `player-name/` 命名空间。初始空名不占名。名字没有敏感词、花费或次数限制；规则应在受理前增加。没有旧数据兼容或导入：原来直接写进 Profile 的名字不会自动补登记。

## 正式入口

- 客户端：`NamingRename(RequestKey, Name)`。同一次操作重发同一个 key 与目标；key 为 1–64 字节，不能带首尾空白。响应 Code 为 0 才表示此次操作及旧名清理完成。失败时不要随意换 key：可能已有在途改名。
- 服务端：`game/controllers/naming.FromRegistry(registry)` 后 `controller.Rename(ctx, playerID, key, name)`，只能在事务之外调用。
- 原有 `ProfileRename` 客户端协议自动接到同一个协调器；它用目标名字的 SHA-256 作重试键。原来的本地 `ProfileRenameSender` 被实体守卫拒绝，答 `naming_required`，不会绕过目录。
- `ProfileComponent.Rename` 和直接 DAO setter 是可信的持锁内部写入原语，不能用作装有 naming 工程的业务改名入口。它们与任意业务直接改 DAO 一样不承担唯一性；协调器只在目录 Commit 成功后调用该原语。模块未新增绕过 API。

同一个 key 改成别的目标会拒绝。仅保留一笔最近回执：完成之后再开始其他操作，旧回执就被替换；无限期业务幂等由调用方自己的账本承担。完全相同的当前显示名可以作为新 key 的已完成回执。只改大小写也会重新占名，当前目录 owner 不同会被拒绝；需要这种展示规则时应专门增加保持原 claim 的改写路径。

## 持久状态机与恢复

所有 Player 阶段转换是 `rollback=undo durability=strict` 的独立 Nest 事务。目录 Reserve 的同 owner 幂等不能当互斥；先在 Player 上持久化在途操作，后续不同 key 被拒绝。Seq 单调增加，owner 包含完整 Player EntityID 与 Seq，因此晚到的旧 Release 不能删除同一玩家后来重新取得的名字。

| Phase | 已确认的事实 | 下一步 |
| --- | --- | --- |
| 0 | 无在途；保留最近 key/目标作为回执 | 接收新 key |
| 1 | 玩家改名意图已提交，旧名仍有效 | Reserve 新名；把 claim/token 写入 Player |
| 2 | 新 claim 已提交到 Player；Profile 仍是旧名 | Commit 目录，再原子写 Profile 与当前 claim 的 owner，进入 3 |
| 3 | Profile 已是新名，目录新名已永久占有 | 按旧 owner Release 旧名，再清在途阶段 |

超时、目录提交响应丢失、Nest 提交结果未知、旧名释放失败都保留在途状态，**不 Cancel 或 Release 新名字**。同一请求重试会从存下来的阶段继续；进程重启后使用同一 key/目标调用相同入口即可，不依赖进程内锁或缓存，也没有后台 goroutine。客户端或业务必须保存待确认的 key/目标；一期没有全服扫描器、自动清扫或按玩家查询 pending 的协议。

Reserve 明确答 `directory: key is taken` 时，只能撤掉尚未保存 claim 的 Phase 1 意图。Phase 2 的 Commit 明确答 claim 已过期/失效时，只清旧 token 并回到 Phase 1；本次仍返回该错误，下一次同 key 重试重新 Reserve。其余错误不能推断成“没提交”。旧名 Release 的响应丢失后，旧名可能已被别人占有；重试得到 OwnerMismatch 时按旧 owner 已退出处理，绝不删除新 owner 的名字。

人工处理在途记录之前必须同时核对 Player 与 Directory，不能只删某一边。

永久故障可能让一个玩家暂时持有新旧两个名字，这是保留唯一性与恢复凭据的边界。成功改名并清理后才释放旧名，不提供跨两个存储的分布式 ACID。调用方应在有限超时内返回并按原 key 重试，不把未知结果显示为成功。

## 验证

`roost module verify naming` 固定 15 条，无子测试：唯一性、重放、旧名释放、目录提交丢响应、真实 Nest 档案提交失败、档案回包丢失、释放失败恢复、同玩家不同操作互斥、同操作并发重试、过期 claim 恢复、旧 Sender 拒绝、旧 controller 统一入口、输入与受理提交拒绝零写入、晚到清理防代际误删、唯一 Nest worker 不被目录 I/O 占用（其中受理拒绝与输入为一个场景）。Directory 使用真实机制加内存 versionstore，Nest 使用真实引擎加替身提交器；未验证真实 Redis/Mongo、跨进程故障、TCP 网络或长稳。

专项安装与可编译变异在 `codegen/internal/roost/module_naming_friends_test.go`。业务文件归生成工程所有，调整规则后重跑 module verify。
