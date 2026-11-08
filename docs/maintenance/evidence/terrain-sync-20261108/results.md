# 地形块高度字段 DAO 形状实验（2026-11-08）

服务于[可编辑地形方案稿](../../FEATURE-2026-11-08-editable-terrain.md) D8：被改过的块作为 Nest 实体 `TerrainChunk`，用 entitysync 同步。比较两种高度字段：

- **A**：`Heights []byte`，每点 2 字节小端，共 8192 字节。按 [DAO 同步 PB](../../../framework/DAO-SYNC-PROTOBUF.md#pb-表示)，`[]byte` 是标量，每次整字段替换；可在业务层压缩后再存。
- **B**：`Heights []uint16`，同步走 list delta（`updates[index]`），持久化按下标 `$set`。

两种 DAO 都另带 `BaseRevision`、`Version`（int64）和 `Materials []byte`（4096 字节）。定义在 [def/terrain.go](def/terrain.go)。

## 边界

- 单机微基准：Apple M5（10 核）、macOS 26（Darwin 25.5.0 arm64）、go1.27.3，core 7d260c0a（见 [env.txt](env.txt)）。
- 全部离线。提交器是内存替身（只调用 `ValidateCommitRecord`），**不包含**网络、EntitySync 帧外层、Nest 队列与调度、WAL、Mongo I/O。字节数是 DAO 组件 payload（`MarshalSync` / `MarshalSyncChanges`）和 BSON `$set` 文档的大小，不含帧头、压缩传输和 Mongo 磁盘写放大。
- 合成地形：10cm 精度下 3500～6500（350m～650m），平滑起伏加 ±2 抖动。flate 的压缩率取决于真实地形，这里只能作为参考。
- 笔刷：以块内 (32, 32) 为中心的圆，裁到块内，每点高度 +5（各点都真的改变）。r=32m 在块的一侧被裁，实际 3207 点。
- B 的 DAO 由正式生成器（`codegen/cmd/dao`）生成，没有修改生成器和框架代码。`[]uint16` 可以直接生成：proto 是 `repeated uint32` / `map<uint64, uint32>`，Go 侧类型保持 `uint16`。

复跑：`sh docs/maintenance/evidence/terrain-sync-20261108/run.sh`（mktemp 临时 module，replace 到当前检出；基准约 2 分钟；`TERRAIN_SKIP_BENCH=1` 只跑测量）。下面的表格由测试生成：[single-edit.md](single-edit.md)、[fallback-threshold.md](fallback-threshold.md)、[multi-edit.md](multi-edit.md)、[document-sizes.md](document-sizes.md)。原始基准输出见 [bench.txt](bench.txt)（count=5），中位数见 [bench-benchstat.txt](bench-benchstat.txt)，测试输出见 [test.log](test.log)。

## 1. 单次编辑的同步字节

同步 mask 取 Heights + Version（一次编辑实际变化的字段）。单位为字节。

| 编辑 | 改动点数 | A 同步 | A flate 后同步 | B 同步 | B 形式 | B / A |
| --- | ---: | ---: | ---: | ---: | --- | ---: |
| r=1m | 5 | 8207 | 6637 | 56 | delta | 0.7% |
| r=2m | 13 | 8207 | 6637 | 120 | delta | 1.5% |
| r=5m | 81 | 8207 | 6639 | 665 | delta | 8.1% |
| r=10m | 317 | 8207 | 6639 | 2553 | delta | 31% |
| r=20m | 1257 | 8207 | 6635 | 10073 | delta | 123% |
| r=32m | 3207 | 8207 | 6637 | 25658 | delta | 313% |
| 全块 4096 点逐点 +5 | 4096 | 8207 | 6627 | 8207 | 整字段回退 | 100% |
| 整平成平面（`SetHeightsAll`） | 4096 | 8207 | 40 | 8207 | 整字段 | 100% |

- B 的每个改动点约 8 字节：每条 map entry 由外层 tag、长度、下标 varint 和值 varint 组成。整字段全量按 packed varint 编码，每点 2 字节。因此 **B 只在改动点少于约 1020 时（r≈18m）小于 A 的整字段**；超过后补丁可达整字段的 4 倍（见第 2 节），直到 4093～4094 点才回退整字段。
- 这一结果符合文档写明的取舍：“不保证每份补丁都比完整字段更小”，框架不做字节比较。
- flate（DefaultCompression）对小端原始字节只压掉约 19%（8192 → 约 6620）。先做行差分 zigzag varint 再 flate 约 4KB（第 3 节），是业务层另外的编码选择，这里没有接入 DAO 测量。

## 1b. B 的回退阈值（实测与文档核对）

改前 k 个不同下标（单次事务）。完整表格见 [fallback-threshold.md](fallback-threshold.md)。

| 条件 | 同步首次整字段 | 持久化首次整字段 |
| --- | ---: | ---: |
| 先改高度，后写 Version | k = 4094 | k = 4094 |
| 先写 Version，后改高度 | k = 4093 | k = 4094 |

- **决定阈值的是 4096 节点预算，不是“达到元素数”。** 生成代码的规则是 `len(change.Children) >= len(heights)`，对 4096 点的块需要 4096 个子节点才成立。但变化树在登记前检查 `nodes + len(path) > 4096`（`framework/dataengine/sync_changes.go` 的 `mark`，持久化侧是 `persist_paths.go` 的 `Record`）。节点数包括根、Heights 字段节点和各下标节点，所以第 4094 个下标就整树回退。先写 Version 时 Version 多占一个同步节点，阈值提前到 4093。持久化侧的标量走 `FullMask`，不进路径树，所以两种顺序都是 4094。
- 两条规则文档都写了（[密集变化回退](../../../framework/DAO-SYNC-PROTOBUF.md#pb-表示)、[完整替换边界](../../../framework/DATAENGINE-INCREMENTAL-PERSISTENCE.md#完整替换边界)），与实现一致。只是对“元素数 ≈ 预算”的列表，“达到元素数”这条在实践中永远到不了。
- 预算回退会把根标为整体替换，但 `MarshalSyncChanges(mask)` 只整编 mask 里的字段，所以 Materials 不会被带上。
- 同一事务里 4000 个下标各写 3 次仍是 delta：重复写同一下标不新增节点。
- 4093 点的 delta 是 32.6KB，比 4094 点回退后的 8.2KB 大 4 倍：阈值附近最贵。

## 2. 同步前多次编辑未结算

每次编辑是一个独立事务。同步变化树跨事务累积，直到确认（ack）为止。

| 场景 | 编辑次数 | 累计不同下标 | B 同步 | B 形式 | A 同步 |
| --- | ---: | ---: | ---: | --- | ---: |
| 5 × r=5 不重叠 | 5 | 405 | 3257 | delta | 8207 |
| 5 × r=5 重叠（中心相距 1m） | 5 | 125 | 1017 | delta | 8207 |
| 5 × r=5 同一中心 | 5 | 81 | 665 | delta | 8207 |
| 4 × r=20 四角 | 4 | 3996 | 31882 | delta | 8207 |
| 5 × r=20 四角 + 中心 | 5 | 4021 | 32082 | delta | 8207 |

- B 的补丁大小只取决于累计的不同下标数，重叠的编辑合并。累计不到 4093 个下标时不会提前回退，所以大面积多次编辑会停在约 32KB 的 delta 上（A 的 4 倍）。这仍在 `Limits.MaxComponentBytes` 缺省值 64KB 之内。
- 顺序应用：50 次随机笔刷（r=1～12，每次确认后取下一份补丁），客户端视图与服务端的 4096 点一致。

## 3. 持久化大小

整文档 BSON（`Marshal()`）与全量同步（`MarshalSync(全部字段)`）：

| 数据 | A 整文档 | A flate 整文档 | B 整文档 | A 全量同步 | A flate 全量同步 | B 全量同步 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 高度 < 16384（varint 2 字节） | 12361 | 10790 | **44019** | 12313 | 10742 | 12313 |
| 高度 ≥ 16384（varint 3 字节） | 12361 | 10801 | **44019** | 12313 | 10753 | 16409 |

单独压缩 Heights（8192 字节）：flate(LE) 约 6.6KB，行差分 varint + flate 约 4.1KB。

单次编辑冻结的持久化更新（`Mutation.Patch.SetBSON`，DataEngine 递归增量持久化）：

| 编辑 | A `$set` | B `$set` | B heights 形式 |
| --- | ---: | ---: | --- |
| r=1m | 8216 | 76 | 按下标 5 个（`"3.<i>"`） |
| r=5m | 8216 | 988 | 按下标 81 个 |
| r=10m | 8216 | 3820 | 按下标 317 个 |
| r=20m | 8216 | 15051 | 按下标 1257 个 |
| r=32m | 8216 | 37908 | 按下标 3207 个 |
| ≥4094 点或 `SetHeightsAll` | 8216 | 39874 | 整字段（BSON int32 数组） |

- B 的 list 确实按下标 `$set`，每点约 12 字节（键 `"3.1234"` 加 int32）。A 的 `[]byte` 每次整个 binary 写入（文档表格中“bytes / binary 字段变化”一行）。
- B 的代价在存储形态：Mongo 里是 4096 个元素的 BSON 数组，每个元素要存数组下标键和 4 字节 int32，**整文档约 44KB，是 A 的 3.6 倍**；整字段回退每次写约 40KB。小编辑（约 680 点以内）的 `$set` 比 A 小。WiredTiger 的块压缩和文档整体重写这里没有测。

## 4. CPU（每次编辑）

每次迭代 = Nest 隔离事务（setter + undo 登记 + 持久化 BSON 冻结与校验）+ `MarshalSyncChanges` + 确认。count=5，取中位数（[bench-benchstat.txt](bench-benchstat.txt)）。`Bsetall` 是 B 的大笔刷写法：取副本、改完后一次 `SetHeightsAll`。基准里的 flatten 是“全块 4096 点逐点改”，不是整平成平面。

| 编辑 | A 耗时 | A 分配 | B 逐点耗时 | B 逐点分配 / 次数 | Bsetall 耗时 | Bsetall 分配 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| r=1m（5 点） | 9.7µs | 86KiB | 5.9µs | 9.8KiB / 170 | 111µs | 285KiB |
| r=5m（81 点） | 9.7µs | 86KiB | 80.6µs | 143KiB / 1557 | 111µs | 285KiB |
| r=20m（1257 点） | 10.6µs | 86KiB | 1.27ms | 2.2MiB / 21.7k | 111µs | 285KiB |
| r=32m（3207 点） | 11.4µs | 86KiB | 3.36ms | 5.1MiB / 54.9k | 112µs | 285KiB |
| 全块 4096 点 | 12.0µs | 86KiB | 1.72ms（预算回退） | 3.4MiB / 24.6k | 113µs | 285KiB |

A 选业务层压缩时，每次编辑另加解压和压缩各一次：

| 操作（8192 字节） | 耗时 | 分配 | 输出 |
| --- | ---: | ---: | ---: |
| flate 压缩 DefaultCompression | 105.6µs | 1.04MiB | 6621B |
| flate 压缩 BestSpeed | 61.0µs | 804KiB | 8199B（几乎不压缩） |
| flate 解压 | 50.5µs | 57KiB | — |

- B 逐点写法每个改动点约 1µs、约 1.8KB、约 17 次分配。粗略 pprof（B/r20，alloc_space）显示成本分散在几处：undo token（`RollbackTx.RecordUndoToken`）、两棵变化树（`syncChanges.mark`、`PersistPaths.Record`、`SyncField`），以及持久化侧每个下标一条 `$set`：BSON 编码、`validatePatchPaths` 对每个键做 `strings.Split`、`Raw.Elements`。这是逐元素记账的固有成本，不是缺陷。
- `Bsetall` 约 111µs，不随改动点数变化，主要是编码 40KB 的 BSON 数组。CPU 上逐点写法在约 110 点（r≈6m）以内更省，但按同步字节算逐点写法到约 1000 点都更省（第 1 节）。
- A 约 10µs 基本不变。A 加 flate（解压 + 压缩）约 155µs，比 B 的 r=5 逐点写法还贵。

## 5. 客户端应用（Go）

- 正确性：每个单次编辑档位、阈值扫描的 24 个 k、多次编辑的 5 个场景，都断言 `ApplySyncTerrainListDao(patch, 上一份全量)` 和镜像 DAO 的 `ApplySync` 得到的 4096 点及 Version 与服务端一致。50 次随机编辑的顺序应用也一致（`TestTerrain*` 全部通过，见 [test.log](test.log)）。
- 耗时（解码 + Apply，中位数）：r=1 为 1.1µs，r=5 为 4.3µs，r=20 为 55µs，r=32 为 122µs，整字段（4096 点）为 9.8µs。C# / C++ Apply 没有测。

## 结论（供 D8）

- 玩家常用的小笔刷（r=1～5m）：B 每次下发 56～665 字节，A 是 8.2KB（flate 后 6.6KB），**B 小 12～150 倍**。这部分字节会乘以观察者数量和编辑频率，是两种形状的主要差别。
- 大笔刷与整平：B 逐点写法超过约 1000 点时补丁比整字段大（最多 4 倍），CPU 是毫秒级。业务用 `SetHeightsAll` 守卫后，同步与 A 相同（8.2KB），CPU 约 111µs，持久化每次约 40KB。
- 持久化：B 整文档约 44KB，A 约 12KB。B 小编辑的 `$set` 更小，大编辑更大。
- 推荐 B 加大笔刷守卫（单次改动超过约 1000 点改用 `SetHeightsAll`），理由与代价见方案稿 D8。

## 生成器 / 框架观察（未修改，均不是确认缺陷）

1. **list delta 没有按字节回退**：`[]uint16` 每个更新约 8 字节，全量每点 2 字节。改动点在约 1020～4093 之间时 delta 比整字段大（最多 4 倍）。文档已写明这是取舍。若要改，生成器的 `captureSync<Dao><Field>Field`（模板在 `codegen/internal/dao/sync_tracked.go`）可以对定长标量列表按“更新数 × 估算条目字节 ≥ 全量字节”提前回退，或使用紧凑编码。需要维护者决定。
2. **对 4096 元素的列表，“达到元素数”这条回退永远到不了**：节点预算先触发（4093 / 4094）。行为符合文档，只是两个阈值重合时前者没有意义。
3. **`[]uint8` 等同于 `[]byte`**（`codegen/internal/dao/sync_proto.go` 的 `expression`，以及 `parse.go` 的 `IsBytes`），总是整字段替换。材质要按下标增量，只能用更宽的元素类型。
4. **逐元素记账成本**：约 1µs / 1.8KB / 17 次分配每个改动元素，分布见第 4 节。其中准入校验 `ValidateMutation` → `validatePatchPaths`（每个键 `strings.Split`，并用 `Raw.Elements` 重新解析刚编码的 `$set`）占 B/r20 分配的约 37%（pprof alloc_space，单次运行）。这是可选的优化方向，不影响正确性。
5. **`[]uint16` 存成 BSON int32 数组**：整文档是 `[]byte` 形状的 3.6 倍。这是驱动对无符号小整数切片的默认编码，没有 binary 打包选项。

## 未验证

- 真实 Mongo 下 `"3.<i>"` 数组下标 `$set` 的落库、冷加载与 WiredTiger 磁盘大小（DataEngine 已有隔离 Mongo 的数组下标回归，本实验没有重跑）。
- 真实 TCP、EntitySync 帧外层、`SnapshotBudget`、`MaxObjects` 与 Interest 下的端到端流量和会话行为。
- C# / C++ 客户端对 `repeated uint32` / `map<uint64, uint32>` 的 Apply 正确性与耗时。
- Linux 与其他 CPU 的数字；真实地形的压缩率。
