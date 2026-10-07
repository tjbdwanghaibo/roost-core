# v1.23.1 核心剩余项实施方案

基线 `6f0c1bd8`；范围为剩余交接 B5，之后继续 B1～B4、B6～B8，不重复已验证的修复。

## 目标与改动面

- 原有包结构保留：Nest 调度、DataEngine/WAL 持久化、App 生命周期、生成器各自在原包完成修复。
- F02-2：删除无效 heartbeat worker 配置及未调用的异步检查函数，清理已不影响调度的 HB worker 参数；调用方、schema 快照与现行文档一起迁移，heartbeat 仍在快池调度。
- F02-4 / F03-9：生成器接受 pipelined 并生成对应 HandlerMeta；沿用运行期 committer/allowlist 检查，不另造提交路径。
- F02-7：ActionRunner 最外层提交用 defer 保证恢复执行标记，保留现有延后队列与回调 recover 语义；作为结构性加固，不伪称已发现可达 panic。
- F03-10 / F03-12：先核对 async 投影门槛与 marker 保留窗口的真实契约，再写行为回归及必要校验；健康阈值不能冒充持久性保证。
- F01-10：核对启动窗口中的取消与生命周期所有权，按既有 App 入口处理，不给每个 Mod 加一套信号控制。
- 其余文档项逐条以源码核实；保留历史原始证据，更新使用说明并在分区实现篇追加本版更正。

## 验证与回退

确认缺陷先取得修前行为失败；结构性加固说明保证范围。按影响跑目标 race、根包、glsvet；配置/生成器变更刷新生成物，并跑正式生成工程、全量 build/vet/test。修复分批提交，可按提交回退；协议或存储变化另记版本与升级要求。

## 进度

实施中。当前工作树 `codex/remaining-fixes`，原始证据保存到主检出 `artifacts/perf/remaining-fixes-20261007/`。远端交接最新基线 `494fe096` 已包含于本地 main；CBM 代际 2026-09-30，相关修改文件已通过当前源码补证。

### B5 续批

- F02-4 / F03-9：RR-20261007-04，标记生成 pipelined。
- F03-12：RR-20261007-05，Kit / Assembly 共用 TTL 校验，明确超 TTL 恢复限制。
- F01-10：RR-20261007-06，启动信号从 registry 前接管，统一回调期限，未结束时保留依赖。
- F02-2：删除无效 HbWorkerNum、heartbeat_worker_num、参数和 ensureAsyncDispatchAllowed；调用方、模板与生成配置同步。
- F02-7：submit / Update 最外层 defer 清理标记、命令引用与未执行推进标志；不吞内部 panic，不承诺动作回滚。内部未初始化状态的守卫修前失败 `outer execution flags retained after internal panic`，修后通过；这不是可达业务故障的证据。
- F01-1～7、F02-5、F03-2～8/10/11 文档按当前源码更正。
- 7 个目标包 race 通过；增加启动改动后 App race 再次通过。全仓、生成工程验证进行中。
- B1～B4、B6～B8 尚未完成；Remote 唯一发布者方向等待维护者答复。

### B5 验收

本批全仓 build / vet / test（130 个包）、三大模块 glsvet、正式生成 game-demo 的 build / vet / test 均通过；App 启动取消新增场景 3 轮 race 通过，7 个目标包 race 通过。全仓 go generate 已执行。
