#!lua
-- Flagged, so a store over maxmemory refuses it whole (DR-048); consume.lua
-- says why.
-- consume_boolean.lua: a consume against a boolean entitlement (DR-050).
--
-- KEYS[1]  qc:{t:<tenant_id>}:bal:<feature_key>, named so the script runs in
--          the tenant's hash slot and never read or written: a boolean has no
--          balance hash (DR-045 does not apply to it)
-- KEYS[2]  qc:{t:<tenant_id>}:idem:<idempotency_key>
-- ARGV[1]  amount           0 or 1; anything larger is refused by the caller
--                           as feature_not_metered before this runs
-- ARGV[2]..ARGV[6]          empty; the positions of the metered scripts'
--                           window arguments, which an inclusion has none of
-- ARGV[7]  fingerprint      the operation, tenant, feature and amount, hashed (DR-027)
--
-- A boolean entitlement has no counter, so the only state a consume has is
-- its idempotency record: a retry is a replay, and the same key on a
-- different operation is a reuse, exactly as for a metered consume. The
-- idempotency block below is consume.lua's, byte for byte, and source_test.go
-- fails if the two ever differ. Its remember() is not called here, because it
-- writes the six balance fields and an inclusion has none; the record is
-- written below with the verdict `current` and the six fields empty, which the
-- block's own replay pattern reads back.
--
-- Replies with one string: applied, invalid, or the idempotency block's
-- nine-field replayed or reused answer, whose first field is the decision.

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

local amount = tonumber(ARGV[1])
if amount ~= 0 and amount ~= 1 then
  return { 'invalid' }
end

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

-- The record is the inclusion's whole effect: written once, for a day from
-- first application, so a repeat finds it and returns above (DR-029).
redis.call('SET', KEYS[2], FINGERPRINT .. '\tcurrent\t\t\t\t\t\t', 'EX', DAY)
return { 'applied' }
