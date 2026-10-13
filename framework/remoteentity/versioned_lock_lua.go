package remoteentity

// versionedTryLockLua 取得租约并从独立的、不过期的计数器分配 fence；计数器绝不能与锁哈希共用 TTL。
//
// ARGV[3] 是锁对象的 token 前缀，ARGV[4] 是本次取锁的序号（ARGV[1] = ARGV[3] .. ARGV[4]）。owner 带同一前缀且序号
// 小于本次时，它是这个锁对象更早一代"结果未知"留下的租约（取锁回复丢失或迟到、释放无答复）：
// 同样视为可取得，在同一条脚本里换成新 token 并分配新 fence——是新的一代，不是延续旧代。序号不小于本次（迟到落地的旧代脚本
// 遇到新代际）或 owner 是别人时照旧返回 {0,0,0,0}。ARGV[3] 为空时不做这项判定。
// version 字段不动：失败的那次释放没有写进版本缓存，这里也不替它补写。
//
// ARGV[5] / ARGV[6] 是进程代际的范围前缀与本代前缀（见 lockIncarnation）：owner 带 ARGV[5] 但不带 ARGV[6]，
// 是同一单实例锁持有者上一代进程留下的——本进程持有单实例锁，说明那一代已死或已失锁 fail-stop——同样视为可取得，
// 与 owner 不存在（TTL 过期）走同一分支：先 INCR fence、换新 token。返回的第 4 项为 1 表示这次是接管。
// ARGV[5] 为空（没有代际，或单实例锁未启用）时不做这项判定；别的 sid / 服务类型 / 项目前缀、本代自己的、旧格式的 owner 都不接管。
const versionedTryLockLua = `
local owner = redis.call("HGET", KEYS[1], "owner")
local earlier = false
local takeover = false
if owner ~= false and ARGV[3] ~= "" and string.sub(owner, 1, #ARGV[3]) == ARGV[3] then
    local seq = tonumber(string.sub(owner, #ARGV[3] + 1))
    earlier = seq ~= nil and seq < tonumber(ARGV[4])
end
if owner ~= false and not earlier and ARGV[5] ~= nil and ARGV[5] ~= "" and string.sub(owner, 1, #ARGV[5]) == ARGV[5] and string.sub(owner, 1, #ARGV[6]) ~= ARGV[6] then
    takeover = true
end
if owner == false or earlier or takeover then
    -- Lua 报错不会回滚前面的写入。先分配 fence，失败时不能留下没有 TTL 的 owner。
    redis.call("INCR", KEYS[2])
    -- Lua number 不能精确表达完整 int64；INCR 的返回值也不能直接传回 Go。
    local fence = redis.call("GET", KEYS[2])
    local ver = redis.call("HGET", KEYS[1], "version")
    redis.call("HSET", KEYS[1], "owner", ARGV[1])
    redis.call("PEXPIRE", KEYS[1], ARGV[2])
    if ver == false then
        ver = "0"
    end
    if takeover then
        return {1, ver, fence, 1}
    end
    return {1, ver, fence, 0}
end
return {0, 0, 0, 0}
`

// versionedUnlockLua：校验 owner → 写入版本与本次操作回执 → HDEL owner → PEXPIRE。返回 1 表示本次解锁，2 表示之前
// 一次尝试已经生效（幂等重试），0 表示锁确实不由本方持有。
//
// 幂等分支绑定到一次 UnlockWithRetry 操作：业务版本可能合法地保持不变，不能用它证明某一次解锁尝试到达了 Redis。
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

// versionedTouchLua：校验 owner → 在剩余 PTTL 上加时长（不超过上限）；返回新 TTL，不由本方持有时返回 -1。
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

// versionedRefreshLua：校验 owner → PEXPIRE 到 TTL；返回 1 或 0。
const versionedRefreshLua = `
if redis.call("HGET", KEYS[1], "owner") == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`
