# Service 学习：Pipeline 错误、维护义务与审查完成

本轮实际实现见[RR-34](../bugfix/RR-20260929-34.md)，阶段结论见[第十一轮](REVIEW-2026-09-29-services-11.md)。先用框架已有工具：go-redis Exec 的完整命令结果、已有 Future 和 batch 生命周期足以修根因，不需另造批处理后端。

## 批次成功与缺失读取

单条 GET 缺失只说明那次读取缺失。go-redis aggregate 可以只是第一条命令错误；第一条是 redis.Nil，后面 HSET却因 WRONGTYPE失败，不能将整个批次解释成成功。尤其无future写入口，调用者只能由Exec获知拒绝。

修复顺序是：执行一次 → 完成全部future → 保留真实aggregate/传输错误 → aggregate为nil/Nil时扫描每个命令 → 清空本批跟踪。成功future、ErrNil、真实命令错误各自保留；某一错误不应阻止其他future完成。下一次Exec使用新跟踪数组，不修改旧future。

Pipeline 是批量发送而非事务。正式用例中的 INCR 已执行一次，即使后续 HGETALL WRONGTYPE令Exec报错，值仍为1；复用后仍为1，wrapper不回滚、不重放。外部超时也不能推定没写，依业务幂等与权威读回决定下一步。

新检查是O(batch)本地结果扫描；原批次命令/future仍O(batch)，没有增加网络命令。未测新p99/吞吐，不能凭少一次API或测试耗时宣称性能提升。Cluster不同owner可能并行执行，不能赋予跨owner全局顺序/原子性；本轮排序反例用同tag核实根因。

## 故障注入必须命中目标边界

go-redis 会为连接初始化使用内部pipeline。第一次在连接建立前装“成功后返回未知错误”hook，先打中了初始化而非业务写；断言没有看到applied并非产品新bug。先用实际key预热同owner连接，再安装hook，真实Set/Get执行后返回sentinel，原断言两后端重复通过。

保留最初失败结果、解释具体边界和fixture修正，不能删除失败历史后只报绿。这个hook只证明wrapper保留执行后未知错误，物理丢包、failover仍需真实网络故障环境；当前3Toxiproxy测试skip不能算通过。

## 后台维护与完成口径

有deadline不等于有后台回收；ticker也不等于拿得到公平任务页。Match/Activity/Session/Platform/Chat分别需要queue/group/rotating owner/pending/频道接线。单页有界、配置集合有界/公平和callback响应ctx是不同契约，按各域处理，不能用通用ticker掩盖责任。

结果未知时保留义务证明；确定缺失的pending清理要原子核对，坏记录要延期避免挡住健康任务，观察待发活动不消耗游戏侧attempt。Account token过期访问拒绝、Global lease访问围栏、Rank无期限义务无需凭空加定时器。

本阶段10域主链整理、34项新编号缺陷的声明场景修复、当前本机回归已完成；源码100路径按内容证据核算，不能叫100%行覆盖。Match归档、购买fulfilled/drain、外部资源回执、HA/强杀/真实长稳继续是具名设计和环境验收，不退回“其余service没安排”，也不说所有生产故障已经证明安全。[对应方案](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)。
