# portal：登录时顺带进服（2026-11-18）

维护者 2026-11-18：登录 → 清单 → 进服三步里，清单可以跳过，进服这一步也可以并进登录。`/portal/login` 新增可选的 `enter`，说明见 [portal README](../../../../modules/portal/README.md) 的 “POST /portal/login”。

基线：main `59e2d352`。验证在临时生成的 game 工程里做（`roost project new -template game` + `roost module add login portal`，依赖 replace 到本检出）。

## 先红后绿

- **红**：新场景 `TestPortalLoginCanEnterInTheSameRequest` 配旧的 `portal.go`：

  ```
  --- FAIL: TestPortalLoginCanEnterInTheSameRequest
      alice into 1000: 200 {"ok":true,"account_ticket":"at2.…","account_ticket_expires_at":1800000600}
          ; want server 1000 at 203.0.113.7:7000
  ```

- **绿**：新代码下 `go vet` 与 `go test -race ./services/account/portal/` 全部通过（包内含与 login 的联动用例）。

## 变异

逐一改坏新代码，跑相关场景：

| 变异 | 结果 |
| --- | --- |
| M1 去掉 `enter` 的预检（负服号、服号与 service 同写） | 红：请求到了 account 的 `Login` |
| M2 不写服号时不算推荐 | 红 |
| M3 进服失败时吞掉错误、照样登录成功 | 红 |
| M4 没有推荐时整个登录失败 | 红 |
| M5 进推荐的服时忽略 `service` | 先是绿（没有覆盖），在 `TestPortalListsServersByService` 补了按服务推荐的断言后变红 |

## 场景数

portal 场景 16 → 17：`modules/portal/module.yaml` 的 `verify.pass`，以及 `scripts/test-modules-generated.sh` 与 `codegen/internal/roost/module_portal_test.go` 里写死的数同步改（第一次跑生成器用例时漏了 module.yaml，verify 报 Want 16 / Passed 17，补上后重跑）。

## 没有验证

- 真实进程（HTTP 端口、总线、Redis）下的登录顺带进服；场景是离线的，与 portal 其余场景一样。
