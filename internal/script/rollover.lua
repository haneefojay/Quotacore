#!lua
-- Flagged, so a store over maxmemory refuses it whole (DR-048); consume.lua
-- says why.
-- rollover.lua: refill one feature's current window, now.
--
-- KEYS[1]  qc:{t:<tenant_id>}:bal:<feature_key>
-- ARGV[1]  limit            the effective limit (DR-018)
-- ARGV[2]  index            the current window's index under the tenant's anchor
-- ARGV[3]  window_start_ms  that window's start
-- ARGV[4]  window_end_ms    that window's end; never '' (DR-008)
--
-- A control-plane script, run by POST /v1/admin/tenants/{id}/force-rollover
-- (UC-12, IP-10 D5). It sets the window the cycle engine computes, a fresh
-- allowance and no bonus, whatever is stored: that is also the repair for a
-- hash whose stored index is ahead of the anchor's, which the shared
-- transition would leave stale forever (consistency-and-recovery.md section
-- 4.4). It is the one path besides a re-anchor that may lower a stored
-- cycle_index, and it is audited (INV-C1).
--
-- A hash that is absent or unusable is state_missing and is left alone: this
-- is a refill, not a provisioning (DR-045). A `never` entitlement has no
-- window to roll, and the caller refuses it before this script runs; the
-- script refuses it too.
--
-- Replies with nine strings: applied, the transition (rolled), the six fields
-- written, and the balance before the refill.

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
local finish = tonumber(ARGV[4])
if not limit or not index or not start or not finish or limit < 0 or limit > BOUND or index < 0 or
   start < 0 or finish <= start then
  return { 'invalid', '', '', '', '', '', '', '', '' }
end

local stored = redis.call('HMGET', KEY, 'balance', 'limit', 'bonus', 'cycle_index',
                          'window_start', 'window_end')
local balance = tonumber(field(stored[1]) or '')
local own_limit = tonumber(field(stored[2]) or '')
local bonus = tonumber(field(stored[3]) or '')
local own_index = tonumber(field(stored[4]) or '')
local from = tonumber(field(stored[5]) or '')
local end_field = field(stored[6])
if not balance or not own_limit or not bonus or not own_index or not from or end_field == nil or
   balance < 0 or own_limit < 0 or bonus < 0 or own_index < 0 or from < 0 or
   balance > BOUND or own_limit > BOUND or bonus > BOUND then
  return { 'state_missing', '', '', '', '', '', '', '', '' }
end

redis.call('HSET', KEY, 'balance', num(limit), 'limit', num(limit), 'bonus', '0',
           'cycle_index', num(index), 'window_start', num(start), 'window_end', num(finish))
redis.call('PERSIST', KEY)
return { 'applied', 'rolled', num(limit), num(limit), '0', num(index), num(start), num(finish), num(balance) }
