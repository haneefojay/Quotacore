#!lua
-- Flagged, so a store over maxmemory refuses it whole (DR-048); consume.lua
-- says why.
-- refund_rolling.lua: a refund against a rolling-window entitlement (DR-052).
--
-- KEYS[1]  qc:{t:<tenant_id>}:f:<feature_key>, the window's bucket hash
-- ARGV[1]  amount        decimal integer, 0 <= amount <= 2^53-1 (DR-022, DR-023)
-- ARGV[2]  limit         the entitlement's limit for the window
-- ARGV[3]  width_ms      the window, a multiple of sixty milliseconds (DR-052)
-- ARGV[4]  now_ms        the instant the request was accepted, its one clock read
-- KEYS[2]  qc:{t:<tenant_id>}:idem:<idempotency_key>
-- ARGV[5], ARGV[6]       empty; the positions keep the fingerprint at ARGV[7]
-- ARGV[7]  fingerprint   the operation, tenant, feature and amount, hashed (DR-027)
--
-- A refund returns units to the window by taking them out of the newest
-- buckets first, which is the use most likely to be the one being undone. It
-- is all or nothing: a refund larger than the window sum would leave more
-- than the limit remaining, which is the ceiling DR-019 forbids, so it is
-- refused_ceiling and nothing changes. The idempotency block is consume.lua's,
-- byte for byte.
--
-- Replies with nine strings; see answer() below. The decision is applied,
-- refused_ceiling, invalid, state_missing, replayed or reused.

-- BEGIN binding
-- This block is byte-for-byte identical in the eight data-plane scripts:
-- consume, refund, check, consume_boolean, check_boolean, consume_rolling,
-- check_rolling and refund_rolling. source_test.go fails if any two differ.
-- ARGV[8] is the internal id of the one tenant a bound runtime key may
-- address, or '' for a key that reaches every tenant (DR-053). Every key the
-- script was handed must be in that tenant's slot, or it answers foreign and
-- reads and writes nothing, so the binding holds even for a caller that
-- skipped the application's own check. A caller that sent no binding at all
-- is refused as invalid: a missing argument fails closed, never open.
if type(ARGV[8]) ~= 'string' then
  return { 'invalid', '', '', '', '', '', '', '', '' }
