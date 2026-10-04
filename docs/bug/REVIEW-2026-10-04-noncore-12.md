# N04 第二批：RefHMap 类型、Patch、存储名称与测试替身分页

**后续状态（2026-10-04）：NC-16～20五项已修、声明场景验证，未发版。** [16](../bugfix/RR-20261004-NC-16.md) · [17](../bugfix/RR-20261004-NC-17.md) · [18](../bugfix/RR-20261004-NC-18.md) · [19](../bugfix/RR-20261004-NC-19.md) · [20](../bugfix/RR-20261004-NC-20.md) · [红绿/生成消费](../bugfix/evidence/noncore-bugfix-20261004-07/README.md)。以下原失败与未修结论保留历史时点。

2026-10-04，源码基线main`1502f97372139a238e73850ff7337b9454146f9e`；同一工作树先修NC-13～15，**本页五个新RR已确认、未修**。RefHMap/mongotest产品未修改。[运行/范围](../review/REVIEW-2026-10-04-noncore-12.md) · [真实Redis/分页日志与复跑](../review/evidence/noncore-review-20261004-12/README.md) · [机制与实施交接](../review/IMPLEMENTATION-REFHMAP-LAYOUT-PATCH-AND-REDIS-LIFETIME.md)。

真实Redis八项RefHMap场景5fail/3控制；mongotest八项分页5fail/3控制，十个失败叶子归为五根因，不当十bug。原缓存六项和Redis七项全部通过，是修复验证，不新增RR。

| RR | 等级 | 确认影响 |
| --- | --- | --- |
| NC-16 | P2 | 被layout接受且Set成功的指针根V，在Get类型断言panic |
| NC-17 | P2 | 指针receiver TextMarshaler被识别却未调用，Set成功写入错误编码，Get解码失败 |
| NC-18 | P2 | 已存在记录的nil嵌套指针Patch返回nil，Get仍为nil，写入不可见 |
| NC-19 | P2 | 接受的字段名称与内部root/__keys名称相撞，返回成功但数据改变 |
| NC-20 | P3 | 公开mongotest分页与正式driver的FindOption不一致，测试证据失真 |

## RR-20261004-NC-16

[ref_hmap.go](../../cache/ref_hmap.go):180–219的layout明确接受单层指针根并剥为struct；:119返回`value.Interface().(V)`却永远是struct。实际`RedisRefHMapStore[int64,*refHMapSession]`Set成功后Get：`interface conversion: interface {} is cache.refHMapSession, not *cache.refHMapSession`。普通struct根同数据控制通过。

