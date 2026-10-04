# RefHMap 布局、Patch 可见性与 Redis 生命周期

2026-10-04，RefHMap/Redis/Mongo源文基线`1502f973`；[运行](REVIEW-2026-10-04-noncore-12.md)、[问题](../bug/REVIEW-2026-10-04-noncore-12.md)、[实际Redis和分页证据](evidence/noncore-review-20261004-12/README.md)。本文件解释现有实现；NC-16～20建议尚未实施。

## 类型树与物理键

[RedisRefHMapStore](../../cache/ref_hmap.go)用sync.Once为V建立layoutRoot和prefix。递归field分为scalar/struct，检测类型cycle和MaxDepth；redisdao/json标签决定名称，未导出或`-`跳过。每节点suffix由字段路径决定，plan把entity key放在同槽标签内；root内`__keys`登记物理键，下一次Set/Delete包含旧schema登记的key。Get对当前plan所有key做HGetAll，再按parent中的引用存在与否恢复子结构；它不把任意被篡改的引用当远端重定向地址。

同槽仅保障脚本/Cluster路由条件，不能证明不同节点的逻辑key唯一，也不能隔离root保留字段。NC-19的Root/id与__keys反例说明目前layout接受了会别名的名字。建议先在plan建成前校验存储名称，保留已有合法持久格式；需要逃逸或换根名称时单独版本化，不能为了修错键静默换所有业务键。

Get用reflect.New(root.typ).Elem构造struct，V=*struct时最后类型断言不匹配（NC-16）。修复应把根形状作为V契约的一部分，明确typed nil和不支持形状，不把panic变成成功miss。文本scalar判断支持*typ的MarshalText/UnmarshalText，但普通value字段不可寻址时编码未调用指针方法（NC-17）；安全地址副本应保持业务值不被MarshalText意外改写，Patch编码也需同样规则。

## 全量 Set 与字段 Patch

Set验证/keyOf，Stale时先Get（建议性比较），encode各节点，添加registry，再执行同槽Lua：删除登记键→写所有新hash→设置每hashTTL。Lua错误当前全部走pipeline/串行DEL+HSET fallback并告警/计数；降级非原子、网络未知和Lua运行时错误不具备回滚保证。尚未故障注入，不把源码风险说成新的实测RR；需要按错误分类/权威结果恢复，不能默认“错误就未写”。

Patch只找scalar路径并HSET目标hash，最多续目标TTL；它不补nil parent引用，也不校验版本。在已存在记录Meta=nil时写Meta.Label返回nil但Get看不到（NC-18）。local/raw生成DAO路径则Get→PatchStructPath（分配nil parent）→Set；ref-hmap模板直接转Patch，因此类型化接口并不自动让两种模式等价。

实施优先复用plan、patchTarget与同槽脚本，维护引用可见性/registry/TTL，明确不存在根记录与并发全量Set的行为。禁止无版本读改写掩盖Patch拒绝；若选择不支持nil祖先，应写前明确拒绝。只更新叶TTL可能造成root先过期的取舍尚未本轮实测，需独立场景。

## Redis 锁、订阅与恢复责任

[driver lock](../../redis/driver/lock.go)在单个对象mu下保存每次Acquire随机token和idle/acquired/uncertain状态。SetNX错误保留token供Release的Lua compare-delete收敛，不能将不确定结果当未取得锁并覆盖token。Extend同样比较token；AutoExtend只是在间隔内发协作可取消Extend，成功后watch生命周期与Acquire caller分开，不提供fence。敏感持久写应走IVersionedLock/权威fence。watch退出和Release等待/opMu预算需要实测非协作依赖，不能凭一个ctx宣称全链有界。

[pubsub](../../redis/driver/pubsub.go)复制SDK消息到有缓冲channel，接收/发送都有done分支，Close负责取消桥和SDK资源。现有接口无业务可靠交付/订阅ready承诺；Subscribe返回不代表可立即发布且绝不丢第一条。本批读到了双select与Once，没有执行全部connected订阅/重连矩阵。

[clusterRecoveryHook](../../redis/driver/cluster_recovery.go)对network/EOF请求合并异步槽表刷新，取消/超时不刷新，原错原样返回，未自动重放写。真实Cluster选主、路由变化和外部弱网未测。[client](../../redis/driver/client.go)的EvalBatchDurable另在同一物理连接执行scripts+WAITAOF，Cluster明确拒绝该保证；本机无持久化Redis测试不等于fsync验证。

## Mongo 测试证据不能超出替身能力

正式[driver collection](../../mongo/driver/collection.go)把Skip传给SDK，生产cursor与Bulk/transaction结果仍需真实Mongo。公开[mongotest](../../mongo/mongotest/mongotest.go)Find在Skip>=结果数时不切片、StreamFind漏Skip（NC-20），使基于它的分页测试偏离正式语义。修复应共用排序后skip→limit，不无限扩展查询模拟，也不把这个替身缺陷说成服务端缺陷。

mongotest snapshot/restore前缀已读，后半filter/update/bulk/index及并发事务忠实性还没全读。它的内存快照不是服务端隔离级别/HA证明；需要差分测试与明确unsupported矩阵。N04源文39/40、场景部分完成，下一轮继续这些留项。[缓存准入及迁移](IMPLEMENTATION-CACHE-ADMISSION-AND-MIGRATION.md)已同步NC-13～15修复，不混淆本页未实施建议。
