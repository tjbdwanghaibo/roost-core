-- 公共函数：Go 侧装载时把本文件拼在每个脚本前面（再在最前面加 #!lua 头）。
-- 约定：脚本只用返回值表达业务结论（第一个元素是结果码），不用 redis.error_reply；先做完全部校验再写第一条。
-- 脚本在内部按前缀拼键（单主部署才可用，Cluster 在配置与启动探针处被拒绝）。

local function key_is(key, want)
  local t = redis.call('TYPE', key)['ok']
  return t == 'none' or t == want
end

local function server_ms()
  local t = redis.call('TIME')
  return tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end

local function is_terminal(st)
  local n = tonumber(st)
  return n ~= nil and n >= 4
end

local function num(value)
  return tonumber(value) or 0
end

local function due_score(next_ms, lease_ms)
  local a, b = num(next_ms), num(lease_ms)
  if a > b then return a end
  return b
end

-- 流条目 ID 按“毫秒-序号”两段数值比较：a < b 返回 -1，相等 0，a > b 1。
local function stream_cmp(a, b)
  local am, as = string.match(a, '^(%d+)-(%d+)$')
  local bm, bs = string.match(b, '^(%d+)-(%d+)$')
  am, as, bm, bs = tonumber(am), tonumber(as), tonumber(bm), tonumber(bs)
  if am ~= bm then
    if am < bm then return -1 end
    return 1
  end
  if as ~= bs then
    if as < bs then return -1 end
    return 1
  end
  return 0
end
