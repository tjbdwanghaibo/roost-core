# B4 配置与时间边界实施方案

范围来自 REMAINING-FIXES B4，维护者已授权按推荐完成。

- tablegen 的 JSON 名默认遵循 encoding/json 的 Go 字段名，显式 json 标签优先；CSV 名仍为 snake_case。错误 key= 与单例多行在生成期和生成后的转换入口都拒绝。
- configdata 的发布与撤回均同步外部 featureflag 视图，运维 Rollback 的 apply 失败计撤回指标。
- 异步 Nest 是新的调度单元：每次入队捕获当时 RuntimeConfig，保留 Meta/Trace，剥离执行局部状态。已入队消息仍固定一代；同步派生链保持当前代。这使持续自续发链能在下一跳读到更新。
- configschema 仍以声明为唯一规则源：源码结构与数据快照同样执行 closed；MapSource 用真实 YAML 层级点连接，与 Viper 一致。拒绝非有限浮点和有符号范围外整数；共享键错误在附加 Mod 名之前去重。
- 按 D-L3 统一缺省：业务期限使用 clock.Now；安全令牌/保留/存储 TTL 使用 time.Now，不让缺省 SystemNow 跟随业务偏移。进程时钟保留 Duration 纳秒精度，与 Registry 一致。
- 非法存量 timer 加载时告警、标记 NeedsCleanup；正常 Tick 才发出删除变更并计数，不能因实体加载而写持久字段。宿主存储精度必须与期限入队精度一致，通用 timer 不擅自截断所有使用者的亚毫秒期限。

验证：逐项修前行为失败、目标 race、生成 game-demo build/vet/test；跨包改动最终全仓检查。证据不足的疑点不作为修复完成。
