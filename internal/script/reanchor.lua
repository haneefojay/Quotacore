-- reanchor.lua: rewrite a balance hash for a tenant's new anchor.
--
-- KEYS[1]  qc:{t:<tenant_id>}:bal:<feature_key>
-- ARGV[1]  limit            the effective limit under the new plan (DR-013)
-- ARGV[2]  index            the current window's index under the new anchor
-- ARGV[3]  window_start_ms  that window's start
-- ARGV[4]  window_end_ms    that window's end, or '' for never (DR-006)
-- ARGV[5]  expect           'absent', 'any', or the cycle_index the caller read
-- ARGV[6]  expect_start_ms  the window_start the caller read, with an index
--
-- A control-plane script, run by the re-anchor drain after a plan change or an
-- apply-now has committed in Postgres (IP-10 D2). It writes all six fields and
-- clears any expiry, so the hash is whole or untouched, and it is the plan-assignment
-- moment DR-045 names: the one other writer, besides provisioning, that may
-- materialise a hash.
--
-- It is a compare-and-set. The caller read the hash, decided in Go, with the
-- cycle engine, that it is not already one of the new anchor's windows, and
-- passes what it read as the expectation; if the stored window has moved
-- since, because a request rolled it in between, nothing is written and the
-- answer is moved, so the caller reads again. That is what makes a retried
-- drain, or two drains at once, rewrite a tenant once and never reset a cycle
-- the new anchor already opened. 'any' is for a corrupt hash, which no request
-- writes to, and 'absent' for a feature that has no hash yet.
--
-- Replies with nine strings: applied or moved, the transition (rolled), the
-- six fields written, and the balance that was discarded, or '' when there was
-- none to read. A moved answer carries nothing else.

local BOUND = 9007199254740991
local KEY = KEYS[1]

local function num(v)
  return string.format('%d', v)
end

local function field(v)
  if type(v) ~= 'string' then
    return nil
  end
  return v
end

local limit = tonumber(ARGV[1])
local index = tonumber(ARGV[2])
local start = tonumber(ARGV[3])
local end_arg = ARGV[4]
local finish = end_arg == '' and nil or tonumber(end_arg)
local expect = ARGV[5]
if not limit or not index or not start or limit < 0 or limit > BOUND or index < 0 or start < 0 or
   (end_arg ~= '' and (not finish or finish <= start)) or (end_arg == '' and index ~= 0) or
   not expect or expect == '' then
  return { 'invalid', '', '', '', '', '', '', '', '' }
end

local stored = redis.call('HMGET', KEY, 'balance', 'limit', 'bonus', 'cycle_index',
                          'window_start', 'window_end')
local present = field(stored[1]) ~= nil or field(stored[2]) ~= nil or field(stored[3]) ~= nil or
                field(stored[4]) ~= nil or field(stored[5]) ~= nil or field(stored[6]) ~= nil
if expect == 'absent' then
  if present then
    return { 'moved', '', '', '', '', '', '', '', '' }
  end
elseif expect ~= 'any' then
  if field(stored[4]) ~= expect or field(stored[5]) ~= ARGV[6] then
    return { 'moved', '', '', '', '', '', '', '', '' }
  end
end

local before = tonumber(field(stored[1]) or '')
redis.call('HSET', KEY, 'balance', num(limit), 'limit', num(limit), 'bonus', '0',
           'cycle_index', num(index), 'window_start', num(start), 'window_end', end_arg)
redis.call('PERSIST', KEY)
local discarded = ''
if before and before >= 0 and before <= BOUND then
  discarded = num(before)
end
return { 'applied', 'rolled', num(limit), num(limit), '0', num(index), num(start), end_arg, discarded }
