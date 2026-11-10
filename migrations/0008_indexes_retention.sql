-- Round-2 audit indexes (task 20261108-routegate-round2-store): the sync
-- worker's per-tick GC deletes sessions on three independent predicates
-- (expires_at / absolute_expires_at / revoked_at — an OR the planner serves
-- as an index-merge union) and instances on updated_at; without these indexes
-- every tick full-scans both tables. The audit list endpoint filters
-- actor_name and orders by id, and the identity list's correlated subquery
-- (policies.caller = identities.name AND policies.enabled = 1) cannot use
-- idx_route_caller, which leads with route_id.
--
-- Each migration runs exactly once (tracked in schema_migrations, applied in
-- one transaction), so no re-run guard is needed — same as the bare ALTER
-- TABLE statements in 0002/0004/0005. MySQL 8 has no CREATE INDEX IF NOT
-- EXISTS anyway.
SET NAMES utf8mb4;

CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);
CREATE INDEX idx_sessions_absolute_expires_at ON sessions (absolute_expires_at);
CREATE INDEX idx_sessions_revoked_at ON sessions (revoked_at);
CREATE INDEX idx_instances_updated_at ON instances (updated_at);
CREATE INDEX idx_admin_audit_actor_id ON admin_audit (actor_name, id);
CREATE INDEX idx_policies_caller_enabled ON policies (caller, enabled);
