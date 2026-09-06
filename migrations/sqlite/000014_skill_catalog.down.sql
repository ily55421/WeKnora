-- Description: Reverse of 000014 — drop the skill catalog module tables.
-- Backfilled catalog rows are aggregates of tenant_skills, so dropping the
-- module wholesale (not per-table) is the only meaningful rollback.

DROP TABLE IF EXISTS tenant_user_env_vars;
DROP TABLE IF EXISTS tenant_skill_catalog;
DROP TABLE IF EXISTS tenant_skill_snapshots;
DROP TABLE IF EXISTS tenant_skills;
