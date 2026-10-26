-- 领取到期的 outbox 命令（Store.ClaimOutbox）：outbox 的 score = max(next_attempt, lease_until)。
-- KEYS: 1 outbox
-- ARGV: 1 now_ms  2 owner  3 lease_ms  4 limit  5 前缀
-- 每条返回 id、cmd、attempt、next_attempt、lease token、lease until、created。
local outbox = KEYS[1]
local now, owner, lease, limit, prefix = num(ARGV[1]), ARGV[2], num(ARGV[3]), num(ARGV[4]), ARGV[5]
-- check:claim_outbox.type
if not key_is(outbox, 'zset') then return {'CORRUPT'} end
-- end
local out = {'OK'}
local ids = redis.call('ZRANGEBYSCORE', outbox, '-inf', now, 'LIMIT', 0, limit)
for _, id in ipairs(ids) do
  local ob = prefix .. ':ob:' .. id
  local t = redis.call('TYPE', ob)['ok']
  if t == 'none' then
    redis.call('ZREM', outbox, id)
  elseif t ~= 'hash' then
    redis.call('ZADD', outbox, now + lease, id)
    table.insert(out, id); table.insert(out, ''); table.insert(out, '0'); table.insert(out, '0')
    table.insert(out, '0'); table.insert(out, '0'); table.insert(out, '0')
  else
    local f = redis.call('HMGET', ob, 'na', 'lu', 'cmd', 'att', 'ca')
    if due_score(f[1], f[2]) > now then
      redis.call('ZADD', outbox, due_score(f[1], f[2]), id)
    else
      local token = redis.call('HINCRBY', ob, 'lt', 1)
      local untils = now + lease
      redis.call('HSET', ob, 'lo', owner, 'lu', untils)
      redis.call('ZADD', outbox, due_score(f[1], untils), id)
      table.insert(out, id); table.insert(out, f[3] or ''); table.insert(out, f[4] or '0'); table.insert(out, f[1] or '0')
      table.insert(out, tostring(token)); table.insert(out, tostring(untils)); table.insert(out, f[5] or '0')
    end
  end
end
return out
