#!/bin/zsh
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/r5
zsh $S/mutate.sh wiring/modulehost/mod.go "	return m.loadConfigs(cfg)" "	return nil" ./wiring/modulehost/
zsh $S/mutate.sh wiring/modulehost/config.go "		if err := module.Config.check(cfg); err != nil {" "		if err := error(nil); err != nil {" ./wiring/modulehost/
zsh $S/mutate.sh wiring/modulehost/config.go "			schemas = append(schemas, module.Config.schema)" "			_ = module.Config.schema" ./wiring/modulehost/
zsh $S/mutate.sh wiring/modulehost/config.go "		use(value)" "		_ = value" ./wiring/modulehost/
