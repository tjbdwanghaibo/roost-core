package roostcore_test

// 这条空白 import 只为让 go.mod 保留对拆分后 google.golang.org/genproto 的需求（go mod tidy 只保留
// 被 import 的模块）。原因与门禁见 workspace_module_graph_test.go：没有它，任何包含 roost-core 的
// go.work 都会从不剪枝的 gmsm v1.4.1 依赖链选到 2019 年的 genproto，与 googleapis/{api,rpc} 冲突。
// 版本与 go.mod 里 googleapis/{api,rpc} 取同一提交；升级它们时一起升级。field_mask 只是对
// google.golang.org/protobuf 的别名包，不引入新的包依赖。
import _ "google.golang.org/genproto/protobuf/field_mask"
