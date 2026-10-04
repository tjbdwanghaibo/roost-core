# N04 第一批：缓存拒绝、回填与结果契约

2026-10-04，N04源文等于main`3560a19b5c2b2fbeb936bf1bb6a9c1fa0be1f956`，本工作树仅先修NC-11/12，**以下三个新RR已确认、未修复**。Tier2图谱发现/双向trace/snippet及coverage后以当前源码补证；旧代际、启发式重名边不支持全域穷尽。

26个普通overlay叶子/独立项：9行为失败、16控制通过、1容量观察；3根因。五个有测试包race/vet通过，113 test pass事件、0fail、7skip；随后专属真实Redis8.8.0将原7skip逐项补测通过，另6个真实缓存场景2fail/4pass补证NC-15。Mongo接口包/KitRedis/KitMongo无测试，仅编译/vet。真实Mongo/etcd、Cluster/HA与长稳未执行。 [运行](../review/REVIEW-2026-10-04-noncore-10.md) · [复跑/原始日志](../review/evidence/noncore-review-20261004-10/README.md) · [机制与实施方向](../review/IMPLEMENTATION-CACHE-ADMISSION-AND-MIGRATION.md)。

| RR | 等级 | 影响 | 反例 |
| --- | --- | --- | --- |
| NC-13 | P2 | FatalRemoteError被Get/Delete当作可降级故障，错误读取发布或失败删除报告成功 | Get返回nil且loader/L1发布；Delete返回nil且清掉L1，两失败；Set正确拒绝为对照 |
| NC-14 | P2 | Layered回填已被L1一致性规则拒绝，仍交付被拒值 | stale、同版本conflict、较新删除后tombstone三个场景返回captured，ReadThrough同样三场景正确 |
| NC-15 | P3 | 四种Store的stale拒写返回nil，与共同API契约不一致 | Local/Grouped/RawJSON/JSONHash四失败；Atomic/JSON拒写报告ErrStaleWrite，六种较新写正常 |

## RR-20261004-NC-13

**P2，已确认、未修。** [read_through.go](../../cache/read_through.go):28–34声明FatalRemoteError不允许IgnoreRemoteError吞掉一致性裁决；:150仅检查IgnoreRemoteError，:271–276删除也只检查该flag。Set及loader回写已调用degradable，策略不一致。

复现配置IgnoreRemoteError=true、FatalRemoteError=errors.Is(ErrConflictingWrite)。正式Get遇L2 Get错误仍调用loader、向L2/L1写fallback，返回nil/命中；正式Delete遇L2拒绝仍删L1，返回nil。原日志：`operation=get err=<nil> loads=1 ... held=true`、`operation=delete err=<nil> ... held=false`。不是网络真实故障测试；依赖仅提供错误，正式ReadThrough实现执行决策。

影响是对明确不可降级裁决报告成功并产生后续副作用，不能靠指标RemoteError代替错误返回。旧RR-20260913-05处理Set的一致性错误；本批是Get/Delete遗漏，同一机制关联，但不否定已验证Set修复。

建议复用degradable统一所有L2入口，fatal在loader/本地变更前拒绝，保留errors.Is。普通可降级Delete的L1清除及strict错误行为另列兼容，不把所有错误改成同一策略。验收fatal/普通故障×Get/Set/Delete/backfill、wrapped cause与本地副作用。当前不改产品。

## RR-20261004-NC-14

**P2，已确认、未修。** [layered.go](../../cache/layered.go):51–66忽略回填一致性拒绝，最终仍返回remote捕获的值。ErrConflictingWrite只是计指标后吞掉，ErrStaleWrite被当成功设置expiry；与[Store契约](../../cache/store.go):26–28及[ReadThrough拒绝回填处理](../../cache/read_through.go):157–180不一致。

