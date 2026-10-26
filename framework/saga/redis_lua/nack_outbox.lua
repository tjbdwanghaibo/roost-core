-- 发布失败，退避后重发（Store.NackOutbox）：清租约、attempt 加一、下次时间 next。命令已不在时返回 GONE。
-- KEYS: 1 ob:<id>  2 outbox
-- ARGV: 1 command id  2 owner  3 token  4 next_ms  5 last_error
local ob, outbox = KEYS[1], KEYS[2]
-- check:nack.types
if not (key_is(ob, 'hash') and key_is(outbox, 'zset')) then return {'CORRUPT'} end
-- end
-- check:nack.gone
if redis.call('EXISTS', ob) == 0 then return {'GONE'} end
-- end
local f = redis.call('HMGET', ob, 'lo', 'lt')
-- check:nack.lease
if f[1] ~= ARGV[2] or num(f[2]) ~= num(ARGV[3]) then return {'CONFLICT'} end
-- end
redis.call('HSET', ob, 'na', ARGV[4], 'le', ARGV[5], 'lo', '', 'lu', '0')
redis.call('HINCRBY', ob, 'att', 1)
redis.call('ZADD', outbox, num(ARGV[4]), ARGV[1])
return {'OK'}
