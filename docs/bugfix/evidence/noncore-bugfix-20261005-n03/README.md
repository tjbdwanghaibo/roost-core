# N03 修复证据（2026-10-05，revn03）

基线 `be7bcc18`；审查记录 `e81d81bc`。环境同[审查证据](../../../review/evidence/noncore-review-20261005-n03/README.md)。

| 问题 | 修前红 | 修后绿 |
| --- | --- | --- |
| [NC-90](../../RR-20261005-NC-90.md) | [包内](nc90-unit-red.txt) · [真实 NATS](nc90-real-red.txt) | [包内](nc90-unit-green.txt) · [真实 NATS](nc90-real-green.txt) |
| [NC-91](../../RR-20261005-NC-91.md) | [包内](nc91-unit-red.txt) · [真实 NATS](nc91-real-red.txt) | [包内](nc91-unit-green.txt) · [真实 NATS](nc91-real-green.txt) |
| [NC-92](../../RR-20261005-NC-92.md) | [包内](nc92-unit-red.txt) · [真实 NATS](nc92-real-red.txt) | [包内](nc92-unit-green.txt) · [真实 NATS](nc92-real-green.txt) |
| [NC-93](../../RR-20261005-NC-93.md) | [包内](nc93-unit-red.txt) · [真实 etcd](nc93-real-red.txt) | [包内](nc93-unit-green.txt) · [真实 etcd](nc93-real-green.txt) |

修复后：[NATS 探针复跑](probe-nats-after-fix.txt) · [etcd 探针复跑](probe-etcd-after-fix.txt) · [改动包 race×3](final-race.txt) · [integration race](final-integration.txt)（这次整包运行带上了既有 Toxic 用例，见本轮记录“环境事故”）。
