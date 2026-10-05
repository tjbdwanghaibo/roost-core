# 请求准入与正式生成 Webroute

2026-10-04，源码`483350ca`。[三个确认问题](../bug/REVIEW-2026-10-04-noncore-04.md)尚未实现修复；本文描述当前机制与实施方向，[实跑与边界](REVIEW-2026-10-04-noncore-04.md)。

## 校验身份、控制tokens与控制key数

Session token 将player/毫秒过期/随机nonce组成payload，用HMAC签名；Verify先检查签名，再解析claims、核对expected player与到期。签名正确不表示会话未撤销，ExpectPlayerID=0也不是新的登录审批，这些须由业务会话策略决定。PayloadSignature对原始bytes签名，header兼容sha256=与hex，不含协议自动防重放。

RateLimiter复用x/time/rate连续补充，同时在全局锁下维护key→bucket、lastSeen、IdleTTL和MaxKeys。tokens容量和key容量是两类资源；当前n>burst检查放在创建bucket后，NC-05就是非法需求只守住tokens，仍占了key名额。最小修复是先做不可满足需求的准入拒绝，再动key表；nil/非正数语义保持，并明确是否更新idle活性。

满表陌生key会先扫描所有bucket再拒绝，是有界内存但可能昂贵的拒绝路径。需要真实满表/高基数拒绝负载再决定扫频/增量索引，不凭机制推断SLO不达标，也不删除active key换取表面成功。

## Gateway middleware 的责任

Chain按声明次序套外层，RequireAuthenticated把稳定principal与context取消检查放在业务前；RateLimit即使nil limiter也核对principal，不能把关闭限流等同关闭认证。Timeout保留更短父deadline，context取消仍需要endpoint协作，不会自动杀掉业务。

Recover将endpoint panic转换为固定ErrEndpointPanic而不返回私有详情；report是观察callback，但目前先执行report再赋返回值，二次panic破坏边界（NC-06）。修复应让报告异常留在独立保护内，固定返回错误先确立；不引入每请求goroutine、不再调用同一失败reporter。观察失败不应改变业务请求的错误结算。

## Marker 到实际 URL

CLI Run → GenerateDir递归扫描 → ParseFile/parseMarker/parseRoute检查选项与签名 →按包validateRoutes/排序/render →生成Service.RegisterRoutes；业务提供Service实例，通过RegisterModules共享Registrar注册到httpserver/chi。生成器只写当前包集合，对有自己生成头而零marker的旧文件执行退役。

JSON handler先DecodeJSON、再调用业务、最后WriteResult；完整第二次Decode必须EOF，坏尾随文档与超限不进业务。raw模式先受限ReadBody，再复制Header/Body交给业务。默认业务error映射400，是机制默认值，不替业务选择权限/服务错误HTTP状态。

本批正式CLI、独立module编译和真实HTTP请求证实正常/错误/退役链；当path带非法括号、regexp或wildcard结构，parse只查前缀、runtime只查基础字段，chi会panic（NC-07）。建议在既有webroute包复用chi模式解析做共享校验，再让生成器和Registrar引用，不写第二套路由语法。seen应在成功安装后更新，普通失败给出可定位handler/path error；如果实际router可半安装，必须保留失败/重建边界，不能把recover当作完整回滚。

当前重复检查是method+字面path；不同参数名匹配形状相同会覆盖，需明确禁止或允许的契约，暂为观察。分包注册也不能自动建立业务鉴权：正式consumer本批只验证框架生成/HTTP责任，没有模拟完整游戏登录与Nest调用。

## 本批覆盖与成本

34个runtime项含5fail/28控制/1观察；13个生成消费者执行含3fail/10控制，三个非法路径是同一个根因在两个层次的证据。六包race/vet通过不关闭新RR。body/string/JSON分配、full key sweep、schema复制的成本没有本批benchmark。N02源文8/8读取但完整鉴权/限流容量/非协作回调/跨模块路由仍待验证，后续接N03通信链，保留具名余项。

## 2026-10-04 已实施的三个修复

后续[修复运行](REVIEW-2026-10-04-noncore-05.md)落实了NC-05～07：AllowN按实际burst在key/idle副作用前拒绝不可满足需求；Recover先固定结果，再分别保护report与失败日志；新增ValidatePath在scratch chi router复用正式解析，生成扫描与运行期同一规则，seen在安装成功后登记。原有基础路径错误文本保持，生成文件形状未改。上文描述旧实现与拟议修法为原审查时点。

正式最终30项、原overlay34项及13次真实生成消费者均通过；非法模式生成1且未写产物，旧已生成文件仍可编译并在注册时返回error。保护不终止阻塞report，不回滚任意自定义installer的半安装，不禁止不同参数名的语义等价模式；这些没有因本次修复变成已实现功能。满表扫描与scratch-router启动分配未做benchmark。[复跑与限制](../bugfix/evidence/noncore-bugfix-20261004-03/README.md)。

**更正（2026-10-04，A 线）**：上文“生成扫描与运行期同一 `ValidatePath`”在 `74e1ba39` 之后不再成立——生成器 import core 运行时包违反层次边界（`TestCoreDependencyBoundary`），改为 `codegen/internal/webroute/parse.go` 的 `validateChiPath` 直接调用 chi 解析器，与运行期 `webroute.ValidatePath` 同一语法、两份调用。见 `docs/bugfix/RR-20261004-NC-07.md` 末尾“复核后的补修”。

## 2026-10-05 N02 续审：结果判定在副作用边界之后

[本轮](REVIEW-2026-10-05-noncore-n02.md)把 N02 剩余四项走到生成器输出→装配→真实传输→拒绝/取消/关闭。学到的三点：

- **状态码是编码结果的一部分。** 业务副作用已经发生之后，HTTP 层剩下的唯一职责是如实报告；先写 2xx 再编码，编码错误就只能被吞掉（NC-80）。同理 recover 只能改写“尚未开始”的响应，已开始的响应只能中止，不能拼接（NC-81）。两处都靠减少分支修复：先 Marshal 后写、按是否已开始二选一，没有新增状态或重试。
- **关闭所有权是三段式。** 发起关闭（幂等）→ 在调用方 ctx 内等待排空（超时返回、可重试）→ 排空后才释放对象。把“已停”编码为某字段为空，会在第一次超时后让重试假成功（Ops NC-04、TCP NC-83 同一模式）。不配合 ctx 的回调不能被杀，契约只要求停机如实超时并保留责任。
- **容量要分主体看。** 限流 key 表、握手名额、连接名额都是全局有界资源；有界只保证内存，不保证公平。生成 TCP 接入用 per-IP/handshake 分层，gateway 限流器缺 per-owner 层，单主体即可占满（NC-82，待选择）。

业务鉴权全链（真实 account.Service 签票据 → TCP 握手 → demo 认证器 → ValidateSession → Principal）本轮作为控制通过；票据无状态、TTL 内可复用、会话建立后不随 TTL 失效，这是 account 的设计选择而非 N02 缺陷。
