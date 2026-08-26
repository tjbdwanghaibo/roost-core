# Changelog

本文件从 v1.6.2 起维护；更早版本见 git 历史。格式遵循 Keep a Changelog，版本号遵循语义化版本。

## [Unreleased]

### Fixed
- `cache/ref_hmap`：Redis Lua 写失败降级为非原子回退时不再静默——记录 `slog.Warn` 并递增 `cache.refhmap.write_degraded_total` 指标（降级行为本身保留，可用性优先）。
- `entity/ManagerAccess`：冷缓存加载增加 single-flight 合并——并发请求同一实体只发出一次 `LoadEntity`，消除热实体冷启动对数据库的惊群；失败航班共享错误且立即移除（重试触发全新加载），等待者可被自身 context 取消。

### Added
- CI 增加 `release-hygiene` 门禁：module 路径必须能被 `git ls-remote` 解析、版本 tag 必须与 module major 后缀匹配。
- `nest`：每 handler 锁内耗时指标 `nest.handler.lock_hold`（pipelined 提前放锁按提前点计），超阈值（`NestOptionWithSlowLockThreshold`，默认 100ms）计 `nest.handler.lock_hold.slow.total` 并告警——`DurabilityPipelined` 灰度对象的选择依据。
- `cmd/glsvet`：静态检查器，扫描 `go` 语句内对 goroutine 绑定 API（RecordUndo/CurrentRollbackTx/fctx.CurrentContext 等）的调用；已接入本仓库 CI。
- `NEST_PIPELINED_COMMIT.md` §12：pipelined 灰度扩大到默认提交档的四步路线（含量化门槛与回退开关）。

## [1.6.2] - 2026-08

- 修复 pipelined 完成泵：降级路径改为按实体完成链（链序 = LSN 序），三条路径统一等待前驱——降级只牺牲延迟不牺牲同实体完成顺序；submit/stop TOCTOU 加固；stop 超时不再泄漏池。
- v1.5.0–v1.6.x 主线：实体锁由自旋改停车信号量、`DurabilityPipelined` 两阶段提交（锁内仅 append、fsync 锁外、外化闸门）、checkpoint FlushAll 唤醒修复、saga Incarnation 折入 CommandID、bus/worker/sync 竞态修复等，详见 `NEST_PIPELINED_COMMIT.md` 与 git 历史。
