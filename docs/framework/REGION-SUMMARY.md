# 普通地图的可选区域层：公开对象或计数

同一远区互斥选择 `Detail`（逐个公开地标）或 `Count`（每个非空矩形一个数）；`Off` 撤两种来源。原近景、自身与队伍来源不动。默认 `scene.Config.RegionSummary == nil`，无额外视图、对象、索引或订阅。不改 hosted game-demo、Cell/Ghost、地形、物理或 c4。

## 在真实地图上打开

已安装 `scene` 的工程：`scene/service.Manager.Open → Scene → NewSpace`。`actor.Body` 经 `movement.MapBasic` 走原有提交后的进场、移动与离场；`Space` 复用同一 Interest/Manager 向 `gameplay/regionsummary` 投影。`publicLandmarkID` 是业务已登记的公开地标。

```go
cfg := scene.DefaultConfig() // 半开世界 [0,1000) × [0,1000)
cfg.RegionSummary = &regionsummary.Config{
    View: 20, DetailView: 21, DetailProfile: scene.Public,
    Public: func(id int64) bool { return id == publicLandmarkID },
    Areas: []regionsummary.Area{
        {SubjectID: 8001, Bounds: spatial.Rect{Max: spatial.Point{X: 500, Y: 1000}}},
        {SubjectID: 8002, Bounds: spatial.Rect{Min: spatial.Point{X: 500}, Max: spatial.Point{X: 1000, Y: 1000}}},
    },
}
area, err := sceneManager.Open(2, cfg)
if err != nil { return err }
// 已有认证、业务准入和地图授权之后；observer 是玩家唯一号，不是完整实体 ID。
space := area.Space()
if err := space.SetRegionMode(observer, regionsummary.Detail); err != nil { return err }
// 拉远：撤区域详情来源，挂同一片区域的计数来源；不放大原来的 AOI 半径。
if err := space.SetRegionMode(observer, regionsummary.Count); err != nil { return err }
// 回详情用 Detail；关闭区域层回普通近景用 Off。
return space.SetRegionMode(observer, regionsummary.Off)
```

- 冻结 1–64 个图内、不相交的半开矩形和 `Public` 允许条件。业务必须允许对所有获准成员公开入选源的**身份、详情和数量**。`DetailProfile` 应由每种入选源的 packer 执行真实白名单（Player 可用 `OnSceneFields`），拒绝 default/self/组合键。需要战争迷雾、动态阵营或只能知道总数时，不启用此方案。
- 两个非零 ViewID 由本图专用，统计 SubjectID 在整个 Manager（含分线）中预留。按业务授权，不按客户端 LOD 授权。无主视野拒绝；目标视图满时保留原模式。离图解除、再进图重新授权。关闭投影后再关 Interest，等待旧 Remove/退役后才能复用 marker ID。
- 普通 `Space.Nearby` 排除**仅**由专用层带来的对象。`OrdinarySeen` 与本地图切换、Close 串行：宿主显式 Close 后为空；宿主破坏所有权契约（提前移除或同号重定义专用视图、提前关 Interest）使投影失败关闭时，退回整张 Interest 的 `Seen`，附近聊天 / AI 候选不因投影停用而整图变空。通用 `SeenWithoutViews` 要求调用方保证查询期间被排除的视图不被移除或同号重定义。

## 客户端消费与一致性边界

沿用 entitysync 解码器。`region_count` / `default` / codec 1 的 payload 是标准 protobuf `UInt64Value`，Full、Delta 都**替换**为绝对数；Remove 删除图标，新会话 Full 清旧镜像。源 ID、位置和私有字段不上摘要线，客户端使用同一份公开 marker ID → 矩形表。最后一个源退出发 Remove；区域内移动不标脏。无新帧或 UI。

容量、快照、拆帧预算仍生效，不保证跨区域或跨帧原子显示。可用 `ManagerConfig.Namespaces["region_count"]` 单列预算。主 AOI 或其它来源仍持有的详情不会因为切换而节省。

## 固定样例与轻型验证

`go test -p=2 -race -count=3 -v ./gameplay/regionsummary`。固定 1 个观察者、1 个近处对象、两区各 32 个远处公开地标和 1 个未允许对象；详情载荷为一个标量。Transport 计完整 entitysync 帧（不含 TCP/TLS/Gate）；Manager.AuditStats 与 Decoder 的实际镜像核对。

| 同一远区的表示 | 首屏字节 | 帧 | 实际订阅数 |
| --- | ---: | ---: | ---: |
| 地图未配置 | 39 | 1 | 1 |
| 地图已配置、观察者未打开 | 39 | 1 | 1 |
| 专用 Detail | 2,955 | 1 | 65 |
| 专用 Count | 109 | 1 | 3 |

前两组帧逐字节相等；后两组是不同信息量的表示，不是无损压缩。另一个含自身/近景/队伍重叠的同会话 Detail→Count 用了 112 B，独占远区个体消失，切回后无 marker 残留。内部移动、私有移动、满额拒绝各无额外帧；另验跨区、删除、新会话基线、并发关闭查询。

从本检出构建 `roost`，`project new … -template game --skip-deps`、`module add scenespace`，go.mod replace 到本检出。安装工程运行 `go test -p=2 -race -count=3 -run '^TestSceneSpace' ./game/modules/scene`；4 项均通过，含真实 `Enter/QueueMove/Forget/SetRegionMode` 与解码器，以及移动的提交后发布晚于 `Forget` 时不把已离开对象重新计进区域层（两者分属不同实体的提交后回调，互不保序）。未跑千人负载、TCP/Gate、引擎 UI，不外推线上容量。
