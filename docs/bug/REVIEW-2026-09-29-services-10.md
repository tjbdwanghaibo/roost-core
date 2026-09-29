# Service 第十轮：Mail 批读修后邻接审查

2026-09-29；基线 `68cf87fc79bb3d63c970bb0ce1439dd05466f039` 加[本轮 RR-33 修复](../bugfix/RR-20260929-33.md)。Mail跨槽原反例已经转绿；下面是另一根因，未改 driver。原RR-31/32沿已验收证据保留，无新远端源码/Wanted候选。

## RR-20260929-34：Pipeline 的首个缺失回复掩盖后续写命令错误

**P2；真实 standalone/Cluster 已确认，未实施。**位置：`redis/driver/pipeline.go:101` Exec，`:125–129` 仅根据 aggregate error 判断；HSet 等写入口没有返回 Future。`redis/pipeline.go` 的检查未来结果不能覆盖这种写命令。

触发：同一个 pipeline 按顺序 GET 一个不存在 key，然后 HSET 一个已经是 string 的 key。独立 HSET 控制的 Exec 返回 WRONGTYPE；组合的缺失 future 是 ErrNil，HSET 实际没应用、string 仍是 original，但 Exec 返回 nil。真实 standalone 与三 master Cluster 同 tag/同 owner 都失败，排除跨节点顺序与偶然槽位影响。[结果](../review/evidence/service-review-20260929-10/RESULTS.json)含实际值与错误，[源码/复跑](../review/evidence/service-review-20260929-10/README.md)。两个反例 fail，复用/Discard/旧future隔离控制 pass，0skip/build-fail。

根因：go-redis 的 Exec 汇总为第一条命令错误；该错误是 redis.Nil 时 wrapper 无条件吞掉，却没有检查 Exec 返回的全部 Cmder.Err。bytes/int/map futures会保存对应错误，但 HSet/Set/Del/Expire/RPush/ZAdd 写命令没有 future，调用方可能只有 nil Exec 作为成功依据。用例确认实际 HSet 错误丢失；未声称发生生产资产丢失。

影响边界：公有 IPipeline 中混合可缺失读取和无future写入的调用者会被误导。本轮 Mail全部GET并逐future检查，缺失先于WRONGTYPE正式回归通过。当前源码literal补查四个 `cache/ref_hmap.go` pipeline 入口：loadHashes逐future，Patch/删除/写回批次没有前置可缺失GET；没有把这些现有调用者登记成已复现缺陷。EvalBatchDurable用原生pinned connection Pipeline，不是这个wrapper，还检查脚本/WAITAOF结果。图谱 receiver重名/接口解析不完整，不推定仓外或动态调用全部安全。

实施交接：复用 Exec 返回的命令列表，不新增客户端/抽象层或改变写方法签名。在完成全部future赋值与本次数组清理后，保留真实传输/aggregate error；aggregate为nil/redis.Nil时，按排队顺序检查全部命令，返回首个非nil、非redis.Nil错误。缺失仍只表现为对应ErrNil，errors.Is不可丢失；部分写已经执行时不伪称未应用/事务回滚，也不自动重放整个批次。未来可讨论多错误聚合，但不是修本根因的必要API改造。

验收：沿本反例转正式driver回归；缺失first/middle/last + HSet/Set错误、仅缺失、仅真实错误、成功读写、good/missing/bad futures、int/map future、执行后复用、Discard、取消/网络未知回复、两真实后端。确保写失败不会nil、所有已排future仍可读取且旧结果不被下一次Exec覆盖。原 `TestPipelineExecToleratesNilButPropagatesRealErrors` 只有单个缺失及传输错误，没覆盖首缺失加后命令错误，这也是上轮候选漏查的具体原因。

## 本轮其他结论

Mail保持100输入上限、空批次不访问backend、重复/缺失位置映射、坏字节拒绝、确定性过期/取消和两页cursor；没有新增其他确认缺陷。一次Pipeline仍是O(page size)命令和future，不是降低成O(1)存储工作或跨节点事务；未测端到端吞吐/p99，不提供无证据性能提升百分比。全service生产路径100：97blob不变复用上轮、3Mail变化复读；共享redis/cache相关路径另列，不把复用当本轮逐行阅读。外部資源/HA/强杀/长期容量按既有具名边界保留。
