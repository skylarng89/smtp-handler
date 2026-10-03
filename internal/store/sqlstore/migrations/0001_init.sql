CREATE TABLE IF NOT EXISTS messages (
    id               TEXT PRIMARY KEY,
    project_id       TEXT    NOT NULL,
    template         TEXT    NOT NULL DEFAULT '',
    key_id           TEXT    NOT NULL DEFAULT '',
    state            TEXT    NOT NULL,
    version          BIGINT  NOT NULL DEFAULT 1,
    attempt          INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL,
    max_age          BIGINT  NOT NULL,
    next_attempt_at  BIGINT  NOT NULL,
    deadline_at      BIGINT  NOT NULL,
    lease_owner      TEXT    NOT NULL DEFAULT '',
    lease_token      TEXT    NOT NULL DEFAULT '',
    lease_until      BIGINT  NOT NULL DEFAULT 0,
    message_id_hdr   TEXT    NOT NULL,
    subject          TEXT    NOT NULL DEFAULT '',
    from_addr        TEXT    NOT NULL DEFAULT '',
    to_addrs         TEXT    NOT NULL DEFAULT '',
    payload          TEXT    NOT NULL DEFAULT '',
    body_dropped     INTEGER NOT NULL DEFAULT 0,
    last_error_class TEXT    NOT NULL DEFAULT '',
    last_error_code  INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT    NOT NULL DEFAULT '',
    created_at       BIGINT  NOT NULL,
    updated_at       BIGINT  NOT NULL,
    sent_at          BIGINT  NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_messages_due ON messages (state, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_messages_lease ON messages (state, lease_until);
CREATE INDEX IF NOT EXISTS idx_messages_project ON messages (project_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_owner ON messages (lease_owner);
CREATE INDEX IF NOT EXISTS idx_messages_sent ON messages (state, sent_at);
CREATE INDEX IF NOT EXISTS idx_messages_updated ON messages (state, updated_at);

CREATE TABLE IF NOT EXISTS attempts (
    message_id    TEXT    NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    attempt       INTEGER NOT NULL,
    node          TEXT    NOT NULL,
    started_at    BIGINT  NOT NULL,
    finished_at   BIGINT  NOT NULL,
    outcome       TEXT    NOT NULL,
    smtp_code     INTEGER NOT NULL DEFAULT 0,
    enhanced_code TEXT    NOT NULL DEFAULT '',
    error         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (message_id, started_at, node, outcome)
);

CREATE TABLE IF NOT EXISTS idempotency (
    project_id  TEXT   NOT NULL,
    idem_key    TEXT   NOT NULL,
    fingerprint TEXT   NOT NULL,
    message_id  TEXT   NOT NULL,
    expires_at  BIGINT NOT NULL,
    PRIMARY KEY (project_id, idem_key)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_expires ON idempotency (expires_at);

CREATE TABLE IF NOT EXISTS rate_counters (
    bucket       TEXT    NOT NULL,
    window_start BIGINT  NOT NULL,
    count        INTEGER NOT NULL,
    expires_at   BIGINT  NOT NULL,
    PRIMARY KEY (bucket, window_start)
);

CREATE INDEX IF NOT EXISTS idx_rate_counters_expires ON rate_counters (expires_at);

CREATE TABLE IF NOT EXISTS smtp_slots (
    project_id  TEXT    NOT NULL,
    slot        INTEGER NOT NULL,
    lease_owner TEXT    NOT NULL,
    lease_until BIGINT  NOT NULL,
    PRIMARY KEY (project_id, slot)
);
