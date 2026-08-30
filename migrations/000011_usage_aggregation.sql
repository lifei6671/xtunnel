-- M6-04：按 UTC Bucket、Tunnel 与 Service 唯一归属的 Usage 聚合。
-- 三层表使用相同计数契约，Rollup 可以在同一事务中先累加上层再删除下层。
-- Usage 是已发生的历史事实，故不建立会随 Tunnel/Service 删除而级联清理的外键。
CREATE TABLE usage_minutes (
    bucket_time INTEGER NOT NULL CHECK (bucket_time > 0 AND bucket_time % 60 = 0),
    tunnel_id TEXT NOT NULL,
    service_id TEXT NOT NULL,
    connections INTEGER NOT NULL DEFAULT 0 CHECK (connections >= 0),
    ingress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (ingress_bytes >= 0),
    egress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (egress_bytes >= 0),
    errors INTEGER NOT NULL DEFAULT 0 CHECK (errors >= 0),
    PRIMARY KEY (bucket_time, tunnel_id, service_id)
);

CREATE TABLE usage_hours (
    bucket_time INTEGER NOT NULL CHECK (bucket_time > 0 AND bucket_time % 3600 = 0),
    tunnel_id TEXT NOT NULL,
    service_id TEXT NOT NULL,
    connections INTEGER NOT NULL DEFAULT 0 CHECK (connections >= 0),
    ingress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (ingress_bytes >= 0),
    egress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (egress_bytes >= 0),
    errors INTEGER NOT NULL DEFAULT 0 CHECK (errors >= 0),
    PRIMARY KEY (bucket_time, tunnel_id, service_id)
);

CREATE TABLE usage_days (
    bucket_time INTEGER NOT NULL CHECK (bucket_time > 0 AND bucket_time % 86400 = 0),
    tunnel_id TEXT NOT NULL,
    service_id TEXT NOT NULL,
    connections INTEGER NOT NULL DEFAULT 0 CHECK (connections >= 0),
    ingress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (ingress_bytes >= 0),
    egress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (egress_bytes >= 0),
    errors INTEGER NOT NULL DEFAULT 0 CHECK (errors >= 0),
    PRIMARY KEY (bucket_time, tunnel_id, service_id)
);

CREATE INDEX usage_minutes_by_service_time
ON usage_minutes(tunnel_id, service_id, bucket_time);

CREATE INDEX usage_hours_by_service_time
ON usage_hours(tunnel_id, service_id, bucket_time);

CREATE INDEX usage_days_by_service_time
ON usage_days(tunnel_id, service_id, bucket_time);
