-- M1-01：Tunnel 与可重复获取的 Connection Token 持久化模型。
-- Connector、Instance、Session、实时状态和认证 Secret 明文均不得写入 SQLite。
CREATE TABLE tunnels (
    -- tun_ + 26 位大写 Crockford ULID。
    id TEXT PRIMARY KEY
        CHECK (
            length(id) = 30
            AND substr(id, 1, 4) = 'tun_'
            AND substr(id, 5, 1) GLOB '[0-7]'
            AND substr(id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    -- Management 平面展示的隧道名称。
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),

    -- Tunnel Aggregate 乐观锁版本，与 Token Credential Version 严格区分。
    version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    desired_revision INTEGER NOT NULL DEFAULT 0 CHECK (desired_revision >= 0),
    revoked_at INTEGER CHECK (revoked_at IS NULL OR revoked_at > 0),
    created_at INTEGER NOT NULL CHECK (created_at > 0),
    updated_at INTEGER NOT NULL CHECK (updated_at > 0)
);

CREATE TABLE tunnel_tokens (
    -- tok_ + 26 位大写 Crockford ULID。
    id TEXT PRIMARY KEY
        CHECK (
            length(id) = 30
            AND substr(id, 1, 4) = 'tok_'
            AND substr(id, 5, 1) GLOB '[0-7]'
            AND substr(id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    tunnel_id TEXT NOT NULL,
    -- SHA-256(认证 Secret) 用于认证，不需要解密完整 Token。
    secret_hash BLOB NOT NULL UNIQUE CHECK (length(secret_hash) = 32),
    -- 完整 Token 只以 AES-256-GCM 密文保存；nonce 由密文封装格式携带。
    token_ciphertext BLOB NOT NULL CHECK (length(token_ciphertext) > 28),
    -- 同一 Tunnel 内从 1 起递增的 Credential 代次。
    version INTEGER NOT NULL CHECK (version >= 1),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'REVOKED_FOR_NEW_SESSION', 'REVOKED')),
    created_at INTEGER NOT NULL CHECK (created_at > 0),
    revoked_at INTEGER CHECK (revoked_at IS NULL OR revoked_at > 0),

    FOREIGN KEY(tunnel_id) REFERENCES tunnels(id) ON DELETE CASCADE,
    UNIQUE(tunnel_id, version),
    CHECK (
        (status = 'ACTIVE' AND revoked_at IS NULL)
        OR (status IN ('REVOKED_FOR_NEW_SESSION', 'REVOKED') AND revoked_at IS NOT NULL)
    )
);

-- 同一 Tunnel 下的全部 Connector 共用当前 Credential，因此最多只能有一个 ACTIVE Token。
CREATE UNIQUE INDEX one_active_token_per_tunnel
ON tunnel_tokens(tunnel_id)
WHERE status = 'ACTIVE';
