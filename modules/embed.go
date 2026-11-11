// Package modules 携带 `roost module add` 安装的业务模块源码。
//
// 每个模块一个目录：module.yaml 是清单（依赖、挂在哪个实体上、往主 DAO 追加哪些状态字段、
// 需要分配哪些 ID、验收场景数），files/ 下是模板，按安装后的真实路径存放、多一个 .tmpl 后缀；
// with/<另一个模块>/ 下是联动文件，两个模块都装着时才写进工程。
// 模板里的占位符：{{MODULE}}（工程的 Go module）、{{PROJECT}}（工程名）、{{GAME_SERVICE}}（业务服务名），
// 以及 {{ID:<kind>:<name>}}（安装时从 roost.yaml 的 ID 空间分配的号，kind 为 protocol / errcode / component）。
//
// .tmpl 后缀让这些文件不参与本模块的编译：带占位符，安装进工程之前不是合法的 Go。
// 它们由 codegen/internal/roost 的模块测试安装进生成工程后编译并运行。
//
// 模块与生成器的边界：生成器（roost generate / roost add）只产出框架骨架；模块是业务代码，
// 安装后归工程所有，project sync / upgrade 不改写。说明见 modules/README.md。
package modules

import "embed"

// Files 是全部模块的清单与模板，键是 <模块>/module.yaml、<模块>/files/<工程内路径>.tmpl
// 与 <模块>/with/<另一个模块>/<工程内路径>.tmpl。
//
//go:embed */module.yaml */files */with
var Files embed.FS
