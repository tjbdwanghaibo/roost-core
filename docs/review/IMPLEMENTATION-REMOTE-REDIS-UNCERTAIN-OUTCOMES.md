# Remote Redis：执行成功与调用成功的区别

2026-09-13，Core `18a3790c463011050e4f05a5878d300c6144fdfe`。现有实现学习及本轮验证；建议均未实施。关联[问题](../bug/REVIEW-2026-09-13-03.md)、[运行](REVIEW-2026-09-13-03.md)。

## 当前 ownership 转移链

Manager.TransferRemoteOwnership 先取得 wrapper writeGate 和 ownershipMu；shared 模式还取得分布式锁并刷新 marker。随后 live 进入 Draining，调用 ownershipStore.TransferExpected。Redis Lua 使用当前 lease 字符串与 expected 精确比较，成功后推进 marker/route 并变更 owner。正常成功时，Manager 更新 wrapper 和 live，旧 owner 进入 Fenced。

本轮真实脚本验证：正常 claim→shared→local→transfer、旧 lease CAS 拒绝、32 个不同 owner 竞争 claim 的唯一赢家均通过。它们证明正常原子操作成立，不覆盖调用方在应答丢失后的恢复。

## 不确定结果必须保留不确定性

网络调用错误可以发生在发出请求前，也可以发生在 Redis 已提交之后。当前 Manager 对 Transfer 错误统一恢复旧 live 状态，缓存 marker 不失效。因 marker 热缓存是本地写准入的优化，这种恢复让旧 owner 继续通过 Prepare。

本轮在真实 Lua 成功后丢弃返回值并返回错误，重现 Redis owner=2000 而本地 owner=1000 仍获写准入。相同注入放在执行前则是合法恢复对照。Loader/锁仍使用替身，未做业务 commit，所以报告准确限定到准入违背所有权状态。

建议恢复状态机明确区分确定失败、确定成功和未知：未知结果使 marker 失效、live 不能写；重新查询权威 lease 后才选择 Fenced 或恢复。用于恢复的 context 不能直接复用已过期请求 context；查询有界，查不到继续隔离。不要为避免误拒绝而静默恢复旧权限。

## 数值与线格式也是协议

快照 Lua 的比较值经过 tonumber，而 Go 允许 uint64；真实 Redis 测试在 2^53 附近出现旧值被接纳/新值被判同版本冲突。现有 snapshotRedisFake 用 Go uint64 实现比较，因此替身测试永远不会表现这种舍入。

Ownership Lua 的数字还被拼接进字符串。大 epoch 可能变成科学计数法，既不满足 Go ParseUint，也不满足下一条 Lua 的 `%d+` 匹配。Get 解析失败并不能撤销之前的 HSET；因此输出可表示性是持久化前条件。

默认从小计数运行时风险很低，按 P3 管理，但协议必须声明支持域。修复可选择显式限域或精确字符串算法，需同时考虑比较、递增、持久化与解析；只改其中一层会产生新的不一致。

## 热缓存收益与安全边界

MarkerCacheTTL 默认 500ms，降低每次本地 owner 写的 Redis 查询成本。但 ownership 操作是本机主动参与的控制面变更，遇到未知结果时主动失效缓存，比等待自然 TTL 更符合安全语义。本轮一分钟 TTL 只是确定性暴露窗口，不是修改默认配置。

快照真实 Redis 的 32 并发发布正确保留最大普通版本，PTTL 为正且不超过配置一分钟；这验证原子版本比较和 TTL 设置的一个正常切片，不是吞吐压测，也没测 key 真正到期后的恢复。

后续：Enter/LeaveShared 应答丢失、恢复查询失败、默认 TTL 时序、真实存储 commit fencing，以及 Redis failover/重连。Mirror 设计依旧应复用这些机制，但复用前必须携带已知边界，不能因为已有 CAS 就宣称端到端所有权转移完成。
