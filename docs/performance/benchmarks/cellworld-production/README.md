# CellWorld 本轮验收证据

基线 `cd2493ec`，工作树 `feat/cellworld-production`，macOS arm64 / Go 1.27.3 / Apple M5 10 核 / 32GiB / 接电，GOMAXPROCS=10（环境默认）；采样时为未提交工作树，具体源码以随附 SHA-256 为准。口径、命令与容量失败解释见 [当前验收报告](../../CELLWORLD.md)。

- `fault-drill.log`：真实账号 / TCP / Nest / 私有 Mongo、Redis、NATS 的 15 步演练；包括进程重启，不代表 Redis HA / 跨机验证。
- `robot-smoke.json`：20 人 10s 普通分区场景，Robot 正式报告。`robot-smoke-cellworld.json` 是请求 ACK 与 Sync 统计；不是 10000 Actor 容量结论。
- `behavior-evidence.json`：续活的修前失败、修后组合回归、完整 CellWorld race 与全新 realm 工程的验证结果；只收小型可携带日志。

- `witness-decode-before.txt` / `witness-decode-after.txt` / `witness-decode-benchstat.txt`：同机交替 10 轮内部解码微基准。
- `core-capacity-1pct.log` / `core-capacity-5pct.log` / `core-capacity.json`：四 Cell 核心容量，均未过 50ms 指标。
- `robot-admission-failed.json`：首次同机单 IP 配额不足，实际只接入 128 人，整轮失败。
- `robot-capacity.json`：修正配额后的完整联合结果，81 完成 / 919 失败；包含失败分类、资源与服务计数，不能当千人稳态通过。
- `source-sha256.json`：本轮 122 个修改代码 / 模板 / 脚本 / 配置的最终 SHA-256。
- `index-coverage.json`：CBM 新代际与 5 个模板局部解析缺口；缺口已直接读源码补证。

原始大日志、临时数据库与二进制不入库，复跑使用仓库脚本。Lua / Redis 记录未丢失、单实例租约、安全时钟窗口、固定拓扑是本轮契约前提。
