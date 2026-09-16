# Core / Kit：服务与 manager 迁移后的机制

基线 Core 41d49b9、Kit 4830150、Codegen 242b438，2026-09-16。[验收与限制](REVIEW-2026-09-16-05.md)。以下区分当前实现与待实施建议。

## 服务调用链

Core/service/{mail,session,match} 持有领域类型、状态机、store、RPC 接口及传输实现；Core/servicemetrics 持有报告契约。Kit/service 对应目录通过类型别名和函数包装保留旧导入入口，Mod 读取配置、取得 Redis 等能力、构造服务并发布 capability。

Codegen servicerpc 的 transport 半生成 wire、RegisterHandlers、BusClient、Capability 包装与方法常量，只依赖 Core。assembly 半生成 Server、ClientMod、OwnerCapabilities，留在 Kit；通过 `-dir <core import path> -emit assembly -out .` 从同一接口生成，用 Kit 别名解析类型。跨包搬迁不应改变 wire 身份，生成物六份正文已与生成器比较，demo 两种依赖模式构建通过。

match 的队列负责 Candidates 和原子 Commit；应用负责 Grouping。策略接口与 FirstCome/ScoreWindow 在 Core，Kit NewMod 只接受指标报告者，不再接收无执行者的策略。应用的候选读取和提交间仍可发生竞争，Commit 必须保留拒绝已取消/已匹配票的检查；不能将配对算法的副作用塞入 CAS 重试回调。

alias 保留类型身份和同一 sentinel，便于消费者分批迁移；不代表可以无限保留两份领域实现。当前其他服务尚在 Kit，不能用这三个包的迁移宣布全仓完成。RPC 生成头部应使用可重复命令，当前 Core 三份仍带机器绝对路径，见运行记录。

## manager 生命周期和资源归属

Kit ManagerMod 负责 Mod 名、Registry capability 注册与生命周期转发；Core/manager.Engine 负责 managers、pending 启动顺序、started 成功列表与 stopping 标志。Order 验证依赖并排序；Start 失败只回滚此前成功者，失败对象自己清理半初始化资源。Stop 在锁内取走 started，然后锁外逆序调用用户 Stop，聚合错误。

当前 Start 的 pending 是起始快照，但 Register 以 started 是否非 nil 判断准入；第一个管理器返回之前两者不一致。当前停止检查仅在每轮 Start 前，最后一个 Start 返回后没有再次与停止请求交接。因此“锁保护容器”不能等价为“生命周期所有权正确”。[RR-20260916-06/07](../bug/REVIEW-2026-09-16-05.md) 各有独立复现。

建议（尚未实现）：显式 starting/running/stopping/stopped；在启动快照形成时关闭注册，在用户 Start 成功返回后完成资源接纳或清理归属的原子判定；停止等待/超时与启动回滚必须有一致契约。锁只保护状态，不跨任意用户回调持有。用最后一项/唯一一项、途中注册和失败的边界测试，而非只断言没有双重停止。

## 性能与接入边界

此次迁移主要改变职责与依赖方向，不据此宣称吞吐提高。匹配窗口仍需有界，领域存储语义仍由原 versionstore/CAS 保证。Recover 全局 revision 采用 O(1) 比较，但不相关流修改也可使捕获失效；调用方需要有界重试，未做负载基准前不承诺高并发下重试代价。真实 broker 游标迁移和部署环境验证仍在后续清单。
