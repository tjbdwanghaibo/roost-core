# Docker 部署

镜像只包含二进制，不包含生产配置或密钥：

    docker build --build-arg VERSION=v1.0.0 -t planet:v1.0.0 .
    docker run --rm --name planet-game-1000 \
      --read-only --tmpfs /tmp:rw,noexec,nosuid,size=64m \
      --cap-drop ALL --security-opt no-new-privileges \
      -p 9100:9100 \
      -p 7000:7000 \
      -v "$PWD/config.prod.yaml:/etc/roost/config.yaml:ro" \
      -v planet-wal:/var/lib/roost/wal \
      -v planet-log:/app/log \
      planet:v1.0.0 game --sid 1000 --config /etc/roost/config.yaml

配置必须让 ops 监听 0.0.0.0:9100，日志输出 stdout，WAL 使用挂载卷。只读根文件系统下 stats_log.dir（相对的 log，即 /app/log）必须挂可写卷，否则统计文件写不进去（进程启动时 WARN，计入 stats_log.write_failures）；生产 compose 已为每个 Service 挂 <app>-<service>-log 命名卷。Player TCP 已声明，因此示例同时发布 7000；若生产配置修改 player_access.tcp.addr，端口映射、LB 和防火墙必须同步修改。 镜像 tag 必须不可变，生产流水线应进一步使用 digest、签名和 SBOM。

命名卷里的统计文件 <service>-<sid>.stats.log 不轮转（进程以 O_APPEND 追加写、不重开文件，也没有可发的 reload 信号）。宿主机上按卷的挂载点配 logrotate，必须用 copytruncate：卷名用 docker volume ls 看（compose 给 planet-<service>-log 加了项目名前缀），挂载点用 docker volume inspect -f '{{ .Mountpoint }}' <卷名> 看，默认在 /var/lib/docker/volumes/<卷名>/_data。示例：

    /var/lib/docker/volumes/*planet-*-log/_data/*.stats.log {
        daily
        rotate 14
        compress
        delaycompress
        missingok
        notifempty
        copytruncate
    }

Docker Desktop 这类把卷放在虚拟机里的环境宿主机够不到挂载点，改用日志采集 sidecar 或调大 stats_log.interval。复制与截断之间追加的记录会丢，至多一条；同一份数据已发布为 metrics gauge 与 ops /statsz。

configdata 的数据表随镜像发布：Dockerfile 把 configs/data 拷到 /app/configs/data（运行层 WORKDIR /app，config_data.dir 的生成值是相对路径，按它解析），和二进制作为同一份制品晋级；环境配置与密钥仍在部署时挂载。要按环境换数据，把只读卷（compose 用 bind，k8s 用 ConfigMap / 卷）整目录挂到 /app/configs/data 遮盖镜像内容，更新后经 configdata Reload 热加载；把 config_data.dir 改成别的路径时镜像里的数据不会跟着移动，须自己挂到新路径。

生产 Compose：

    cp deploy/docker/env.example deploy/docker/.env.production
    # 编辑 ROOST_CONFIG_ROOT，准备每个 Service 的 config.<service>.yaml
    ROOST_IMAGE=ghcr.io/example/planet@sha256:<digest> sh deploy/docker/deploy.sh
    sh deploy/docker/rollback.sh

deploy.sh 要求 digest，使用容器内 /app/healthprobe 等待所有实例 readiness，并记录 current/previous image。生产 workflow 使用受保护 Environment 和带 roost-docker 标签的 self-hosted runner。

make compose-check 除了 docker compose config --quiet（只校验语法）还跑 deploy/docker/compose_check_test.go：读 docker compose config --format json 解析后的结果，核对每个 Service 的 read_only / user / cap_drop / security_opt、tmpfs 恰好一条绝对路径挂载、stop_grace_period、healthcheck 与 config bind / 命名卷（RR-20260930-17；RR-20260927-33 那种"语法合法、语义错"的 tmpfs 在这里红）。这个测试只在设置 ROOST_COMPOSE_CHECK 时执行（CI 的 generated-and-deployment 作业设置它），平时 go test ./... 跳过它。
