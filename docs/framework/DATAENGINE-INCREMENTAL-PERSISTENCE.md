# DataEngine 递归增量持久化

自 v1.39.0 起，普通 Mongo DAO 生成物采用以下持久化方式；版本状态见 [CHANGELOG](../../CHANGELOG.md)。需使用配套 core 与生成器重新生成 DAO，无新增 tag 或配置。持久格式仍为 BSON，WAL codec 不变，客户端 PB 同步协议不变。

## 正式链路

生成 setter → Nest 事务内 PersistPaths → PrepareMutation 冻结 BSON → WAL → Mongo 投影。setter 只记录字段/key/index 的修改路径，不立即落库，也不扫描旧文档计算差值。提交时沿这些路径读取最终值；相同路径合并，父节点整体替换覆盖子操作。删除后重建写最终状态。

PersistPaths 与 Sync 共用路径节点类型，但各自拥有变化树。持久树只属于当前 Nest 事务，不依赖 Sync 确认；客户端同步确认不会清除待持久变化。事务拒绝时数据、版本与变化记录一起回滚。冻结后的 Mutation 不引用可变业务数据，WAL 回放不再读取在线 Entity。

## 具体写入粒度

| 业务操作 | Mongo 更新 |
| --- | --- |
| struct 的一个字段变化 | `$set: {"pos.x": value}` |
| map 元素内部嵌套字段变化 | `$set: {"equips.2.gems.3.level": value}` |
| map 删除 key | `$unset: {"items.5": ""}` |
| list 某个元素或元素内部字段变化 | `$set: {"friends.50": value}` 或 `"squad.50.star"` |
| 非空 list 追加 | 用追加后的 index 写入；连续追加只记录新增位置 |
| bytes / binary 字段变化 | 整个 BSON binary，不按 list index 更新 |
| 首次持久化、显式整 DAO 写入 | Put，完整文档 |

路径使用 BSON 存储键，不使用 Go 字段名或 PB 字段名。业务继续使用生成 setter；嵌套对象必须由生成的拥有者回调连接，不支持绕过 setter 修改内部容器。

## 完整替换边界

- list 整体赋值、重排、截断使用完整字段；不生成会改变后续 index 含义的删除补丁。
- nil/空容器第一次新增使用完整字段，兼容冷加载后的 BSON null，避免向 null 写子路径。
- map key 不能安全表达为 Mongo 点路径时，整个 map 替换。
- 一个容器直接替换/删除的子节点数达到当前元素数时，整个容器替换；仅修改元素内部叶子不计入该密度，单元素大对象仍能局部更新。这是结构启发式，不是逐次编码后比较字节大小。
- 当前事务变化树最多 4096 个节点；超过预算后，所有脏顶层字段完整替换，不保留无限增长的细粒度记录。

因此不是所有变化都强制增量，也没有为凑最小包额外序列化完整旧值。实际效果见[同机微基准](../performance/DATAENGINE-PERSIST-PATHS.md)。

## 持久安全边界

WAL 准入、fsync 策略、版本 CAS、事务原子性、幂等回放和连续 checkpoint 均保持原契约。Patch 准入拒绝重复路径及父子重叠路径，包括跨 `$set` / `$unset` 的冲突；手写 DAO 不得混入有歧义的路径，避免不可投影记录进入 WAL。

本轮不改 Remote 权威信封：Remote 仍按现有完整 payload 提交。也没有消除普通 Entity 的 WAL 成本或 Mongo 的事务/journal 成本；BSON 更新小不等于 Mongo 磁盘写放大等比例下降。

## 实现与验证

- `framework/dataengine/persist_paths.go`：有界事务路径树。
- `framework/nest/persist_change.go`：事务标记、深拷贝与生命周期。
- `codegen/internal/dao/persist_paths.go`：类型化递归 BSON 更新代码；模板负责 setter 接线。
- `framework/dataengine/validate.go`：准入前路径冲突检查。

生成运行回归在 `codegen/internal/dao/testdata/runtime/persist_paths_test.go`。真实隔离 Mongo 验证覆盖嵌套字段、集合删除、列表索引/追加、父子替换、null 冷加载、文件 WAL 关闭重开、重复投影及冷加载结果；另有事务拒绝、Sync 确认独立性、冻结数据不变、特殊 key 和预算回退回归。

```sh
# 自备隔离 Mongo replica set；每个用例创建并清理自己的数据库。
ROOST_DATAENGINE_IT_MONGO_URI='mongodb://127.0.0.1:29373/?replicaSet=roostPersist' \
ROOST_CORE_REPLACE="$PWD" sh codegen/scripts/dao-golden-runtime.sh
go test -race ./framework/dataengine ./framework/nest ./framework/nestwal ./framework/dataengine/engine ./codegen/internal/dao ./codegen/internal/entity -count=1
```

未设置 Mongo URI 时，真实 Mongo 用例会跳过，不能记为落库验收通过。此次实跑平台为 macOS；没有 Linux、生产长稳或远端 Mongo 容量结论。

2026-11-02 实际验收：全仓 `go build ./...`、`go vet ./...`、`go test ./...`，根包非缓存门禁及上述六包 race 通过。仓内 `go generate ./...` 前后 Go 文件哈希一致；以自定义 module 生成 game-demo、replace 到当前 core 后重生成，build/vet/test 与 `roost generate --check` 通过。消费工程场景中的存储替身不替代前述独立 Mongo 回归。

验证入口区别：core 根仓使用 `go generate ./...`；`roost generate --check` 用于有 roost.yaml 的生成消费工程。
