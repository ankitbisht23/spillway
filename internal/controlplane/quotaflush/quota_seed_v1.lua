#!lua
-- Spillway quota seed script v1 (RL-10, ADR-0070): re-seeds one tenant's
-- quota counters from Postgres. All keys are in the tenant's slot.
--
-- KEYS: the tenant's epoch key, then counter keys.
-- ARGV: the epoch value ("" leaves the epoch alone: a later call sets it),
-- the epoch's TTL in ms, then per counter: Postgres's used and the TTL in
-- ms.
-- Each counter becomes the larger of its value (0 if missing or not a
-- count) and used, with the TTL; a counter that would be 0 and is missing
-- stays missing. Then the epoch is set, and the gateways count again.
-- Reply: the number of counters written.

local n = #KEYS - 1
if n < 0 or #ARGV ~= 2 + 2 * n then
  return redis.error_reply('spillway: want the epoch key, counters, and 2 + 2 per counter arguments')
end
local written = 0
for i = 1, n do
  local key, used, ttl = KEYS[i + 1], tonumber(ARGV[1 + 2 * i]), tonumber(ARGV[2 + 2 * i])
  if not used or used < 0 or not ttl or ttl < 1 then
    return redis.error_reply('spillway: counter ' .. i .. ': used must be >= 0 and the TTL >= 1')
  end
  local v = redis.pcall('GET', key)
  local cur = 0
  if type(v) == 'string' then
    cur = tonumber(v) or 0
  end
  local val = math.max(cur, used)
  if val > 0 or v ~= false then
    redis.call('SET', key, string.format('%.0f', val), 'PX', string.format('%.0f', ttl))
    written = written + 1
  end
end
if ARGV[1] ~= '' then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
end
return written
