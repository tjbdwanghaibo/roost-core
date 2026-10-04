# DAO 迁移加载与 RefHMap schema 清理

2026-10-04，基线 `e7027a65`，NC-30 修后当前源码；[本轮范围与结果](REVIEW-2026-10-04-noncore-19.md)。

## 两种 schema 机制

生成持久 DAO 的 marker `schema=3` 经 DaoDef.Schema/funcMap 到 SchemaVersion 常量。生产 hydration 入口 [RestorePersisted](../../codegen/internal/dao/template_dao.go) 先拒绝比 runtime 新的 schema；旧 schema 调 Migrate，后者按 collection/from/target 进入 [DAORegistry](../../migration/migration.go)。每步拿独立 raw 副本，成功输出再复制给下一步；失败保留 errors.Is，但不返回可持久化的部分数据。成功后 Unmarshal/Init，再恢复 tracker version、CleanDirty。schema version 和数据 version 各自负责不同事情。

本批 CLI 实编生成消费者从 schema1 经两步到3，RestorePersisted 成功后值、version 和 remarshal 的 `_schema=3` 均验证。当前 schema 不迁移；较新 schema、首/次步失败、缺路径、坏 BSON 输出均拒绝，保留已有 DAO/版本和原输入字节。本批只针对生成 hydration 及 scalar fixture，不据此证明复杂 BSON 解码所有错误都不改对象。

Migrate 接口没有 ctx，当前全局入口用 Background；这不承诺外部请求预算自动进入迁移。加载时的数据转换也不等于持久 schema 写回、WAL durable 或升级任务执行。写回必须走原有正式业务提交/确认链；下一批从 Repository 调用和生成消费补证，不新增独立迁移写库工具。

RefHMap 由 Redis 字段与节点路径形成布局，没有上述 schema-version 迁移契约。新增字段默认/旧读者忽略字段可工作，不保证更改类型或滚动写者不覆盖新增字段。全量 Set 会重建所有节点，Patch 只更新指定 scalar 和沿途引用；不要把生成持久 DAO 的 migration 能力套到 RedisDAO。

## 清理清单的所有权

NC-30 修前 registry 的 HGet 与 DEL 分离。新版 [Set/Delete](../../cache/ref_hmap.go) 仍先声明待操作 KEYS，但把读到的原始 `__keys` 一起交给同槽 Lua；执行时逐字对照，一致才删。root 始终是 KEYS[1]，历史 key 顺序不影响 guard。变化返回 `ErrRefHMapRegistryChanged`，明确本次脚本未写；这不是 `ErrConflictingWrite` 的同版本值冲突。

有错误的 Eval 仍可能已执行，返回原因不会变成“肯定没应用”；禁止自动 DEL 补偿。registry 变化时应读回当前 schema/业务意图，再选择显式操作；不能直接把旧完整值覆盖回新布局。脚本不发现未声明键，也无跨进程锁、无界 retry。旧 TTL=0 孤儿须依靠已知历史键清单制定显式清理方案，新 guard 不追溯修复旧数据。

正式生成 RedisDAO 转发错误；CachedDAO 的 Layered 先写 remote，失败在碰 L1 前返回。本批四个实际生成消费者证明直接与 Cached 的 Set/Delete 都保留错误，L1 不被拒绝操作覆盖/删除；读回后显式 Delete 可以清理新 child。

## 已观察到的边界

1. Get 不是跨 hash 快照。支持的无 Pipeline adapter 在 root 读完后遇 Set，会返回旧 Version 和新 child 值；Pipeline 本身也没有事务快照承诺，本批不声称已注入其 socket 命令间隙。
2. Patch 只续沿路 TTL。旁支先过期时，root 仍命中，引用存在会解成非 nil 的零值 child；需要完整对象保鲜的业务不能把这种 hit 当完整缓存命中。测试用真实 PEXPIREAT 精确过期，不靠墙钟 sleep。
3. Stale 读前比较仍建议性。相同 registry 下更旧 Set 可以覆盖中途完成的较新写；registry guard 没有保护 value/version、相同布局 delete/recreate 的 lifetime。正确性依赖版本裁决时复用正式权威/versionstore/CAS，不能直接把本缓存当权威。

这些边界此前已有明确记录，本批给可复现观察，不重复登记成新的 RR，也未借修复清理竞争改变它们。缺失 child 的 miss/error 策略、类型迁移、历史键异常和 Cluster 等需要单独具名场景/契约。性能多 Lua 读取成本没有 benchmark，本轮不给收益数字。

**提交前更正/关联**：上游独立审计已把第2项登记为[RR-20261004-03 P2未修](../bug/RR-20261004-03.md)，约束是Patch续全部登记键∪路径、Get可进一步拒绝缺失的引用节点。本轮实际观察与其现象一致，不能因为此前文档已写“旁支不续”就推定用户接受错误记录；本页保留观察事实，后续按RR-03修复/回归。第1/3项仍是当前未承诺的快照/CAS边界，NC-30修复不改变它们。

**最终同步更新**：RR-03已由上游5c1647c6实施（见[修复](../bugfix/RR-20261004-03.md)），本轮合并后真实Redis正式回归及当前16场景通过，缺失必然非空的引用子hash现在整条miss，Patch续全部当前布局声明键。原观察是修前事实，不再描述当前错误行为；全指针空子hash仍可合法省略。第1/3项没有因此成为快照/CAS承诺。[独立合并证据](../bugfix/evidence/noncore-bugfix-20261004-10/merged/README.md)。
