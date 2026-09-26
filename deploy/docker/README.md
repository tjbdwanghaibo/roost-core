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
      planet:v1.0.0 game --sid 1000 --config /etc/roost/config.yaml

配置必须让 ops 监听 0.0.0.0:9100，日志输出 stdout，WAL 使用挂载卷。Player TCP 已声明，因此示例同时发布 7000；若生产配置修改 player_access.tcp.addr，端口映射、LB 和防火墙必须同步修改。 镜像 tag 必须不可变，生产流水线应进一步使用 digest、签名和 SBOM。

生产 Compose：

    cp deploy/docker/env.example deploy/docker/.env.production
    # 编辑 ROOST_CONFIG_ROOT，准备每个 Service 的 config.<service>.yaml
    ROOST_IMAGE=ghcr.io/example/planet@sha256:<digest> sh deploy/docker/deploy.sh
    sh deploy/docker/rollback.sh

deploy.sh 要求 digest，使用容器内 /app/healthprobe 等待所有实例 readiness，并记录 current/previous image。生产 workflow 使用受保护 Environment 和带 roost-docker 标签的 self-hosted runner。
