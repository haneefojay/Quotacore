#!lua
-- Flagged, so a store over maxmemory refuses it whole (DR-048); consume.lua
-- says why.
-- rebuild.lua: create a balance hash the fast store lost, and nothing else.
--
-- KEYS[1]  qc:{t:<tenant_id>}:bal:<feature_key>
-- ARGV[1]  balance          the balance the ledger ends at
-- ARGV[2]  limit            the cycle's limit, as the ledger records it
-- ARGV[3]  bonus            the cycle's bonus, as the ledger records it
-- ARGV[4]  index            the cycle's index under the tenant's anchor
-- ARGV[5]  window_start_ms  that window's start
-- ARGV[6]  window_end_ms    that window's end, or '' for never (DR-006)
--
-- A recovery script, run only by `quotacore rebuild --apply` and its operator
-- actions, while the fleet is read-only (IP-11 D6, D7). It is create-only: a
-- hash that exists in any form, whole or corrupt, is left exactly as it is
-- and the answer is exists, so a rebuild can never overwrite a balance a
-- request, an operator or another rebuild wrote. Otherwise it writes the six
-- fields in one execution, so the hash is whole or absent, and keeps no
-- expiry (INV-C3).
--
-- Replies with seven strings: applied, exists or invalid, then the six fields
-- written, which are empty unless applied.

local BOUND = 9007199254740991
local KEY = KEYS[1]

local function num(v)
  return string.format('%d', v)
end

local balance = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local bonus = tonumber(ARGV[3])
local index = tonumber(ARGV[4])
local start = tonumber(ARGV[5])
local end_arg = ARGV[6]
local finish = end_arg == '' and nil or tonumber(end_arg)
if not balance or not limit or not bonus or not index or not start or (end_arg ~= '' and not finish) or
   balance < 0 or limit < 0 or bonus < 0 or index < 0 or start < 0 or
   balance > BOUND or limit > BOUND or bonus > BOUND or (finish and finish <= start) or
   (end_arg == '' and index ~= 0) then
  return { 'invalid', '', '', '', '', '', '' }
end

local stored = redis.call('HMGET', KEY, 'balance', 'limit', 'bonus', 'cycle_index',
                          'window_start', 'window_end')
if stored[1] or stored[2] or stored[3] or stored[4] or stored[5] or stored[6] then
  return { 'exists', '', '', '', '', '', '' }
end

redis.call('HSET', KEY, 'balance', num(balance), 'limit', num(limit), 'bonus', num(bonus),
           'cycle_index', num(index), 'window_start', num(start), 'window_end', end_arg)
redis.call('PERSIST', KEY)
return { 'applied', num(balance), num(limit), num(bonus), num(index), num(start), end_arg }
