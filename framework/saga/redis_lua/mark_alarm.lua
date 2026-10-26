-- 在 tombstone 上记下“这个操作第 inc 代放弃后的迟到成功已告警”（LateSuccessAlarmStore）。没有 tombstone 返回 NONE。
-- KEYS: 1 rec  2 log:<p>
-- ARGV: 1 操作  2 代际  3 now_ms  4 id
local rec, log = KEYS[1], KEYS[2]
-- check:alarm.types
if not (key_is(rec, 'hash') and key_is(log, 'stream')) then return {'CORRUPT'} end
-- end
-- check:alarm.need_reload
if redis.call('EXISTS', rec) == 0 then return {'NEED_RELOAD'} end
-- end
-- check:alarm.tombstone
if not redis.call('HGET', rec, 'o:' .. ARGV[1]) then return {'NONE'} end
-- end
if redis.call('HSETNX', rec, 'a:' .. ARGV[1] .. ':' .. ARGV[2], ARGV[3]) == 0 then return {'NONE'} end
local cur = redis.call('HMGET', rec, 'v', 'doc')
local entry = redis.call('XADD', log, '*', 'id', ARGV[4], 'v', cur[1], 'doc', cur[2], 'a', ARGV[1], 'ai', ARGV[2], 'at', ARGV[3])
redis.call('HSET', rec, 'ls', entry)
return {'MARKED', entry}
