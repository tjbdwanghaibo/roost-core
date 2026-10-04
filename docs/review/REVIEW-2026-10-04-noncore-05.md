# N02 三项修复与原触发验收

2026-10-04，远端 fetch/pull --ff-only 无新增提交，起点 `c4aa1e7dd69bbf795fee878b1fb68e0bba11525a`、干净 main；技能正本与本机资源包无差异。按用户“修复，并继续review”处理上轮三个 P2，不改其他 agent 的 Nest/Sync/DataEngine。

| RR | 结果/兼容 | 正式证据 |
| --- | --- | --- |
| [NC-05](../bugfix/RR-20261004-NC-05.md) | 超额需求在 key/idle/扫描副作用前拒绝；nil/非正数/tokens保留 | 4红→绿、2控制 |
| [NC-06](../bugfix/RR-20261004-NC-06.md) | 固定 ErrEndpointPanic，report及失败日志 panic 隔离；同步/阻塞边界不变 | 3红→绿、3控制，加1诊断失败控制 |
| [NC-07](../bugfix/RR-20261004-NC-07.md) | 生成/运行期共用 ValidatePath；失败不占seen，保留旧基础路径错误文本 | 7红→绿、10合法模式控制 |

29个原正式项为14fail/15pass，修后29全绿，正式最终30项。原 review overlay34项全绿（其中1观察）；正式CLI四个独立工程的13消费者叶子全绿，另三个坏路径在生成阶段直接拒绝；旧生成物也能编译后返回普通启动 error。[原始反例、退出、复跑与源码摘要](../bugfix/evidence/noncore-bugfix-20261004-03/README.md)。

受影响 security/gateway/webroute/httpserver/httpclient + 全 Codegen，最终20个有测试包 race通过、754 test pass事件、9个原测试skip，vet通过。首次回归发现 Codegen基础路径错误文本变化，已恢复旧文本并定向重跑；Windows无sh的部署脚本静态检查原失败保留并在该包支持范围重跑时具名排除。15个no-test包不计为验证。没有把支持范围通过写成Linux/外部依赖验收，没有重复已通过无关包。

Tier2 Verify，图谱roost-core ready/root核对，generation=`2026-09-30T11:58:14Z`。AllowN/Recover/Register/parseRoute搜索与双向depth1/片段核对，候选coverage无记录gap但metadata_changed，新测试not_tracked；现行代码、diff与编译补证。同名heuristic错边不作调用证据，新ValidatePath不凭旧图谱推断全部caller；未重建/中断共享索引。

[请求机制学习](IMPLEMENTATION-REQUEST-ADMISSION-AND-GENERATED-WEBROUTES.md)已追加实际修复；N02场景仍部分完成，容量/真实业务鉴权/非协作回调/语义重复路由另列。新审查接N03，结果另记，三项关闭不等于整域或框架全收敛。未发版/tag/部署。
