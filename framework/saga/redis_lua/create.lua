-- 创建一条驻留记录（Store.Create）。
-- KEYS: 1 rec  2 bk  3 due  4 log:<p>  5 evict:<p>
-- ARGV: 1 id  2 v  3 st  4 nr  5 lo  6 lt  7 lu  8 ta  9 doc  10 bk 键名  11 p  12 read_ms  13 guard_ms  14 max_backlog  15 retention_ms
-- 校验（全部在第一条写之前）：键类型 → id 已存在 → 业务键已存在 → Mongo 读到脚本的间隔超过 guard/2 → 分区日志积压。
local rec, bk, due, log, evict = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local id = ARGV[1]
-- check:create.types
if not (key_is(rec, 'hash') and key_is(bk, 'string') and key_is(due, 'zset') and key_is(log, 'stream') and key_is(evict, 'zset')) then
  return {'CORRUPT'}
end
-- end
-- check:create.exists_id
if redis.call('EXISTS', rec) == 1 then return {'EXISTS_ID'} end
-- end
-- check:create.exists_bk
local owner = redis.call('GET', bk)
if owner then return {'EXISTS_BK', owner} end
-- end
-- check:create.guard
if server_ms() - num(ARGV[12]) >= num(ARGV[13]) / 2 then return {'STALE_GUARD'} end
-- end
-- check:create.backpressure
if redis.call('XLEN', log) >= num(ARGV[14]) then return {'BACKPRESSURE'} end
-- end
redis.call('HSET', rec, 'v', ARGV[2], 'st', ARGV[3], 'nr', ARGV[4], 'lo', ARGV[5], 'lt', ARGV[6], 'lu', ARGV[7],
  'ta', ARGV[8], 'la', '', 'bk', ARGV[10], 'p', ARGV[11], 'doc', ARGV[9])
redis.call('SET', bk, id)
if is_terminal(ARGV[3]) then
  redis.call('ZADD', evict, num(ARGV[8]) + num(ARGV[15]), id)
else
  redis.call('ZADD', due, due_score(ARGV[4], ARGV[7]), id)
end
local entry = redis.call('XADD', log, '*', 'id', id, 'v', ARGV[2], 'doc', ARGV[9])
redis.call('HSET', rec, 'ls', entry)
return {'OK', entry}
