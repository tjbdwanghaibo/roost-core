# scenespace：普通地图的空间和视野

## 能力

把 `game/modules/scene` 的配置、空间、区域与视野规则装进工程。不安装 Player 字段、Scene 实体、协议、服务入口或默认地图。完整 `scene` 依赖它，Go 包路径与原来相同。

## 能力边界

每张图的 `Space` 有一个 Interest（管理谁看得见谁），借用宿主的 `entitysync.Manager`。它不创建、启动或停止同步管理器，不接 Nest 观察者，不负责会话或持久化。不能装一套后再对同一批成员手动重复登记视野。

## 复用方式

自带 Session / 地形的工程安装 `roost module add scenespace`，用 `NewHostedSpace` 传入已有管理器、宿主的 AOI 参数、元数据关系视图号与可见字段。宿主按自己的顺序登记 subject；成功登记（或已排队等待旧代退役）后用 `EnterRegistered`、`AttachMetadata`、`QueueMove`、`Forget`。关闭 `Space` 不关闭借来的管理器。宿主模式不创建 AI 感知用的第二份位置索引；地形落点与成员授权仍由宿主管理。需要多地图实体、场景交互与成员账本时直接安装 `scene`。

成员准入另有 `AdmitMember` / `DepartMember`：调用者传入 Scene 的事务 DAO，规则不维护第二份集合，不创建会话，也不以断线判定玩家已经离场。完整模块仍用原来的满员／关图拒绝码。

## 联动

`scene` 使用 `NewSpace`，保留公开视图与感知索引；宿主模式使用宿主原有的公开／私有字段划分。两者共享 `Space` 的同一份实现。

## 验证范围

3 个模块场景：借用并保留宿主管理器、重复进入与离开、无管理器时拒绝。完整 `scene` 的移动提交/回滚、多地图与关图另由其模块场景覆盖。
