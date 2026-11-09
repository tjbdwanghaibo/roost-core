# game-demo 混合同步生产接线（2026-11-09）

基于 main 5ae1b3d7；Linux、Go 1.27.2。未执行新的压测/benchmark。图谱工具不可用，使用精确源码与正式生成工程验证。

## 完成范围

- `wiring/nest.NestSyncObserverProvider`：从 app.ModService 在 Provide 期间取得唯一提交观察者；返回 nil 或与 EntitySyncSetup/self_sync、显式观察者竞争时拒绝启动。
- Service 提前提供 sceneSyncObserver；实际 Nest 构造调用绑定之后，Scene 才能接上 Manager。CaptureSync、WakeSync、RetractSyncSubject 全部转发，目标只接一次。
- 生产 NewScene 使用 ModeOnChange，TerrainChunk 按 EntityKind 显式配置 periodic 1 秒；Player/Monster 等其余类型继承 on_change。应用仍复用原会话、AOI、地形独立预算与持久水位。
- 无观察者的旧构造器仅保留供历史功能/测量夹具使用；正式 NewScene 必须通过服务观察者。历史 50ms 周期测量不能冒充新模式性能结果。

## 本机验证

| 检查 | 结果 |
| --- | --- |
| `go test -race ./wiring/nest -count=1` | 通过，wiring-green.log；含正常服务绑定、nil、显式第二观察者、Mod 自持 Manager 冲突 |
| `go test ./codegen/internal/roost -run TestDemo -count=1` | 通过，codegen-final.log；新增模板随正式生成步骤写出 |
| 生成工程 `go test -mod=mod -race ./internal/service/game -run 'Terrain|Scene' -count=1 -v` | 通过，regression.log |
| 生成工程 `go build -mod=mod -buildvcs=false ./...` | 通过；临时生成目录关闭 VCS stamping |
| 生成工程 `go vet -mod=mod ./...` | 通过 |

生成命令：`go run ./codegen/cmd/roost project new planet -module example.com/planet -out <临时目录> -template game-demo --skip-deps`。生成模块 replace 指向本次 core 工作树。

新增 TCP 用例使用正式生成 Nest handler/Sender、玩家 TCP 服务、帧 Receiver 与 DAO Apply：连续三次地形编辑合为一次更新，同期间真实 AddExp 事务及时到达；新玩家立即收到当前地形全量，旧观察者不被提前更新，后续两方逐点一致。持久层为既有替身提交器，不是 Mongo/WAL 集群验收。Monster 沿用相同默认类型规则，本轮没有独立的 Monster 事务 TCP 用例。

观察者用例以真实提交门和一小时兜底周期验证锁内捕获、Release 后唤醒及撤回 remove；采用 synctest，避免用 50ms 兜底掩盖 WakeSync 漏接。未实际启动引擎的绑定单测只证明构造时绑定，不代替 TCP 用例中的正式运行引擎。

预期跳过四项：三项 TerrainMeasure 测量门和 TestTerrainRestartRealMongo 外部依赖门。TCP 用例结束关闭连接时可能记录 push_refused（测试清理过程），不把它报告为运行期降级失败。

原始失败保留：wiring.log 的新增测试误用了 Stop(context) 签名，修正为现有 Stop()；mixed.log 的新增模板缺少 syncsender 显式包别名，修正模板与生成夹具后通过。最终回归包含修正后的完整相关场景。

## 最新 main 集成复验

提交前将本批改动 rebase 到 main `fd9538f0`，包含 `f5736ea2` 的心跳不追赶突发修复。以下检查重新执行并通过：

- 生成工程 `go test -mod=mod -race ./internal/service/game -run 'Terrain|Scene' -count=1`：20.272s，见 rebased-regression.log / .exit。
- core `go test -race ./gameplay/heartbeat ./wiring/nest -count=1`：两包通过，见 rebased-root.log / .exit。

复验仍沿用上述外部依赖与测量门，不扩大生产容量验收范围。

## 后续

圆滑岸线、原两项地形去重修复和 P3～P6 仍未完成。新混合模式的真实 Mongo/WAL、Unity、跨机及容量验收尚未执行，未给出带宽/CPU 改善百分比。当前生成模板须与包含 TypeSchedules/服务观察者入口的同一 main/tag 配套。
