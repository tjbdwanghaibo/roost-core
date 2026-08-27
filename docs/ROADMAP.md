# Roost 薄弱点与功能路线图

本清单基于 core、kit、skill、codegen 当前源码，不把“功能很多”等同于“已生产验证”。优先级按数据正确性、发布风险和运维成本排序。

## 已在本轮关闭的工程薄弱点

1. Mod 可选集成曾依赖书写顺序。core 现提供 `OptionalDependsOn`，kit 的 NATS→Redis、Nest→Remote Entity 已接入拓扑排序；缺失可选 Mod 不报错，存在时自动排在前面。
2. 生产镜像曾把配置复制进镜像，且生产替换会把 ops 地址改成不可监听的占位符。新模板改为运行时 Secret 挂载，ops 固定 `0.0.0.0:9100`，镜像为 distroless nonroot。
3. 部署只有开发 compose。codegen v1.7.0 增加 Shell/systemd、Docker、Kubernetes/Kustomize、探针、PDB、WAL PVC 和安全上下文，并在 CI 校验 Shell/YAML。
4. 文档曾把 Saga/Nest 停机描述成无上界。实际上 App 已把统一 deadline 传给 `StopWithContext`，本轮文档以当前实现为准。

## P0：下一次大规模生产前应补

### 1. 可重复的容量基准与回归阈值

现状：core/kit 没有 `BenchmarkXxx` 基准套件。单元测试覆盖语义，但不能证明 Nest 吞吐、锁竞争、WAL fsync、checkpoint batch、Remote L1/L2、20 Hz 房间和技能 runtime 在版本升级后没有明显退化。

建议：新增 `bench/` 场景驱动器和固定硬件基线，输出 ops/s、p50/p95/p99、alloc/op、GC、队列高水位、磁盘 fsync 和网络字节；CI 做统计稳定的相对回归门禁，nightly 在 Linux 裸机/固定云实例跑真实 Redis/Mongo/NATS。至少覆盖 1/4/16 核、热点 Entity、多 Entity 事务、100 Entity×20 Hz、1%/5% 丢包。

验收：每个关键路径有预算、可复现实验命令、历史趋势和容量规划公式，而不只是一份某次压测截图。

### 2. 真实基础设施故障矩阵自动化

现状：大量 fake/单元测试能验证算法，但 Mongo primary 切换、Redis WAITAOF 不满足、JetStream redelivery、etcd compaction、磁盘满/torn write、网络分区仍主要依赖人工 staging 演练。

建议：用 Testcontainers 或独立 Linux CI profile 建集成套件；故障注入通过容器 kill、iptables/toxiproxy、磁盘 quota 和副本集 stepdown 实现。普通 PR 跑短集，nightly/release 跑全矩阵并保存恢复证据。

验收：相同事务 ID 在每个 crash point 后只产生一条权威结果；旧 writer 全部被 fence；WAL replay、Saga/outbox、Remote owner transfer 有自动断言。

### 3. 供应链与发布证明

现状：已有 no-replace、go mod verify、测试和生成 smoke gate，但未生成 SBOM、SLSA provenance、镜像签名，也没有 Kubernetes 签名准入模板。

建议：GitHub Actions 构建一次多架构镜像，Syft 生成 SPDX/CycloneDX，Cosign keyless 签名，上传 provenance；Dependabot/Renovate 只提锁定升级 PR；Trivy/Grype 按严重级别阻断；Kyverno/Gatekeeper 验证 digest 与签名。

验收：能从生产 Pod digest 追溯源码 commit、依赖清单、测试 run 和签名主体。

## P1：显著降低运维和开发成本

### 4. Kubernetes SID/ownership 控制器

现状：模板故意 replicas=1。有状态 Service 直接扩副本会复用 SID，这是正确性风险；当前需要平台手工为每个实例生成 workload。

建议：提供轻量 operator/controller，根据 RoostService CRD 分配稳定 SID、独占 PVC、etcd ownership 和迁移状态；支持 drain→Flush→transfer→ready 的受控滚动更新。无状态网关可单独允许 HPA。

验收：节点故障和滚动升级期间不会出现同 SID 双 writer；每次 ownership 变化有 epoch/fence 审计。

