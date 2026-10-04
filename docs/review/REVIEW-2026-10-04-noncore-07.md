# N03 三项修复与正式回归

2026-10-04，最新远端main`e62729aca28f8db8e9613145c3b3cf5e1bc1701e`，干净工作树fetch/pull --ff-only无新main提交；其他remote branch更新没有改变审查目标。按本轮“修复，并继续review”授权执行roost-bugfix后接roost-review。仓库维护的roost-bugfix/coding/optimize包与本机副本逐文件hash一致，无需更新；本地roost-review为该仓长期入口。

NC-08～10三项 **已修、声明场景验证、未发版**：[回包](../bugfix/RR-20261004-NC-08.md)、[停止](../bugfix/RR-20261004-NC-09.md)、[发现预算](../bugfix/RR-20261004-NC-10.md)。复用encodeRPCFailure、Pool.StopWithContext、sync.Once/WaitGroup、标准库child context；未新增框架层/包或必选接口方法。

17正式修前叶子4fail/13pass，最终新增控制后的28叶子全绿，原overlay17原文通过。六测试包race/vet通过、126 test pass事件、0fail/skip；最终只新增Bus测试后单独补Bus/vet，按最后每包统计，不重复累计。[命令/日志/源码指纹](../bugfix/evidence/noncore-bugfix-20261004-04/README.md)。

正式CallReliable消费经过实际请求编码/handler/回包/关联/解码，但底层capture不是broker；停止使用真实pool，发现为协作替身。真实connected KitNats/Assembly、弱网、HA、吞吐和永久阻塞callback不可声称已验。callback内可取消停止等待通过，无期限等待自身仍禁止；重复Stop改为等同一停止任务。

Tier2 Verify确认roost-core root/ready；generation仍09-30。针对三主链结构定位、双方depth1 trace与片段；同名Close/Stop和heuristic误边以具体文件核对，不把图谱0caller当无消费者。13初始证据路径及追加路径coverage无记录gap但metadata_changed/not_tracked，当前源文和测试补证；最初误列不存在servicerpc/client_test.go，已用实际servicerpc_test.go纠正，不据missing作负结论。未重启或重建共享索引。

接续[etcd/KitEtcd审查](REVIEW-2026-10-04-noncore-08.md)，新问题和上述已修三项分别登记。[跨轮进度](PROGRESS.md)与[计划](NONCORE-REVIEW-PLAN-2026-10-03.md)不因回归全绿把N03标完整收口。
