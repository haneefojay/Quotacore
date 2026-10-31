#!lua
-- Flagged, so a store over maxmemory refuses it whole (DR-048), exactly as it
-- refuses check.lua; consume.lua says why.
-- check_boolean.lua: a check against a boolean entitlement (DR-050).
--
-- KEYS[1]  qc:{t:<tenant_id>}:bal:<feature_key>, named so the script runs in
--          the tenant's hash slot and never read or written
--
-- An included boolean entitlement is always allowed: there is no counter to
-- read, and inclusion was decided from the configuration before this ran. The
-- script still costs the round trip a metered check costs, deliberately: a
-- boolean check then fails with 503 exactly when a metered one does, so a
-- client's choice to fail open or closed (DR-037) behaves the same for both
-- kinds, and a boolean check is never cheaper in a way that hides an
-- unreachable store (IP-17 DoD 4).
--
-- Replies with one string: applied. It reads and writes nothing.

return { 'applied' }
