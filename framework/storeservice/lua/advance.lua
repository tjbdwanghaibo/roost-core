-- 推进投影进度并裁剪已投影的日志（Store Service）。只有持有分区租约（owner + token、未过期）的实例能推进。
-- KEYS: 1 plock:<p>  2 proj:<p>  3 log:<p>
-- ARGV: 1 owner  2 token  3 upto（本批最后一条条目 ID）
-- 返回 OK（推进并裁剪）、NOOP（upto 不超过当前进度）、LOST（租约不是自己的）。
local lock, proj, log = KEYS[1], KEYS[2], KEYS[3]
-- check:advance.types
if not (key_is(lock, 'hash') and key_is(proj, 'string') and key_is(log, 'stream')) then return {'CORRUPT'} end
-- end
local cur = redis.call('HMGET', lock, 'owner', 'token', 'until')
-- check:advance.lease
if cur[1] ~= ARGV[1] or num(cur[2]) ~= num(ARGV[2]) or num(cur[3]) <= server_ms() then return {'LOST'} end
-- end
local done = redis.call('GET', proj) or '0-0'
-- check:advance.noop
if stream_cmp(ARGV[3], done) <= 0 then return {'NOOP'} end
-- end
redis.call('SET', proj, ARGV[3])
-- XTRIM MINID 删掉 ID 小于阈值的条目：阈值取 upto 的下一个 ID，upto 本身也删掉。
local ms, seq = string.match(ARGV[3], '^(%d+)-(%d+)$')
redis.call('XTRIM', log, 'MINID', ms .. '-' .. tostring(tonumber(seq) + 1))
return {'OK'}
