-- M1-04：最小安全审计事件。该表只允许追加，禁止修改或删除既有证据。
CREATE TABLE security_audit_events (
    -- evt_ + 26 位大写 Crockford ULID；重复写入同一事件必须保持内容完全一致。
    event_id TEXT PRIMARY KEY
        CHECK (
            length(event_id) = 30
            AND substr(event_id, 1, 4) = 'evt_'
            AND substr(event_id, 5, 1) GLOB '[0-7]'
            AND substr(event_id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    -- op_ + 26 位大写 Crockford ULID；M1 的一个安全操作只产生一个结果事件。
    operation_id TEXT NOT NULL UNIQUE
        CHECK (
            length(operation_id) = 29
            AND substr(operation_id, 1, 3) = 'op_'
            AND substr(operation_id, 4, 1) GLOB '[0-7]'
            AND substr(operation_id, 4) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    event TEXT NOT NULL CHECK (event IN ('SECURITY_OPERATION_RESULT')),
    action TEXT NOT NULL CHECK (action IN ('GATEWAY_KEY_ROTATE')),
    actor_type TEXT NOT NULL CHECK (actor_type IN ('LOCAL_OPERATOR')),
    -- 离线维护命令没有已认证的个人身份或网络来源；M1 必须保持 NULL。
    actor_id TEXT CHECK (actor_id IS NULL),
    source_ip TEXT CHECK (source_ip IS NULL),
    resource_type TEXT NOT NULL CHECK (resource_type IN ('GATEWAY_IDENTITY')),
    resource_id TEXT NOT NULL
        CHECK (length(CAST(resource_id AS BLOB)) BETWEEN 1 AND 256 AND resource_id = trim(resource_id)),
    result TEXT NOT NULL CHECK (result IN ('SUCCEEDED', 'FAILED')),
    error_code TEXT CHECK (error_code IS NULL OR (length(CAST(error_code AS BLOB)) BETWEEN 1 AND 64 AND error_code = trim(error_code))),
    request_id TEXT CHECK (request_id IS NULL OR (length(CAST(request_id AS BLOB)) BETWEEN 1 AND 128 AND request_id = trim(request_id))),
    trace_id TEXT CHECK (trace_id IS NULL OR (length(CAST(trace_id AS BLOB)) BETWEEN 1 AND 128 AND trace_id = trim(trace_id))),
    before_state_digest BLOB CHECK (before_state_digest IS NULL OR length(before_state_digest) = 32),
    after_state_digest BLOB CHECK (after_state_digest IS NULL OR length(after_state_digest) = 32),
    occurred_at INTEGER NOT NULL CHECK (occurred_at > 0),

    CHECK (
        (result = 'SUCCEEDED' AND error_code IS NULL)
        OR (result = 'FAILED' AND error_code IS NOT NULL)
    )
);

CREATE INDEX security_audit_events_chronological
ON security_audit_events(occurred_at, event_id);

CREATE TRIGGER security_audit_events_no_update
BEFORE UPDATE ON security_audit_events
BEGIN
    SELECT RAISE(ABORT, 'security_audit_events is append-only');
END;

CREATE TRIGGER security_audit_events_no_delete
BEFORE DELETE ON security_audit_events
BEGIN
    SELECT RAISE(ABORT, 'security_audit_events is append-only');
END;
