-- 一次记录转移（Store.Apply）：After、回执、关闭操作（tombstone + 删排队命令）、新命令、领取索引与日志条目在一个脚本里写完。
-- KEYS: 1 rec  2 due  3 evict:<p>  4 log:<p>  5 outbox
-- ARGV: 1 前缀  2 id  3 exp_v  4 exp_lo  5 exp_lt  6 v  7 st  8 nr  9 lo  10 lt  11 lu  12 ta  13 doc  14 请求摘要
--       15 retention_ms  16 回执 command id  17 回执摘要  18 回执毫秒  19 关闭的操作  20 关闭方式  21 关闭毫秒
--       22 新命令 id  23 新命令所属操作  24 命令编码  25 next_attempt  26 attempt  27 created  28 lo  29 lt  30 lu
-- 校验顺序：键类型 → 记录不在（需装回）→ 回执重复 / 身份冲突 → 同一请求的重试（la 摘要）→ 版本 → 租约 → 关闭操作的归属与
-- 命令键类型 → 新命令的键类型。回执检查在版本之前：同一结果带旧版本重来是重复，不是冲突；也在请求摘要之前：两次送达
-- 可能算出逐字节相同的请求（时间戳同一微秒），带回执的请求一律按回执去重，只有第一个算“已应用”。
local rec, due, evict, log, outbox = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local prefix, id = ARGV[1], ARGV[2]
local exp_v, exp_lo, exp_lt = num(ARGV[3]), ARGV[4], ARGV[5]
local receipt, receipt_digest, receipt_ms = ARGV[16], ARGV[17], ARGV[18]
local close_op, closure, close_ms = ARGV[19], ARGV[20], ARGV[21]
local cmd_id, cmd_op = ARGV[22], ARGV[23]
-- check:apply.types
if not (key_is(rec, 'hash') and key_is(due, 'zset') and key_is(evict, 'zset') and key_is(log, 'stream') and key_is(outbox, 'zset')) then
  return {'CORRUPT'}
end
-- end
-- check:apply.need_reload
if redis.call('EXISTS', rec) == 0 then return {'NEED_RELOAD'} end
-- end
local cur = redis.call('HMGET', rec, 'v', 'la', 'lo', 'lt', 'ls')
-- check:apply.receipt
if receipt ~= '' then
  local prior = redis.call('HGET', rec, 'c:' .. receipt)
  if prior then
    if prior == receipt_digest then return {'DUPLICATE'} end
    return {'IDENTITY_CONFLICT'}
  end
end
-- end
-- check:apply.applied_before
if num(cur[1]) == exp_v + 1 and cur[2] == ARGV[14] then return {'APPLIED_BEFORE', cur[5] or ''} end
-- end
-- check:apply.version
if num(cur[1]) ~= exp_v then return {'CONFLICT'} end
-- end
-- check:apply.lease
if exp_lo ~= '' and (cur[3] ~= exp_lo or num(cur[4]) ~= num(exp_lt)) then return {'CONFLICT'} end
-- end
local tombstone = false
local close_cmd = false
if close_op ~= '' then
  -- check:apply.close_owner
  if string.sub(close_op, 1, #id + 1) ~= id .. ':' then return {'IDENTITY_CONFLICT'} end
  -- end
  tombstone = redis.call('HGET', rec, 'o:' .. close_op)
  local index = prefix .. ':obk:' .. close_op
  -- check:apply.close_types
  if not key_is(index, 'string') then return {'CORRUPT'} end
  -- end
  if redis.call('TYPE', index)['ok'] == 'string' then close_cmd = redis.call('GET', index) end
  -- check:apply.close_command_type
  if close_cmd and not key_is(prefix .. ':ob:' .. close_cmd, 'hash') then return {'CORRUPT'} end
  -- end
end
local old_cmd = false
if cmd_id ~= '' then
  local index = prefix .. ':obk:' .. cmd_op
  -- check:apply.outbox_types
  if not (key_is(index, 'string') and key_is(prefix .. ':ob:' .. cmd_id, 'hash')) then return {'CORRUPT'} end
  -- end
  if redis.call('TYPE', index)['ok'] == 'string' then old_cmd = redis.call('GET', index) end
  -- check:apply.outbox_old_type
  if old_cmd and not key_is(prefix .. ':ob:' .. old_cmd, 'hash') then return {'CORRUPT'} end
  -- end
end

-- 以下只写。
redis.call('HSET', rec, 'v', ARGV[6], 'st', ARGV[7], 'nr', ARGV[8], 'lo', ARGV[9], 'lt', ARGV[10], 'lu', ARGV[11],
  'ta', ARGV[12], 'doc', ARGV[13], 'la', ARGV[14])
if receipt ~= '' then
  redis.call('HSET', rec, 'c:' .. receipt, receipt_digest)
end
local effective_closure, effective_ms = closure, close_ms
if close_op ~= '' then
  if not tombstone then
    redis.call('HSET', rec, 'o:' .. close_op, closure .. '|' .. close_ms)
  else
    local old_closure, old_ms = string.match(tombstone, '^([^|]*)|(.*)$')
    effective_ms = old_ms
    if closure == 'result' and old_closure ~= 'result' then
      redis.call('HSET', rec, 'o:' .. close_op, 'result|' .. old_ms)
    else
      effective_closure = old_closure
    end
  end
  if close_cmd then
    redis.call('DEL', prefix .. ':ob:' .. close_cmd)
    redis.call('ZREM', outbox, close_cmd)
  end
  redis.call('DEL', prefix .. ':obk:' .. close_op)
end
if cmd_id ~= '' then
  if old_cmd then
    redis.call('DEL', prefix .. ':ob:' .. old_cmd)
    redis.call('ZREM', outbox, old_cmd)
  end
  redis.call('HSET', prefix .. ':ob:' .. cmd_id, 'cmd', ARGV[24], 'na', ARGV[25], 'att', ARGV[26], 'ca', ARGV[27],
    'lo', ARGV[28], 'lt', ARGV[29], 'lu', ARGV[30], 'op', cmd_op, 'le', '')
  redis.call('SET', prefix .. ':obk:' .. cmd_op, cmd_id)
  redis.call('ZADD', outbox, due_score(ARGV[25], ARGV[30]), cmd_id)
end
if is_terminal(ARGV[7]) then
  redis.call('ZREM', due, id)
  redis.call('ZADD', evict, num(ARGV[12]) + num(ARGV[15]), id)
else
  redis.call('ZADD', due, due_score(ARGV[8], ARGV[11]), id)
  redis.call('ZREM', evict, id)
end
local fields = {'id', id, 'v', ARGV[6], 'doc', ARGV[13]}
if receipt ~= '' then
  table.insert(fields, 'c'); table.insert(fields, receipt)
  table.insert(fields, 'cd'); table.insert(fields, receipt_digest)
  table.insert(fields, 'ct'); table.insert(fields, receipt_ms)
end
if close_op ~= '' then
  table.insert(fields, 'o'); table.insert(fields, close_op)
  table.insert(fields, 'oc'); table.insert(fields, effective_closure)
  table.insert(fields, 'ot'); table.insert(fields, effective_ms)
end
local entry = redis.call('XADD', log, '*', unpack(fields))
redis.call('HSET', rec, 'ls', entry)
return {'APPLIED', entry}
