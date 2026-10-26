-- 把一条已驱逐的记录从 Mongo 冷副本装回 Redis（冷记录要写，先装回）。不写日志：数据来自 Mongo，Mongo 已有。
-- KEYS: 1 rec  2 bk  3 due  4 evict:<p>
-- ARGV: 1 id  2 v  3 st  4 nr  5 ta  6 doc  7 bk 键名  8 p  9 retention_ms  10 字段对数 n，之后 n 对（字段名，值）：
--       tombstone o:<op> 与告警 a:<op>:<代际>
-- 校验：键类型；记录已在（并发装回或从未驱逐）→ EXISTS（调用方按成功处理）。
local rec, bk, due, evict = KEYS[1], KEYS[2], KEYS[3], KEYS[4]
-- check:reload.types
if not (key_is(rec, 'hash') and key_is(bk, 'string') and key_is(due, 'zset') and key_is(evict, 'zset')) then return {'CORRUPT'} end
-- end
-- check:reload.exists
if redis.call('EXISTS', rec) == 1 then return {'EXISTS', redis.call('HGET', rec, 'v') or ''} end
-- end
local id = ARGV[1]
redis.call('HSET', rec, 'v', ARGV[2], 'st', ARGV[3], 'nr', ARGV[4], 'lo', '', 'lt', 0, 'lu', 0, 'ta', ARGV[5], 'la', '',
  'ls', '0-0', 'bk', ARGV[7], 'p', ARGV[8], 'doc', ARGV[6])
local n = num(ARGV[10])
for i = 0, n - 1 do
  redis.call('HSET', rec, ARGV[11 + 2 * i], ARGV[12 + 2 * i])
end
redis.call('SET', bk, id)
if is_terminal(ARGV[3]) then
  redis.call('ZADD', evict, server_ms() + num(ARGV[9]), id)
else
  redis.call('ZADD', due, num(ARGV[4]), id)
end
return {'OK'}
