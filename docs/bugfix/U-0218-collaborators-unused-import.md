# U-0218:托管服务的 collaborators 文件无条件 import 服务包,match 工程编译不过

**仓库 / 位置**:roost-codegen `internal/roost/framework_services.go`,`renderFrameworkCollaborators`。
**修复单元**:U-0218(C4:跨文件耦合——import 列表与正文各写一份,正文变了 import 没跟着变)。**定位文档**:TROUBLESHOOTING T-112。
**来源**:无 RR 编号;2026-09-16 发版 codegen v1.15.5 后对着 core v1.15.3 / kit v1.14.4 tag 不带 go.work 生成 game-demo 工程做验证时发现。已随 v1.15.6 发出。

## 问题

U-0217 删掉 match 的 `Grouping()` collaborator 之后,match 的 collaborators 正文只剩 `Metrics()`(只用 `servicemetrics`),
`renderFrameworkCollaborators` 仍无条件写入 `"github.com/tjbdwanghaibo/roost-kit/service/match"`。生成的
`internal/service/match/collaborators.go` 报 `imported and not used`,**整个业务工程编译不过**。

## 根因

import 列表按"这个服务一定用到自己的包"的假设硬写;正文是否引用它没有被检查。codegen 自己的测试只断言字符串片段、
不编译生成物,所以 v1.15.5 带着它发了出去(CI 的 framework-compat 会红,但 tag 已经在)。

## 方案

- **文本判断 `match.` 是否出现**:注释里恰好写着 `match.Grouping`,会误判为"用到了"。不采用。
- **AST 判断(采用)**:`bodyUsesPackage(body, pkg)` 把正文包成一个文件解析,找 `pkg.X` 选择表达式;没有就不 import。
  解析失败时保守保留 import,让后面的 `format.Source` 报真正的错。

## 证明

`internal/roost/collaborators_imports_promises_test.go` `TestFrameworkCollaboratorsImportOnlyWhatTheyUse`:对目录里每个托管
服务渲染 collaborators,解析后断言每个 import 都被某个选择表达式引用。修前红:
`match collaborators import "github.com/tjbdwanghaibo/roost-kit/service/match" but never use it`。修后 codegen 全套绿;
对已发布 tag 生成的 game-demo 工程不带 go.work `go build / vet / test / generate --check` 全绿,据此把 framework-compat
的 demo scenario 排入 `released`。

## 未做 / 边界

- codegen 的单测仍不编译生成物;"生成后能编译"这一层只有 CI 的 framework-compat 与这次的手工发版验证。
  想在单测里补一层,可以对渲染出的每个 Go 文件跑 `go/parser` + import 使用检查(本测试就是这个形状),但类型检查仍需真实依赖。
- 用 v1.15.5 生成过带 match 服务的工程:手删那一行 import 即可,文件是业务所有,`project sync` 不回写。
