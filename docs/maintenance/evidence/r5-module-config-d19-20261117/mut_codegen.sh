#!/bin/zsh
# codegen 侧的变异：每条撤掉一行关键接线，跑端到端用例，应当变红。
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/r5
E2E=(-timeout 30m -run '^TestModuleConfigIsWrittenCheckedAndLoaded$' ./codegen/internal/roost/)
UNIT=(-run '^TestModuleConfigKeysReadFromTheSourceMatchTheRuntimeDeclaration$' ./codegen/internal/roost/)
# 1. 语法树读键：`_` 结尾的平铺前缀按点拼
zsh $S/mutate.sh codegen/internal/roost/module_config.go '			if prefix != "" && !strings.HasSuffix(prefix, "_") {' '			if prefix != "" {' $UNIT
# 2. 语法树读键：不读 example
zsh $S/mutate.sh codegen/internal/roost/module_config.go '	key.Example, key.Starter = tag.Lookup("example")
	if enum' '	if enum' $UNIT
# 3. 安装器不写配置段
zsh $S/mutate.sh codegen/internal/roost/module_install.go '		if err := writeModuleConfigSection(root, moduleSpecConfigHost(manifest, plan.Service, item.spec), item.spec, options.Stderr); err != nil {' '		if err := error(nil); err != nil {' $E2E
# 4. bootstrap 不给业务服务加只带配置的 Mod
zsh $S/mutate.sh codegen/internal/roost/render.go '	if configs := businessModuleConfigs(m, name); len(configs) > 0 {' '	if configs := businessModuleConfigs(m, name); len(configs) > 0 && false {' $E2E
