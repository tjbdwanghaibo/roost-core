-- 驱逐已投影、过了保留期的终态记录（Store Service 的 saga 投影在推进进度之后调用）。
-- KEYS: 1 evict:<p>  2 proj:<p>
-- ARGV: 1 limit  2 业务键守卫毫秒  3 还没投影到时推迟的毫秒  4 前缀
-- 对 score <= 服务端现在的每个成员：记录不在或已不是终态（被重开）→ 移出；ls 晚于进度（Mongo 还没有这一版）→ 推迟；
-- 否则删除记录（连同回执、tombstone、告警字段），业务键守卫改为 guard 后过期，移出。返回驱逐数。
local evict, proj = KEYS[1], KEYS[2]
-- check:evict.types
if not (key_is(evict, 'zset') and key_is(proj, 'string')) then return {'CORRUPT'} end
-- end
local now = server_ms()
local done = redis.call('GET', proj) or '0-0'
local evicted = 0
local ids = redis.call('ZRANGEBYSCORE', evict, '-inf', now, 'LIMIT', 0, num(ARGV[1]))
for _, id in ipairs(ids) do
  local rec = ARGV[4] .. ':rec:' .. id
  local f = redis.call('HMGET', rec, 'st', 'ls', 'bk')
  if not f[1] or not is_terminal(f[1]) then
    redis.call('ZREM', evict, id)
  -- check:evict.projected
  elseif stream_cmp(f[2] or '0-0', done) > 0 then
    redis.call('ZADD', evict, now + num(ARGV[3]), id)
  -- end
  else
    redis.call('DEL', rec)
    if f[3] and f[3] ~= '' and redis.call('GET', f[3]) == id then
      redis.call('PEXPIRE', f[3], num(ARGV[2]))
    end
    redis.call('ZREM', evict, id)
    evicted = evicted + 1
  end
end
return {'OK', tostring(evicted)}
