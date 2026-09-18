# Scene 兴趣、实体身份与会话生命周期

基线：Core `9a97d7e`、Codegen `c73bc12`。本文记录当前 game-demo 的实现事实、已验证边界与实施建议；六项未修复问题见[本轮报告](REVIEW-2026-09-18-03.md)。

## 1. 四种身份不要混用

完整 entity ID 是实体世界中的全局身份，包含 kind/category 与 unique 部分。scene 的 spatial observer/subject、room subject 与 entitysync `SubjectID` 当前都使用完整 entity ID。只有把最终下行投递到玩家连接时，`RoomSessionResolver` 才从 entity ID 取 Player unique ID 并解析 SessionID。

`SubscriberRef` 是 Core 的通用投递身份，还允许 Player、Server、Entity、Group；因此 Core 不能假定每个 `SubscriberRef.ID` 都是实体 ID。scene bridge 应保持强约定并用生成消费测试锁住，Core 类型继续保持通用。

运行期 monster 同样必须拥有全局实体身份。进程本地序列只在单进程成立；多副本需要稳定 sid 派生的号段或已有的原子号段租赁。ID 分配器的寿命必须长于实体可被寻址的寿命。

## 2. spatial 管角色和几何，不拥有业务位置

spatial 的 observer/subject 是兴趣计算角色；Point 是调用方提供的观察点/可见点，可以来自摄像机、载具、量化坐标或隐身策略，不承诺等于某个 DAO pos。demo 选择把 Player/Monster DAO 位置作为输入，是该游戏的权威策略。

`InterestManager.resubscribe` 用 LeaveRadius 的包围盒枚举 block，并在观察者移动时做集合差分。整图 `MaxBlockCount` 保护索引构造，不能保护每个观察者的窗口。实现 RR-08 时应在配置验证阶段计算最坏观察者 block 数，并在运行期暴露实际分布。

## 3. 多来源兴趣的所有权是引用集合

当前 demo 的 `RelationSource` 把 self/team 关系变成与空间 Enter/Leave 同形的输入。聚合层按 `(observer,subject,source)` 持有引用：第一来源进入才 Subscribe，最后来源离开才 Unsubscribe；重叠来源不会互相误删。多个来源给出不同 band 时选择最小 band，即最高保真。

关系事实属于游戏服务：match/team/friend/guild 谁拥有数据，谁向 source feed 推送。框架可以约定事件形状，但在第二个实际消费者出现前，不需要把 demo aggregator 提升成 Core API。若未来提升，必须先定义来源代际、重复事件、乱序 Leave 与全量重建语义。

## 4. 订阅和连接是两条生命周期

entitysync/room 拥有主体复制订阅，TCP Runtime 拥有物理会话。当前 scene 能在 Join 或 push 失败时发现会话已经消失，但连接关闭本身不会驱动 Unsubscribe/Remove。没有后续流量时，在线集合不会收敛。

建议新增 Runtime 生命周期源：关闭栈只向有界队列投递事件；独立 worker 多播；注册返回 unsubscribe；Stop 明确拒绝新事件、处理已接收事件并结束 worker。scene 接到最后一个 session 关闭后移除 observer/subject/relations，chat presence 同样消费。慢消费者、panic 和队列满应有隔离与指标，不能拖住 socket 资源释放。

## 5. ephemeral DAO 仍可作为同步权威

Monster DAO 的字段全为 `nopersist,sync`，Entity lifetime 为 ephemeral 且 `noPersist=true`。这让位置、属性和同步序列仍走统一 DAO/component/packer 形状，同时不进入加载或持久化生命周期。生成出的 collection 元数据不会让它实际写 Mongo。

当前只有一个消费者，新增“无集合 DAO”语法的收益小于生成契约成本。更重要的门禁是：任何全 nopersist DAO 都不得意外注册持久化 mutation；MarshalSync 与 dirty propagation 必须继续可用。

## 6. 已验证与未验证

当前 demo 完整生成/全包测试通过；关系重叠与最后来源离开已有模板测试。本轮额外确认会话关闭不会主动清理、两个 spawner ID 碰撞、极端合法 AOI 配置接受 40,401 blocks。

仍未验证真实多 game 实例路由、sid 重启、关系事件乱序、连接关闭风暴、满队列策略、10k 观察者的内存与尾延迟。修复功能问题时应分别补场景，不能用单进程 demo 全绿替代。
