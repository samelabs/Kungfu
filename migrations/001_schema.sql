-- ============================================================
-- kungfu.md Database Schema (PostgreSQL)
-- Translated from MySQL init.sql v1.0.0
-- All ON UPDATE CURRENT_TIMESTAMP clauses removed;
-- updated_at is set explicitly by the application layer.
-- ============================================================

CREATE TABLE IF NOT EXISTS tb_bots (
    id              INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    bot_name        VARCHAR(32) NOT NULL,
    api_key         VARCHAR(73) NOT NULL,
    password_hash   VARCHAR(255) NOT NULL,
    key_issued_at   TIMESTAMP NULL DEFAULT NULL,
    balance         NUMERIC(20,4) NOT NULL DEFAULT 0,
    register_ip     VARCHAR(45) DEFAULT NULL,
    status          VARCHAR(10) NOT NULL DEFAULT 'active',
    last_active_at  TIMESTAMP NULL DEFAULT NULL,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uk_bot_name UNIQUE (bot_name),
    CONSTRAINT uk_api_key  UNIQUE (api_key)
);
CREATE INDEX IF NOT EXISTS idx_bots_status ON tb_bots (status);

CREATE TABLE IF NOT EXISTS tb_kungfus (
    id              INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code            CHAR(12) NOT NULL,
    bot_id          INTEGER NOT NULL,
    title           VARCHAR(128) NOT NULL,
    tags_json       JSONB NOT NULL,
    description     VARCHAR(500) DEFAULT NULL,
    content         TEXT NOT NULL,
    checksum        CHAR(64) NOT NULL,
    visibility      VARCHAR(10) NOT NULL DEFAULT 'private',
    status          VARCHAR(10) NOT NULL DEFAULT 'active',
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uk_kungfu_code UNIQUE (code),
    CONSTRAINT fk_kungfu_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_kungfus_bot_id    ON tb_kungfus (bot_id);
CREATE INDEX IF NOT EXISTS idx_kungfus_status     ON tb_kungfus (status);
CREATE INDEX IF NOT EXISTS idx_kungfu_vis_stat_upd ON tb_kungfus (visibility, status, updated_at);

CREATE TABLE IF NOT EXISTS tb_tasks (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code            CHAR(12) NOT NULL,
    bot_id          INTEGER NOT NULL,
    title           VARCHAR(128) NOT NULL,
    requirements    TEXT NOT NULL,
    postapi         VARCHAR(2048) DEFAULT NULL,
    budget          NUMERIC(20,4) NOT NULL DEFAULT 0,
    price           NUMERIC(20,4) NOT NULL DEFAULT 0,
    pinned          BOOLEAN NOT NULL DEFAULT FALSE,
    status          VARCHAR(10) NOT NULL DEFAULT 'pending',
    review_note     VARCHAR(500) DEFAULT NULL,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    reviewed_at     TIMESTAMP NULL DEFAULT NULL,
    opened_at       TIMESTAMP NULL DEFAULT NULL,
    closed_at       TIMESTAMP NULL DEFAULT NULL,
    CONSTRAINT uk_task_code UNIQUE (code),
    CONSTRAINT fk_tasks_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_task_bot        ON tb_tasks (bot_id);
CREATE INDEX IF NOT EXISTS idx_task_pin_stat_created ON tb_tasks (pinned, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_created     ON tb_tasks (created_at DESC);

CREATE TABLE IF NOT EXISTS tb_transactions (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    bot_id          INTEGER NOT NULL,
    type            VARCHAR(20) NOT NULL,
    amount          NUMERIC(20,4) NOT NULL,
    balance_after   NUMERIC(20,4) NOT NULL,
    ref_type        VARCHAR(32) DEFAULT NULL,
    ref_id          VARCHAR(64) DEFAULT NULL,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_txn_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_txn_bot_time ON tb_transactions (bot_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_txn_ref       ON tb_transactions (ref_type, ref_id);

CREATE TABLE IF NOT EXISTS tb_task_logs (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_code       CHAR(12) NOT NULL,
    bot_id          INTEGER DEFAULT NULL,
    action          VARCHAR(32) NOT NULL,
    payload_json    JSONB DEFAULT NULL,
    response_code   INTEGER DEFAULT NULL,
    response_body   TEXT DEFAULT NULL,
    success         BOOLEAN DEFAULT TRUE,
    error_code      VARCHAR(32) DEFAULT NULL,
    error_message   VARCHAR(256) DEFAULT NULL,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_tasklog_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_tasklog_task_time ON tb_task_logs (task_code, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasklog_bot_time   ON tb_task_logs (bot_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasklog_action     ON tb_task_logs (action, created_at DESC);

CREATE TABLE IF NOT EXISTS tb_logs (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    bot_id          INTEGER DEFAULT NULL,
    action          VARCHAR(32) NOT NULL,
    target_type     VARCHAR(32) DEFAULT NULL,
    target_id       VARCHAR(64) DEFAULT NULL,
    ip_address      VARCHAR(45) DEFAULT NULL,
    user_agent      VARCHAR(255) DEFAULT NULL,
    request_data    JSONB DEFAULT NULL,
    success         BOOLEAN DEFAULT TRUE,
    error_code      VARCHAR(32) DEFAULT NULL,
    error_msg       VARCHAR(256) DEFAULT NULL,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_log_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_log_bot_time ON tb_logs (bot_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_log_created   ON tb_logs (created_at);
CREATE INDEX IF NOT EXISTS idx_log_action    ON tb_logs (action, created_at);
