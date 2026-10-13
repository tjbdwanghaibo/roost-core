// Package skillcompose 为组合已编译的技能 Program 构造并校验绑定权威身份的组合契约。
//
// 它只消费 skill 包导出的稳定 inspector 视图，从不执行 Runtime、也不修改 Program。调用方应当用 BuildContract
// 构造契约，只把 DeriveContractPromptView 暴露给生成器，并在编译之前校验每一个生成的候选。
package skillcompose
