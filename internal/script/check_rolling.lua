#!lua
-- Flagged, so a store over maxmemory refuses it whole (DR-048), exactly as it
-- refuses check.lua; consume.lua says why.
-- check_rolling.lua: a check against a rolling-window entitlement (DR-052).
--
-- KEYS[1]  qc:{t:<tenant_id>}:f:<feature_key>, the window's bucket hash
-- ARGV[1]  amount        decimal integer, 0 <= amount <= 2^53-1 (DR-022, DR-023)
-- ARGV[2]  limit         the entitlement's limit for the window
-- ARGV[3]  width_ms      the window, a multiple of sixty milliseconds (DR-052)
-- ARGV[4]  now_ms        the instant the request was accepted, its one clock read
--
-- Reports whether the amount would fit in the window now, and writes nothing:
-- not even the prune, so a check changes no state at all (DR-026). The buckets
-- that have left the window are skipped by the sum whether or not they have
-- been removed yet.
--
-- Replies with nine strings; see answer() below. The decision is applied
-- (it would fit), denied, invalid or state_missing.

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

local w, why = window()
if w == nil then
  return answer(why, nil)
end
if w.sum + w.amount > w.limit then
  return answer('denied', state_of(w))
end
return answer('applied', state_of(w))
