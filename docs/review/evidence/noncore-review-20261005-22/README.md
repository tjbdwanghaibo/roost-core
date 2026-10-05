# N04 接续复跑与证据

基线 `c3aa0edd`，2026-10-05。Tier2图谱导航+当前源码+正式生成消费。[运行报告](../../REVIEW-2026-10-05-noncore-22.md) · [NC-32红绿/完整本地检查](../../../bugfix/evidence/noncore-bugfix-20261005-12/README.md) · [来源摘要](source-hashes.csv) · [图谱覆盖](coverage.json)。

## 28个消费叶子

正式 DAO CLI，独立 module replace 到本机当前源码，真实Nest/Repository/MigrationRunner/文件WAL/Projector/MongoStore，后端为mongotest。[原始事件](consumer.jsonl)、[退出码](consumer-exits.json) 和无输出vet日志。17沿用上轮 + 6深层提交 + 4嵌套迁移 + 1持续竞争 = 28，不重复算作28个新场景。

- [定义](consumer-definition.go.txt) 加指针/值父结构，内部leaf/map/slice子对象，schema3。
- [原11消费](consumer-test.go.txt)、[上轮6聚合/竞争/进程恢复](aggregate-test.go.txt)保持正式链；setup新增可选ProjectionStore只给本轮竞争夹具使用。
- [六个深层提交](nested-test.go.txt)：恢复后在真实Nest事务内修改，记录数必须1，覆盖指针和值父入口；修前对应0。
- [四个迁移消费](nested-migration-test.go.txt)：两步深层string→int64成功、坏目标类型、坏源字符串、null子项；成功后的下一业务事务实际投影后由新Manager无迁移器重载。用Decoder.DefaultDocumentM处理本夹具的递归文档，非产品解码更改。
- [持续竞争](contention-test.go.txt)：每次迁移投影前用正式Store.Project CAS写竞争者，连续两次淘汰。第三视图返回预算冲突且不发布；再次Load迁移成功。Stats检查在Flush barrier之后，避免票完成与计数刷新的窗口误报；不靠sleep/fake迁移成功。

```powershell
./docs/review/evidence/noncore-review-20261005-22/Run-Consumer.ps1 -GoExecutable $GoExecutable -OutputRoot D:/whb_s/.tmp/n04-review22-new
```

输出目录须全新，脚本保留CLI、生成源码、json事件与退出码，不污染业务仓库。框架类消费统一race/count1，生成/编译失败或全包timeout不能充当产品红。

## 其他验证与进度边界

[Run-Golden.ps1](Run-Golden.ps1)执行canonical金样runtime：正式9红→绿，完整53pass；[Run-SyncModes.ps1](Run-SyncModes.ps1)与 `scripts/test-sync-modes-generated.sh` 同步骤，两模式均pass，退出见[sync-exits](sync-exits.json)。全仓编译、988相关包叶子、根包14、vet/glsvet与环境重跑过程详见bugfix证据，11个实际skip保留，历史外部skip不关闭。

coverage metadata generation为09-30，相关路径metadata_changed，新正式回归not_tracked；未把ready冒称索引已到最新HEAD。精确当前源码已核对，hash记录修改身份，graph只导航。没有完整图谱/全仓review覆盖声明。

N04本机迁移具名缺口已补，功能域整体仍部分完成；外部Mongo/Cluster/HA/长期容量及大嵌套树性能另列，下一N05路由/mirror增量。未发版、不等GitHub CI。
