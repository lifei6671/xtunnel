-- M3-01：Service 直接归属 Tunnel；Origin、Health 与版本字段由同一行持久化。
-- V0.1 不存在 tunnel_bindings，中间关联表也不得由后续业务代码补建。
CREATE TABLE services (
    -- svc_ + 26 位大写 Crockford ULID。
    id TEXT PRIMARY KEY NOT NULL
        CHECK (
            length(id) = 30
            AND substr(id, 1, 4) = 'svc_'
            AND substr(id, 5, 1) GLOB '[0-7]'
            AND substr(id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    tunnel_id TEXT NOT NULL,
    name TEXT NOT NULL
        CHECK (length(trim(name, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0),
    required_revision INTEGER NOT NULL DEFAULT 0 CHECK (required_revision >= 0),

    origin_scheme TEXT NOT NULL
        CHECK (origin_scheme IN ('http', 'https', 'tcp', 'udp', 'quic', 'unix')),
    origin_host TEXT CHECK (
        origin_host IS NULL
        OR length(trim(origin_host, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0
    ),
    origin_port INTEGER CHECK (origin_port IS NULL OR origin_port BETWEEN 1 AND 65535),
    -- unix 只表示文件系统 SOCK_STREAM；不使用假 Host/Port，也不接受抽象 Namespace。
    origin_path TEXT CHECK (
        origin_path IS NULL
        OR (
            length(origin_path) > 1
            AND substr(origin_path, 1, 1) = '/'
            AND instr(origin_path, char(0)) = 0
        )
    ),
    -- quic 表示 Agent 原生 QUIC Dial；透明 QUIC 流量应使用 udp。
    origin_quic_alpn TEXT CHECK (
        origin_quic_alpn IS NULL
        OR (
            length(CAST(origin_quic_alpn AS BLOB)) BETWEEN 1 AND 255
            AND length(trim(origin_quic_alpn, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0
            AND instr(origin_quic_alpn, char(0)) = 0
        )
    ),
    tls_verify INTEGER NOT NULL DEFAULT 1 CHECK (tls_verify IN (0, 1)),
    tls_server_name TEXT CHECK (
        tls_server_name IS NULL
        OR length(trim(tls_server_name, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0
    ),
    origin_http_host TEXT CHECK (
        origin_http_host IS NULL
        OR length(trim(origin_http_host, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0
    ),
    connect_timeout_ms INTEGER NOT NULL DEFAULT 5000
        CHECK (connect_timeout_ms BETWEEN 1 AND 4294967295),

    -- NULL health_type 是唯一的 Disabled 表示；此时其余 Health 列必须全部为空。
    health_type TEXT CHECK (health_type IS NULL OR health_type IN ('TCP', 'HTTP')),
    health_path TEXT,
    health_interval_ms INTEGER,
    health_timeout_ms INTEGER,
    health_expected_status_min INTEGER,
    health_expected_status_max INTEGER,
    health_failure_threshold INTEGER,
    health_success_threshold INTEGER,

    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at INTEGER NOT NULL CHECK (created_at > 0),
    updated_at INTEGER NOT NULL CHECK (updated_at > 0),

    FOREIGN KEY(tunnel_id) REFERENCES tunnels(id) ON DELETE RESTRICT,

    CHECK (
        (
            origin_scheme IN ('http', 'https', 'tcp', 'udp', 'quic')
            AND origin_host IS NOT NULL
            AND origin_port IS NOT NULL
            AND origin_path IS NULL
        )
        OR (
            origin_scheme = 'unix'
            AND origin_host IS NULL
            AND origin_port IS NULL
            AND origin_path IS NOT NULL
        )
    ),

    CHECK (
        (
            origin_scheme = 'http'
            AND tls_server_name IS NULL
            AND origin_quic_alpn IS NULL
        )
        OR (
            origin_scheme = 'https'
            AND origin_quic_alpn IS NULL
        )
        OR (
            origin_scheme IN ('tcp', 'udp', 'unix')
            AND tls_server_name IS NULL
            AND origin_http_host IS NULL
            AND origin_quic_alpn IS NULL
        )
        OR (
            origin_scheme = 'quic'
            AND origin_http_host IS NULL
            AND origin_quic_alpn IS NOT NULL
        )
    ),

    CHECK (
        (
            health_type IS NULL
            AND health_path IS NULL
            AND health_interval_ms IS NULL
            AND health_timeout_ms IS NULL
            AND health_expected_status_min IS NULL
            AND health_expected_status_max IS NULL
            AND health_failure_threshold IS NULL
            AND health_success_threshold IS NULL
        )
        OR (
            health_type IS NOT NULL
            AND health_type = 'TCP'
            AND health_path IS NULL
            AND health_interval_ms IS NOT NULL
            AND health_interval_ms BETWEEN 1000 AND 3600000
            AND health_timeout_ms IS NOT NULL
            AND health_timeout_ms BETWEEN 100 AND health_interval_ms - 1
            AND health_expected_status_min IS NULL
            AND health_expected_status_max IS NULL
            AND health_failure_threshold IS NOT NULL
            AND health_failure_threshold BETWEEN 1 AND 20
            AND health_success_threshold IS NOT NULL
            AND health_success_threshold BETWEEN 1 AND 20
        )
        OR (
            health_type IS NOT NULL
            AND health_type = 'HTTP'
            AND health_path IS NOT NULL
            AND length(health_path) > 0
            AND substr(health_path, 1, 1) = '/'
            AND health_interval_ms IS NOT NULL
            AND health_interval_ms BETWEEN 1000 AND 3600000
            AND health_timeout_ms IS NOT NULL
            AND health_timeout_ms BETWEEN 100 AND health_interval_ms - 1
            AND health_expected_status_min IS NOT NULL
            AND health_expected_status_min BETWEEN 100 AND 599
            AND health_expected_status_max IS NOT NULL
            AND health_expected_status_max BETWEEN health_expected_status_min AND 599
            AND health_failure_threshold IS NOT NULL
            AND health_failure_threshold BETWEEN 1 AND 20
            AND health_success_threshold IS NOT NULL
            AND health_success_threshold BETWEEN 1 AND 20
        )
    ),

    -- UDP、QUIC 与 Unix Health 语义留给后续协议版本冻结；预留行只能 Disabled。
    CHECK (origin_scheme IN ('http', 'https', 'tcp') OR health_type IS NULL)
);

-- Tunnel Snapshot 与容量校验都按 Tunnel 枚举 Service；ID 作为稳定次序的第二键。
CREATE INDEX services_by_tunnel_id
ON services(tunnel_id, id);
