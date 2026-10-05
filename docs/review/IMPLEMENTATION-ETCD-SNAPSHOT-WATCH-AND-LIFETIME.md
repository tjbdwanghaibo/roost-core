# etcd：快照接续、镜像隔离与生命周期预算

2026-10-04，首次审查时etcd产品等于`e62729ac`；后续以`3560a19b`为基线修复NC-11/12，本文已同步本轮实现。见[原审查范围/实证](REVIEW-2026-10-04-noncore-08.md)、[修复与验证](REVIEW-2026-10-04-noncore-09.md)、[NC-11记录](../bugfix/RR-20261004-NC-11.md)和[NC-12记录](../bugfix/RR-20261004-NC-12.md)。快照/镜像实现本轮未修改。

## 服务发现与选主各自拥有什么

Assembly共用一个clientv3连接，KitEtcd只读配置、声明三项能力、转发生命周期。Discovery注册时Grant→PutWithLease→独立KeepAlive，keepalive结束后按退避重注册；Deregister停止循环和keepalive，再revoke当前lease。业务使用ServiceInfo定位实例，不以“本地仍有sid”证明租约有效。其等待lifecycleMu/loopDone与网络revoke预算是不同阶段，需继续补关闭证明。

Election session独立于一次Campaign等待；获得领导权时先发布CreateRevision fence再置IsLeader，session结束/Resign通过finish清理并关闭LeaderChan。第一次Campaign保留之前已取走的LeaderChan，重新竞选换新通道，旧session完成不能清掉新session。IsLeader有网络观察滞后，敏感写必须在权威路径比较fence，不能仅凭bool授权。

NC-11原先在session建立阶段：NewSession默认使用client生命周期ctx，Campaign的ctx只传入之后的elect.Campaign。现在为session建立独立生命周期ctx，通过context.AfterFunc将caller取消接到setup，并把该ctx传给concurrency.WithContext；成功后解除连接，避免caller稍后取消撤销已获得的领导权。失败先取消session再清理，创建失败时保留caller取消/超时与SDK错误两种cause。campaignSession统一Close/释放责任，并避免SDK在Revoke前发出的Done让正常Close提前取消其清理ctx。

本轮证明建立阶段可取消、成功后caller取消不失去领导权，以及旧session退出不清掉新session。正常Resign仍可能等待SDK默认TTL 60秒的Revoke；失败取消不等于服务端租约已立即撤销，也不证明超时Grant一定未创建租约。不能从一处Grant修复推断整条生命周期有界。

## Snapshot 后从哪个 revision 接续

Client.GetPrefixSnapshot返回一致前缀KV和header revision R；LocalMirror先decode/clone，形成独立本地值，再从R+1 watch，处理成功Created/终止错误后更新Synced。watch关闭/compaction/坏解码会置错误并退避重快照，snapshot revision倒退被拒绝。Ready通道在建立或终止时都解开，必须一起看WatchError，不能Ready关闭就断言成功。

Get/Snapshot在锁外clone不可变内部值，订阅注册和初始snapshot由callbackMu与更新串联，避免注册缝隙丢事件；回调不在镜像状态锁内运行。每subscription独立环队列，慢消费者只终止自己，停止watch不会等待不协作handler退出；订阅自己的Done才代表其回调结束。镜像Close与订阅Close是两种完成责任。

Publish/Delete不直接修改本地镜像，权威watch才推进本地状态。CAS期望revision=0比较Version=0，否则比较ModRevision；失败/未知结果不能直接从本地“还没变化”判断权威未写，更不能自动重试副作用。旧watch事件低于当前revision被忽略，同revision多key事件仍逐项交付；本文不宣称任意调用时Snapshot已经具备跨多个watch事件的事务批次原子性。

## 关闭请求与实际退出分开

默认driver watcher取消ctx后在接收/发送处都可退出；原审查128事件、64容量的关闭控制通过。NC-12原先源于requestClose同步调用第三方IWatcher.Close，使caller尚未进入ctx等待就可能卡住。现在每subscription用sync.Once启动唯一底层关闭任务；关闭请求和parent取消都能触发它，caller按自己的ctx等待同一个完成信号，重试不会重复关闭。callback循环必须等handler和watcher关闭实际结束后才关闭Done；Err保留handler/生命周期错误并合并底层Close错误，Close返回底层关闭错误。

关闭调用预算到期只终止该caller的等待，subscription继续拥有清理责任。非协作handler或第三方watcher仍可无限期占用该责任，本轮未提供强制终止，也不把提前返回当作已关闭。

修复保留原8项探针的红/绿证据，并新增控制，最终15项正式用例通过，两个etcd测试包race回归和三包vet通过。实际gRPC LeaseGrant验证了真正SDK调用，但server只实现故障注入接口，不能代表完整etcd一致性。NC-12使用受支持第三方watcher门闩，不误报core watcher永久阻塞。O(n)快照、JSON克隆、subscriber数量/队列总内存是后续容量观察，尚无benchmark/HA/长稳结论。

## 10-05 后续（revn03）

上文“正常Resign仍可能等待SDK默认TTL 60秒的Revoke”在真实etcd上复现（SIGSTOP后Resign(500ms)阻塞>20s），登记并修复为[NC-93](../bug/RR-20261005-NC-93.md)：正常Resign与失败放弃现在走同一个`abandon`——Orphan停keepalive，Revoke由election持有、5s截止，caller在等就拿结果，期限先到返回ctx错误。同轮在真实etcd上验证了lease消失后约1.2s重注册、断网期间写入+压缩后Mirror重载恢复；Mirror的`Synced`表示watch已建立而非连接可达（断网期间仍为true），见[本轮观察O6](REVIEW-2026-10-05-noncore-n03.md#观察未登记-rr)。选举API在仓内已无生产调用方，去留见本轮方向判断。