### 5. 数据迁移发布编排

现状：core `migration` 已支持进程内 Entity/DAO 逐版本迁移，但缺少全服 schema 状态、后台批量迁移、进度/暂停/回滚判定和 expand-contract 发布门禁。

建议：codegen 生成 migration manifest；kit 提供带 lease 的 migration coordinator、分片扫描、幂等 checkpoint、速率限制和 metrics；部署流水线读取兼容矩阵决定是否允许回滚。

验收：亿级文档迁移可暂停续跑，不阻塞在线 Load，旧新版本混跑窗口有明确读写兼容测试。

### 6. OpenTelemetry 跨服务追踪

现状：日志、健康和 Prometheus 指标较完整，但没有标准 trace context、OTLP exporter 和跨 Nest/NATS/Saga/Remote Entity 的 span 链路。

建议：core 定义无供应商 tracing 接口与 context propagation；kit 提供 OTel bridge。高频同步帧默认采样/聚合，不能每 Entity 每帧生成 span。

验收：一个玩家跨服请求能从 gateway 追到 Nest transaction、Remote/Saga 和存储，且关闭 tracing 时热路径开销可测且接近零。

### 7. 部署模板产品化

现状：已有安全的 Kustomize 基线，但没有 Helm chart、NetworkPolicy、ServiceMonitor、ExternalSecret、备份 Job、多环境 overlay 和云厂商 StorageClass 示例。

建议：这些放 codegen/独立 ops 仓库，不放 core。提供 dev/staging/prod overlays 和 JSON Schema/OPA 配置检查；模板版本独立于业务版本升级。

验收：新项目无需复制粘贴即可进入标准集群，同时每个安全默认都有自动策略测试。

### 8. 协议与客户端 SDK 兼容测试

现状：服务端协议、robot 和 UDP/KCP/QUIC transport 已有，但客户端多语言 SDK、握手/重连/ACK/resync 的正式 conformance suite 不完整。

建议：从协议 manifest 生成 Go/C#/TypeScript 客户端骨架和兼容测试向量；用 packet corpus 测旧客户端连新服、乱序/重复/MTU/密钥轮换。

验收：每个发布版本都有 wire compatibility report，客户端团队不需要重新解释底层控制报文。

## P2：按游戏类型选做

### 9. 大世界跨进程分片与 handover

kit/spatial 已有单进程网格、AOI 和 InterestCluster，但跨进程区域负载、动态切片、ghost entity、无缝 handover 与热点迁移尚未形成框架闭环。建议复用 Remote Entity 的 ownership/route epoch 和 replication baseline，在独立 `world` 包实现 placement/handover 状态机；不要把具体地图规则塞入 core。

### 10. 更丰富的空间与 AI 能力

当前 spatial 是整数网格/四方向 A*，足够 SLG 和简单场景，但 ARPG 可能需要 3D navmesh、动态障碍和 crowd；AI 可能需要 utility/GOAP、共享黑板和调试可视化。建议这些作为 kit 可选包或外部适配，不增加 Entity/Nest 核心复杂度。

### 11. 游戏运营工作台

基于现有 admin 元数据、configdata、featureflag、robot 和 statslog，可增加带审批/RBAC/审计的 GM 操作、灰度配置、回放检索、压测编排和事故时间线。控制面应独立部署，core 只保留命令契约和审计 hook。

## 不建议加入 core 的内容

- 具体登录渠道、支付 SDK、聊天审核、排行榜规则、公会/背包等玩法实现。
- 某个云厂商的数据库、Ingress 或 Secret 管理细节。
- 把所有同步模式统一成一个巨大接口；状态同步、lockstep 和可靠业务消息应保持不同语义。
- 自动隐藏一致性选择。Remote read level、Nest durability、Saga 补偿和同步可靠性必须在业务声明中显式可审查。

## 建议实施顺序

先做容量基准和故障矩阵，让“高性能、生产级”变成可持续验证的数字；随后完成供应链、SID controller 和 migration coordinator；再按目标游戏选择大世界、客户端 SDK、3D spatial 或运营控制面。这样新增功能不会稀释 Entity/Nest/WAL/Remote/Saga 的核心正确性。
