// Package remoteentity 实现跨节点实体的所有权、写入准入、原子提交及快照分发。
//
// 正式入口是 Assemble：业务提供 Entity loader，Mongo 后端持久化所有权与写许可，
// Redis 协调共享写竞争并缓存快照。Nest 通过 PrepareRemoteWriteBatch 取得实体，
// guard 内构造冻结提交，DataEngine 在同一 Mongo 事务中检查最新许可、更新多个 DAO
// 并写入 outbox；finalizer 发布快照并释放本次写入资源。
//
// Redis 异步复制可能丢失锁或计数器，因此正式写权限由 Mongo majority 授予，
// 不能以 Redis-only 锁替代。MongoCommitter 默认强制校验持久许可，不提供弱校验开关
// 或旧协议迁移入口；不支持的元数据拒绝启动，不自动清理。
package remoteentity
