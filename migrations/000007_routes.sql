-- M4-01：Route Desired State 与全局 Generation 是 SQLite 中的唯一配置权威。
-- 运行时只发布由完整事务快照构建成功的不可变 Route Snapshot，热路径不得查询这些表。
CREATE TABLE route_config_state (
    -- V0.1 只有一行全局状态；固定主键阻止误建第二个 generation 流。
    id INTEGER PRIMARY KEY NOT NULL CHECK (id = 1),
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0)
);

INSERT INTO route_config_state(id, generation) VALUES (1, 0);

CREATE TABLE http_routes (
    -- Route ID 的外部格式尚未冻结，数据库只保留文本主键语义。
    id TEXT PRIMARY KEY NOT NULL,
    service_id TEXT NOT NULL,
    hostname TEXT NOT NULL
        CHECK (length(trim(hostname, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0),
    path_prefix TEXT NOT NULL DEFAULT '/'
        CHECK (length(path_prefix) > 0 AND substr(path_prefix, 1, 1) = '/'),
    preserve_host INTEGER NOT NULL DEFAULT 1 CHECK (preserve_host IN (0, 1)),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at INTEGER NOT NULL CHECK (created_at > 0),
    updated_at INTEGER NOT NULL CHECK (updated_at > 0),

    FOREIGN KEY(service_id) REFERENCES services(id) ON DELETE RESTRICT,
    UNIQUE(hostname, path_prefix)
);

CREATE INDEX http_routes_by_service_id
ON http_routes(service_id, id);

CREATE TABLE tcp_routes (
    -- Route ID 的外部格式尚未冻结，数据库只保留文本主键语义。
    id TEXT PRIMARY KEY NOT NULL,
    service_id TEXT NOT NULL,
    public_port INTEGER NOT NULL UNIQUE CHECK (public_port BETWEEN 1 AND 65535),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at INTEGER NOT NULL CHECK (created_at > 0),
    updated_at INTEGER NOT NULL CHECK (updated_at > 0),

    FOREIGN KEY(service_id) REFERENCES services(id) ON DELETE RESTRICT
);

CREATE INDEX tcp_routes_by_service_id
ON tcp_routes(service_id, id);
