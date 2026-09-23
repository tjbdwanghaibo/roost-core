# M-17：ARCH-12 S3 + S4——同步块收进 `sync/`，`statesync` → `sync/frame`，AOI 从 `spatial` 搬进 `policy`，文档收口

- 单元：M-17（结构整理，不占 U 编号，不计缺陷）· 承接 [ARCH-12](ARCH-12-sync-package-layout.md) §4 S3 / S4，前置 [M-15](M-15-statesync-dead-code.md)、[M-16](M-16-transport-contracts-home.md)
- 仓库：roost-core `sync/`（新根）、`spatial`、`kit`、`cache`、`remoteentity`、`robot`、`codegen/internal/roost`、`demo` 模板、living 文档
- 分支：`feature/arch-10-sync-policy`
- 破坏性：import 路径全部变化；`frame` 的 API 去前缀；`spatial.Interest*` 改名并搬家。业务工程用 `roost project upgrade --consolidate` 改写路径（T-175）

## 目录

```
sync/                    只有 README，没有 Go 文件（不构成叫 sync 的包）
  entitysync/            ← entitysync
  entitysync/policy/     ← entitysync/policy + spatial 的 InterestManager / InterestCluster（→ AOI / AOICluster）
  frame/                 ← statesync（包名 frame）
  nettransport/          ← nettransport
  lockstep/              ← lockstep
  syncbus/               ← syncbus
  syncbus/driver/        ← syncbus/driver
  syncbus/mirror/        ← mirror
```

不动的：`entity/subject_sync.go`（内容层归实体）、`spatial`（纯几何）、`syncstream`（基建）、`cache`、`remoteentity`（dataengine 块）、`kit/syncbus`、`kit/remoteentity`（kit 胶水，跟随 core 包名不跟随目录）。

## 改名

| 旧 | 新 | 为什么 |
| --- | --- | --- |
| `statesync.EncodeFrame / DecodeFrame / DeltaFrame / FrameKind / FrameFull / FrameDelta` | `frame.Encode / Decode / Frame / Kind / Full / Delta` | 包叫 frame 之后前缀是重复 |
| `statesync` 错误串前缀 `replication:` | `frame:` | 三个迭代前的名字 |
| `spatial.InterestConfig / InterestManager / NewInterestManager` | `policy.AOIConfig / AOI / NewAOI` | 与 `policy.InterestConfig / Interest` 同居一个包，必须区分；AOI 是它的本名 |
| `spatial.InterestCluster / NewInterestCluster / RoomID / AddRoom / ErrRoom*` | `policy.AOICluster / NewAOICluster / AreaID / AddArea / ErrArea*` | 多区域 AOI 的"room"是坐标平面分片，不是房间 |
| `spatial.InterestEvent / InterestEnter / InterestLeave / DefaultMaxObserverBlocks / ErrInterest*` | `policy.` 同名 | 组织层的事件词汇 |
| `policy.InterestConfig.Spatial` | `policy.InterestConfig.AOI` | 字段类型改了 |
| `spatial` 的 `saturatingAdd / saturatingSub / divideCeil / safeSpan` | 导出 `SaturatingAdd / SaturatingSub / DivideCeil / SafeSpan` | AOI 搬走后仍要用这四个坐标算术 |
| codegen `renderReplication`、别名 `kitnet` | `renderSyncTransport`、`nettransport` | 三个迭代前的名字 |

线格式一个字节没变：`frame.Encode` 的输出、datagram 分片头、entitysync wire v2 都在原地，现有编解码测试未改一字。

## 为什么 AOI 属于 policy 而不是 spatial

维护者的原话：spatial 应该是纯空间相关，不能有其他逻辑。`InterestManager` 回答的是"谁在谁的视野里、什么时候进出、进出时属于哪一档"——这是**同步的组织问题**（谁订谁），滞回半径、band、观察者预算都是同步的旋钮；它只是**用**了格索引与矩形。搬过去之后 `spatial` 的对外面只剩几何：`Point / Rect / BlockIndex / Terrain / FindPath` 与四个算术函数。

## 传输包名为什么仍叫 `nettransport`

见 ARCH-12 §6：`entitysync.Transport` 是机制对下游的契约，"网络传输"与它是两个概念；且 `transport` 作为局部变量在 entitysync、lockstep、demo 模板里几十处，同名包会被遮蔽。

## 业务工程怎么跟

`codegen/internal/roost/migration/consolidation_imports.yaml` 新增第三阶段 `layout:`（boundary core v1.17.0，六条 from → to，`statesync` 带 `package: frame`）。`roost project upgrade --consolidate`：

- 跑在前两阶段的**结果**上（`layoutPath` 在 `singleModulePath` 之后）
- 包名变化的（statesync → frame）给文件补显式别名 `statesync "…/sync/frame"`，文件里的标识符不用改；去了前缀的六个符号留给编译器指出（T-175）
- `needsConsolidation` 现在也会扫 Go 文件里的旧 sync 路径，所以一个已在单模块布局上的工程也会被认出

## 文档

`README.md` 包表（同步块一行 + 旧名对照表）、`ENTITY_SYNC.md`、`kit/README.md`、`demo/README.md`、`docs/feature/GAME_DEMO_TEMPLATE.md`、`docs/review/ENTITYSYNC-MAP` §1.1 重写为新布局、新增 `sync/README.md`、ARCH-12 §6 实施记录、TROUBLESHOOTING T-175、CHANGELOG。历史文档（`docs/bug`、`docs/review/REVIEW-*`、U-/RR- 记录）保留旧路径。

## 验证

- `go build ./...`；`go vet` sync/...、kit/...、codegen、cache、remoteentity、robot、entity、skill/skillsync、spatial 通过
- `go test` 同一组包全绿；`codegen/internal/roost` 新增 `TestLayoutStageMovesTheSyncBlock`、`TestLayoutPathRules`
- 用本地 codegen 渲染 demo 工程（`project sync` + replace 到本地 core）：build / vet / `go test ./...` 全绿
- core 全量 `go test ./...`（见提交信息）
