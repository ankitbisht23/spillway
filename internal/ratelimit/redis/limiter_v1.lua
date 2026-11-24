#!lua
-- Spillway limiter script v1 (docs/design.md "Limiter script contract",
-- ADR-0021). One call checks every limit in KEYS (all in one hash slot), then
-- writes per the write rule; a negative cost refunds. The maths and the state
-- layout are part of v1: changing either is a v2 script and key prefix (RL-29).
--
-- KEYS: one per limit, then the tenant's quota epoch key when a limit is a
-- quota (ADR-0070).
-- ARGV: cost, then per limit: level, mode, algorithm, rate, period_ms, burst,
-- lease_k, group, period_start_ms, period_end_ms; refunds add admitted_us
-- (0: the current window and time).
-- Reply: allowed, denying_level, denying_limit, remaining, reset_after_ms,
-- retry_after_ms, shadow_would_deny, now_us, quota_unseeded, then per limit:
-- allowed_i, remaining_i, reset_after_ms_i, retry_after_ms_i, granted_i.
--
-- group is the 1-based index of the limit's policy in the call: a policy's
-- limits are adjacent and share it and lease_k. A group with lease_k = k > 0
-- (an enforce policy in batched mode, RL-18, ADR-0069) is decided as a
-- strict one for cost, but an allowed call writes its grant
-- g = min(k, available on each of its limits) instead of cost; granted_i is
-- what was written to each limit (0 when nothing was). Refunds carry
-- lease_k 0: a grant stays with the gateway's lease.
--
-- A quota limit (algorithm quota, RL-10, ADR-0070) counts cost units in a
-- plain integer against rate, its quota, for the UTC period
-- [period_start_ms, period_end_ms) the gateway chose; it is enforce, with
-- period_ms, burst and lease_k 0. While the epoch key is missing (Redis lost
-- the tenant's counters and the flusher has not re-seeded them) or TIME is
-- outside the period (the gateway's clock and Redis disagree near a UTC
-- boundary), every quota limit is skipped: it neither decides nor is
-- written, and quota_unseeded is 1. A refund skips them only while the epoch
-- is missing.

-- Every number written goes through fmt: Lua's default %.14g would cut a
-- 16-digit µs timestamp.
local function fmt(n)
  return string.format('%.0f', n)
end

local function fail(msg)
  return redis.error_reply('spillway: ' .. msg)
end

-- int returns s as an integer, or nil if it is not one.
local function int(s)
  local n = tonumber(s)
  if n and n == math.floor(n) and math.abs(n) < 2 ^ 53 then
    return n
  end
end

local LEVELS = { global = true, tenant = true, consumer = true, route = true }

local cost = int(ARGV[1])
if not cost or cost == 0 then
  return fail('cost must be a non-zero integer')
end
local refund = cost < 0
local per = 10 -- arguments per limit
local extra = 0
if refund then
  extra = 1
end
local n = (#ARGV - 1 - extra) / per
if n ~= math.floor(n) or n < 1 or n > 32 then
  return fail('want 1 to 32 limits of ' .. per .. ' arguments, got ' .. #ARGV .. ' arguments')
end
local admitted = 0
if refund then
  admitted = int(ARGV[#ARGV])
  if not admitted or admitted < 0 then
    return fail('admitted_us must be an integer >= 0')
  end
end

local lim = {}
local quotas = false -- a limit is a quota
for i = 1, n do
  local a = 1 + per * (i - 1)
  local l = {
    key = KEYS[i], level = ARGV[a + 1], mode = ARGV[a + 2], alg = ARGV[a + 3],
    rate = int(ARGV[a + 4]), period = int(ARGV[a + 5]), burst = int(ARGV[a + 6]),
    k = int(ARGV[a + 7]), group = int(ARGV[a + 8]),
    ps = int(ARGV[a + 9]), pe = int(ARGV[a + 10]),
  }
  if not LEVELS[l.level] then
    return fail('limit ' .. i .. ': level ' .. tostring(l.level))
  end
  if l.mode ~= 'enforce' and l.mode ~= 'shadow' then
    return fail('limit ' .. i .. ': mode ' .. tostring(l.mode))
  end
  if not l.rate or l.rate < 1 or not l.period or not l.burst or not l.ps or not l.pe then
    return fail('limit ' .. i .. ': rate must be an integer >= 1, and period_ms, burst and the period bounds integers')
  end
  if l.alg == 'quota' then
    if l.mode ~= 'enforce' or l.period ~= 0 or l.burst ~= 0 or l.k ~= 0 or l.ps < 0 or l.pe <= l.ps then
      return fail('limit ' .. i .. ': a quota is enforce, with period_ms, burst and lease_k 0 and period_start_ms < period_end_ms')
    end
    quotas = true
  elseif l.period < 1 or l.ps ~= 0 or l.pe ~= 0 then
    return fail('limit ' .. i .. ': period_ms must be >= 1 and the period bounds 0')
  elseif l.alg == 'gcra' then
    if not l.burst or l.burst < 1 then
      return fail('limit ' .. i .. ': gcra burst must be an integer >= 1')
    end
    -- Emission interval T in whole µs, rounded up like the local GCRA's ns,
    -- so τ = T·burst admits exactly burst at once and never more than
    -- burst + w·rate/period in any window w.
    l.t = math.ceil(l.period * 1000 / l.rate)
    l.tau = l.t * l.burst
  elseif l.alg == 'sliding_window' then
    if l.burst ~= 0 then
      return fail('limit ' .. i .. ': sliding_window burst must be 0')
    end
    l.p = l.period * 1000
  else
    return fail('limit ' .. i .. ': algorithm ' .. tostring(l.alg))
  end
  if not l.k or l.k < 0 then
    return fail('limit ' .. i .. ': lease_k must be an integer >= 0')
  end
  if l.k > 0 and (refund or l.mode ~= 'enforce' or l.k < cost) then
    return fail('limit ' .. i .. ': lease_k > 0 needs an enforce limit, a cost of at most lease_k and no refund')
  end
  local prev = lim[i - 1]
  local first = prev == nil or l.group == prev.group + 1
  if not l.group or not (first or prev and l.group == prev.group) or prev == nil and l.group ~= 1 then
    return fail('limit ' .. i .. ': group must be 1, then the same as the previous limit\'s or one more')
  end
  if not first and l.k ~= prev.k then
    return fail('limit ' .. i .. ': lease_k differs within group ' .. l.group)
  end
  lim[i] = l
end

local want = n
if quotas then
  want = n + 1
end
if #KEYS ~= want then
  return fail('want ' .. want .. ' keys for ' .. n .. ' limits, got ' .. #KEYS)
end

local tm = redis.call('TIME')
local now = tonumber(tm[1]) * 1000000 + tonumber(tm[2])

-- Quota limits count only while the tenant's epoch exists and TIME is in
-- their period (refunds: while the epoch exists).
local unseeded = 0
if quotas then
  local seeded = redis.call('EXISTS', KEYS[n + 1]) == 1
  local now_ms = math.floor(now / 1000)
  for _, l in ipairs(lim) do
    if l.alg == 'quota' then
      l.skip = not seeded or not refund and (now_ms < l.ps or now_ms >= l.pe)
      if l.skip then
        unseeded = 1
      end
    end
  end
end

-- read loads l's state. A key holding the other algorithm's type (a policy
-- whose algorithm changed) reads as empty and is replaced when written.
local function read(l)
  if l.alg == 'quota' then
    local v = redis.pcall('GET', l.key)
    if type(v) == 'table' then
      l.wrong = true -- another type: replaced when written
      v = false
    end
    l.exists = v ~= false
    l.count = tonumber(v) or 0
    return
  end
  if l.alg == 'gcra' then
    local v = redis.pcall('GET', l.key)
    if type(v) == 'table' then
      v = false
    end
    l.tat = tonumber(v) -- nil when missing
    return
  end
  local v = redis.pcall('HMGET', l.key, 'w', 'cur', 'prev')
  if v.err then
    l.wrong = true
    v = {}
  end
  local w, cur, prev = tonumber(v[1]), tonumber(v[2]) or 0, tonumber(v[3]) or 0
  l.exists = w ~= nil
  l.w = now - now % l.p
  if w and w >= l.w then
    -- Same window, or a later one written by a primary whose clock was
    -- ahead (a failover, RL-12): keeping the counts errs towards denying.
  elseif w == l.w - l.p then
    prev, cur = cur, 0
  else
    prev, cur = 0, 0
  end
  l.cur, l.prev, l.e = cur, prev, now - l.w
end

local function estimate(l)
  return l.prev * (l.p - l.e) / l.p + l.cur
end

-- check decides whether l alone admits c > 0, without writing.
local function check(l, c)
  l.retry = 0
  if l.alg == 'quota' then
    l.ok = l.skip or l.count + c <= l.rate
    if not l.ok then
      l.retry = l.pe * 1000 - now -- until the period ends
    end
    return
  end
  if l.alg == 'gcra' then
    l.s = math.max(l.tat or 0, now)
    l.next = l.s + c * l.t
    l.ok = l.next - now <= l.tau
    if not l.ok then
      l.retry = l.next - l.tau - now
    end
    return
  end
  l.est = estimate(l)
  l.ok = l.est + c <= l.rate
  if l.ok then
    return
  end
  -- ADR-0017: the earliest time c would fit if nothing else were admitted.
  local r
  if l.cur + c <= l.rate then
    r = l.p - (l.rate - c - l.cur) * l.p / l.prev - l.e -- the deny means prev > 0
  else
    r = 2 * l.p - l.e
    if l.rate - c > 0 then
      r = r - (l.rate - c) * l.p / l.cur -- then cur > rate − c > 0
    end
  end
  l.retry = math.max(0, math.ceil(r))
end

-- TTLs (RL-15): GCRA the period plus the burst time, which outlives any
-- stored TAT − t ≤ τ; sliding window two windows.
local function save_gcra(l, tat)
  redis.call('SET', l.key, fmt(tat), 'PX', fmt(l.period + math.ceil(l.tau / 1000)))
  l.s = tat
end

local function save_window(l)
  if l.wrong then
    redis.call('DEL', l.key)
  end
  redis.call('HSET', l.key, 'w', fmt(l.w), 'cur', fmt(l.cur), 'prev', fmt(l.prev))
  redis.call('PEXPIRE', l.key, fmt(2 * l.period))
end

-- available is how many units l admits now, after check: the most c that
-- check would allow, never below 0.
local function available(l)
  if l.alg == 'gcra' then
    return math.max(0, math.floor((l.tau - (l.s - now)) / l.t))
  end
  return math.max(0, math.floor(l.rate - l.est))
end

-- write takes c > 0 from l, which admits at least c (check, or available).
local function write(l, c)
  if l.skip then
    return
  end
  l.granted = c
  if l.alg == 'quota' then
    l.count = l.count + c
    redis.call('SET', l.key, fmt(l.count), 'PX', fmt(l.pe - math.floor(now / 1000) + 86400000))
    return
  end
  if l.alg == 'gcra' then
    save_gcra(l, l.s + c * l.t)
    return
  end
  l.cur = l.cur + c
  l.est = l.est + c
  save_window(l)
end

-- give_back reverses an admission of c > 0 made at admitted_us. It never
-- goes below empty, and a missing (or wrong-typed) key stays as it is. GCRA
-- gives back c·T less the time since admitted_us: an admission that found
-- the key idle raised the TAT from its own time, and the requests admitted
-- since may have used that much of its room (ADR-0028).
local function give_back(l, c)
  l.ok, l.retry = true, 0
  if l.alg == 'quota' then
    if not l.skip and l.exists and not l.wrong then
      l.count = math.max(0, l.count - c)
      redis.call('SET', l.key, fmt(l.count), 'KEEPTTL')
    end
    return
  end
  if l.alg == 'gcra' then
    l.s = now
    if l.tat then
      local back = c * l.t
      if admitted > 0 then
        back = math.max(0, back - math.max(0, now - admitted))
      end
      local tat = l.tat - back
      if tat <= now then
        redis.call('DEL', l.key)
      else
        save_gcra(l, tat)
      end
    end
    return
  end
  if l.exists then
    local aw = l.w
    if admitted > 0 then
      aw = admitted - admitted % l.p
    end
    if aw == l.w then
      l.cur = math.max(0, l.cur - c)
    elseif aw == l.w - l.p then
      l.prev = math.max(0, l.prev - c)
    end -- older: the deduction has left the window
    save_window(l)
  end
  l.est = estimate(l)
end

for _, l in ipairs(lim) do
  read(l)
end

local allowed, deny, mask = 1, 0, 0
if refund then
  for _, l in ipairs(lim) do
    give_back(l, -cost)
  end
else
  for i, l in ipairs(lim) do
    check(l, cost)
    if not l.ok then
      if l.mode == 'enforce' then
        allowed = 0
        if deny == 0 or l.retry > lim[deny].retry then
          deny = i
        end
      else
        mask = mask + 2 ^ (i - 1)
      end
    end
  end
  -- Write rule: nothing on a deny; otherwise every enforce limit and each
  -- shadow limit that admits on its own, a lease group's limits its grant.
  if allowed == 1 then
    local i = 1
    while i <= n do
      local j, units = i, cost
      while j < n and lim[j + 1].group == lim[i].group do
        j = j + 1
      end
      if lim[i].k > 0 then
        units = lim[i].k
        for x = i, j do
          units = math.min(units, available(lim[x]))
        end
      end
      for x = i, j do
        if lim[x].ok then
          write(lim[x], units)
        end
      end
      i = j + 1
    end
  end
end

-- State after the call: remaining and reset_after (µs) per limit.
for _, l in ipairs(lim) do
  if l.alg == 'quota' then
    l.reset = math.max(0, l.pe * 1000 - now)
    l.rem = math.max(0, l.rate - l.count)
  elseif l.alg == 'gcra' then
    l.reset = l.s - now
    l.rem = math.max(0, math.floor((l.tau - l.reset) / l.t))
  else
    l.reset = l.p - l.e
    l.rem = math.max(0, math.floor(l.rate - l.est))
  end
end

local function ms(us)
  return math.ceil(us / 1000)
end

-- The summary limit: the denying one, else the enforce limit with the least
-- remaining (first on a tie; skipped quotas aside), else limit 1.
local pick = deny
if pick == 0 then
  for i, l in ipairs(lim) do
    if l.mode == 'enforce' and not l.skip and (pick == 0 or l.rem < lim[pick].rem) then
      pick = i
    end
  end
  if pick == 0 then
    pick = 1
  end
end

local level, retry = '', 0
if deny > 0 then
  level, retry = lim[deny].level, ms(lim[deny].retry)
end
local out = { allowed, level, deny, lim[pick].rem, ms(lim[pick].reset), retry, mask, now, unseeded }
for _, l in ipairs(lim) do
  local ok = 0
  if l.ok then
    ok = 1
  end
  out[#out + 1] = ok
  out[#out + 1] = l.rem
  out[#out + 1] = ms(l.reset)
  out[#out + 1] = ms(l.retry)
  out[#out + 1] = l.granted or 0
end
return out
