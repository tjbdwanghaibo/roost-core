# 停机 WAL 检查与隔离副本

承接历史A1。新增 `nestwal.Inspect` 和 `cmd/walinspect`，工具已实施，普通测试及race三轮通过；非压测验收继续，负载保持暂停。

## 使用与恢复边界

先停止拥有该WAL的业务进程，保留原目录，不启动会自动修尾的 `nestwal.Open`。从仓库构建工具：

```sh
go build -o /tmp/roost-walinspect ./cmd/walinspect
/tmp/roost-walinspect -dir /absolute/wal -snapshot /absolute/new-isolation-dir
```

工具必须能取得原有writer.lock，不能检查活跃writer或通过删除锁文件绕开它。`-snapshot`目标必须不存在且在原目录之外；其父目录先由操作者准备。原WAL采用非默认记录上限时，用 `-max-record-bytes` 设置相同上限。

隔离目录内容：

- `wal/`：所有原始文件和子目录的完整独立副本，包含checkpoint、坏记录及其后缀。
- `manifest.json`：每个文件的相对路径、大小、SHA-256。
- `inspection.json`：checkpoint槽状态、每段有效前缀与首个错误的位置。
- `INCOMPLETE`：复制或诊断未完成时保留；存在就不能把该目录当作已完成副本。

副本须同时具备wal目录、manifest、inspection报告，报告complete=true且INCOMPLETE不存在，并按manifest核验文件；仅凭INCOMPLETE不存在不能判断中途掉电留下的目录完整。

退出码非0表示拒绝、损坏或I/O/取消等错误。`complete=true`只表示诊断扫描结束，不表示WAL无损坏；即使发现损坏，也应有完整副本和报告。坏checkpoint槽单独保留诊断，两个槽都无效时明确报错。工具不会截断尾部、跳过记录、推进checkpoint、删除事务或设置任何提交标记。

先根据segment、`last_good_offset`定位首个坏记录，再关联事务ID、生成器/codec版本、权威数据与事务回执。帧CRC和记录解码通过，只证明本地格式合法；没有连接Mongo/Redis，也没有验证所有投影业务约束。权限、版本冲突或未知提交需要沿原事务ID查权威结果，不能因为“检查通过”而重新生成事务。

修复导致历史合法记录无法消费的代码后，先在副本和隔离依赖中重放核对；原始坏字节需要权威备份或具名数据修复，不提供自动跳过。选择恢复副本时应保留原件并由正常WAL恢复路径重放，确认全部结果后再作运维切换。本轮不执行生产恢复或部署。

## 实现与验收边界

复用现有frame CRC扫描、record decoder、checkpoint decoder与目录互斥，数据不经重新编码。副本逐文件fsync并同步目录，使用固定32KiB复制缓冲，取消逐块检查；不把整个WAL读入内存。拒绝链接和特殊文件，目录锁要求其他操作者也遵守正常writer排他协议，不支持检查期间手动篡改文件。

已通过活跃writer拒绝、CRC/codec坏记录及合法后缀完整保留、真实截断尾不修复、坏checkpoint保留、哈希核对、目标存在/位于源内/链接拒绝和开始前取消测试。这些行为已在macOS普通/race验证；真实大目录属于容量验证，本轮不跑。取消时未完成报告不能移除INCOMPLETE；诊断不能代替物理掉电恢复验收。Linux目标race及两平台CLI也已通过，见[验收记录](../review/OUTBOX-HISTORICAL-CLOSURE-2026-10-08.md)。
