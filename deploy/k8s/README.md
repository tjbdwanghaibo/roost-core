# Kubernetes 部署

模板默认每个 Service 一个副本和唯一 SID，避免多个 writer 共享身份或 WAL。先创建配置 Secret，再选择 staging/production overlay。生产流水线使用不可变 digest 和 deploy.sh：

- `account`：复制 `secret.account.example.yaml` 为 `secret.account.local.yaml`，替换全部 `CHANGE_ME`。
- `activity`：复制 `secret.activity.example.yaml` 为 `secret.activity.local.yaml`，替换全部 `CHANGE_ME`。
- `chat`：复制 `secret.chat.example.yaml` 为 `secret.chat.local.yaml`，替换全部 `CHANGE_ME`。
- `game`：复制 `secret.game.example.yaml` 为 `secret.game.local.yaml`，替换全部 `CHANGE_ME`。
- `global`：复制 `secret.global.example.yaml` 为 `secret.global.local.yaml`，替换全部 `CHANGE_ME`。
- `mail`：复制 `secret.mail.example.yaml` 为 `secret.mail.local.yaml`，替换全部 `CHANGE_ME`。
- `match`：复制 `secret.match.example.yaml` 为 `secret.match.local.yaml`，替换全部 `CHANGE_ME`。
- `platform`：复制 `secret.platform.example.yaml` 为 `secret.platform.local.yaml`，替换全部 `CHANGE_ME`。
- `rank`：复制 `secret.rank.example.yaml` 为 `secret.rank.local.yaml`，替换全部 `CHANGE_ME`。
- `session`：复制 `secret.session.example.yaml` 为 `secret.session.local.yaml`，替换全部 `CHANGE_ME`。

    kubectl apply -f deploy/k8s/base/secret.<service>.local.yaml
    ENVIRONMENT=staging ROOST_IMAGE=ghcr.io/example/planet@sha256:<digest> sh deploy/k8s/deploy.sh
    kubectl -n roost rollout status <deployment-or-statefulset>/planet-<service>

生产要求：

- 把 ghcr.io/CHANGE_ME/planet:v1.0.0 替换为不可变 image digest。
- 每个有 Data Engine 的实例独占 RWO PVC；扩容时复制 workload 并分配新 SID，不能直接提高 replicas。
- Secret 不加入 kustomization，也不得提交；示例中的 CHANGE_ME 会让应用 fail-closed。
- 默认 NetworkPolicy 只允许 roost/monitoring 命名空间访问 ops 9100，不把管理端口暴露给公网。
- 声明 player TCP 时模板会开放 Service 7000，但只允许带 roost.tjbdwanghaibo.io/player-access=true 标签的调用方命名空间；监听端口变化时同步修改 Service、LB 和 NetworkPolicy。
- /healthz 仅表示进程存活，流量切换必须使用 /readyz；terminationGracePeriodSeconds 必须大于框架总停机预算。
- 上线前补 NetworkPolicy、镜像签名校验、监控抓取权限以及节点/PVC 故障演练。
