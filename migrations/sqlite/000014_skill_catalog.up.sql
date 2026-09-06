-- Description: SQLite counterpart of versioned 000086/000087/000088/000089/
-- 000090 — the v0.8.0 skill catalog module. These tables were missing from the
-- SQLite migration set, breaking GET /api/v1/skills/catalog with
-- "no such table: tenant_skill_catalog". Schema below is the consolidated
-- final shape (all columns from the five PG migrations merged into one).
-- tenant_sandbox_configs already exists (000000_init); skills/snapshots/env
-- vars/catalog do not, and fresh Lite deployments have no data to preserve,
-- so a single forward migration is safe.

-- Skills installed onto a sandbox config (metadata projection; files live in
-- the config's snapshot image).
CREATE TABLE IF NOT EXISTS tenant_skills (
    id                    VARCHAR(36)  PRIMARY KEY,
    tenant_id             INTEGER      NOT NULL,
    sandbox_config_id     VARCHAR(36)  NOT NULL,
    name                  VARCHAR(255) NOT NULL,
    version               VARCHAR(64),
    description           TEXT,
    instructions          TEXT,
    bundle_ref            VARCHAR(1024),
    bundle_sha256         VARCHAR(64),
    enabled               BOOLEAN      NOT NULL DEFAULT 1,
    installed_snapshot_id VARCHAR(255),
    install_session_id    VARCHAR(36),
    install_message_id    VARCHAR(36),
    envs                  TEXT,
    catalog_id            VARCHAR(36),
    status                VARCHAR(32)  NOT NULL,
    error                 TEXT,
    installing_since      DATETIME,
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at            DATETIME
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenant_skills_config_name
    ON tenant_skills (sandbox_config_id, name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tenant_skills_catalog
    ON tenant_skills (catalog_id);

-- Image chain ledger. Old snapshots are kept (state=superseded), never deleted
-- on switch, so recorded IDs stay resolvable.
CREATE TABLE IF NOT EXISTS tenant_skill_snapshots (
    id                  VARCHAR(36)  PRIMARY KEY,
    tenant_id           INTEGER      NOT NULL,
    sandbox_config_id   VARCHAR(36)  NOT NULL,
    skill_id            VARCHAR(36),
    snapshot_id         VARCHAR(255),
    parent_snapshot_id  VARCHAR(255),
    generation          INTEGER      NOT NULL DEFAULT 0,
    planned_name        VARCHAR(255),
    trigger             VARCHAR(16)  NOT NULL,
    state               VARCHAR(16)  NOT NULL,
    superseded_at       DATETIME,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tenant_skill_snapshots_config
    ON tenant_skill_snapshots (sandbox_config_id);
CREATE INDEX IF NOT EXISTS idx_tenant_skill_snapshots_state
    ON tenant_skill_snapshots (state);

-- Environment variables for skill execution. Keyed by principal, not user_id:
-- the IM path stores a synthetic tenant account, which would make every IM
-- user of a workspace share one value. `value` is AES-GCM encrypted.
CREATE TABLE IF NOT EXISTS tenant_user_env_vars (
    id                 VARCHAR(36)  PRIMARY KEY,
    tenant_id          INTEGER      NOT NULL,
    principal_type     VARCHAR(32)  NOT NULL,
    principal_id       VARCHAR(512) NOT NULL,
    sandbox_config_id  VARCHAR(36)  NOT NULL,
    skill_id           VARCHAR(36)  NOT NULL DEFAULT '',
    name               VARCHAR(255) NOT NULL,
    value              TEXT,
    created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_user_env_var
    ON tenant_user_env_vars (tenant_id, principal_type, principal_id, sandbox_config_id, skill_id, name);
CREATE INDEX IF NOT EXISTS idx_user_env_var_skill
    ON tenant_user_env_vars (tenant_id, skill_id);
CREATE INDEX IF NOT EXISTS idx_user_env_var_config
    ON tenant_user_env_vars (tenant_id, sandbox_config_id);

-- Workspace-level skill definition. Installations onto sandbox configs live
-- in tenant_skills and point back here via catalog_id.
CREATE TABLE IF NOT EXISTS tenant_skill_catalog (
    id            VARCHAR(36)  PRIMARY KEY,
    tenant_id     INTEGER      NOT NULL,
    name          VARCHAR(255) NOT NULL,
    version       VARCHAR(64),
    description   TEXT,
    instructions  TEXT,
    bundle_ref    VARCHAR(1024),
    bundle_sha256 VARCHAR(64),
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at    DATETIME
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenant_skill_catalog_name
    ON tenant_skill_catalog (tenant_id, name) WHERE deleted_at IS NULL;

-- Backfill: one catalog row per (tenant, name) from pre-catalog installs.
-- Prefer the row that still has a stored archive, then the most recently
-- updated one. Fresh Lite deployments have no rows; kept for parity with
-- versioned 000090 (SQLite has no DISTINCT ON — window function instead).
INSERT OR IGNORE INTO tenant_skill_catalog (
    id, tenant_id, name, version, description, instructions,
    bundle_ref, bundle_sha256, created_at, updated_at
)
SELECT id, tenant_id, name, version, description, instructions,
       bundle_ref, bundle_sha256, created_at, updated_at
FROM (
    SELECT *,
           ROW_NUMBER() OVER (
               PARTITION BY tenant_id, name
               ORDER BY CASE WHEN bundle_ref IS NULL OR bundle_ref = '' THEN 1 ELSE 0 END,
                        updated_at DESC,
                        created_at DESC
           ) AS rn
    FROM tenant_skills
    WHERE deleted_at IS NULL
) WHERE rn = 1;

UPDATE tenant_skills AS s
SET catalog_id = c.id
FROM tenant_skill_catalog AS c
WHERE s.deleted_at IS NULL
  AND c.deleted_at IS NULL
  AND s.tenant_id = c.tenant_id
  AND s.name = c.name
  AND (s.catalog_id IS NULL OR s.catalog_id = '');
