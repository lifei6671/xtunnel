-- V0.1 Service 代理参数与 Origin/Health 一样属于 Desired State；Snapshot 构建只能
-- 从 services 行读取这些类型化列，不得另建 JSON 配置或进程内第二权威。
ALTER TABLE services ADD COLUMN disable_chunked_encoding INTEGER NOT NULL DEFAULT 0
    CHECK (disable_chunked_encoding IN (0, 1))
    CHECK (origin_scheme IN ('http', 'https') OR disable_chunked_encoding = 0);

ALTER TABLE services ADD COLUMN disable_happy_eyeballs INTEGER NOT NULL DEFAULT 0
    CHECK (disable_happy_eyeballs IN (0, 1));

ALTER TABLE services ADD COLUMN http_idle_connection_timeout_ms INTEGER NOT NULL DEFAULT 90000
    CHECK (http_idle_connection_timeout_ms BETWEEN 1 AND 4294967295)
    CHECK (origin_scheme IN ('http', 'https') OR http_idle_connection_timeout_ms = 90000);

ALTER TABLE services ADD COLUMN http_max_idle_connections INTEGER NOT NULL DEFAULT 100
    CHECK (http_max_idle_connections BETWEEN 1 AND 4294967295)
    CHECK (origin_scheme IN ('http', 'https') OR http_max_idle_connections = 100);

-- 0 显式关闭 TCP Keepalive；非零值使用毫秒，uint32 上界与 Go 领域类型一致。
ALTER TABLE services ADD COLUMN tcp_keepalive_interval_ms INTEGER NOT NULL DEFAULT 30000
    CHECK (tcp_keepalive_interval_ms BETWEEN 0 AND 4294967295);