这是已接受类型的读取panic，不是要求支持任意interface/双层指针，也未断言当前codegen默认生成指针根。建议保留原V形状重建/返回，或明确在写入前拒绝不支持形状；优先正常支持既有单层指针。验收struct/*struct、miss、nil指针输入合法错误、无panic、调用方input别名。当前只确认nonnull pointer roundtrip，nil-input/双层指针仍需补证。

## RR-20261004-NC-17

[ref_hmap.go](../../cache/ref_hmap.go):796–816用*typ的MarshalText识别scalar，却只在value.CanAddr时调用指针方法。Set普通struct值的reflect.ValueOf不可寻址，定义在*自定义int上的编码被跳过，退化为strconv；Get却调用*typ.UnmarshalText。

真实Redis：自定义*Amount MarshalText编码`code=7`、UnmarshalText要求该格式，Set返回nil，Get报`input does not match format`；同数据value-receiver MarshalText控制通过。错误在框架编码，不由Redis模拟解码。本例是正常单层指针receiver，不要求双层指针支持。

建议为不可寻址值建立安全可寻址副本，再调用已有TextMarshaler，避免意外改业务输入；同时核对Patch的encodeRefHMapScalarForType共享路径。验收Set/Patch、value/pointer receiver、primitive/struct文本类型、marshal失败写前拒绝、input隔离。没有自动重编码历史数据，迁移/旧字节兼容需显式方案。

## RR-20261004-NC-18

[ref_hmap.go](../../cache/ref_hmap.go):233–263只HSET目标hash；:612–643的decode需要parent hash对应字段的引用。Set记录Meta=nil会不写root.meta；Patch`Meta.Label=after`只创建meta hash，没有补root引用，返回nil后Get.Meta仍nil。Meta已存在的相同Patch控制通过；根记录均已存在，不混淆“Patch是否创建缺失整个记录”。

正式[Redis DAO模板](../../codegen/internal/dao/template_redis.go):115–135对ref-hmap直接转发Patch，local/raw路径则PatchStructPath会分配nil parent，两种接入行为漂移。模板可达由当前源码确认，本批实际生成消费者为raw修复控制，未把它冒称已执行生成ref-hmap反例。

建议复用plan/patchTarget与既有同槽脚本，原子维护缺失父引用、目标hash及需要的registry/TTL；或者对nil路径明确拒绝，不能继续返回成功。验收深层nil祖先、已有路径、root miss、TTL/删除/registered keys与并发全量Set。不要以local副本单独变更替代权威Patch，也不能无版本恢复丢失引用。

## RR-20261004-NC-19

[ref_hmap.go](../../cache/ref_hmap.go):511–576根suffix固定`:root`，child suffix直接由字段path构造，无名称/重合检查；:402–418向root HSET保留字段`__keys`，同样未拒绝业务同名字段。

真实Redis两反例：常见`Root struct { ID int64 } json:"root"`与外层ID都名id，Set外层1/内层99返回nil，Get外层1/内层1；`Data string redisdao:"__keys"`写payload后读成root key registry字符串。normal nested struct控制通过。不能把Redis同槽当作逻辑key唯一，也不是重复普通Go字段名。

建议layout构建时在任何I/O前拒绝内部名称碰撞、同hash字段别名、展开后的重复key；先保留现有合法格式，以明确ErrRefHMapUnsupported类错误拒绝坏布局。不要直接改root后缀/逃逸方式而静默换持久格式。若必须支持这些名字，另提供格式版本与迁移方案。验收root、__keys、重复tag、colon路径、合法旧布局及无写入副作用；当前只实测root/__keys两类。

## RR-20261004-NC-20

[mongotest.go](../../mongo/mongotest/mongotest.go):523–550仅在Skip<len(matched)时切片，Skip等于/超过结果数反而返回全部；:554–591的StreamFind完全不应用Skip。正式[driver](../../mongo/driver/collection.go):58–100两入口都传正Skip到SDK。

两个已排序文档[1,2]：Find Skip2/3仍[1,2]；StreamFind Skip1/2/3也[1,2]，五失败；Find Skip0/1及Stream Skip0三控制通过。仅公开内存测试替身有误，不声称真实Mongo分页错误。消费者依赖该替身验证页边界、分批扫描时会得到失真的证据。

建议Find/StreamFind共用已排序结果的skip→limit处理，Skip>=count返回空；补limit组合、int64边界和callback次数控制。该修复应守替身现有unsupported返回约定，不能为了模拟全部Mongo再造查询引擎。真实Mongo差分测试本机未执行。

## 观察与边界

Redis distLock保留不确定token供Release校验，AutoExtend没有fence；Release在opMu内等待watch退出，非协作Extend仍可卡caller，先登记边界而未新增RR。pubsub转换的双select可退出满队列；本批未补真实订阅ack/重连/关闭并发矩阵，不宣称无丢消息。clusterRecovery只请求合并刷新且保留原错，不重放未知写；无真实Cluster。

RefHMap任意Eval错误走DEL+HSET fallback、Lua runtime错误非回滚、TTL只Patch目标hash的取舍仍需独立故障/容量证据；本批未把这些源码观察写成复现失败。mongotest余下595行以后未全读，snapshot restore/事务并发忠实性不能凭本次分页控制宣布正确。