end
if ARGV[8] ~= '' then
  local own = 'qc:{t:' .. ARGV[8] .. '}:'
  if string.sub(KEYS[1], 1, #own) ~= own or (KEYS[2] and string.sub(KEYS[2], 1, #own) ~= own) then
    return { 'foreign', '', '', '', '', '', '', '', '' }
  end
end
-- END binding

local DAY = 86400

-- BEGIN window
-- This block is byte-for-byte identical in consume_rolling.lua,
-- check_rolling.lua and refund_rolling.lua, and rolling_test.go fails if any
-- two ever differ. It is the whole of the rolling-window arithmetic (DR-052):
-- the bucket the instant falls in, the horizon at and before which a bucket has
-- left the window, and the sum of the buckets after it. Nothing here reads a
-- calendar, an anchor or a cycle index, because a rolling window has none and
-- shares no rule with the anchored scripts (ADR-0008).
local BOUND = 9007199254740991
local BUCKETS = 60

-- The bucket hash, taken once from KEYS[1]: field = a bucket's start in Unix
-- milliseconds, value = the units consumed in it.
local KEY = KEYS[1]

local function num(v)
  return string.format('%d', v)
end

-- An integral number in [low, high], or nil. Every argument is a decimal
-- integer the runner formatted, so anything else is a caller that bypassed it.
local function int(v, low, high)
  local n = tonumber(v)
  if not n or n ~= math.floor(n) or n < low or n > high then
    return nil
  end
  return n
end

-- The window as it stands at the instant: the amount, the limit, the width and
-- its bucket, the current bucket, the horizon, the sum of every bucket after
-- the horizon with each one's units, the oldest and newest such bucket, and the
-- fields at or before the horizon, which have left the window and are the only
-- ones a mutation prunes. A bucket ahead
-- of the instant, written by a host whose clock runs fast, is after the horizon
-- and is counted: a skew can make the sum larger, never quietly smaller, and
-- the skew alert bounds it (NFR-D8).
local function window()
  local w = {
    amount = int(ARGV[1], 0, BOUND),
    limit = int(ARGV[2], 0, BOUND),
    width = int(ARGV[3], BUCKETS, BOUND),
    now = int(ARGV[4], 0, BOUND),
  }
  if not w.amount or not w.limit or not w.width or not w.now or w.width % BUCKETS ~= 0 then
    return nil, 'invalid'
  end
  w.bucket = w.width / BUCKETS
  w.current = w.now - (w.now % w.bucket)
  w.horizon = w.current - w.width
  w.sum = 0
  w.stale = {}
  w.counted = {}
  local raw = redis.call('HGETALL', KEY)
  for i = 1, #raw, 2 do
    local start = int(raw[i], 0, BOUND)
    local units = int(raw[i + 1], 0, BOUND)
    if not start or not units then
      return nil, 'state_missing'
    end
    if start <= w.horizon then
      w.stale[#w.stale + 1] = raw[i]
    else
      w.sum = w.sum + units
      w.counted[#w.counted + 1] = { start = start, units = units }
      if not w.oldest or start < w.oldest then
        w.oldest = start
      end
      if not w.newest or start > w.newest then
        w.newest = start
      end
    end
  end
  return w, nil
end

-- The reply, always nine strings, in the anchored scripts' positions so the
-- idempotency block's replay reads back the same shape: decision, verdict,
-- remaining, limit, bonus (always 0), window sum, window start, next release,
-- and an empty ninth. The remaining is the limit less the sum, never below
-- zero; the next release is when the oldest counted bucket leaves the window,
-- empty when nothing is counted.
local function state_of(w)
  local remaining = w.limit - w.sum
  if remaining < 0 then
    remaining = 0
  end
  return {
    verdict = 'current', balance = remaining, limit = w.limit, bonus = 0, index = w.sum,
    start = w.current + w.bucket - w.width,
    finish = w.oldest and (w.oldest + w.width) or nil,
  }
end

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
  end
  return reply
end
-- END window

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

-- Drop the buckets that have left the window, and keep the key no longer than
-- its newest bucket is useful: it expires the instant that bucket leaves the
-- window, so an idle tenant-feature holds no key at all, and a busy one holds
-- at most the sixty buckets of one window (IP-19 DoD 4).
local function settle(w)
  if #w.stale > 0 then
    redis.call('HDEL', KEY, unpack(w.stale))
  end
  if w.newest then
    redis.call('PEXPIREAT', KEY, num(w.newest + w.width))
  end
end

local w, why = window()
if w == nil then
  return answer(why, nil)
end

if w.amount > w.sum then
  return answer('refused_ceiling', state_of(w))
end

settle(w)
if w.amount > 0 then
  -- The counted buckets, newest first. There are at most sixty of them plus
  -- any a fast clock wrote, because settle() has just removed the rest.
  table.sort(w.counted, function(a, b) return a.start > b.start end)
  local left = w.amount
  w.oldest = nil
  w.newest = nil
  for _, b in ipairs(w.counted) do
    local take = 0
    if left > 0 then
      take = b.units
      if take > left then
        take = left
      end
      left = left - take
      if take == b.units then
        redis.call('HDEL', KEY, num(b.start))
      else
        redis.call('HINCRBY', KEY, num(b.start), num(-take))
      end
    end
    if take < b.units then
      if not w.oldest or b.start < w.oldest then
        w.oldest = b.start
      end
      if not w.newest or b.start > w.newest then
        w.newest = b.start
      end
    end
  end
  w.sum = w.sum - w.amount
  if w.newest then
    redis.call('PEXPIREAT', KEY, num(w.newest + w.width))
  end
end
local state = state_of(w)
remember(state)
return answer('applied', state)
