# RV-20261118-08：跨域白名单写明默认端口（2026-11-18）

审查来源：`fix/core-game-server-review` 分支的 `88d55951`（`docs/maintenance/MAIN-REVIEW-20261118.md` 第 08 项，只读，没有合并那条分支）。基线 main `8a2cf3a9`。

## 根因

`httpserver.CheckCORSOrigins` 只要求“重新拼出的 `scheme://host` 与原文逐字节相同”，`https://play.example.com:443` 能过。浏览器序列化 Origin 时省掉默认端口，发的是 `https://play.example.com`，所以预检永远 403。

同类问题：lobby 的 `lobby.cors_origins` 根本不检查写法（`wiring/lobby`），结尾斜杠、大写主机、默认端口都会让大厅照常启动，而页面的预检全部 403。

## 修复

- [cors.go](../../../../infra/network/httpserver/cors.go)：https 写了 443、http 写了 80 时拒绝，错误里给出该写的值。选择拒绝而不是悄悄改写：配置里写的就是比较时用的值，与已有的“结尾斜杠拒绝”一致。非默认端口（含 `https://…:80`、`http://…:443`）照常接受。
- [lobby_mod.go](../../../../wiring/lobby/lobby_mod.go)：lobby 配置加 `ValidateConfig`，用同一个 `CheckCORSOrigins` 检查 `lobby.cors_origins`（加载与 `--check-config` 都会查）。

## 先红后绿

- 红（`go test ./infra/network/httpserver/ -run TestCheckCORSOriginsRefusesWhatABrowserNeverSends`）：

  ```
  cors_test.go:114: "https://play.example.com:443": err = <nil>, want refused naming the key and suggesting "https://play.example.com"
  ```

- 红（`go test ./wiring/lobby/ -run TestModInitRefusesMissingGamesAndKey`）：

  ```
  lobby_mod_test.go:97: cors origin "https://play.example.com/": Init = <nil>; want a refusal naming lobby.cors_origins
  ```

- 绿：`go test -race ./infra/network/httpserver/ ./wiring/lobby/` 通过。审查探针 `TestReviewPortalDefaultPort`（临时放进包内）两个子用例都通过：显式 443 的配置被拒绝（探针认可这种处理）。

## 变异

| 变异 | 结果 |
| --- | --- |
| 去掉默认端口判断 | `TestCheckCORSOriginsRefusesWhatABrowserNeverSends` 红 |
| lobby `ValidateConfig` 直接返回 nil | `TestModInitRefusesMissingGamesAndKey` 红 |

## 升级

- 已有配置里写了默认端口的 `portal.cors_origins` / `lobby.cors_origins` 启动会失败，按错误提示去掉端口。
- lobby 以前接受的不规范写法（结尾斜杠、大写主机、带路径）现在启动失败；这些写法以前本来就不生效。
