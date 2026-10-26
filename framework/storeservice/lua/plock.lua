-- 分区租约（Store Service）：租约过期或 owner 是自己时取得 / 续约。
-- KEYS: 1 plock:<p>
-- ARGV: 1 owner  2 lease_ms
-- 返回 {'OK', token}（同一 owner 续约时 token 不变，换人时加一）或 {'HELD', 当前 owner}。时间用服务端时钟。
local key = KEYS[1]
-- check:plock.type
if not key_is(key, 'hash') then return {'CORRUPT'} end
-- end
local now = server_ms()
local cur = redis.call('HMGET', key, 'owner', 'token', 'until')
local token = num(cur[2])
-- check:plock.held
if cur[1] and cur[1] ~= ARGV[1] and num(cur[3]) > now then return {'HELD', cur[1]} end
-- end
if cur[1] ~= ARGV[1] then token = token + 1 end
redis.call('HSET', key, 'owner', ARGV[1], 'token', token, 'until', now + num(ARGV[2]))
return {'OK', tostring(token)}
