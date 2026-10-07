# Kubernetes 部署

模板默认每个 Service 一个副本和唯一 SID，避免多个 writer 共享身份或 WAL。先创建配置 Secret，再选择 staging/production overlay。生产流水线使用不可变 digest 和 deploy.sh：

- `account`：复制 `secret.account.example.yaml` 为 `secret.account.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `activity`：复制 `secret.activity.example.yaml` 为 `secret.activity.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `chat`：复制 `secret.chat.example.yaml` 为 `secret.chat.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `game`：复制 `secret.game.example.yaml` 为 `secret.game.local.yaml`，替换全部 `CHANGE_ME`。停机：23 个 Mod，`shutdown.total_timeout` 111s，`terminationGracePeriodSeconds` 116。
- `global`：复制 `secret.global.example.yaml` 为 `secret.global.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `mail`：复制 `secret.mail.example.yaml` 为 `secret.mail.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `match`：复制 `secret.match.example.yaml` 为 `secret.match.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `platform`：复制 `secret.platform.example.yaml` 为 `secret.platform.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `rank`：复制 `secret.rank.example.yaml` 为 `secret.rank.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。
- `session`：复制 `secret.session.example.yaml` 为 `secret.session.local.yaml`，替换全部 `CHANGE_ME`。停机：6 个 Mod，`shutdown.total_timeout` 23s，`terminationGracePeriodSeconds` 28。

    kubectl apply -f deploy/k8s/base/secret.<service>.local.yaml
    ENVIRONMENT=staging ROOST_IMAGE=ghcr.io/example/planet@sha256:<digest> sh deploy/k8s/deploy.sh
    kubectl -n roost rollout status <deployment-or-statefulset>/planet-<service>

生产要求：

- 把 ghcr.io/CHANGE_ME/planet:v1.0.0 替换为不可变 image digest。
- 每个有 Data Engine 的实例独占 RWO PVC；扩容时复制 workload 并分配新 SID，不能直接提高 replicas。
- Secret 不加入 kustomization，也不得提交；示例中的 CHANGE_ME 会让应用 fail-closed。
- 默认 NetworkPolicy 只允许 roost/monitoring 命名空间访问 ops 9100，不把管理端口暴露给公网。
- 声明 player TCP 时模板会开放 Service 7000，但只允许带 roost.tjbdwanghaibo.io/player-access=true 标签的调用方命名空间；监听端口变化时同步修改 Service、LB 和 NetworkPolicy。
- /healthz 仅表示进程存活，流量切换必须使用 /readyz；每个 Service 的 shutdown.total_timeout 按它实际注册的 Mod 生成（声明预算之和 + 3s × 未声明 Mod 数 + 5s），terminationGracePeriodSeconds 为 max(它, 配置里实际的 total_timeout) + 5s（见上方列表）；增减 Mod 或调大 total_timeout 后执行 roost project sync 重算宽限期与 systemd TimeoutStopSec；dataengine.shutdown_timeout 与 player_access.tcp.shutdown_timeout 不参与生成（按 30s / 10s 计），调大它们须同时手动调大 total_timeout 再 sync（roost doctor 检查模板宽限期不低于配置 total + 5s）。
- configdata 的数据表随镜像发布：Dockerfile 把 configs/data 拷到 /app/configs/data（运行层 WORKDIR /app，config_data.dir 的生成值是相对路径，按它解析），和二进制作为同一份制品晋级；环境配置与密钥仍在部署时挂载。要按环境换数据，把只读卷（compose 用 bind，k8s 用 ConfigMap / 卷）整目录挂到 /app/configs/data 遮盖镜像内容，更新后经 configdata Reload 热加载；把 config_data.dir 改成别的路径时镜像里的数据不会跟着移动，须自己挂到新路径。
- 只读根文件系统下 stats_log.dir（相对的 log，即 /app/log）挂 stats-log emptyDir（sizeLimit 1Gi，默认 1 分钟一条约可存一年以上，随 Pod 删除）；要长期保留或调小 interval 时改成 PVC 或接日志采集。写不进去时进程启动即 WARN，并计入 stats_log.write_failures。
- 上线前补 NetworkPolicy、镜像签名校验、监控抓取权限以及节点/PVC 故障演练。
