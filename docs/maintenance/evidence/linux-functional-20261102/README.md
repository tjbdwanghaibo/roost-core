# 验收证据索引

对应 [Linux 非压测验收报告](../../LINUX-FUNCTIONAL-20261102.md)。`results.json` 统计各原始日志的 SHA-256、字节数、退出码（有独立 exit 文件时）、测试 PASS/FAIL/SKIP 事件及跳过名称；不是只保留最后一次绿色运行。

- `default.json`：全仓默认运行，三条失败测试事件对应一个 Lockstep 叶节点及其父节点。
- `lockstep-isolated.log`：该顶层用例单独 race 复跑。
- `integration-entry.log`：最初依赖启动无 subreaper，停止进程 zombie 造成故障测试失败。
- `integration-reaped.log`：旧 Toxiproxy 尚占用端口，环境未正确启动。
- `fault-matrix.log` 与 `linux-functional-20261102.tsv`：代理缺失的第一轮矩阵，不能冒充有效弱网验收。
- `integration-final.log`：修复进程回收和代理环境后的完整依赖入口，仍有 etcd 冻结用例失败；后续生成脚本另行补跑。
- `fault-matrix-final.log` 与 `linux-functional-verified-20261102.tsv`：环境恢复后的独立最终矩阵。
- `real-*.log`：正式游戏脚本的真实依赖模式；失败即退出的脚本所余用例以单独日志补充，不覆盖原结果。
- `genres.json`：13 类默认生成消费者矩阵，明确采用持久化替身，零跳过只适用于该矩阵。
- `codegen-network.json`、`published-module.json`、`dotnet-build.log`、`distroless-build.log`、`distroless-offline.log`：网络认证、SDK 迁移和构建资源阻塞，详见报告。

原始日志位于执行环境 `/workspace/linux-acceptance/logs`；机器环境不会随 Git 克隆分发。`results.json` 的哈希可核对归档原件，但仅凭哈希不能重建日志。独立复跑原始日志及全仓 Lockstep 摘录随本目录提交；其余失败摘录与重跑入口在主报告中；保留的完整日志可供本环境继续定位。
