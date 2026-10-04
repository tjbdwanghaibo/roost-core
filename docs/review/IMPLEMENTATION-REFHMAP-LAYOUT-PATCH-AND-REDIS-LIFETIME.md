# RefHMap 布局、Patch 可见性与 Redis 生命周期

2026-10-04，首次RefHMap/Redis/Mongo源文基线`1502f973`，随后[修复NC-16～20](REVIEW-2026-10-04-noncore-13.md)，并在`ce90e90d`起点[修复NC-21～25](REVIEW-2026-10-04-noncore-15.md)、[继续N04](REVIEW-2026-10-04-noncore-16.md)。[原反例](evidence/noncore-review-20261004-12/README.md)保留；新NC-26～29仍未修。

## 类型树与物理键

[RedisRefHMapStore](../../cache/ref_hmap.go)用sync.Once为V建立layoutRoot和prefix。递归field分为scalar/struct，检测类型cycle和MaxDepth；redisdao/json标签决定名称，未导出或`-`跳过。每节点suffix由字段路径决定，plan把entity key放在同槽标签内；root内`__keys`登记物理键，下一次Set/Delete包含旧schema登记的key。Get对当前plan所有key做HGetAll，再按parent中的引用存在与否恢复子结构；它不把任意被篡改的引用当远端重定向地址。

同槽仅保障脚本/Cluster路由条件，不能证明不同节点的逻辑key唯一，也不能隔离root保留字段。NC-19修后layout在任何I/O前检查全树键唯一，并拒绝根__keys、同hash字段重复和冒号/换行名称；原合法持久格式保持，非法旧布局不自动改名/清理。需要支持这些名字时单独版本化，不能为了修错键静默换所有业务键。

Get用reflect.New(root.typ).Elem构造内容，再按根V返回struct或指针（NC-16）；nil根Set在KeyOf前拒绝，miss仍返回零值/false。文本scalar判断支持*typ的MarshalText/UnmarshalText，NC-17修后为指针receiver建立地址副本，不再依赖原value可寻址；Patch复用同一codec，失败写前返回。副本只隔离scalar本身，不宣称任意引用字段深copy；历史坏字节需显式兼容。

## 全量 Set 与字段 Patch

Set验证/keyOf，Stale时先Get（建议性比较），encode各节点，添加registry，再执行同槽Lua：删除登记键→写所有新hash→设置每hashTTL。[NC-21现已修](../bugfix/RR-20261004-NC-21.md)：Eval错误直接保留原始原因，不再DEL重放/降级返成功，旧degraded告警/计数移除。真实Lua后丢回复注入、另一正式写者v3与生成消费者已验；没有整体CAS、网络未知自动回滚或真实TCP故障证明。

Patch找scalar路径，由patchTarget列出根到叶的键与引用；NC-18修后单一同槽Lua先检查全部路径TYPE、要求root已存在，再维护父引用、目标字段、registry和路径TTL。nil parent可以建立，缺整条root明确Unsupported，错误不降级重放。local/raw生成DAO仍Get→PatchStructPath→Set，ref-hmap直接转Patch；正式生成ref-hmap的nil父与已有路径更新已实测，两种模式缺root等边界仍各守其契约。

修复复用plan、patchTarget与同槽脚本，不进行Get→全量重写；祖先和目标TTL一起续，旁支TTL不续。当前不校验版本、Get跨hash也非原子快照；Patch与并发Set/schema发布仍需矩阵，不能由单一Lua把所有读写宣布线性化。

## Redis 锁、订阅与恢复责任

[driver lock](../../redis/driver/lock.go)在单个对象mu下保存每次Acquire随机token和idle/acquired/uncertain状态。SetNX错误保留token供Release的Lua compare-delete收敛，不能将不确定结果当未取得锁并覆盖token。Extend同样比较token；AutoExtend只是在间隔内发协作可取消Extend，成功后watch生命周期与Acquire caller分开，不提供fence。敏感持久写应走IVersionedLock/权威fence。watch退出和Release等待/opMu预算需要实测非协作依赖，不能凭一个ctx宣称全链有界。

[pubsub](../../redis/driver/pubsub.go)复制SDK消息到有缓冲channel，接收/发送都有done分支，Close负责取消桥和SDK资源。现有接口无业务可靠交付/订阅ready承诺；Subscribe返回不代表可立即发布且绝不丢第一条。本批读到了双select与Once，没有执行全部connected订阅/重连矩阵。

[clusterRecoveryHook](../../redis/driver/cluster_recovery.go)对network/EOF请求合并异步槽表刷新，取消/超时不刷新，原错原样返回，未自动重放写。真实Cluster选主、路由变化和外部弱网未测。[client](../../redis/driver/client.go)的EvalBatchDurable另在同一物理连接执行scripts+WAITAOF，Cluster明确拒绝该保证；本机无持久化Redis测试不等于fsync验证。

## Mongo 测试证据不能超出替身能力

正式[driver collection](../../mongo/driver/collection.go)把Skip传给SDK，生产cursor与Bulk/transaction结果仍需真实Mongo。公开[mongotest](../../mongo/mongotest/mongotest.go)的NC-20已修，两入口共用排序后skip→limit，越界为空且大int64不先窄化；20个正式分页场景通过，不把替身修复说成服务端认证。

mongotest累计源文已读，NC-22～25复制/身份/精度已修，见[机制学习及新方案](IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。其D路径/索引建立/bulk Type/并发restore已确认NC-26～29四项未修；内存快照不是服务端隔离级别/HA证明。N04源文40/40、场景部分完成，下一轮继续Redis真实故障、schema/恢复及正式迁移消费。[缓存准入及迁移](IMPLEMENTATION-CACHE-ADMISSION-AND-MIGRATION.md)沿用既有NC-13～15结论。
