# Mirror 三层身份与路由接入

2026-10-05，基线 b2232db5。本轮修复 [NC-33](../bugfix/RR-20261005-NC-33.md) / [NC-34](../bugfix/RR-20261005-NC-34.md)，完整范围见[review](REVIEW-2026-10-05-noncore-23.md)。这是现有适配链学习记录，DTO Mirror 方案未在本轮实施。

## 消息需要在三个层次保持一致

| 层次 | 责任与限制 |
| --- | --- |
| SyncMsg | topic/key/version 用于传输；MessageID 是每次发布的交付身份，不能用业务版本代替 |
| mirror.Envelope | Replicator 校验内外层一致、补齐旧消息省略字段；不知道业务 payload 的形状 |
| 业务 payload | 接收适配器校验实际写入对象；通用 cache 用已配置 KeyOf/VersionOf，Snapshot 用完整 scope key/update version，interest 用 scope key/SID 哈希和 ExpiresAt |

两层信封完全一致仍可能改错对象。判断测试是否覆盖，必须看最终 Store/registry 的副作用，而不是只看 JSON roundtrip 或传输错误。拒绝后还要验证正常消息可继续消费，避免用“全部丢弃”让负例通过。

Interest 的三种数字不可混淆：Envelope.Version=ExpiresAt 是现有 wire 字段，Generation 控制续租/撤销先后，MessageID 区分交付。不能把到期时间大的消息自然当作新 generation，不能把新的 MessageID 当作新的业务状态。协议完整性检查也不替代 topic 发布权限和服务身份。

## 路由与关闭的边界

[ownerroute.Route](../../ownerroute/route.go) 解析 owner 后调用本地 Executor 或远程 Transport，错误直接返回；bus adapter 的 SendByType 是发送接口，处理器记录执行错误。它不提供持久业务完成回执或权威写权限，业务需要用既有幂等命令、确认链与 owner 准入。Prepare 发生在 key 校验前，因此应只准备命令元数据，不承担不可逆业务副作用。

[Kit Mod](../../kit/remoteentity/remote_entity_mod.go) 解析配置/发布能力并转发生命周期；[core Assembly](../../remoteentity/assemble.go) 承担构造、绑定、恢复、finalizer 与停止顺序。这仍是组装职责。Assembly Stop 超时保留复制资源供已接收工作完成，后续 Stop 才清理；通用 Replicator.Stop 本身只取消订阅，不承诺排空已进入的业务回调。本轮复用现有回归，没有证明任意第三方 bus 的 unsubscribe 排空语义。

## 设计与性能评价

复用现有 Replicator、Store 和 RemoteSnapshotCache 是合理方向，缺口是各适配器必须承担自己的协议准入。新增校验在既有解码之后，无网络往返；KeyOf/VersionOf 应保持纯函数。没有实测延迟/分配，不宣称吞吐提升。

Generic cache Delete 仍是无版本 Store.Delete，不能承担全局删除水位。Snapshot 的本机墓碑也不等于跨实例 L2 全局屏障；已有 [Mirror 方案](PLAN-REMOTE-POLICY-MIRROR.md)要求首次加载、重连补洞、墓碑、独立 DTO reader，不因本轮校验修复自动完成。继续沿方案复用框架工具，避免直接把带 backend/锁/finalizer 的写 Assembly当作轻量只读客户端。
