package remoteentity

// versionedTryLockLua acquires the lease and allocates a fence from a separate
// non-expiring counter. The counter must never share the lock hash TTL.
//
// ARGV[3]（可空）是本地上一次释放结果未知的 token（RR-20260930-21）：owner 仍是它时同样视为可取得，
// 在同一条脚本里换成新 token 并分配新 fence——是新的一代，不是延续旧代；owner 是别人时照旧返回 {0,0,0}。
// version 字段不动：失败的那次释放没有写进版本缓存，这里也不替它补写。
const versionedTryLockLua = `
local owner = redis.call("HGET", KEYS[1], "owner")
if owner == false or (ARGV[3] ~= "" and owner == ARGV[3]) then
    -- Lua 报错不会回滚前面的写入。先分配 fence，失败时不能留下没有 TTL 的 owner。
    redis.call("INCR", KEYS[2])
    -- Lua number 不能精确表达完整 int64；INCR 的返回值也不能直接传回 Go。
    local fence = redis.call("GET", KEYS[2])
    local ver = redis.call("HGET", KEYS[1], "version")
    redis.call("HSET", KEYS[1], "owner", ARGV[1])
    redis.call("PEXPIRE", KEYS[1], ARGV[2])
    if ver == false then
        return {1, "0", fence}
    end
    return {1, ver, fence}
end
return {0, 0, 0}
`

// versionedUnlockLua: verify owner → store version and operation receipt →
// HDEL owner → PEXPIRE.
// Returns 1 on unlock, 2 when a previous attempt already landed (idempotent
// retry), 0 when the lock is genuinely not owned.
//
// The idempotent branch is tied to one UnlockWithRetry operation. Business
// versions may legitimately be unchanged and therefore cannot prove that a
// particular unlock attempt reached Redis.
const versionedUnlockLua = `
local cur = redis.call("HGET", KEYS[1], "owner")
if cur == ARGV[1] then
    redis.call("HSET", KEYS[1], "version", ARGV[3])
    redis.call("HSET", KEYS[1], "last_unlock", ARGV[2])
    redis.call("HDEL", KEYS[1], "owner")
    redis.call("PEXPIRE", KEYS[1], ARGV[4])
    return 1
end
if redis.call("HGET", KEYS[1], "last_unlock") == ARGV[2] then
    return 2
end
return 0
`

// versionedTouchLua: verify owner → add duration to remaining PTTL (capped at max); returns newTTL or -1
const versionedTouchLua = `
if redis.call("HGET", KEYS[1], "owner") ~= ARGV[1] then
    return -1
end
local pttl = redis.call("PTTL", KEYS[1])
if pttl < 0 then
    pttl = 0
end
local newTTL = pttl + tonumber(ARGV[2])
local maxTTL = tonumber(ARGV[3])
if maxTTL > 0 and newTTL > maxTTL then
    newTTL = maxTTL
end
redis.call("PEXPIRE", KEYS[1], newTTL)
return newTTL
`

// versionedRefreshLua: verify owner → PEXPIRE to TTL; returns 1 or 0
const versionedRefreshLua = `
if redis.call("HGET", KEYS[1], "owner") == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`
