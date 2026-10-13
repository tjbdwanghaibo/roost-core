// Package skill 是 roost 技能的公开编译器与确定性运行时 API。
//
// 技能定义使用单独版本化的 wire 契约：JSON schema 是 roost.skill/v2，编译器语义修订是 skillv2-compiler-2，
// 两者都不是 Go 包版本。应用导入 github.com/tjbdwanghaibo/roost-core/gameplay/skill，并持久化包给出的 wire 与
// 编译器身份，而不是从导入路径推导。
//
// 支持的生命周期是 Parse、Compile、查看不可变的 Program，再经 Runtime 针对一个 Host 实现执行。生产 Host 必须遵守
// Host 与 Runtime 上写明的串行化、不可重入、有界工作、权威身份与确定性契约。
package skill
