-- 确认一条已发布的命令（Store.AckOutbox）。命令已被关闭操作或同一操作的新尝试删掉时返回 GONE（Go 侧按已确认处理）。
-- KEYS: 1 ob:<id>  2 outbox
-- ARGV: 1 command id  2 owner  3 token  4 前缀
local ob, outbox = KEYS[1], KEYS[2]
-- check:ack.types
if not (key_is(ob, 'hash') and key_is(outbox, 'zset')) then return {'CORRUPT'} end
-- end
-- check:ack.gone
if redis.call('EXISTS', ob) == 0 then return {'GONE'} end
-- end
local f = redis.call('HMGET', ob, 'lo', 'lt', 'op')
-- check:ack.lease
if f[1] ~= ARGV[2] or num(f[2]) ~= num(ARGV[3]) then return {'CONFLICT'} end
-- end
local index = ARGV[4] .. ':obk:' .. (f[3] or '')
-- check:ack.index_type
if not key_is(index, 'string') then return {'CORRUPT'} end
-- end
redis.call('DEL', ob)
redis.call('ZREM', outbox, ARGV[1])
if redis.call('GET', index) == ARGV[1] then redis.call('DEL', index) end
return {'OK'}
