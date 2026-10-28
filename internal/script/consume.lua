#!lua
-- The shebang declares this a flagged script, which the store refuses whole,
-- before its first command, while it is over maxmemory (DR-048). Without it
-- the store checks memory only at a script's first write, and only when that
-- write is one that can grow memory: PERSIST is not, and the transition block
-- runs one on every call, so a full store took every consume after it and
-- noeviction refused nothing (IP-15, T-13).
-- consume.lua: the deduction, as one indivisible step.
--
-- KEYS[1]  qc:{t:<tenant_id>}:bal:<feature_key>
-- KEYS[2]  qc:{t:<tenant_id>}:idem:<idempotency_key>
-- ARGV[1]  amount           decimal integer, 0 <= amount <= 2^53-1 (DR-022, DR-023)
-- ARGV[2]  target_index     the index the cycle engine gave the caller (DR-004)
-- ARGV[3]  window_start_ms  the start of that window (DR-005)
-- ARGV[4]  window_end_ms    the end of that window, or '' for never (DR-006)
-- ARGV[5]  effective_limit  the limit for that window, after any reduction (DR-018)
-- ARGV[6]  now_ms           the instant the caller chose its window with (DR-004)
-- ARGV[7]  fingerprint      the operation, tenant, feature and amount, hashed (DR-027)
--
-- The keys arrive as KEYS and every value as ARGV. Nothing here is assembled
-- from request text, so nothing a caller sends is ever parsed as Lua (ADR-0002,
-- testing-strategy section 4).
--
-- Replies with nine strings, always nine, in this order: decision,
-- transition, balance, limit, bonus, cycle_index, window_start_ms,
-- window_end_ms, closed_balance. The closed_balance is empty unless the
-- transition is rolled, and a replay leaves it empty because it rolled nothing.
-- The decision is applied, denied, refused_ceiling, state_missing, invalid,
-- replayed or reused; the transition is current, rolled or stale. The state_missing and invalid decisions leave the six values empty
-- rather than zero, so a caller that ignored the decision gets a parse failure
-- and not a plausible balance; the reused decision does the same. A replayed
-- decision carries the state that was recorded when the operation was first
-- applied, not the state as it stands now (DR-027).

-- BEGIN transition
-- This block is byte-for-byte identical in consume.lua, refund.lua,
-- check.lua, grant.lua, set.lua and relimit.lua, and source_test.go fails if
-- any two ever differ. Six copies exist because Redis Lua has no include and a
-- script is loaded whole, and one script per operation is what performance.md
-- section 5 asks for. The duplication is what INV-C2 requires: there is no
-- second implementation of rollover to drift out of agreement with the first.
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

-- The ninth value is the balance a rollover discarded, empty unless the
-- verdict is rolled. The caller records it as the closing of the cycle that
-- ended, so the ledger shows every opening as a row (ADR-0016, DR-042, U-6).
local function answer(decision, state)
  local reply = { decision, '', '', '', '', '', '', '', '' }
  if state then
    reply[2] = state.verdict
    reply[3] = num(state.balance)
    reply[4] = num(state.limit)
    reply[5] = num(state.bonus)
    reply[6] = num(state.index)
    reply[7] = num(state.start)
    reply[8] = state.finish and num(state.finish) or ''
    reply[9] = state.closed and num(state.closed) or ''
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
    -- stands and the call proceeds against it, so the answer describes the
    -- window that is really stored (INV-C1, C-3, DR-004).
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
    state.closed = state.balance
    state.balance = limit
    state.limit = limit
    state.bonus = 0
    state.index = target
    state.start = start
    state.finish = finish
    redis.call('HSET', KEY, 'balance', num(limit), 'limit', num(limit), 'bonus', '0',
               'cycle_index', num(target), 'window_start', num(start), 'window_end', end_arg)
  end

  -- A balance hash never expires (INV-C3, as amended 2026-10-20). An expiry
  -- at window_end + 24h removed an idle tenant's hash a day after its window
  -- closed, and nothing on the request path may write one again (DR-045), so
  -- the tenant was refused until a rebuild. PERSIST runs on every call, so a
  -- hash an earlier binary wrote with an expiry loses it at its next request.
  redis.call('PERSIST', KEY)
  return state, nil
