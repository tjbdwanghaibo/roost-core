# A4 / C1（NC-192）/ A5（NC-203 残余）修前证据（2026-10-05）

基线 `bb3aa647`（origin/main），在独立检出上运行修复分支 `a4config` 的新用例，修复代码不在场。

| 文件 | 内容 | 怎么得到 |
| --- | --- | --- |
| [a4-red.txt](a4-red.txt) | `ValidateServiceConfig` 对 14 种写错类型的框架值返回 nil；生产校验要求无读取方的开关（`TestValidateServiceConfigDoesNotRequireSwitchesNothingReads`） | 把 app 包两条新用例复制成基线检出里的临时文件 `app/a4_red_test.go`，`GOWORK=off go test -count=1 ./app/ -run '…'` |
| [kit-red.txt](kit-red.txt) | remoteentity / saga / dataengine / syncbus 的 Mod Init 对写错类型的值返回 nil | 复制 `kit/strict_config_promises_test.go`，`go test ./kit/ -run TestKitModsRefuse` |
| [c1-red.txt](c1-red.txt) | 生成的 game-demo 生产示例与 k8s Secret 示例打开 `env: production` 被拒，错误是无读取方的开关 | 修复分支上 `ROOST_A4_CORE_REPLACE=<基线检出> go test ./codegen/internal/roost -run TestGeneratedConfigsPassStrictAndProductionValidation`（生成工程 replace 到基线） |
| [a5-red.txt](a5-red.txt) | 全局命令运行期间不持锁（垫片观察到 `lock=free`）；根包守卫点名三个 failover 用例与 `scripts/test-remote-generated.sh` | 把新的 `dataengine_env_test.sh`（`fail` 改为只打印）与 `acceptance_lock_promises_test.go` 复制到基线检出运行 |

修后绿与验证命令见 [NC-192 bugfix](../../RR-20261005-NC-192.md) 与 [NC-203 bugfix 的复核补修](../../RR-20261005-NC-203.md#复核后的补修2026-10-05维护者决定-a5)。
