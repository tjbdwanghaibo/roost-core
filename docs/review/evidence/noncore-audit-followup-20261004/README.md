# 三组NC复审的独立核对证据

当前产品d3e69336（同7fbdc735内容），历史实现74e1ba39（v1.19.0）。[Run-Verify.ps1](Run-Verify.ps1) `-Mode Red/Green -GoExecutable <go> -RedisExecutable <redis-server> -RedisCliExecutable <redis-cli> -RepoRoot <repo> -OutputRoot <new directory>`，GOWORK=off，真实Lua使用每次独占loopback Redis、Hidden，最终只关闭自己创建的实例。依赖由已有Go1.27离线缓存提供，脚本不安装资源。

| 模式 | 结果 | 材料 |
| --- | --- | --- |
| Red | 28行为失败/4控制通过/0skip，test exit1 | [事件](red-tests.jsonl)、[退出与selector](red-exits.json)、[实例退出](red-cleanup.json) |
| Green | 原32场景加3个etcd SDK形态单元与1个Kit组合，共36叶子pass/0fail/0skip，test exit0 | [事件](green-tests.jsonl)、[退出](green-exits.json)、[实例退出](green-cleanup.json) |
| Checks | 相关vet exit0、根包exit0 | [退出](green-checks.json)、[根包事件](green-root.jsonl) |

Red在overlay里替换5个历史实现/原替身，保持现有正式断言：Layered8、ReadThrough5、RefHMap6、mongotest9条红。NC-30测试引用历史不存在的新API且不在本范围，overlay为空package cache；不计该文件覆盖。这是具名历史实现负对照，不是整份旧tag工程。脚本拒绝将编译失败、panic、race报告或整包timeout当行为红。初次MSYS Redis绝对配置路径启动失败，没有形成测试结果；改为工作目录内相对配置路径后，以上两次实际执行成功，失败scratch保留但不计统计。

本轮没有真实etcd/Mongo/NATS的故障环境，不据SDK形态单元或无连接Assembly判外部验收完成。真实TTL窗口回归是对正式RR-03测试的复跑，另有无sleep续期/缺子hash控制。绿只代表命令selector中的行为，不是全仓/33条修复/HA或长期容量全部验收。

[merge-status.json](merge-status.json)来自`git merge-base --is-ancestor <fix> 7fbdc735`，6个修复及Wanted登记提交全部已包含；[summary.json](summary.json)从JSONL终止事件按叶子计数。合并证明独立于运行结果。

[coverage.json](coverage.json)21材料路径，Tier2、旧generation2026-09-30T11:58:14Z，当前源码按具名分支补证；图谱heuristic误边与旧snippet行号截断不能证明调用关系/完整读取。SDK Session.Close另外读取本机`go.etcd.io/etcd/client/v3@v3.7.1/concurrency/session.go`，可按当前go.mod/go list -m定位，不要求另一机器有同一缓存绝对路径。[复盘与限制](../../REVIEW-2026-10-04-fix-audit-retrospective.md)。

收尾整合aa35d4a1、RR-08：Kit/NatsDriver普通包45叶子race通过、0fail/0skip，[事件](late-nats-verified.jsonl)/[退出](late-nats-verified-exits.json)，[最终vet/根包退出0](late-checks.json)、[根包事件](late-root.jsonl)。两个实际Client连接协议桩用例不等于真实broker集成验收。首次默认沙箱[两条dial权限环境失败](late-nats.jsonl)/[退出1](late-nats-exits.json)不计产品红，放行本机loopback后才取得绿；完整保留。新增4材料路径[late-coverage](late-coverage.json)仍旧generation，读取当前具名源码/用例，不称全矩阵重审。[ci快照](ci-snapshot.json)只记录采样时状态。

最后rebase接入c9aab877（RR-09缓存修改）后，原Green选择器重跑：36叶子pass/0fail/0skip、vet/root退出0、Redis已退出；见final-green-*。这是既有六项影响面复验，不含新RR-09八个Redis竞争用例，不冒认另一线实际资源证据。
