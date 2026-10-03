# Shell/systemd 部署

1. sh deploy/shell/build.sh 构建 Linux 静态二进制。
2. 从 configs/service/config.<service>.prod.example.yaml 复制生产配置，替换全部 CHANGE_ME，为每个 SID 设置独占 WAL 目录。
3. 执行 sudo sh deploy/shell/install.sh <service> <sid> <version> <config>。
4. 用 sh deploy/shell/healthcheck.sh 验证 readiness。
5. 需要人工回退时执行 sudo sh deploy/shell/rollback.sh <service> <sid> <installed-version>。

安装器把二进制和配置写入不可变版本化 releases 目录并生成 SHA256SUMS，原子切换 current；unit 的 WorkingDirectory 是 current 指向的 release，相对的 config_data.dir（configs/data）与 stats_log.dir（log）都按它解析——用 configdata 的 Service 安装时把工程里的 configs/data（可用 CONFIG_DATA 指定）拷进 release，与镜像布局一致，每个 release 里的 log 链接到实例日志目录 /var/log/roost/<instance>（LOG_ROOT，服务唯一可写的日志位置）；创建专用 systemd unit、非登录用户、只读系统保护和按 Service 生成的 SIGTERM 停机预算：TimeoutStopSec = max(该 Service 按实际注册的 Mod 生成的 shutdown.total_timeout, 配置里实际的 total_timeout) + 5s，当前为 account 28s、activity 28s、chat 28s、game 113s、global 28s、mail 28s、match 28s、platform 28s、rank 28s、session 28s；增减 Mod 或调大 total_timeout 后执行 roost project sync 重算。同一版本名拒绝覆盖。每个 release 在它自己安装时写的 systemd unit 下运行：unit 记在 $APP_ROOT/units/<version>.service，覆盖前先把正在用的那份记给当前 release，切换 current 时一并装回目标 release 的 unit 并 daemon-reload；切换前先按正在用的 unit 停掉当前进程，正在运行的版本按它自己 unit 的 TimeoutStopSec 停机，再装入目标 unit 启动。readiness 未在预算内成功、或目标 unit 装不上（install / daemon-reload 失败）时自动切回上一 release 及其 unit；首次安装失败则停服。rollback.sh 只允许切换到已经安装且不可变的版本（版本号不能是 . 或 ..），目标版本 readiness 失败或 unit 装不上会恢复原版本并以非零退出。多实例部署必须使用不同 SID、配置文件和 WAL 目录；不要让两个进程共享 WAL。可用 HEALTH_URL/HEALTH_ATTEMPTS 覆盖探测地址和次数。

统计文件 LOG_ROOT/<service>-<sid>.stats.log 不轮转：statslog 以 O_APPEND 打开它、进程运行期间不重开，长期运行的实例由运维配 logrotate，必须用 copytruncate（create / 改名式轮转会让进程继续写旧文件，新文件一直是空的；也没有可发的 reload 信号）。示例 /etc/logrotate.d/planet-stats：

    /var/log/roost/*/*.stats.log {
        daily
        rotate 14
        compress
        delaycompress
        missingok
        notifempty
        copytruncate
    }

copytruncate 在复制与截断之间追加的记录会丢，至多一条（默认 stats_log.interval 1 分钟）；同一份数据每次采集都已发布为 metrics gauge 与 ops /statsz，统计文件只是旁路留档。