反例用真实AtomicLocal L1、门闩控制L2读结果返回顺序：Get先捕获v1，随后正式Layered.Set发布v2，再放行旧Get；L1拒绝v1，调用方却拿v1。等版本不同payload和外部已登记较新删除Superseded门禁也是一样：本地保持published/miss，调用方仍收到captured。ReadThrough同样三组分别返回current/current/miss。

这里不是把TTL期间允许的旧缓存一般读法当bug，也不宣称Get与并发Set都必须线性化在Set之后。问题是包装层已经拿到**一致性准入拒绝**，却按普通可用性回填失败继续发布被拒值，conflict/tombstone不应被降级；建议在强制规则适用时返回已准入当前值、miss或明确错误，沿用ReadThrough处理，不增无界version表，不替业务凭空生成墓碑。别把L1容量/暂时不可用等可降级失败一律改成读取失败。

验收旧读与publish/delete顺序、同版本冲突、拒绝后当前值读取失败、TTL合法缓存、零TTL重查、第三方L1非一致性错误。当前不改产品。

## RR-20261004-NC-15

**P3，已确认、未修。** [local.go](../../cache/local.go):73–75、[grouped_local.go](../../cache/grouped_local.go):58–60、[redis_raw.go](../../cache/redis_raw.go):64–66、[redis_hash.go](../../cache/redis_hash.go):83–85在Stale(old,next)时直接return nil；[store.go](../../cache/store.go):42–45声明拒写返回ErrStaleWrite。AtomicLocal、RedisJSON、RefHMap已有报告路径，替换Store类型会改变调用方结果语义。

四种正式实现先写v5，再写v4：读取确实仍为v5，但Set(v4)返回nil。Redis替身只提供string/hash存取，不替换Stale判定；随后core cache→正式driver→专属真实Redis的6叶子再次确认Raw/Hash旧写2失败、JSON旧写和三种较新写4通过，没有验证并发原子性。Atomic和RedisJSON普通overlay同样旧写正确报告，六种v6写均成功，确认不是全部Set被错误拒绝。

影响是合法通用调用方不能区分“写成功”和“已丢弃”，本批没有证明当前线上业务因此丢持久数据。已有TestLocalStoreRejectsStaleVersion还要求nil，说明历史期待与后更新共同声明发生漂移，而不是新写测试证明并发CAS安全。

建议统一ErrStaleWrite，核对旧tests与实际消费者：有意容忍的地方显式errors.Is，保留first/equal/newer写语义；这属于行为收紧，写兼容说明，不能靠删Stale或无条件重试解决。分离Redis建议性Get→Set与真正CAS保证，nil/ErrStaleWrite契约修复不会使两个RPC变成原子事务。当前不改产品。

## 观察与未验证

Layered TTL元数据没有独立容量/定期回收：两个LocalStore各MaxEntries=1，写1000不同key，TTL=1ns后读取最后key，数据各1条但expiry仍1000。这是实际观察，不套不存在的公开metadata上限，不新增RR；长生命周期高基数场景需定容/回收方案，优先复用ExpiringStore或有界清理，未测OOM/长稳。

AtomicLocal分片上限向上取整，总量可能高于配置少于一个分片数的余量，大value受每片byte预算限制；这是配置取舍需明确，未作为新bug。Migration对象原地修改且每成功step更新version，失败callback已做的修改不自动回滚；DAO迁移复制输入/每步/输出，失败不返回partial bytes，这些控制通过，但不是业务持久原子提交证明。

Mongo StreamFind复制raw BSON、同步consume、defer关闭cursor；取消后killCursors/错误归因仍需真实资源证据。WithTransaction可能重跑业务回调，外部副作用幂等不由本包装保证；BulkWrite失败/网络未知不能假设无写入或回滚。RefHMap只读存取/写降级的具名范围，递归反射/跨节点事务及Lua失败后的结果未知留后续。Redis Pipeline不是事务，Exec错误不回滚；CAS同脚本不等于Lua运行时错误自动回滚。没有为这些观察编造新失败。
