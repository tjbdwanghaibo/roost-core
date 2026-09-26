# Shell/systemd 部署

1. sh deploy/shell/build.sh 构建 Linux 静态二进制。
2. 从 configs/service/config.<service>.prod.example.yaml 复制生产配置，替换全部 CHANGE_ME，为每个 SID 设置独占 WAL 目录。
3. 执行 sudo sh deploy/shell/install.sh <service> <sid> <version> <config>。
4. 用 sh deploy/shell/healthcheck.sh 验证 readiness。
5. 需要人工回退时执行 sudo sh deploy/shell/rollback.sh <service> <sid> <installed-version>。

安装器把二进制和配置写入不可变版本化 releases 目录并生成 SHA256SUMS，原子切换 current，创建专用 systemd unit、非登录用户、只读系统保护和 SIGTERM 45 秒停机预算。同一版本名拒绝覆盖。readiness 未在预算内成功时自动切回上一 release；首次安装失败则停服。rollback.sh 只允许切换到已经安装且不可变的版本，目标版本 readiness 失败会恢复原版本。多实例部署必须使用不同 SID、配置文件和 WAL 目录；不要让两个进程共享 WAL。可用 HEALTH_URL/HEALTH_ATTEMPTS 覆盖探测地址和次数。
