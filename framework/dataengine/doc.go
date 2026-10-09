// Package dataengine 定义实体持久化的数据契约：Mutation、事务记录、Tracker 和 Store。
//
// DAO setter 只登记本事务的变化。Nest 在 Entity 锁内冻结 mutation，再交给
// engine 实现完成 WAL 准入和异步投影。Sync 的 dirty 与落库变化各有用途，
// 不能用客户端同步清理来替代持久化确认。
//
// 本包不依赖 Nest 或数据库驱动；具体实现集中在同目录的 engine 包，避免导入环。
package dataengine
