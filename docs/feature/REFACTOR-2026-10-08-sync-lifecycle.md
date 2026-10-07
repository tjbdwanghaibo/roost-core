# B6：Sync 生命周期和玩家接入边界

依据剩余交接 B6、维护者 D2 和“全部完成”的实施授权。

- Group 的成员关系独立于 Manager 会话；重复 Join 幂等地重新提交该成员的全部订阅，容量只约束新增成员。失败保留已有关系，不误撤原有订阅；调用方在重开会话后重试 Join。
- ReplicaSyncer 的删除信封已有 Version，真正缺口在 Store.Delete 丢掉了它。接收 Store 必须提供原子的版本比较、写入和删除水位；通用 Get/Set/Delete 无法保证跨进程或并发写正确，启动时拒绝不具备该能力的 Store。提供有界 ReplicaLocalStore：值和删除水位共用同一条记录，不按 LRU 丢删除水位，满时拒绝新键。持久 Store 由实现方在同一事务里持久化这条记录。删除和写入同版本时删除优先；已删除同版本不能复活。无线上旧数据；此处没有新线格式，原协议已携带版本，不能无理由升级协议。
- JetStream 最后退订/停机窗口不能被当业务成功 ACK，也不能立即 NAK 烧尽重试预算；consumer 创建与停止按 topic 串行，业务投递与全局锁分开。真实 broker 验证停止窗口、旧消费者退出与重订。
- Nest Mod / 正式生成 Scene 注册自己拥有的 entitysync 健康检查。玩家 TCP 配置统一声明，0 dispatch_timeout 继承 Nest 预算；显式 0 shutdown_timeout 拒绝。D2 心跳和每连接令牌桶按 RS v2 协议实现，默认启用限流，panic 日志保留栈。

验收：每个确认缺陷先保留行为失败，再修复；目标 race、真实私有依赖、全仓 build/vet/test 与重新生成 game-demo build/vet/test。产品选择 Remote 唯一 publisher 不在此方案内。