end
-- END transition

local amount = tonumber(ARGV[1])
if not amount or amount < 0 or amount > BOUND or amount ~= math.floor(amount) then
  return answer('invalid', nil)
end

-- A bad amount is refused before the transition runs, so a request that cannot
-- be honoured rolls no cycle and writes nothing (DR-025).
-- BEGIN idempotency
-- This block is byte-for-byte identical in consume.lua and refund.lua, and
-- source_test.go fails if the two ever differ. A check has no copy: it changes
-- nothing, so it has nothing to replay and nothing to record (DR-026). The
-- lookup is deliberately before transition(), because a retry must return the
-- answer the first attempt recorded, and letting a rollover run first would
-- let a replay report a balance this request never touched.
local FINGERPRINT = ARGV[7]
local existing = redis.call('GET', KEYS[2])
if existing then
  local split = string.find(existing, '\t', 1, true)
  if split and string.sub(existing, 1, split - 1) == FINGERPRINT then
    -- The same logical operation. Return what was recorded and write nothing:
    -- the decision is replayed and its seven values are the stored ones, so a
    -- retrying client is charged once and told the truth about the charge that
    -- already happened (DR-027, T-02). No expiry is refreshed here, so a repeat
    -- cannot extend the window (DR-029).
    local verdict, balance, limit, bonus, index, start, finish = string.match(
      string.sub(existing, split + 1),
      '([^\t]*)\t([^\t]*)\t([^\t]*)\t([^\t]*)\t([^\t]*)\t([^\t]*)\t([^\t]*)')
    if verdict then
      return { 'replayed', verdict, balance, limit, bonus, index, start, finish, '' }
    end
  end
  -- A key already used for a different operation, or a record this script
  -- cannot read back. Refuse, and touch nothing, so the record that is already
  -- there still answers the operation it belongs to (DR-028, INV-I2).
  return { 'reused', '', '', '', '', '', '', '', '' }
end

-- Record the outcome of a mutation that was applied, so a repeat of it is a
-- replay rather than a second charge. Only an applied mutation is recorded: a
-- denial, a refused ceiling or a stale state are answers, not changes, and a
-- record of one would let a later retry replay a charge that never happened
-- (ADR-0004). The separator is a tab, which cannot occur in the fingerprint,
-- the verdict or the six decimal numbers. The window is EX DAY from first
-- write, and this function is only reached on a fresh application, so a repeat
-- finds the record and returns before this line rather than extending it.
local function remember(state)
  redis.call('SET', KEYS[2],
             FINGERPRINT .. '\t' .. state.verdict .. '\t' .. num(state.balance) .. '\t' ..
             num(state.limit) .. '\t' .. num(state.bonus) .. '\t' .. num(state.index) .. '\t' ..
             num(state.start) .. '\t' .. (state.finish and num(state.finish) or ''),
             'EX', DAY)
end
-- END idempotency

local state, why = transition()
if state == nil then
  return answer(why, nil)
end

if state.balance < amount then
  return answer('denied', state)
end

-- One write, and the balance cannot go below zero between the check and the
-- write because they are the same execution (INV-C2). An amount of zero is a
-- valid request that changes no balance and records no event (DR-023).
-- The increment is formatted as an integer: Lua renders the negation of zero
-- as -0, which HINCRBY refuses as not an integer.
redis.call('HINCRBY', KEY, 'balance', num(-amount))
-- The reply reports the balance the key holds now, not the one it held before
-- this script ran, or a caller that spent the last unit would be told it still
-- has one (INV-C2). The value is what HINCRBY computed, kept in step here rather
-- than read back, so the answer costs no second round trip.
state.balance = state.balance - amount
-- The record is written in the same execution as the write it describes, so
-- the store can never hold a charge with no record of it or a record with no
-- charge (ADR-0002, INV-I1).
remember(state)
return answer('applied', state)
