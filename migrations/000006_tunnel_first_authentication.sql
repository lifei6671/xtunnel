-- M3-11：只持久化 Tunnel 是否曾成功完成 Control 认证的单调事实。
-- Connector、Session、在线状态和 last_seen_at 仍然只属于运行态，不得写入 SQLite。
ALTER TABLE tunnels
ADD COLUMN first_authenticated_at INTEGER
    CHECK (first_authenticated_at IS NULL OR first_authenticated_at > 0);
