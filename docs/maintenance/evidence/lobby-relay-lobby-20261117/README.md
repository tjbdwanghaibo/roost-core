# lobby 组证据（2026-11-17）

范围：
- `service/relay/token`：relay 令牌（提交 9e74b5a6）；
- `service/lobby`、`wiring/lobby`：大厅服；
- `docs/framework/LOBBY.md`：大厅服的现行契约。

方案见 `docs/maintenance/FEATURE-2026-11-16-lobby-relay.md` §4.2、§4.3、§4.6.4 第 7 步、§4.6.5、§4.7.1、§5。

## 环境

- 真实 Redis：`~/.roost-it` 的 `127.0.0.1:17379`（`source ~/.roost-it/roost-dataengine-it/env.sh`）。
- Redis 用例以 `ROOST_REDIS_TEST_ADDR` 准入，与仓库现有写法相同。CI 的 service-redis job 已经设了这个变量，也已经跑 `./service/...` 与 `./wiring/...`。
- 键前缀用 `redistest.Prefix`，用例结束时删掉自己写的键。跑完后在 17379 上 `--scan --pattern '*lobby*'` 为空。

## 命令与结果

```
source ~/.roost-it/roost-dataengine-it/env.sh
ROOST_REDIS_TEST_ADDR=$ROOST_DATAENGINE_IT_REDIS_ADDR go test -race -count=1 ./service/lobby/ ./wiring/lobby/ ./service/relay/token/
ok  	github.com/tjbdwanghaibo/roost-core/service/lobby
ok  	github.com/tjbdwanghaibo/roost-core/wiring/lobby
ok  	github.com/tjbdwanghaibo/roost-core/service/relay/token
go vet ./service/lobby/ ./wiring/lobby/ ./service/relay/token/      # 无输出
go test -count=1 ./wiring/                                          # ok（kit 边界、配置声明）
go test -count=1 -run 'TestCIRedis|TestOptIn|TestCoreDependencyBoundary|TestLayerViolation|TestModuleLayer|TestForbiddenCoreImport|TestCorePillar|TestCrossPillar|TestRealProcess' .   # ok
```

`service/lobby` 的每条用例都在内存和真实 Redis 两种存储上各跑一遍（子测试 `/memory`、`/redis`），两边都通过。

## §5 lobby 条目与用例

| §5 条目 | 用例 |
|---|---|
| 建房 | `TestCreateRoomPicksLeastLoadedRelayAndSignsHostToken`：选房间数最少、有对应接入的 relay，按 tcp / ws 给地址，签房主令牌，人数按游戏规则；`TestCreateRoomWithoutAnyRelayForTheTransport` |
| 鉴权 | `TestRequestsNeedAGameTokenForTheRequestedGame`：没带 token、用别的游戏的 token、游戏不在本大厅的列表里；房间按 game 隔开 |
| 满员 | `TestJoinRefusesWhenFullAndIsIdempotentForMembers`：满员拒绝；已在房间里的人重连是幂等的，房主也是；预留过期后让出席位 |
| 密码 | `TestJoinChecksThePassword`：错的拒绝；存的是哈希；成员重连不用再给密码 |
| 房主改状态 | `TestOnlyTheHostChangesStateAndStartedRoomsAreClosedToNewcomers` |
| Redis 过期 | `TestRoomsExpireWithoutRelayReports`：真实 Redis 的 TTL 是 1.5s；心跳续期；不续期就消失；迟到的心跳不让房间复活；索引项在列表时清理 |
| 列房过滤 | `TestListShowsOnlyJoinableConfirmedRoomsOfTheMode`：mode、未确认、满员、已关闭，按时间倒序 |
| `host_changed` 乱序只认更大的 epoch | `TestHostChangedAcceptsOnlyLargerEpochs`：先到 epoch 2、再到 1、再来一条同为 2 的；别的 relay 发的忽略 |
| 换 host 后令牌角色跟着变 | `TestTokenRoleFollowsTheHostAfterAHostChange`：新房主拿 host，旧房主拿 member；`/state` 只认新房主 |
| relay 报告维护席位 | `TestRelayReportsMaintainSeats`：`seq` 更大的才改名单；房主断线也保留席位；补上 relay 报告在线的人；关房后不再接受报告 |
| 离开 | `TestLeave`：成员让出席位，并向 `<subject>.relay.<实例>` 发 leave 命令；房主离开交给 relay；relay 没报告过的房间，房主一走就关房 |
| HTTP / CORS | `TestHTTPRoomLifecycle`、`TestHTTPRefusesMissingOrForeignTokensAndBadBodies`、`TestHTTPCORSAllowsOnlyConfiguredOrigins` |
| etcd 广播 | `TestEtcdRelaysReadsTheRegistryAndSkipsBadEntries` |
| 接线 | `TestModAndServerOnRealRedis`：真实 Redis，从 Mod.Init / Provide 到 Server 的 HTTP 与 NATS 订阅、退订；`TestAccountTokensMapsAccountAnswers`；`TestModLoadsWhatItDeclares`；`TestModInitRefusesMissingGamesAndKey` |
| relay 令牌 | `service/relay/token` 的四条：往返、过期、篡改各段、别的密钥、发给了别的实例、短密钥、读环境变量 |

