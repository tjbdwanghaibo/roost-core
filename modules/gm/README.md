# GM 运维命令

`roost module add gm` 安装命令登记点；单装时没有玩法命令。功能模块通过自己的 `with/gm/` 文件登记命令，两种安装顺序都可用。文件装入工程后归业务维护。

| 同装模块 | 命令 | payload（都还要带 note） | 成功结果 |
| --- | --- | --- | --- |
| wallet | `gm.player.coins` | `player_id, delta`，正数增加、负数扣除 | balance |
| bag | `gm.player.add_item` | `player_id, item_id, count` | 该道具的持有量 count |
| profile | `gm.player.add_exp` | `player_id, amount` | levels_gained |
| mailbox | `gm.mail.send` | `player_id, subject, body, attachment?, request_id` | mail_id、recipients |
| rehome | `gm.player.rehome` | `player_id, to` | accepted、transfer_id、from、to |

`player_id` 可用唯一号或完整 Player EntityID。加币、道具、经验由生成 Sender 进入正式 Nest 事务，冷目标由 Nest 加载，未建档玩家不自动创建。提交失败使用已有回滚机制。换服走 rehome 驱动器，沿用其规则、意图、排空与交出顺序；运维入口免玩家冷却与目标白名单，仍受各模块登记的换服检查限制。`accepted` 只是受理，最终绑定查 account。

## HTTP 与审计

game 模板已有 ops Mod；模块不自动开端口或生成 token。按部署环境配置：

```yaml
ops:
  enabled: true
  addr: 127.0.0.1:9100
  admin_enabled: true
  admin_token: "${OPS_ADMIN_TOKEN}"
```

这里的环境变量写法表示部署时注入，不应把示例字符串当作真实 token。开发 `dev-` token 需要 `ops.allow_dev_token: true`，生产即便写了该开关也拒绝。`GET /admin/commands` 列出命令，`POST /admin/execute` 执行，沿用 ops 的 `X-Admin-Token` / Bearer 认证。

```json
{
  "name": "gm.mail.send",
  "operator": "support-17",
  "trace_id": "incident-42-send-1",
  "payload": {
    "player_id": 1001,
    "subject": "故障补偿",
    "body": "维护期间奖励补发",
    "attachment": {"type": "coins", "amount": 100},
    "request_id": "incident-42-player-1001",
    "note": "工单 42 已核对"
  }
}
```

`operator`（1～64 字节）与 `note`（1～256 字节）不能空白。operator 是有 token 的调用方自报的审计名称，不是框架认证出的个人身份。日志记录命令、trace、操作者、备注和结果，不记录 token 或整份载荷。已登记命令拒绝时 `result.data.code` 保留业务码；基础设施异常使用框架 internal 码，原始 error 保留供日志定位。未识别的 payload 字段拒绝。

## 重试与补偿

- 直接加减币、加道具、加经验每调用一次就执行一次。网络超时可能已经提交，不能盲目重发。需要能重试的补偿优先用邮件。
- 邮件要求显式的稳定 `request_id`，与 trace 独立；相同事件无论重试、换进程还是换 trace，都用原来的键。内容同键不可变，幂等与保留期沿用 mail 服务，不宣称无限保留。GM 先通过只读 Nest 检查收件人，再在锁外调 mail；没有锁内 RPC，也不会发一封信就改玩家余额，奖励由 MailboxClaim 同一事务领取。
- 道具附件写成 `{"type":"item","id":1001,"amount":3}`，需要同时安装 bag；零附件则发送普通邮件。道具是否仍在当前规则表、背包是否有空间，在领取时再核对。

## 扩展

功能模块在 `with/gm/game/gm/` 的 `init` 中调用 `Register(Definition{...})`。重名立即报错；Handler 在实体锁外执行，实体写入只用正式 Sender。要替换某个已登记命令的事务，业务服务初始化时调用 `Replace`，它保留审计与停机拒绝。profile 提供 `ProfileAddExpCommand`，demo 用它调用同时修改 Player 与 World 的 Sender，无需再写解析代码。

本模块没有常驻协程或独立关闭责任；ops 负责 HTTP 在途排空，模块 stop 后拒绝新执行。命令不暴露给玩家协议。

## 验证

基础 9 条场景，每组联动 4 条。`TestModuleGMScenariosAndMutations` 在新 game 上先单装再增量装五组，核对场景数、重复安装与变异，最后定向 race。提交器、mail RPC、account/实体交出使用隔离替身；真实网络与存储验收另记，不把这里的通过当作生产验收。
