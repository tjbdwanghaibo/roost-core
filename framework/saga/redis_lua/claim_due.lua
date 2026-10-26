-- 领取到期记录（Store.ClaimDue）：due 的 score = max(next_run, lease_until)，score <= now 的都可领取。
-- KEYS: 1 due
-- ARGV: 1 now_ms  2 owner  3 lease_ms  4 limit  5 前缀
-- 每条返回 id、doc、lease token、lease until；已删除或终态的成员顺手移出 due（自愈），类型不对的记录推后一个租约并返回空 doc（Go 侧告警）。
local due = KEYS[1]
local now, owner, lease, limit, prefix = num(ARGV[1]), ARGV[2], num(ARGV[3]), num(ARGV[4]), ARGV[5]
-- check:claim_due.type
if not key_is(due, 'zset') then return {'CORRUPT'} end
-- end
local out = {'OK'}
local ids = redis.call('ZRANGEBYSCORE', due, '-inf', now, 'LIMIT', 0, limit)
for _, id in ipairs(ids) do
  local rec = prefix .. ':rec:' .. id
  local t = redis.call('TYPE', rec)['ok']
  if t == 'none' then
    redis.call('ZREM', due, id)
  elseif t ~= 'hash' then
    redis.call('ZADD', due, now + lease, id)
    table.insert(out, id); table.insert(out, ''); table.insert(out, '0'); table.insert(out, '0')
  else
    local f = redis.call('HMGET', rec, 'st', 'nr', 'lu')
    if is_terminal(f[1]) then
      redis.call('ZREM', due, id)
    elseif due_score(f[2], f[3]) > now then
      redis.call('ZADD', due, due_score(f[2], f[3]), id)
    else
      local token = redis.call('HINCRBY', rec, 'lt', 1)
      local untils = now + lease
      redis.call('HSET', rec, 'lo', owner, 'lu', untils)
      redis.call('ZADD', due, due_score(f[2], untils), id)
      table.insert(out, id)
      table.insert(out, redis.call('HGET', rec, 'doc') or '')
      table.insert(out, tostring(token))
      table.insert(out, tostring(untils))
    end
  end
end
return out
