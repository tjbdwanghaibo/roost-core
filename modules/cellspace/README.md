# cellspace：可选的两格分区玩家装配

新工程的推荐入口：

```sh
roost project new explorer -module example.com/explorer -template game -recipe cellworld
```

配方将 `enter`、`cellspace`、`login`、`portal` 以及 `cellworld` 依赖组合成普通 game 工程，并补齐 Player DAO、Base 接口、服务生命周期和登录后的放置动作；不需要复制 game-demo 的 `partition.go`。**单独安装 `cellspace` 不等于完整配方**：已有工程的 Player 和宿主服务由应用拥有，安装器不擅自覆盖。旧版 `cellworld` 若仍使用 `SceneActor`，先按 [升级说明](../cellworld/README.md#从旧名-sceneactor-升级) 在离线副本迁移，不能同时创建两类化身。

| 归属 | 提供什么 |
|---|---|
| `cellworld` | `CellPlayer`、稳定身份转换、玩家工厂、授权、交接、重建和快照映射 |
| `cellspace` | 一个无集合的 `CellRegion` 区域、唯一的玩家 Base 心跳、`CellMove` 速度意图和两格的进程装配 |
| 新建配方 | 接入标准模块的登录/建角与自身同步、为服务登记进场步骤、关闭前排空 Node |

不开 `cellspace.enabled` 就不创建区域或格内玩家。示例左/右格默认 sid 1001/1002；Base 可使用网格之外的 sid，Cell 需要 App singleton。客户端看自身同步中的位置，不能把接纳一条意图当作已移动。这不是完整多人 AOI/战斗样例：默认不装 NPC、战斗、掉落、地形和邻近镜像。完整组合见 game-demo。

纯 game 配方使用嵌入 TCP 的已认证 SessionID、物理连接 nonce 和服务端接入序号。`EnterGame` 进场成功才接管 `CellMove`；同一连接重复进入、短断重连、新连接接替，都保留同一化身与 Entry。接替不继承旧速度。当前连接断开会立即投递零速度，`cellspace.reconnect_grace` 默认 `30s`（`0s` 立即离场）；超期由已有 Base 心跳执行正常 Leave，下次进场分配新 Entry。通知丢失也由该心跳检查本地连接表补偿。独立 Gate 的接入序号不是此配方的进程内序号，不以客户端自报代际替代。

`CellMoveResponse.Code/Reason` 表示 Base 准入结果，`cellspace_disabled`、`cellspace_not_entered`、`cellspace_invalid_velocity`（任一分量不在闭区间 [-1000,1000]）、`cellspace_stale_session` 对应 `ids.errcode` 中的项目业务码；超时等内部错误不报成功。请求格式未变，不要求客户端发送连接号。

验证：`roost module verify cellworld cellspace` 检查基础模块，框架根目录 `python3 scripts/test-cellspace.py --out <新目录>` 运行纯 game 生成工程的真实进程演练。依赖和端口前提见脚本开头。
