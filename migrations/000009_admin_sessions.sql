-- M5-03：持久化浏览器管理 Session。Cookie 中的 32-byte Session Token
-- 只保存 SHA-256 摘要；CSRF Token 必须在 /auth/me 中恢复，因此保存原始随机字节。
CREATE TABLE admin_sessions (
    id TEXT PRIMARY KEY
        CHECK (
            length(id) = 30
            AND substr(id, 1, 4) = 'ads_'
            AND substr(id, 5, 1) GLOB '[0-7]'
            AND substr(id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    user_id TEXT NOT NULL
        CHECK (
            length(user_id) = 30
            AND substr(user_id, 1, 4) = 'adm_'
            AND substr(user_id, 5, 1) GLOB '[0-7]'
            AND substr(user_id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    token_hash BLOB NOT NULL UNIQUE
        CHECK (typeof(token_hash) = 'blob' AND length(token_hash) = 32),

    csrf_token BLOB NOT NULL
        CHECK (typeof(csrf_token) = 'blob' AND length(csrf_token) = 32),

    expires_at INTEGER NOT NULL CHECK (expires_at > 0),
    created_at INTEGER NOT NULL CHECK (created_at > 0),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at > 0),

    FOREIGN KEY(user_id)
        REFERENCES admin_users(id)
        ON DELETE CASCADE,

    CHECK (expires_at > created_at),
    CHECK (last_seen_at >= created_at AND last_seen_at < expires_at)
);

CREATE INDEX admin_sessions_user
ON admin_sessions(user_id);

CREATE INDEX admin_sessions_expiration
ON admin_sessions(expires_at);

CREATE INDEX admin_sessions_idle_expiration
ON admin_sessions(last_seen_at);
