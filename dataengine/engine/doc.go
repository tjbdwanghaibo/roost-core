// Package engine 实现 DataEngine 的 WAL 准入、Mongo 投影和 Outbox 交付。
//
// 阅读入口是 Assembly → Runtime：先恢复 WAL，再发布实体加载器和删除入口。
// Projector 负责准入与票据，projector_replay.go 负责有序重放、分段和 ack；
// MongoStore 的事务编排集中在 mongo_projection.go，存储原语留在 mongo_store.go。
//
// WAL durable 表示重启可恢复，Mongo projected 表示数据已落库，Outbox published
// 表示 Broker 已接受；这三个确认阶段不可混用。Entity 锁释放前不允许投影越过
// held 事务，关闭时须先停止业务生产者，再排空 Projector 和 OutboxWorker。
package engine
