-- check.lua: may this request be served, and what is the state now.
--
-- KEYS[1]  qc:{t:<tenant_id>}:bal:<feature_key>
-- ARGV[1]  amount           decimal integer, 0 <= amount <= 2^53-1 (DR-022, DR-023)
-- ARGV[2]  target_index     the index the cycle engine gave the caller (DR-004)
-- ARGV[3]  window_start_ms  the start of that window (DR-005)
-- ARGV[4]  window_end_ms    the end of that window, or '' for never (DR-006)
-- ARGV[5]  effective_limit  the limit for that window, after any reduction (DR-018)
-- ARGV[6]  now_ms           the instant the caller chose its window with (DR-004)
--
-- The key arrives as KEYS[1] and every value as ARGV, so nothing a caller
-- sends is ever parsed as Lua (ADR-0002, testing-strategy section 4).
--
-- This is the one script of the three that ends in a 200 rather than an
-- allowance decision, so a refusal here is an answer and not an error: a
-- request that would not fit is reported as not allowed with the state beside
-- it, and the caller decides what to do (DR-026, T-05). A balance that is
-- absent or unusable is still the state-missing marker of
-- request-lifecycle.md step 2, and this script reports it rather than
-- answering with an empty balance.
--
-- It writes only what a cycle transition writes, and only when a boundary has
-- closed. Outside a rollover it changes no balance, because the balance view
-- is the current cycle's allowance and a hash that was never rolled forward
-- would answer with last cycle's figure (INV-X5, DR-045).

-- BEGIN transition
-- This block is byte-for-byte identical in consume.lua, refund.lua and
-- check.lua, and source_test.go fails if the three ever differ. Three copies
-- exist because Redis Lua has no include and a script is loaded whole, and
-- one script per endpoint is what performance.md section 5 asks for. The
-- duplication is what INV-C2 requires: there is no second implementation of
-- rollover to drift out of agreement with the first.
local BOUND = 9007199254740991
local DAY = 86400

-- The balance key, taken once from KEYS[1]. Redis hands a script its keys and
-- arguments as globals, so naming them here means the rest of the script reads a
-- value it cannot be tricked into taking from anywhere else, and the one key it
-- is allowed to touch is named in one place.
local KEY = KEYS[1]

local function num(v)
  return string.format('%d', v)
end

-- A hash field that is absent arrives as false, not as an empty string. An
-- absent field and an empty one are different things, and only the window end
-- may legitimately be empty.
local function field(v)
  if type(v) ~= 'string' then
    return nil
  end
  return v
end

local function answer(decision, state)
  local reply = { decision, '', '', '', '', '', '', '' }
  if state then
    reply[2] = state.verdict
    reply[3] = num(state.balance)
    reply[4] = num(state.limit)
    reply[5] = num(state.bonus)
    reply[6] = num(state.index)
    reply[7] = num(state.start)
    reply[8] = state.finish and num(state.finish) or ''
  end
  return reply
end

-- The cycle the caller's request belongs to, from the three verdicts in
-- cycle-engine.md section 5. A window that has not opened, a target the store
-- has already passed, and a target ahead of a window that has not closed are
-- all STALE: the caller is behind, the state is not, so the state is left as
-- it stands and the call proceeds against it (INV-C1, C-3, DR-004).
local function transition()
  local target = tonumber(ARGV[2])
  local start = tonumber(ARGV[3])
  local end_arg = ARGV[4]
  local limit = tonumber(ARGV[5])
  local now = tonumber(ARGV[6])
  local finish = end_arg == '' and nil or tonumber(end_arg)
  if not target or not start or not limit or not now or target < 0 or start < 0 or
     limit < 0 or limit > BOUND or (end_arg ~= '' and not finish) then
    return nil, 'invalid'
  end

  local stored = redis.call('HMGET', KEY, 'balance', 'limit', 'bonus', 'cycle_index',
                            'window_start', 'window_end')
  local balance = tonumber(field(stored[1]) or '')
  local own_limit = tonumber(field(stored[2]) or '')
  local bonus = tonumber(field(stored[3]) or '')
  local index = tonumber(field(stored[4]) or '')
  local from = tonumber(field(stored[5]) or '')
  local end_field = field(stored[6])
  local until_ = end_field == nil and nil or (end_field == '' and nil or tonumber(end_field))
  if not balance or not own_limit or not bonus or not index or not from or end_field == nil or
     balance < 0 or own_limit < 0 or bonus < 0 or index < 0 or from < 0 or
     balance > BOUND or own_limit > BOUND or bonus > BOUND or (end_field ~= '' and not until_) then
    return nil, 'state_missing'
  end

  local state = {
    balance = balance, limit = own_limit, bonus = bonus, index = index,
    start = from, finish = until_, verdict = nil,
  }
  if now < from or index > target or (index < target and (until_ == nil or now < until_)) then
    -- The caller is behind the stored state. The state is left exactly as it
    -- stands and the call proceeds against it, so the expiry below still
    -- describes the window that is really stored (INV-C1, C-3, DR-004).
    state.verdict = 'stale'
  elseif index == target then
    state.verdict = 'current'
  else
    -- A boundary closed between the caller choosing its window and this
    -- script running. The allowance is re-granted exactly once, here, and
    -- nowhere else (DR-045). The stored limit is replaced by the effective
    -- one, so a reduction takes effect and the re-grant is of the reduced
    -- figure (DR-018), and the bonus is spent (DR-020).
    state.verdict = 'rolled'
    state.balance = limit
    state.limit = limit
    state.bonus = 0
    state.index = target
    state.start = start
    state.finish = finish
    redis.call('HSET', KEY, 'balance', num(limit), 'limit', num(limit), 'bonus', '0',
               'cycle_index', num(target), 'window_start', num(start), 'window_end', end_arg)
  end

  -- The key lives until 24 hours after its window closes, which is the
  -- idempotency window, and for a window that never closes it never expires
  -- (data-model.md section 6, INV-X5, INV-X6). Set on every call rather than
  -- on rollover alone, because a hash written by the control plane carries no
  -- expiry, and repeating the same value costs one command inside the one
  -- round trip the request is allowed.
  if state.finish == nil then
    redis.call('PERSIST', KEY)
  else
    redis.call('EXPIREAT', KEY, math.floor(state.finish / 1000) + DAY)
  end
  return state, nil
end
-- END transition

local amount = tonumber(ARGV[1])
if not amount or amount < 0 or amount > BOUND or amount ~= math.floor(amount) then
  return answer('invalid', nil)
end

local state, why = transition()
if state == nil then
  return answer(why, nil)
end

-- The same comparison consume makes, without the write. Reporting it rather
-- than failing is the whole point of this script: a caller is told the answer
-- and the remaining balance in the same execution, so it never has to ask
-- twice or guess (DR-026).
if state.balance < amount then
  return answer('denied', state)
end
return answer('applied', state)
