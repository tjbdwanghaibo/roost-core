# N03 通信审查：协议错误与预算缺口

2026-10-04，审查起点 `c4aa1e7dd69bbf795fee878b1fb68e0bba11525a` 加本轮请求链修复的工作树，产品对应修复提交 `4979651436ddc9b4f1f559af2a0353de64be2994`。bus/nats/servicerpc/etcd/kit/nats 未改，见[源码 blob/范围](../review/evidence/noncore-review-20261004-06/source-manifest.json)。新增三项 P2 **已确认、未修**；NC-05～07 已修，不属本批。

| RR | 问题 | 失败证据 |
| --- | --- | --- |
| RR-20261004-NC-08 | JetStream 无 handler 回包缺少客户端必须的 envelope | wire只有code/reason，decode报unsupported rpc response version 0 |
| RR-20261004-NC-09 | NATS Assembly.Close 的 RPC callback 排空绕过ctx | 已取消ctx下100ms内不返回，释放已进入callback后才返回nil |
| RR-20261004-NC-10 | 服务发现发生在配置call timeout之外 | 配置20ms，Discover无deadline；带150ms父deadline实等150.6812ms |

最终17个叶子/独立场景：4失败对应3根因，13控制通过；六包race/vet通过、93 test pass事件、0fail/skip。[运行](../review/REVIEW-2026-10-04-noncore-06.md) · [原始反例/复跑](../review/evidence/noncore-review-20261004-06/README.md) · [机制与实施方向](../review/IMPLEMENTATION-MESSAGING-RPC-BUDGET-AND-TERMINAL-OWNERSHIP.md)。不是编译失败、钩子未触发或生产外部资源测试。

## RR-20261004-NC-08

**P2：无 handler 的 JetStream RPC response 与客户端协议不一致。** `bus/jetstream_rpc.go:400–410` 的 !ok 分支直接 Marshal rpcErrorResponse，正常/业务错误分支却用 encodeRPCSuccess/encodeRPCFailure。`bus/rpc_error.go:44–85` 两个解码入口都要求 rpcWireVersion=1；实际返回 `{"code":1,"reason":"server error"}`，客户端报 `bus: unsupported rpc response version 0`，无法按远端拒绝的稳定status处理。

触发是订阅仍在但 method 的handler不存在（注册/滚动变更或请求声明的方法不匹配）；不声称不存在任何订阅时broker也会走这个分支。probe直接进入正式 onJetStreamRPCRequest，并用实际发布字节走客户端解码；正常成功、业务error、panic三对照均带正确envelope。没有模拟网络重连，也不声称error返回应当errors.Is本地ErrNoHandler——跨服务本来通过errcode映射。

建议只将无handler分支改用既有encodeRPCFailure，保持publish/ACK及errcode语义；不加legacy裸error解码兼容来掩盖生产侧错包。正式回归覆盖无handler版本/错误状态、正常response、marshal与publish失败以及真实CallReliable响应消费。

## RR-20261004-NC-09

**P2：回调阻塞时 Assembly.Close(ctx) 没有预算。** `nats/driver/assembly.go:59–61` 先同步RPC.Stop，再调用有ctx的DrainWithContext。`nats/driver/rpc.go:219–236` Stop取消pending后用pool.Stop，后者等待Background排空；已有 `worker.Pool.StopWithContext` 没被复用。`kit/nats/nats_mod.go:165–172` 的停止预算因此无法在这段阻塞等待中生效。

本机用真实RPC callback pool确认业务callback已经进入，再调用已取消ctx的Assembly.Close；100ms观察窗没有返回。显式释放callback后，Close在1s保护窗内完成且error=nil。入口hook必达、阻塞因果与释放后完成均已证明，100ms用于观察已取消预算失效，不是sleep造顺序。另两控制为16个terminal竞争只执行一次callback、空闲pool正常排空。未使用真实NATS连接，这证明本地所有权链，不代替connected Assembly/弱网验收。

建议在既有RPCClient增加可重复等待的StopWithContext，复用Pool.StopWithContext与唯一停止状态；Assembly/Kit逐层传ctx。caller超时不等于callback退出，必须保留资源/再次等待的责任，不能立即置nil伪装停止成功。还要处理callback队列满时同步fallback可能在pending遍历内阻塞；单替换pool.Stop不保证整段有预算。验收先覆盖原阻塞项、timeout后再次等待、并发stop、满队列fallback与callback自己发起停止，不用无界goroutine或丢弃已接受callback换成功。

## RR-20261004-NC-10

**P2：发现调用不受配置timeout约束。** `servicerpc/client.go:163–169` 先PickServer(ctx)，其timeout只在Call的第125行附近创建；`etcd/driver/discovery.go:356–359` 直接将传入ctx用于Get。配置20ms但调用CallDiscoveredChecked(Background)时，Discover收到无deadline上下文；真实etcd不可达或卡顿可让这个阶段无限等待，尚未进入transport预算。

另一个协作dependency反例以150ms父期限、20ms配置等待ctx.Done，实等150.6812ms才返回DeadlineExceeded，实际发现budget=150ms。断言比较依赖收到的deadline而不是要求严格计时SLO；原父期限不丢、空/坏候选不发调用、指定sid成功与业务错误四控制通过。没有使用真实etcd或证明生产延迟分布。

建议在CallDiscoveredChecked入口建立一个覆盖发现、picker、transport与status的共同child context；保留更短父deadline，不在发现后重置完整预算。复用现有Call/CallChecked，避免新增路由框架。PickServer作为单独公开操作的timeout契约需另写清，不能由修组合调用偷偷改变；保留discover cause的errors.Is，发现失败不向任意实例回退、不触发业务。补发现占部分预算后transport余量与nilctx场景，再走正式生成服务消费者。

## 观察与余项

同步RetryPolicy默认1次；显式开启多次时timeout不能证明业务未执行，需要业务幂等。JetStream handler→response publish失败可引发重投，同RequestID的业务副作用必须自己幂等，当前async ReliableStore明确不包RPC。异步RPC每call持有pending/sub/timer，源码未有独立max-pending门槛，callback队列有界不代表在途调用有界；未压测，作为容量观察。Affinity排序为复制后插入排序，O(n²)；affinity不是共享状态CAS替代。本批不新增第四个RR或声称N03完整收口。
