# Session 未完成准入的恢复

B3 / F09-V S5：Run.Create 成功但回复丢失，或进程在 claim 前退出，只有 run 被持久化；按 owner claim 扫描无法发现它。不能给所有 run 直接加 TTL，否则外部资源未释放就会丢掉唯一账本。

保留 run→claim→请求账本的顺序。新增 Run.AdmissionPending：创建时 true，取得 claim 后经 CAS 置 false，才允许返回给业务。Redis run 写入与待准入索引同一脚本原子完成；索引分数是 run 的业务截止时间。owner 后台每轮按有界索引读取过期准入，删除尚未交给业务且没有外部资源的 run；正常 run 的释放/保留不变。超期才取得 claim 的慢 Enter 在准入 CAS 拒绝，并按 run 身份释放自己取得的 claim。

不依赖进程内补偿或资源 TTL。索引不会扫描整个 keyspace；自定义 RunStore 若支持 PendingAdmissions，可接入同一恢复入口，否则由部署方承担未完成准入的枚举。Redis 默认自动提供。坏条目单独报错，其他条目继续；删除仍经版本与身份校验。新增索引需 Redis Cluster 同槽前缀，并在 Mod 启动检查。

Run 持久记录升级 v2，旧版本拒绝；尚未部署，升级须停旧进程、清空旧 session run/claim/request 状态和索引。验收：Create 落库丢回复→过期→后台回收；慢 claim 越过截止不能返回已失效 run；正常 Enter/Attach/Finish/Sweep 回归；私有 Redis 验证索引原子维护和命名空间。
