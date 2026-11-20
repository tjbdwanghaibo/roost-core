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

验证：`roost module verify cellworld cellspace` 检查基础模块，框架根目录 `python3 scripts/test-cellspace.py --out <新目录>` 运行纯 game 生成工程的真实进程演练。依赖和端口前提见脚本开头。
