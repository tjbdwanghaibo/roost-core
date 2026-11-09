# 按实体类型同步调度：Linux 功能验证

日期：2026-11-09；基线 main 3bd3f694；Go 1.27.2。图谱工具不可用，按精确源码及测试验证。未运行压测/benchmark，未自动启用 game-demo。

## 实现

- ManagerConfig.TypeSchedules 以 SubjectKind 覆盖捕获模式。未配置类型兼容旧行为，显式周期类型跳过提交冻结、按固定首次变脏窗口合并；配置复制并校验。
- 即时快照使用 PrepareSnapshotViews：不确认变化树、不清脏，必要时下一次普通捕获回退 Full。首发/恢复快照不让其他订阅提前发送，也不丢掉其待发数据。
- remove 不等待周期；普通 Flush 遵守周期，Drain/Stop 绕过此等待但保留持久门槛与原交付约束。
- 当前实现沿用 pending 脏集合检查，未实现时间轮；新写入若发生在捕获期间，从该批结算后启动下一窗口。脏周期内新订阅导致全量回退的成本需后续测量。

## 验证

| 命令/场景 | 结果 |
| --- | --- |
| `go test -race ./framework/entity ./framework/sync/entitysync/... ./wiring/selfsync ./wiring/nest -count=1` | 五包通过，regression-final.log |
| `go test -race ./framework/nest -run 'TestNest.*Sync|TestNestOnChange' -count=1` | 通过，nest.log |
| `go test -race ./framework/sync/entitysync -run TypeSchedule -count=3 -v` | 修正夹具后六项每项三轮通过，targeted-green.log；随后补充 Hold/Ready 第七项并在上述完整包 race 中通过 |
| `go vet ./framework/entity ./framework/sync/entitysync/... ./wiring/nest ./wiring/selfsync` | 通过，vet.log |

新内容层四项测试覆盖：快照保留累积变化、继续写入时版本递增、Abort 不清脏、不引发无意义全量、干净快照不产生工作、冻结资源归还与后续变化确认。新调度七项测试覆盖：同 namespace 不同类型、999ms/1000ms、连续修改不续期、无变化不编码、新观察者即时全量、退订、Drain、Stop、持久水位、发送重试、配置隔离、真实提交门+运行循环及 Hold/Ready。计时用可控时钟/Go synctest，不依赖真实等待误差。

保留失败记录：targeted.log 中退订断言误按 subject ID 查询 remove（帧只带引用 ID），修正为断言线上 ObjectRemove；targeted-final.log 中真实运行循环测试的普通计数器有数据竞争，修正为 atomic 计数。没有通过放宽业务断言或改变运行时行为掩盖这两处夹具错误。

## 未完成范围

- game-demo Scene 仍用旧周期模式；需正式接入 Nest 生产者、校验与自身同步等已有接线的关系后才能切换 Player/Monster on_change、TerrainChunk 1 秒。不能仅调用 BindSyncProducer 绕过启动检查。
- 尚无目标容量、真实 Mongo/WAL、跨机、Unity 客户端的混合同步验收。没有宣称带宽/CPU 下降比例。
- 原两项地形幂等问题与圆滑岸线、P3～P6 的其余任务仍需后续处理。
