# portal 的浏览器跨域调用（portal.cors_origins）实施证据（2026-11-18）

起因：游戏页面跑在第三方外壳网页里时，浏览器跨域调用登录入口（`/portal/*`），入口原来不答 CORS，预检（`OPTIONS`）没有路由。只做通用的 Origin 白名单；具体平台的登录渠道不在 roost-core 里（维护者 2026-11-18：平台渠道的代码放在使用它的服务端模板仓库）。

## 做了什么

- `infra/network/httpserver`
  - 新文件 `cors.go`：`CORS`（`NewCORS(origins, methods, headers)`、`Allows`、`Handle`、`Preflight`）与 `CheckCORSOrigins(key, origins)`。规则：
    - 名单里的 Origin（逐字节相同）：`Access-Control-Allow-Origin` 回写这个 Origin；预检答 204，带 `Access-Control-Allow-Methods`、`Access-Control-Allow-Headers`、`Access-Control-Max-Age: 600`。
    - 不在名单里：没有 CORS 头，预检答 403；请求本身照常处理（白名单不是访问控制）。
    - 每个经过它的响应都加 `Vary: Origin`。从不发 `Access-Control-Allow-Credentials`。
    - `"*"` 放行任何 Origin，但空的与 `"null"` 永远不放行。
    - `CheckCORSOrigins`：每项是 `"*"`，或 `scheme://host[:port]`（http / https，主机小写），重新拼出的 `scheme://host` 必须与原文逐字节相同——带路径、结尾 `/`、查询、片段、用户信息、大写的写法都拒绝（浏览器发的 Origin 永远对不上）。
  - `RouteRegistrar` 加 `Options(path, handler)`，`Engine` / `Group` 实现它（仓库里没有别的实现者，`go build ./...` 通过）。
- `infra/network/webroute`：路由登记接受 `OPTIONS`（注释写明只给 CORS 预检用），转给 `Options`。
- `modules/portal`
  - `portal_config.go.tmpl`：`Config.Portal.CORSOrigins`（`config:"cors_origins"`，无缺省、无 example，安装器不写段）；`ValidateConfig` 调 `httpserver.CheckCORSOrigins("portal.cors_origins", …)`，所以 `--check-config` / `roost config check --service account` 就报写错的 Origin。
  - `portal.go.tmpl`：`Options.CORSOrigins`；`New` 再检查一次并构造 `httpserver.NewCORS(origins, [POST], [Content-Type])`；四个 `POST` 路由经 `cors.Handle` 包一层，并各登记一条 `OPTIONS` → `cors.Preflight`；`Start` 把配置传进去。
  - 新场景 `TestPortalAnswersCrossOriginCallsOnlyFromConfiguredOrigins`（16 条，原 15）：名单里的 Origin 四个端点预检 204、响应带 Allow-Origin；别的 Origin 预检 403、响应不带；不写时不允许跨域；`null` 与写法不对的 Origin 在 `ValidateConfig` 与 `New` 都被拒。
  - `module.yaml` 的 `pass` 15 → 16；`README.md`：配置项、能力边界、“浏览器跨域调用”一节、配置表、验证范围。
- `codegen/internal/roost/module_config_test.go`：portal 声明的键多了 `portal.cors_origins`。
- `scripts/test-modules-generated.sh`：portal 的场景数 16（两处）。

## 先红：变异

每条变异把一处规则改回去（或删掉），跑对应用例，必须变红；跑完源码还原。

- `mutations-infra.txt`（`go test ./infra/network/httpserver/ ./infra/network/webroute/`）：7/7 红——webroute 拒绝 `OPTIONS`（即改动前的行为）、任何 Origin 都放行、预检对所有人答 204、不加 `Vary: Origin`、`"*"` 下放行 `null`、Origin 不与 `scheme://host` 比较（带路径 / 斜杠被接受）、接受大写主机。
- `mutations-module.txt`（生成工程里 `go test ./services/account/portal/`）：4/4 红——没有 `OPTIONS` 路由、`POST` 没包 CORS、`cors_origins` 没从配置传进来、`ValidateConfig` 不查 `cors_origins`。

## 后绿

- `GOWORK=off go vet ./infra/network/httpserver/ ./infra/network/webroute/`；`GOWORK=off go test -race -count=1 ./infra/network/httpserver/ ./infra/network/webroute/`：ok。
- `GOWORK=off go build ./...`：ok。
- `GOWORK=off go test -run '^TestModuleConfigIsWrittenCheckedAndLoaded$' ./codegen/internal/roost/`：ok（234 秒）。
- 新生成的 `-template lobby` 工程（CLI 取自本分支、依赖 replace 到本分支）`roost module add portal` 后 `roost module verify --json`：`green-module-verify.json`，account_auth 7/7、portal 16/16，build / vet / id check / generate --check 通过。
- 新生成的 `-template game` 工程装 `login portal`：`roost module verify` 为 account_auth 7、login 34、portal 16、portal+login 1，全部通过。
- 按维护者要求没有跑 `scripts/test-modules-generated.sh`、codegen 整包与全仓测试。

## 没有验证

- 真实浏览器的跨域请求（只验证了响应头与状态码）；真实进程经反向代理对外。
- 尚未确认：`portal.cors_origins` 写 `"*"` 是否应在生产配置里拒绝（现在接受，与大厅的 `lobby.cors_origins` 一致）。

## 顺带看到的（未改）

- `modules/portal/README.md` 配置表里 `portal.services` 一行写着“与 `portal.servers` 一样不在 App 的配置声明里，写错了启动到入口这一步才报错”，与同文件能力边界一节“`portal.servers` / `portal.services` 在 account 进程的配置声明里……写错了在检查时就报”矛盾；按模板代码，后者是对的。本轮没改那一行，留给维护者确认。

## 建议写进 CHANGELOG 的文字（`[Unreleased]`，由维护者合入）

> - portal：新增 `portal.cors_origins`（account 的配置），按 Origin 白名单答浏览器的跨域调用：名单里的 Origin 四个端点预检 204、响应带 `Access-Control-Allow-Origin`，别的 Origin 预检 403；每个响应带 `Vary: Origin`，不发 `Access-Control-Allow-Credentials`，`null` 永不放行；带路径、结尾斜杠、大写主机的写法在配置检查时拒绝。不写时与以前相同（不允许跨域）。portal 场景 15 → 16。
> - httpserver：新增 `CORS` / `CheckCORSOrigins`；`RouteRegistrar` 多了 `Options`（自己实现 `RouteRegistrar` 的代码要补这个方法）。webroute 接受 `OPTIONS` 路由（只给 CORS 预检用）。
> - 已安装 portal 的工程要用新行为：照模块模板更新 `services/account/portal/portal.go`、`portal_config.go`、`portal_test.go`（有没有一条命令能升级已装的模块，本轮没有核对，尚未确认）。

## 建议写进 KNOWN-LIMITS 的文字（“portal 的边界”，由维护者合入）

> - 跨域白名单不是访问控制：不在 `portal.cors_origins` 里的 Origin 拿不到 CORS 头，但请求照常处理；非浏览器客户端不受约束。限流与拒绝来源放在反向代理上。
