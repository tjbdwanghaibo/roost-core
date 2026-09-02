# Roost 生产部署手册

本文适用于 Linux。`roost-codegen v1.7.0` 生成 Shell/systemd、Docker 与 Kubernetes/Kustomize 基线。模板是安全起点，不替代组织自己的证书、Secret、网络策略和发布平台。

## 1. 部署前置条件

发布制品必须满足：

- `go.mod` 只引用正式 tag：core/kit v1.8.0、skill v1.7.0、codegen v1.7.0；无 `replace`、无伪版本。
- `GOWORK=off go mod verify && go test ./... && go vet ./...` 通过；关键包 race 通过。
- `roost generate` 后工作树无生成差异，`roost project doctor` 通过。
- 二进制注入 Version、Commit、BuildTime；镜像使用 commit tag 和不可变 digest。
- 生产配置无 `CHANGE_ME`、localhost、开发 token；Secret 不进入 Git、镜像或日志。
- Mongo 是 replica set/sharded cluster；Redis 7.2+ 开启 AOF 且满足 WAITAOF；NATS 启用 JetStream；etcd 使用 TLS 与独立凭据。

## 2. 实例身份与存储原则

一个实例由 `(environment, service, sid)` 唯一标识。任何时刻只能有一个活跃 writer 使用该 SID。带 nestwal 的实例还必须独占 WAL 目录/PVC；不要通过共享 RWX 卷运行多个副本。

无状态 Service 可以水平扩容，但每个副本仍需要唯一 SID。当前模板默认 replicas=1，因为框架尚未提供 Kubernetes SID 自动分配控制器。生产扩容应由平台在创建副本时分配 SID，或为每个 SID 建独立 workload。

## 3. 配置与 Secret

生成项目提供 `configs/service/config.<service>.prod.example.yaml` 和 `deploy/k8s/secret.<service>.example.yaml`。复制为本地文件，替换每个占位符后再部署。

规则：

- ops 在容器中监听 `0.0.0.0:9100`，但由 Service/NetworkPolicy 控制访问；admin 默认关闭。
- 日志写 stdout；需要本地审计文件时挂独立 volume 并设置轮转预算。
- Shell 多实例 WAL 设置为 `/var/lib/roost/<app>-<service>-<sid>/wal`；K8s 使用 `/var/lib/roost/wal` 对应独占 PVC。
- 密钥由 Vault、云 Secret Manager、SOPS/External Secrets 等注入；生成 Secret 示例不加入 kustomization。
- 配置变更与二进制版本一起审查。影响一致性的参数（WAL、transaction、receipt TTL、route/lease）不做无人值守热改。

## 4. Shell + systemd

构建：

```bash
VERSION=v1.0.0 COMMIT=$(git rev-parse HEAD) sh deploy/shell/build.sh
```

安装前将生产配置中的 WAL 路径改为本实例目录，然后：

```bash
sudo sh deploy/shell/install.sh game 1001 v1.0.0 /secure/config.game.yaml
systemctl status planet-game-1001.service
sh deploy/shell/healthcheck.sh http://127.0.0.1:9100/readyz
```

安装器创建非登录用户、只读系统保护、独立状态目录、NOFILE 上限和 45 秒 SIGTERM 预算；对带 WAL 的 Service 校验配置路径与实例目录一致。每个版本进入不可覆盖的 release 目录并生成 SHA256SUMS，`current` 原子切换；重启后 readiness 未在预算内成功会自动切回上一版二进制和配置，首次安装失败则停服。自动回滚仍以数据格式向后兼容为前提。

建议由配置管理系统管理 unit 和配置；不要在大量机器上手工执行脚本。Journal 日志应转发到集中系统并限制磁盘占用。

## 5. Docker

生成 Dockerfile 使用多阶段构建和 distroless nonroot 运行层，不把配置复制进镜像：

```bash
docker build \
  --build-arg VERSION=v1.0.0 \
  --build-arg COMMIT=$(git rev-parse HEAD) \
  --build-arg BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t registry.example.com/planet:v1.0.0 .
```

运行：

```bash
docker run --rm --name planet-game-1001 \
  --read-only --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  --cap-drop ALL --security-opt no-new-privileges \
  -p 127.0.0.1:9100:9100 \
  -v /secure/config.game.yaml:/etc/roost/config.yaml:ro \
  -v planet-game-1001-wal:/var/lib/roost/wal \
  registry.example.com/planet@sha256:<digest> \
  game --sid 1001 --config /etc/roost/config.yaml
```

本地 `deploy/dev/docker-compose.yaml` 只用于开发依赖，不是生产编排。生产镜像流水线还应输出 SBOM、漏洞扫描结果和签名，并在准入控制中验证 digest/签名。

## 6. Kubernetes + Kustomize

