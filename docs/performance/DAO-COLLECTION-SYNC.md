# DAO 递归增量同步微基准

2026-11-02，本地 main `773ca443` 加当前工作树改动；macOS ARM64、Apple M5、Go 1.27.3，默认 GOMAXPROCS=10。每档三轮，以下为中位数。正式测量期间没有并行运行本轮其他编译、测试或索引重建；宿主机电源与桌面活动未固定，不能作为生产容量承诺。

当前实现是 **setter 记录路径 → 按变化编码类型化 PB**，不再比较历史全量快照。协议和边界见 [DAO 同步](../framework/DAO-SYNC-PROTOBUF.md)。源码摘要见 [tracked-source-sha256.txt](benchmarks/dao-collections/tracked-source-sha256.txt)。

## 编码成本与包大小

HeroDao 有 N 项 `map[int64]int32` 和 N 项 `[]int64`。稀疏档各改一项，密集档各改全部元素。两种路径编码同一最终数据、相同 Items/Friends mask：

- `trackedfalse`：完整编码两个脏字段；没有把整个 DAO 全量作为对照夸大收益。
- `trackedtrue`：读取 setter 已记录的路径并编码。稀疏只读对应元素；密集直接整体替换。
- 本表不包括 setter、事务、网络、AOI、客户端应用和数据库；mutation 表补充 setter/事务成本。

| 每集合大小 / 变化 | 完整 → 追踪 PB 字节 | 完整 → 追踪耗时 µs | 完整 → 追踪分配 B/op | 完整 → 追踪 allocs/op |
| --- | --- | --- | --- | --- |
| 100 / 各 1 项 | 815 → 29 | 8.758 → 0.483 | 13,624 → 664 | 134 → 19 |
| 100 / 全部 | 914 → 914 | 8.830 → 8.857 | 13,880 → 13,880 | 134 → 134 |
| 1000 / 各 1 项 | 9,760 → 30 | 106.716 → 0.547 | 175,705 → 664 | 1,049 → 19 |
| 1000 / 全部 | 9,886 → 9,886 | 114.418 → 109.567 | 175,705 → 175,705 | 1,049 → 1,049 |

1000 项稀疏档 PB 减少约 99.7%，编码成本不再随完整集合大小线性增长。密集档编码与原字段替换基本一致，约 4% 的时间差不能当成稳定收益。稀疏补丁按变化数选择，不再为精确比较字节数重复完整编码；不能保证每种业务数据都节省字节。

原始输出：[tracked-codec.txt](benchmarks/dao-collections/tracked-codec.txt)。

## 包含 setter 和事务

1000 项集合，每轮修改一个 map key 与一个 list index，经过 `nest.RunIsolatedTransaction`，再编码、确认变化版本。提交器是内存替身；两组都使用当前 setter，因此包含同样的变化记录和 undo 成本。该对照衡量当前链路选择完整编码还是追踪编码，**不是改造前后整个框架 TPS**。

| 路径 | 耗时 µs/op | 分配 B/op | allocs/op | 最后一次 PB 字节 |
| --- | --- | --- | --- | --- |
| 完整字段编码 | 145.290 | 257,908 | 1,127 | 9,760 |
| 追踪编码 | 34.095 | 75,712 | 94 | 32 |

数值随每轮递增，PB varint 宽度会变化，故最后一个包与固定值编码表不完全相同。事务、undo 和既有持久化准备仍有成本，不能拿 0.547µs 当成 handler 总成本。此测试未经过完整 Nest 队列，也没有 WAL/Mongo I/O。

原始输出：[tracked-mutation.txt](benchmarks/dao-collections/tracked-mutation.txt)。

## 已移除方案的对照证据

2026-11-01 的全快照差分方案在 1000 项稀疏场景中，完整字段编码 100.65µs / 9,761B，快照捕获+解码+比较 216.98µs / 30B。它省带宽但增加 CPU，已被变化追踪替代。旧 [codec.txt](benchmarks/dao-collections/codec.txt)、[before-wire-reuse.txt](benchmarks/dao-collections/before-wire-reuse.txt) 与 `source-sha256.txt` 只保留为当时方案证据，其中的旧源码文件不再存在，不能用当前命令复跑旧实现。两天的输入数值和格式不完全相同，不拿绝对耗时做严格倍率对照。

本轮初版编码会复制变化树，密集档 1000 项约 191.97µs / 372,185B；已改为持元数据锁只读遍历，避免这个额外复制。保留 [tracked-before-read-lock.txt](benchmarks/dao-collections/tracked-before-read-lock.txt)。事务回滚快照仍复制变化树，元数据预算为每 DAO 4096 节点，超限回退全量。编码期间确认短暂等待此锁；业务值仍由 Entity 锁保护。

## 功能验收与复跑

生成 DAO 的运行回归覆盖嵌套字段、map 内 map、list 内结构体、索引更新、nil、密集回退、非法补丁原子拒绝、BSON/undo；同一 Go 金样由 C# 与 C++ 实际应用。额外验证旧确认不吃掉新写、回滚不复活已确认记录、元数据预算和冻结隔离。

正式生成工程真实 TCP → Nest → DAO → Sync 覆盖失败 handler 回滚及重连，存储替身；核心 DataEngine/Entity/Nest 与 DAO/Entity 生成器运行 race。原生客户端启用 Protobuf 和 managed interop。未运行 Linux、Unity Editor/IL2CPP、移动设备容量或生产网络长稳。

```bash
ROOST_CORE_REPLACE="$PWD" ROOST_DAO_BENCH='BenchmarkTrackedSync' \
  sh codegen/scripts/dao-golden-runtime.sh
```

当前协议不兼容旧 DAO 同步格式，需服务端/proto/客户端一起重生成并重连；BSON/WAL 不变。
