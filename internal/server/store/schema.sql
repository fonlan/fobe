-- fobe schema (design.md §6). Applied by store.migrate in order.
-- SQLite, WAL mode; single writer is the server process.

CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    password_hash TEXT NOT NULL,
    totp_secret   TEXT,                -- reserved for v1 (design §19.10)
    must_change   INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    ua         TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT '',
    revoked    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS ip_blacklist (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ip         TEXT NOT NULL UNIQUE,
    reason     TEXT NOT NULL DEFAULT '',
    fail_count INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0  -- 0 = never (manual)
);

CREATE TABLE IF NOT EXISTS settings (
    key       TEXT PRIMARY KEY,
    value     TEXT NOT NULL DEFAULT '',
    encrypted INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS reg_tokens (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',   -- node name applied at registration
    note       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER,
    used_by    TEXT
);

CREATE TABLE IF NOT EXISTS nodes (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    machine_id      TEXT NOT NULL UNIQUE,
    node_secret_hash TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'offline', -- online|offline
    note            TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    last_seen       INTEGER,
    agent_version   TEXT NOT NULL DEFAULT '',
    os              TEXT NOT NULL DEFAULT '',
    arch            TEXT NOT NULL DEFAULT '',
    kernel          TEXT NOT NULL DEFAULT '',
    distro_id       TEXT NOT NULL DEFAULT '',       -- os-release ID, e.g. "debian" (§16 基本信息)
    distro_version  TEXT NOT NULL DEFAULT '',       -- os-release VERSION_ID, e.g. "13" / "24.04"
    hostname        TEXT NOT NULL DEFAULT '',
    cpu_cores       INTEGER NOT NULL DEFAULT 0,
    primary_ip      TEXT NOT NULL DEFAULT '',
    country_code    TEXT NOT NULL DEFAULT '',
    country_manual  INTEGER NOT NULL DEFAULT 0, -- §14 手动指定的国旗,主 IP 变化后不回退
    tz              TEXT NOT NULL DEFAULT 'UTC',
    caps            TEXT NOT NULL DEFAULT '{}',
    -- agent self-update bookkeeping (design §5.5). target is the version this
    -- server wants the probe to run; state is planned|downloading|verifying|
    -- committed|failed|unsupported|suppressed. The agent keeps its own copy in
    -- /etc/fobe-agent/update-state.json; this one lets the panel answer
    -- "why did it not follow?".
    agent_target_version    TEXT NOT NULL DEFAULT '',
    agent_update_state      TEXT NOT NULL DEFAULT '',
    agent_update_attempts   INTEGER NOT NULL DEFAULT 0,
    agent_update_error      TEXT NOT NULL DEFAULT '',
    agent_update_planned_at INTEGER NOT NULL DEFAULT 0,
    agent_update_done_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS node_ips (
    node_id        TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ip             TEXT NOT NULL,
    family         INTEGER NOT NULL,
    scope          TEXT NOT NULL,
    is_primary     INTEGER NOT NULL DEFAULT 0,
    manual_primary INTEGER NOT NULL DEFAULT 0,  -- §14 手动指定的主 IP,全量替换后保留
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (node_id, ip)
);

CREATE TABLE IF NOT EXISTS node_network (
    node_id    TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    iface      TEXT NOT NULL DEFAULT '',
    mode       TEXT NOT NULL DEFAULT 'both', -- in|out|both|max
    quota_bytes INTEGER,                      -- NULL = no quota
    cycle_days INTEGER,                       -- NULL = inherit billing anchor monthly
    anchor_at  INTEGER,                       -- cycle anchor (rolling, design §14)
    cycle_type TEXT NOT NULL DEFAULT 'none',  -- none|month|year
    next_reset_at INTEGER,                    -- next automatic traffic reset (unix seconds)
    tz         TEXT NOT NULL DEFAULT 'UTC'
);

CREATE TABLE IF NOT EXISTS node_interfaces (
    node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    is_default  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (node_id, name)
);

CREATE TABLE IF NOT EXISTS node_billing (
    node_id    TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    cycle_type TEXT NOT NULL DEFAULT 'none',  -- none|month|day|year (记账口径, 不驱动到期计算)
    cycle_days INTEGER,                       -- 周期长度, 单位随 cycle_type (month=月/year=年/day=天)
    next_due_at INTEGER,
    note       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS traffic_counters (
    node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    iface        TEXT NOT NULL,
    direction    TEXT NOT NULL,              -- rx|tx
    last_raw     INTEGER NOT NULL DEFAULT 0,
    last_ts      INTEGER NOT NULL DEFAULT 0,
    period_start INTEGER NOT NULL DEFAULT 0,
    period_used  INTEGER NOT NULL DEFAULT 0, -- cache; traffic_daily is authoritative
    PRIMARY KEY (node_id, iface, direction)
);

CREATE TABLE IF NOT EXISTS traffic_daily (
    node_id  TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    date     TEXT NOT NULL,                  -- YYYY-MM-DD in the node's tz
    rx_bytes INTEGER NOT NULL DEFAULT 0,
    tx_bytes INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, date)
);

CREATE TABLE IF NOT EXISTS metrics_samples (
    node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts          INTEGER NOT NULL,
    cpu         REAL NOT NULL DEFAULT 0,
    mem_used    INTEGER NOT NULL DEFAULT 0,
    mem_total   INTEGER NOT NULL DEFAULT 0,
    disk_used   INTEGER NOT NULL DEFAULT 0,
    disk_total  INTEGER NOT NULL DEFAULT 0,
    net_rx_rate REAL NOT NULL DEFAULT 0,
    net_tx_rate REAL NOT NULL DEFAULT 0,
    load1       REAL NOT NULL DEFAULT 0,
    uptime      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_metrics_node_ts ON metrics_samples(node_id, ts);

CREATE TABLE IF NOT EXISTS latency_targets (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL,                -- icmp|tcp
    host       TEXT NOT NULL,
    port       INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS node_latency_targets (
    node_id   TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    target_id INTEGER NOT NULL REFERENCES latency_targets(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, target_id)
);

CREATE TABLE IF NOT EXISTS latency_samples (
    node_id   TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    target_id INTEGER NOT NULL,
    ts        INTEGER NOT NULL,
    icmp_ms   REAL NOT NULL DEFAULT -1,
    tcp_ms    REAL NOT NULL DEFAULT -1,
    loss      REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_latency_node_target_ts ON latency_samples(node_id, target_id, ts);

CREATE TABLE IF NOT EXISTS subscriptions (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    enabled    INTEGER NOT NULL DEFAULT 1,
    ua_filter  TEXT NOT NULL DEFAULT '',
    template_id TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS subscription_nodes (
    subscription_id TEXT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    node_id         TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    PRIMARY KEY (subscription_id, node_id)
);

CREATE TABLE IF NOT EXISTS sub_access_logs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    subscription_id TEXT NOT NULL,
    ts              INTEGER NOT NULL,
    ip              TEXT NOT NULL DEFAULT '',
    ua              TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS templates (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    format     TEXT NOT NULL,                -- singbox|clash
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS node_singbox (
    node_id          TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    version          TEXT NOT NULL DEFAULT '',
    desired_version  TEXT NOT NULL DEFAULT '',
    config_hash      TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'absent', -- absent|installing|running|degraded|rolled_back
    last_error       TEXT NOT NULL DEFAULT '',
    cert_pem         TEXT NOT NULL DEFAULT '',
    cert_sha256      TEXT NOT NULL DEFAULT '',
    cert_not_after   INTEGER NOT NULL DEFAULT 0,
    port             INTEGER NOT NULL DEFAULT 0,
    firewall_hint     TEXT NOT NULL DEFAULT '', -- §9.2 自动放行失败时的手动命令原文
    password_override TEXT NOT NULL DEFAULT '', -- §19.9 节点级 anytls 密码覆盖(''=全局密码,v1 无 UI)
    updated_at       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS commands (
    id          TEXT PRIMARY KEY,
    node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    payload     TEXT NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'pending', -- pending|sent|ok|failed|timeout
    created_at  INTEGER NOT NULL,
    sent_at     INTEGER,
    finished_at INTEGER,
    result      TEXT NOT NULL DEFAULT '',
    ttl_seconds INTEGER NOT NULL DEFAULT 600,
    actor       TEXT NOT NULL DEFAULT 'panel',
    ai_session_id TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    risk        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_commands_node_status ON commands(node_id, status);

CREATE TABLE IF NOT EXISTS audit_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    actor         TEXT NOT NULL DEFAULT '',      -- panel | ai | cli
    node_id       TEXT NOT NULL DEFAULT '',
    action        TEXT NOT NULL,
    command       TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    risk          TEXT NOT NULL DEFAULT '',
    source_ip     TEXT NOT NULL DEFAULT '',
    ai_session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_logs(ts);

-- AI conversations and confirmation records (design.md §12).
CREATE TABLE IF NOT EXISTS ai_sessions (
    id                   TEXT PRIMARY KEY,
    node_id              TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    terminal_session_id  TEXT NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL,
    last_seen            INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ai_sessions_node ON ai_sessions(node_id, last_seen DESC);

CREATE TABLE IF NOT EXISTS ai_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES ai_sessions(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ai_messages_session ON ai_messages(session_id, id);

CREATE TABLE IF NOT EXISTS ai_pending_actions (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES ai_sessions(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    payload      TEXT NOT NULL DEFAULT '{}',
    reason       TEXT NOT NULL DEFAULT '',
    risk         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending', -- pending|confirmed|rejected
    created_at   INTEGER NOT NULL,
    confirmed_at INTEGER,
    command_id   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_ai_pending_session ON ai_pending_actions(session_id, created_at DESC);

CREATE TABLE IF NOT EXISTS ai_node_state (
    node_id              TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    paused_until          INTEGER NOT NULL DEFAULT 0,
    updated_at            INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS alerts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL,
    node_id      TEXT NOT NULL DEFAULT '',
    payload      TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL,
    delivered_at INTEGER,                        -- NULL = not yet sent
    recovered_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_alerts_created ON alerts(created_at DESC);
