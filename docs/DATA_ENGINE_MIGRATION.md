# Data Engine 数据迁移与发布

本文描述历史 Checkpoint 数据如何一次性进入统一 Data Engine。当前 core/kit 已物理删除旧写
引擎：运行时只有 `persistence.engine=dataengine`，不存在双写、灰度选择或回切旧引擎。
`nestwal/checkpoint.go` 中的 checkpoint 只是 WAL ack watermark，不保存 Entity 文档。

## 1. 统一后的职责

Data Engine 同时拥有以下边界：

- Nest transaction 的 Put/Patch/Delete mutation；
- 本地 WAL、group commit、replay 与 ack；
- Mongo version CAS projection 与 transaction receipt；
- 完整聚合 Load、snapshot read 和 singleflight；
- schema migration 与 migration write-back；
- Saga receipt/native step、Remote commit 与 effect outbox。

DAO 的持久化变化只存在于当前 Nest transaction。`dataengine.Tracker` 保存 persisted version
和 sync mask；Entity release 不再采集另一份 after-image dirty。

## 2. 格式前提

新 DAO 由 roost-codegen 生成并实现：

```go
type MutationParticipant interface {
    PrepareMutation(PersistChange) (dataengine.Mutation, error)
    AcceptMutation(dataengine.Mutation) error
}
```

每份 Mongo 文档至少具有 `_id`、`_version`、`_schema`。普通变更生成字段级 BSON Patch；
新建、schema migration/replace 生成 Put；删除生成带严格递增 version 的 tombstone。Patch 没有
full-document fallback，避免冲突时静默覆盖并发数据。

## 3. 首次迁移流程

1. **升级 reader**：所有待部署二进制先具备 WAL v2 reader、Data Engine DAO decoder 和目标
   schema migration；writer 仍未接流。
2. **停止历史 writer**：停止业务流量并确认旧队列、Redis snapshot WAL 和旧 transaction WAL
   backlog 都为 0。旧 runtime 已不在当前代码库中，这一步只能在升级前的历史版本执行。
3. **做一致性备份**：保存 Mongo snapshot、旧 Redis WAL 状态与数量/版本审计结果。备份是迁移
   失败时的数据恢复点，不是在线双写源。
4. **离线导入**：把历史文档规范化为 Data Engine `RawDocument`/Put mutation，补齐 schema 与
   version。按 database scope、collection、ID 稳定排序并分批提交；每批使用确定 transaction ID，
   使重跑可由 receipt 幂等吸收。
5. **运行 migration**：通过 `MigrationRunner` 调 DAO `Migrate(raw, fromSchema)`，等待 migration
   write-back projection 完成后再发布聚合。
6. **核对**：比较 collection/聚合数量、最大 version、schema 分布、tombstone、抽样业务字段和
   transaction receipt；确认 WAL unacked=0、Projector/Outbox healthy。
7. **只启 Data Engine**：配置省略 `persistence.engine` 或显式设为 `dataengine`，启动新节点并先看
   readiness，再逐步接流。

导入器必须调用 Data Engine Store/Projector 契约，不能直接写业务 collection 后假装完成迁移；
否则 receipt、version、schema、tombstone 或 scope 任一项都可能不一致。

## 4. Load 与 schema migration

`EntityRepository` 在一个 Mongo snapshot read transaction 中读取构成聚合的所有 DAO 文档。
任一必需文档缺失、`_id` 不匹配、schema 不可迁移或版本非法时，整个聚合不发布。

迁移后的 BSON 通过 system transaction 写回同一 WAL/Projector，并等待 projection ticket 完成。
因此 Load/Migrate 不是绕过事务的特殊保存路径。迁移函数必须是确定、幂等、无网络副作用的纯转换；
跨 collection 的业务迁移应使用显式离线任务或 Saga，而不是藏进单 DAO `Migrate`。

## 5. Saga、Outbox 与 Remote Entity

- Native Entity Saga step 把业务 mutation、step receipt 和 completion effect 放进同一
  `CommitRecord`，Mongo projection 按需使用一个 transaction。
- effect 先持久化到 Mongo outbox，NATS/JetStream 故障不会阻塞 Entity projection；发布使用
  Effect ID，消费端仍需持久 inbox receipt。
- Remote commit 与普通 mutation 共享 transaction receipt、version/fence 校验和 WAL ack；
  不再存在独立 legacy applier。

## 6. 回滚策略

发布后的回滚只允许两种方式：

- 新二进制仍能读取已经写出的 WAL/文档 schema 时，停止新 writer 后回滚到该兼容版本；
- 否则从第 3 步的一致性备份恢复到隔离环境，修正迁移器后重新前滚。

不能把 `persistence.engine` 改回旧值，也不能重新引入 Checkpoint package 做临时双写。patch-only
DAO 已没有旧 Snapshot/dirty 协议，强行回切会产生不可证明的两条历史。

## 7. 发布与故障门禁

至少验证：

- 多文档 mutation + Saga receipt/effect staging 原子性；
- late version conflict 完整回滚，重复 transaction ID 幂等；
- Put/Patch/Delete、aggregate Load 与 migration write-back；
- Mongo primary failover；
- NATS 全断后的 outbox 恢复与 exactly-once inbox；
- JetStream leader failover、顺序与 MsgID 去重；
- 100k WAL backlog 在时限内完成 projection，最终 version/receipt/ack 一致。

roost-kit 提供隔离环境脚本：

```bash
scripts/integration/dataengine-env.sh up
scripts/integration/dataengine-env.sh status
scripts/integration/dataengine-env.sh test
scripts/integration/dataengine-env.sh fault mongo-primary
scripts/integration/dataengine-env.sh heal
scripts/integration/dataengine-env.sh down
```

脚本使用独立端口、PID 和数据目录，不操作开发机已有的 Mongo/NATS 服务。