生成内容：Namespace、ServiceAccount、默认拒绝业务入口的 NetworkPolicy、每个 Service 的 workload/Service/PDB、Secret 示例和 kustomization。有 WAL 的 Service 使用单副本 StatefulSet + RWO PVC；无 WAL 的 Service 使用 Deployment。容器带 startup/readiness/liveness probe、资源预算、只读根文件系统、drop ALL、seccomp 和 45 秒 termination grace。默认 NetworkPolicy 只允许 roost/monitoring 命名空间访问 ops 9100，开放玩家或服务端口前必须显式补调用方规则。

部署：

```bash
cp deploy/k8s/secret.game.example.yaml deploy/k8s/secret.game.local.yaml
# 编辑本地 Secret；不得提交
kubectl apply -f deploy/k8s/secret.game.local.yaml
kubectl apply -k deploy/k8s
kubectl -n roost rollout status statefulset/planet-game
kubectl -n roost get pod,pvc,svc,pdb
```

正式环境必须补：

- StorageClass、容量、IOPS、扩容和快照策略；验证 fsync 和重新挂载行为。
- NetworkPolicy：业务端口按调用方放行，ops 只对监控/探针开放，数据库只允许服务命名空间。
- Pod anti-affinity/topology spread、节点池、PriorityClass 和合理的 requests/limits。
- 私有镜像凭据、证书 Secret、镜像签名准入、审计。
- Prometheus ServiceMonitor/PodMonitor、日志采集、告警和 dashboard。

单副本 PDB 的 `minAvailable: 1` 会阻止普通节点驱逐，这是为了避免无计划停服，但也会阻塞 drain。维护有状态实例时，先摘流、Flush、确认新 writer 计划，再临时调整/删除该 PDB；不能简单把 StatefulSet replicas 改为 2，因为会复用 SID 和逻辑身份。

## 7. 发布流程

推荐阶段：

1. 构建一次不可变制品，生成 SBOM、签名和 provenance。
2. 在临时环境启动真实依赖，执行 migration/read compatibility、WAL replay 和协议兼容检查。
3. canary 使用新 SID 或明确 ownership transfer；readiness 成功后给少量流量。
4. 观察请求错误、p99、queue、WAL fsync、Data Engine projection/outbox backlog、Remote reject、Saga backlog、GC、CPU/内存和同步重传。
5. 分批切流。每批先停止旧 writer/完成 Flush，再建立新 owner，不能让同 SID 并行。
6. 到达稳定窗口后才清理旧制品和兼容读路径。

数据库 schema 使用 expand→migrate→contract：先让新旧版本都能读，后台迁移，再启用新 writer，最后删除旧字段。禁止把不可逆 schema 变更与二进制滚动更新放在同一步。

## 8. 停机与回滚

Kubernetes 先让 readiness 失败并停止新请求，再发送 SIGTERM。`terminationGracePeriodSeconds` 必须大于 `shutdown.total_timeout`，并给 preStop/网络摘流留余量。最终退出前检查 Nest admission 关闭、Service Shutdown、Saga/consumer drain、Data Engine Flush/WAL shutdown 和连接关闭的错误。

可回滚的前提是旧版本能读取新版本已经写入的 wire、WAL 和数据库数据。否则选择前滚修复。回滚有状态实例前：停止新 writer、记录 route/marker/fence、快照 PVC/数据库、再启动旧制品；绝不能让旧新两个 writer 同时运行。

## 9. 备份与灾难恢复

- Mongo 使用带 oplog/时间点恢复能力的备份；定期在隔离环境演练恢复。
- Redis 不是最终权威数据，但其 durable admission/ownership/receipt 参与一致性；按 RTO/RPO 配置 AOF、复制和故障转移。
- WAL/PVC 做一致性快照前先 Flush/停写；快照恢复后仍通过 transaction receipt 幂等 replay。
- NATS JetStream 备份 stream 配置和持久数据，确认 retention 大于 inbox/receipt 去重窗口。
- etcd 备份服务/选举外的持久配置；恢复后所有租约和 election fence 重新建立。

灾备演练需要记录恢复耗时、重复消息数、被 fence 的旧写、最终状态校验和人工步骤。仅“备份任务成功”不等于可恢复。

## 10. 上线门禁

```text
[ ] 无 replace/伪版本，生成物无差异
[ ] 配置与 Secret 扫描通过
[ ] 镜像 digest/SBOM/签名完成
[ ] 真实依赖集成、race、容量测试通过
[ ] kill -9/磁盘满/主从切换/网络分区演练通过
[ ] WAL 与 SID 单写关系已验证
[ ] readiness、告警、dashboard、值班手册就绪
[ ] schema/wire/WAL/Data Engine projection 回滚路径明确
[ ] 备份恢复最近一次演练在有效期内
```
