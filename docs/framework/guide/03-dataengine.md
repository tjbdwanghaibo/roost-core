# 核心：DataEngine 持久化：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/03-dataengine.md) · [模块总目录](../README.md)


## 1. 唯一正式持久链路

DAO 变更 → Nest CommitRecord → nestwal.WAL → dataengine/engine.Projector → Store/Mongo → outbox。dataengine 定义契约，engine 实现 Assembly、Runtime、Repository 和投影；kit/dataengine 负责依赖、配置与生命周期装配。旧独立 nestwal.Committer/Runtime 不作为现行入口。

一条 CommitRecord 原子准入包含本事务 mutation、effect 与 receipt。Tracker 的同步脏状态与本事务持久变更不是同一份数据。Mutation 表达 Put/Patch/Delete，并携带版本和事务身份；多 DAO 是正式批量契约，不能假定一实体只有一文档。

## 2. WAL 格式与恢复

仅支持 codec 7。旧 codec 明确报 ErrUnsupportedRecordVersion，不能消费后推进 checkpoint，不能自动删除数据“修复”启动。目录由一个持有者独占；尾部截断、校验失败与完整坏记录分开处理。Ack/checkpoint 只推进连续可确认前缀，在前进前满足 WAL 同步要求。

strict 是 WAL fsync，非 Mongo 提交。async 允许先投影，但 Ack 前仍须保证对应 WAL 同步；pipelined 用 LSN 票据确认 fsync。持久结果未知是终止安全写入的原因，不能以一次重试成功覆盖不确定结论。

## 3. 投影与幂等

持实体锁时记录处于 held，投影器不得越过这道边界修改权威。解锁后按依赖推进；同实体版本不能倒序。Mongo 通过条件版本、事务身份、receipt/fence 等判定重复与冲突；“数据库版本更高”不是无条件成功。

投影器可重放同一已提交记录，外部副作用必须通过稳定身份去重。WAL backlog、磁盘/年龄健康阈值、outbox 硬容量分别处理，不能统称任一超限都直接 fail-stop。启用前核对容量与保留期，receipt/事务标记不能在仍有重放可能时提前过期。

## 4. 冷加载与 schema

EntityRepository 读取聚合一致性视图，先验证所有持久 DAO 的完整性与当前 schema，再水合并发布 Entity。旧/未来 schema 都拒绝；生成 RestorePersisted 在解码前校验。自动 schema 迁移已撤销，不能把仍存在的通用 migration 包解释成 Repository 自动转换旧 DAO 的能力。

恢复 gate 未就绪时不能对外发布“空的已恢复实体”。并发加载合并 flight；发布后回调重新连接 Sync 等协作者。缓存只是加速，缺失/过期不得被解释成权威不存在。

## 5. 外部驱动与结果未知

Redis/Mongo 写出后断线不意味着未执行。驱动只在证明命令未执行时重发，结果未知交给调用方。versionstore 用版本、写令牌、墓碑等表达 CAS 和重复；调用方仍须使用业务幂等键并保留未知结论。不要对未知写无限自动重放。

## 6. 维护与升级

要等 Mongo 落库，用明确 Flush/投影等待契约，不能只等 strict handler 成功。静态 Player 跨机迁移只以已落库数据为准，WAL 负责本机恢复与落库，不提供跨机本地 WAL 热迁移。旧版本升级先停写、排空、用旧程序完成需保留数据落库，保留备份；新程序用新 WAL 目录。框架不自动清库或迁移 schema。

只读故障分析使用 cmd/walinspect 与 nestwal 的检查/完整副本入口；不要用正在运行的权威 WAL 目录做破坏性实验。


## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
