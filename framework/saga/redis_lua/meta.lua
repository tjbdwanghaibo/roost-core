-- 元数据守卫的 Redis 一半：首次初始化写入实例 ID 与分区数；已有实例 ID 时核对。
-- KEYS: 1 meta
-- ARGV: 1 实例 ID（来自 Mongo 的 saga 元数据）  2 分区数  3 格式版本  4 是否允许改分区数（1 = Go 已确认所有分区日志为空）
local meta = KEYS[1]
-- check:meta.type
if not key_is(meta, 'hash') then return {'CORRUPT'} end
-- end
local cur = redis.call('HMGET', meta, 'instance', 'partitions', 'format')
-- check:meta.instance
if cur[1] and cur[1] ~= ARGV[1] then return {'MISMATCH', cur[1]} end
-- end
-- check:meta.format
if cur[3] and cur[3] ~= ARGV[3] then return {'FORMAT', cur[3]} end
-- end
-- check:meta.partitions
if cur[2] and cur[2] ~= ARGV[2] and ARGV[4] ~= '1' then return {'PARTITIONS', cur[2]} end
-- end
redis.call('HSET', meta, 'instance', ARGV[1], 'partitions', ARGV[2], 'format', ARGV[3])
return {'OK'}