## 变异

每处变异都去掉或改坏关键的一行，跑对应的用例，再恢复。
- 输出：`token-mutations.txt`（2 处）、`lobby-mutations.txt`（9 处）。
- 脚本：放在 scratchpad 的 `b1117-lr-lobby/mutate.py`，不进仓库。
- 结果：11 处全部变红，恢复后全绿。

| 变异 | 变红的用例 |
|---|---|
| 令牌不核对实例 | `TestVerifyRefusesTamperedForeignAndMisdirectedTokens` |
| 令牌不看过期 | `TestVerifyRefusesExpiredTokens` |
| M1 epoch 用 `!=` 代替 `>` | `TestHostChangedAcceptsOnlyLargerEpochs`（memory 与 redis） |
| M2 令牌一律签 member | `TestCreateRoom…`、`TestTokenRoleFollows…` |
| M3 去掉满员检查 | `TestJoinRefusesWhenFull…` |
| M4 去掉密码检查 | `TestJoinChecksThePassword` |
| M5 去掉 /state 的房主检查 | `TestOnlyTheHostChangesState…`、`TestTokenRoleFollows…` |
| M6 列表不要求 relay 已报告 | `TestListShowsOnlyJoinable…` |
| M7 relay 报告不写入（不续 TTL） | `TestRoomsExpireWithoutRelayReports`（memory 与 redis） |
| M8 Redis 记录不带 TTL | `TestRoomsExpireWithoutRelayReports/redis` |
| M9 迟到的旧报告也改名单 | `TestRelayReportsMaintainSeats` |

## 没有做的

- 生成器：`frameworkCatalog`、`roost add lobby`、doctor 检查 `lobby.token_key_env` 与 `relay.token_key_env` 是否一致，都归生成器组。
- `wiring/config_schema_test.go` 的 `everyKitMod`：那个文件不归本组，没有加入 `new(lobby.Mod)`。同样的检查在 `wiring/lobby` 的 `TestModLoadsWhatItDeclares` 里做了一遍。
- `docs/framework/README.md` 的索引：没有改，建议由协调者加一行指向 `LOBBY.md`。
- 真实 NATS / etcd 的进程演练：接线用例里 NATS 与 etcd 是替身，Redis 是真实的。真实进程演练按方案 §5 放在最后统一做。

## 建议给 CHANGELOG 的文字（由协调者统一写）

- **大厅服 lobby（新增框架服务，方案 FEATURE-2026-11-16-lobby-relay §4.2）**：
  - 新增 `service/lobby` 与 `wiring/lobby`，HTTP + JSON。接口：`POST /rooms`、`GET /rooms?game=&mode=`、`POST /rooms/{id}/join|leave|state`，返回 `{room, relay_addr, token, expires_at}`。
  - 鉴权：凭 account 的游戏账号 token（`ValidateGameToken` + `RequireGame`）。
  - 房间目录：放在 Redis，带 TTL，由 relay 的报告续期，按 `game/mode` 建索引。
  - 选 relay：在 etcd 广播的实例里选房间数最少的（键 `<relay_registry_prefix><实例>`）。
  - 与 relay 之间：NATS 主题 `<subject>.events` 收房间事件，`<subject>.relay.<实例>` 发 leave 命令。`host_changed` 按 epoch 做 CAS；令牌角色按当前房主填。
  - CORS 可配置。
  - 契约见 `docs/framework/LOBBY.md`。
- **relay 令牌 `service/relay/token`**：`rt1.<过期秒>.<实例>.<房间号>.<账号>.<host|member>.<最大人数>.<签名>`，HMAC-SHA256，提供 `Sign`、`Verify`、`KeyFromEnv`。比方案 §4.3 多了“最大人数”一段（§4.4）。

## 建议给 KNOWN-LIMITS 的文字（框架限制，不是遗留问题）

- lobby 选 relay 依据 etcd 里的房间数，这个数由 relay 定时更新，有延迟；多个 lobby 实例同时建房，可能都选中同一个实例。
- 一个 `lobby.subject` 只能对应一份房间目录（一个 `key_prefix`）。共用 relay 的几个 lobby 服务必须共用 `key_prefix` 与 `subject`。
- 过期房间留在 game/mode 索引里的项只在列表时清理。
