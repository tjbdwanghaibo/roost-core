# N04 第五批：Redis 锁续租与订阅退出

**最终状态更新（2026-10-04）：RR-20261004-01已由上游3bb901fb/5d386146修复，本轮独立验收通过，未发版。** 本轮原4场景在真实Redis全部转绿；13条取锁/释放未知正式回归race与Remote vet通过，另3条真实Redis集成通过。下方本轮“新wanted未修”保留发现时点，以本条及独立验收为准；三资源生成消费者、authority故障矩阵与长稳未在本机验收。 [本轮红绿/13正式/3真实集成](evidence/noncore-review-20261004-18/README.md#独立验收上游修复)。最终整合到5d386146之后，原N04证据基线仍3d3b22c9。

2026-10-04，main 基线 `3d3b22c9`，同树[NC-26～29 已修](REVIEW-2026-10-04-noncore-17.md)。重新读 `redis/driver/lock.go`、`pubsub.go`、`client.go` 及接口/已有锁测试，沿 token、watchdog 和关闭所有权补证。**普通锁/续租/订阅14个新增有界场景全部通过，这14项未确认新RR；提交前新wanted另确认一个P2。** 此结论只覆盖下表，不是 Redis 域收敛。

| 已验证不变量 | 新场景 | 实际依据 |
| --- | --- | --- |
| 不确定写不得直接重用对象 | acquire/release/extend 各 1 | 真实 Redis 执行后注入回复错误；errors.Is 保留、Acquire 拒绝、按 token Release 后可重用 |
| 旧持有者不能改新 owner | stale release、stale extend | Redis 自身使旧 key 过期，再用第二对象 acquire；Lua 校验不改变新 token |
| 续租生命周期和丢锁可见 | healthy、caller cancel、transient、persistent、server loss | healthy 跨过原 TTL 后 key 活着；caller Acquire ctx 取消不结束 watch；短暂失败恢复，持续失败/服务端丢锁分别报告原因 |
| 订阅转换能够退出 | delivery、full queue close、concurrent close、multiple channels | server NUMSUB 确认后发布；明确观察满输出队列，再关闭；16 个并发 Close；通道/消息映射及最终 channel closed |

本机独占 loopback Redis 8.8.0，review overlay + `go test -race -count=1 -timeout=120s -json`；最后退出 0，14 pass/0 fail/skip，实例退出记录保留。回复丢失是命令执行后的错误注入，**不等于** toxiproxy 真实弱网；满队列先观察实际容量状态，healthy 等待的是 TTL 契约。[复跑、事件、hash、coverage](evidence/noncore-review-20261004-18/README.md)。

源码事实：distLock 为单实例 SetNX、随机 token 和带值守卫 Lua，uncertain 必须先 reconciliation；AutoExtend 独立 Background watch，逐次 Extend 带 interval 预算，失败到 TTL 后暴露 Err；Release 等 watch 完成再释放。它不产生 fencing。pubsub 两层 select 使满输出队列能响应 done，Close 由 once 管理；Subscribe 没有可供调用者等待的 ack/error API，本轮用 server NUMSUB 做测试就绪判据，不能把刚返回当消息可靠投递承诺。

观察和未验证：非协作 inner Extend 可卡 Release、opMu 等待不受 caller ctx 控制，前批已记录边界，本批不重复登记；服务端执行到回复的延迟会影响 watchdog 本地租期估算，未做时钟/长暂停差分；显式 Extend 更改 TTL 与固定 watchdog TTL 的组合、断线自动重连/丢消息、Client.Close 和独立 subscription 的所有权、真实网络故障、Cluster/HA、长期 goroutine/连接容量与性能仍需具名验证。没有以无 fencing、缺 ack 或样本数量登记伪 bug。

原 N04 固定清单 40/40 源文累计已读；本轮新增事务实现 `mongo/mongotest/transaction.go`，当前快照分母 **41**，累计源文 41/41，39 相同 hash 复用、mongotest 当前 diff/源码补证、事务新文件全文读取。不是新读 41 文件、不是业务 100%，N01～N04 均仍不计 completed/15。

提交前再次fetch发现10-04新wanted，已追加下节分流；10-01四项仍已有分流。下一优先 **RefHMap schema/Patch 与全量 Set 并发及未知结果恢复 → 正式迁移消费者**；真实 Mongo、Cluster/HA 按资源条件另验。[学习](IMPLEMENTATION-REDIS-LOCK-RENEWAL-AND-PUBSUB-LIFETIME.md) · [进度](PROGRESS.md) · [留项](../bug/CARRYOVER.md)。

## 提交前新增wanted

提交前fetch发现 `e3118640`、`5e1f7ee3`、`17ae93be`、`cfe878fe` 四个提交（隔离mongod缓存、负载错误区间核验、wanted、C01判据文档）。保留并整合到最终提交；N04产品与必要消费者源码未变，原红绿/14场景证据基线不改。新增harness/长稳记录仅作接手事实，本机未运行或完整review它们，不冒称已验其24h/1h结果。

W-2026-10-04-01优先分流为 **[RR-20261004-01 P2，已确认未修](../bug/RR-20261004-01.md)**。MCP receiver合并导致TryLock遗漏，按coverage读当前TryLock、Lua和旧未知释放机制；产品文件3d3b22c9..cfe878fe无差异。独占Redis实际取锁后丢回复，首次与释放未知回收后两个反例失败；healthy/未执行两个控制通过，[4叶子2fail/2控制](evidence/noncore-review-20261004-18/wanted.jsonl)，原错DeadlineExceeded保留，服务端owner非空、TTL>23h，本地未获业务许可，随后重取NotAcquired。实例已退出，不采用压测偶发作为唯一证据。

方案优先已有owner/fence Lua与锁状态；不能只覆盖单个unknown token，回收旧S时新的T可能未执行或已执行，连续未知需要有界attempt裁决及authority/续期代际矩阵。本轮只审查，不递归修新RR；该bounded wanted不计N04文件分母，不复审三大核心。[原始证据/复跑](evidence/noncore-review-20261004-18/README.md#新增wanted)。

## 上游修复独立验收

第二次提交前fetch又发现3bb901fb和5d386146：另一线已登记并修复同一RR，合并双方主记录/分流文档，保持原红证据。新实现每个锁对象有随机tokenPrefix和递增tokenSeq，Lua只回收本对象较早代际，拒绝同/较新序号及别的前缀；不是丢弃或无界保存unknown token。语法/owner/fence既有执行链复用，authority分支不改；适配替身须接受第四个ARGV。

独立读当前TryLock/Lua及所选用例，4个原真实Redis场景全部转绿（24h TTL反例立即取回），13条取锁/旧释放未知正式回归race和Remote vet通过；3条真实Redis集成验证丢回复、连续未知/未发送、中途迟到旧脚本、新代际保护及另对象不被挤掉，3pass/0fail/skip。当前RR已修、声明场景验收，无新未修RR；未运行其他线完整三资源生成消费者/authority未知结果/长稳。原source-hash分为N0450路径、cfe红5路径、5d验收9路径，不能用当前hash假装旧红树。下一仍schema/并发/未知恢复→正式迁移消费者。
