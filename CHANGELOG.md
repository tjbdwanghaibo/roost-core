# Changelog

本文件从 v1.6.2 起维护；更早版本见 git 历史。格式遵循 Keep a Changelog，版本号遵循语义化版本。

## [Unreleased]

### Added
- 可观测性统一：`OBSERVABILITY.md`（命名规范、全仓指标清单、告警基线、Prometheus 导出接线）与 `observability/grafana-roost-overview.json` 总览面板（调度/durability/缓存总线/跨服实体四组）。

### Fixed
- `cache/ref_hmap`：Redis Lua 写失败降级为非原子回退时不再静默——记录 `slog.Warn` 并递增 `cache.refhmap.write_degraded_total` 指标（降级行为本身保留，可用性优先）。
- `entity/ManagerAccess`：冷缓存加载增加 single-flight 合并——并发请求同一实体只发出一次 `LoadEntity`，消除热实体冷启动对数据库的惊群；失败航班共享错误且立即移除（重试触发全新加载），等待者可被自身 context 取消。

### Added
- CI 增加 `release-hygiene` 门禁：module 路径必须能被 `git ls-remote` 解析、版本 tag 必须与 module major 后缀匹配。
- `nest`：每 handler 锁内耗时指标 `nest.handler.lock_hold`（pipelined 提前放锁按提前点计），超阈值（`NestOptionWithSlowLockThreshold`，默认 100ms）计 `nest.handler.lock_hold.slow.total` 并告警——`DurabilityPipelined` 灰度对象的选择依据。
- `cmd/glsvet`：静态检查器，扫描 `go` 语句内对 goroutine 绑定 API（RecordUndo/CurrentRollbackTx/fctx.CurrentContext 等）的调用；已接入本仓库 CI。
- `NEST_PIPELINED_COMMIT.md` §12：pipelined 灰度扩大到默认提交档的四步路线（含量化门槛与回退开关）。
- `nest`：实例作用域 handler 注册 `(*NestMgr).RegisterHandlerWithMeta`/`MustRegisterHandlerWithMeta`（Start 前有效；实例优先、全局注册表兜底）——测试与多引擎进程不再共享包级 handler 表。
- `saga`：`NewEngine` 的配置拒绝逐条给出具体字段、实际值与被违反的预算计算式（原先 8 类错误共用一句 "unsafe engine limits"）。

### Fixed
- `nest/ticker`：tick 回调改为每 tick 读取实时注册表（按注册顺序执行）——原先 `NewTicker` 做构造期快照，引擎启动后注册的回调**静默不执行**，且执行顺序来自 map 迭代（跨进程不定）。
- `syncstream/file_journal`：`Record` 从"每条 open+fsync+close"改为常驻句柄 + leader 合批组提交——"返回即持久"语义不变，fsync 次数从每条降为每批；generation 轮转时释放句柄。

## [1.6.2] - 2026-08

- 修复 pipelined 完成泵：降级路径改为按实体完成链（链序 = LSN 序），三条路径统一等待前驱——降级只牺牲延迟不牺牲同实体完成顺序；submit/stop TOCTOU 加固；stop 超时不再泄漏池。
- v1.5.0–v1.6.x 主线：实体锁由自旋改停车信号量、`DurabilityPipelined` 两阶段提交（锁内仅 append、fsync 锁外、外化闸门）、checkpoint FlushAll 唤醒修复、saga Incarnation 折入 CommandID、bus/worker/sync 竞态修复等，详见 `NEST_PIPELINED_COMMIT.md` 与 git 历史。
