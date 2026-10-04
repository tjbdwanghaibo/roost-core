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
