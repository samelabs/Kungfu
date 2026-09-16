-- ============================================================
-- 006: Platform Admin foundation (B1.1)
-- Establishes the Admin plane as a THIRD identity surface,
-- permanently separate from:
--   Agent plane (X-Bot-Key -> bot_id)
--   Owner plane  (kf_owner cookie -> bot_id)
-- Admin plane: kf_admin cookie -> admin_id. Admins are platform
-- subjects; they carry NO bot_id and cannot be authenticated by
-- X-Bot-Key or owner sessions.
--
-- Scope of this migration: identity, RBAC, server-side sessions,
-- append-only audit. NO store permissions (B2 adds store.* via
-- an additive migration).
-- ============================================================

CREATE TABLE IF NOT EXISTS tb_admins (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username            VARCHAR(64)  NOT NULL,
    display_name        VARCHAR(128) NOT NULL,
    password_hash       VARCHAR(255) NOT NULL,
    status              VARCHAR(16)  NOT NULL DEFAULT 'active',
    auth_version        BIGINT       NOT NULL DEFAULT 1,
    last_login_at       TIMESTAMP    NULL,
    password_changed_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at          TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uk_admin_username UNIQUE (username),
    CONSTRAINT chk_admin_status CHECK (status IN ('active', 'disabled')),
    CONSTRAINT chk_admin_auth_version CHECK (auth_version > 0),
    CONSTRAINT chk_admin_username_format CHECK (username ~ '^[a-z0-9._-]{3,64}$')
);

CREATE INDEX IF NOT EXISTS idx_admins_status ON tb_admins (status);

-- -- RBAC: roles, permissions, bindings --

CREATE TABLE IF NOT EXISTS tb_admin_roles (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code        VARCHAR(64)  NOT NULL,
    name        VARCHAR(128) NOT NULL,
    description VARCHAR(500) DEFAULT NULL,
    is_system   BOOLEAN      NOT NULL DEFAULT FALSE,
    status      VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uk_admin_role_code UNIQUE (code),
    CONSTRAINT chk_admin_role_status CHECK (status IN ('active', 'disabled'))
);

CREATE TABLE IF NOT EXISTS tb_admin_permissions (
    code        VARCHAR(64)  NOT NULL PRIMARY KEY,
    description VARCHAR(500) DEFAULT NULL,
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS tb_admin_role_permissions (
    role_id          BIGINT      NOT NULL REFERENCES tb_admin_roles(id),
    permission_code  VARCHAR(64) NOT NULL REFERENCES tb_admin_permissions(code),
    CONSTRAINT pk_admin_role_permission PRIMARY KEY (role_id, permission_code)
);

CREATE TABLE IF NOT EXISTS tb_admin_user_roles (
    admin_id BIGINT NOT NULL REFERENCES tb_admins(id),
    role_id  BIGINT NOT NULL REFERENCES tb_admin_roles(id),
    CONSTRAINT pk_admin_user_role PRIMARY KEY (admin_id, role_id)
);

CREATE INDEX IF NOT EXISTS idx_admin_user_roles_role ON tb_admin_user_roles (role_id);

-- -- Server-side admin sessions --
-- The admin cookie carries only a raw random token (HttpOnly).
-- The DB stores ONLY SHA-256(token); the raw token never reaches
-- the database or logs.

CREATE TABLE IF NOT EXISTS tb_admin_sessions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    admin_id      BIGINT      NOT NULL REFERENCES tb_admins(id),
    token_hash    VARCHAR(64) NOT NULL,
    auth_version  BIGINT      NOT NULL,
    ip_address    VARCHAR(64) DEFAULT NULL,
    user_agent    VARCHAR(256) DEFAULT NULL,
    created_at    TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at    TIMESTAMP   NOT NULL,
    revoked_at    TIMESTAMP   NULL,
    CONSTRAINT uk_admin_session_token UNIQUE (token_hash)
);

CREATE INDEX IF NOT EXISTS idx_admin_sessions_admin ON tb_admin_sessions (admin_id);

CREATE TABLE IF NOT EXISTS tb_admin_audit_logs (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_admin_id BIGINT      NULL REFERENCES tb_admins(id) ON DELETE SET NULL,
    actor_username VARCHAR(64) NOT NULL,
    action         VARCHAR(64) NOT NULL,
    target_type    VARCHAR(32) DEFAULT NULL,
    target_id      VARCHAR(64) DEFAULT NULL,
    success        BOOLEAN     NOT NULL,
    before_json    JSONB       DEFAULT NULL,
    after_json     JSONB       DEFAULT NULL,
    metadata_json  JSONB       DEFAULT NULL,
    ip_address     VARCHAR(64) DEFAULT NULL,
    user_agent     VARCHAR(256) DEFAULT NULL,
    error_code     VARCHAR(64) DEFAULT NULL,
    created_at     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_admin_audit_actor ON tb_admin_audit_logs (actor_admin_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_action ON tb_admin_audit_logs (action, created_at DESC);

-- Append-only: currently enforced by the architecture guard test
-- (production code contains no UPDATE/DELETE against this table).
-- No DB RULE is used, because the FK's ON DELETE SET NULL requires
-- an internal UPDATE that a rule would intercept, breaking the
-- mandated "admin deletion must not damage the audit trail" semantics.

-- -- Seed: system role, permissions, wildcard binding --

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('*',                  'Wildcard: grants every admin permission'),
    ('admin.users.read',   'View admin accounts'),
    ('admin.users.manage', 'Create, update, enable/disable admin accounts'),
    ('admin.roles.read',   'View roles and permissions'),
    ('admin.roles.manage', 'Create, update roles and manage role assignments'),
    ('admin.sessions.manage', 'Revoke admin sessions and force logout'),
    ('admin.audit.read',   'Read the admin audit trail')
ON CONFLICT (code) DO NOTHING;

INSERT INTO tb_admin_roles (code, name, description, is_system, status) VALUES
    ('superadmin', 'Super Administrator', 'System role bound to the wildcard (*) permission; assigned to the bootstrap admin.', TRUE, 'active')
ON CONFLICT (code) DO NOTHING;

INSERT INTO tb_admin_role_permissions (role_id, permission_code)
SELECT r.id, p.code
FROM tb_admin_roles r, tb_admin_permissions p
WHERE r.code = 'superadmin' AND p.code = '*'
ON CONFLICT DO NOTHING;
