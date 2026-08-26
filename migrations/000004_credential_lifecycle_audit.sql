-- M2-04/M2-05：扩展 append-only Security Audit 契约以覆盖管理面 Credential 生命周期。
-- SQLite 不能原地修改 CHECK；在同一 Migration 事务内重建表，并完整保留 v3 证据。
DROP TRIGGER security_audit_events_no_update;
DROP TRIGGER security_audit_events_no_delete;
DROP INDEX security_audit_events_chronological;

ALTER TABLE security_audit_events RENAME TO security_audit_events_v3;

CREATE TABLE security_audit_events (
    event_id TEXT PRIMARY KEY
        CHECK (
            length(event_id) = 30
            AND substr(event_id, 1, 4) = 'evt_'
            AND substr(event_id, 5, 1) GLOB '[0-7]'
            AND substr(event_id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),
    operation_id TEXT NOT NULL UNIQUE
        CHECK (
            length(operation_id) = 29
            AND substr(operation_id, 1, 3) = 'op_'
            AND substr(operation_id, 4, 1) GLOB '[0-7]'
            AND substr(operation_id, 4) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),
    event TEXT NOT NULL CHECK (event IN ('SECURITY_OPERATION_RESULT')),
    action TEXT NOT NULL CHECK (action IN (
        'GATEWAY_KEY_ROTATE',
        'CONNECTION_TOKEN_REVEAL',
        'CONNECTION_TOKEN_ROTATE',
        'CONNECTION_TOKEN_REVOKE',
        'TUNNEL_REVOKE'
    )),
    actor_type TEXT NOT NULL CHECK (actor_type IN ('LOCAL_OPERATOR', 'ADMIN')),
    actor_id TEXT CHECK (
        actor_id IS NULL
        OR (
            length(actor_id) = 30
            AND substr(actor_id, 1, 4) = 'adm_'
            AND substr(actor_id, 5, 1) GLOB '[0-7]'
            AND substr(actor_id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        )
    ),
    source_ip TEXT CHECK (
        source_ip IS NULL
        OR (length(CAST(source_ip AS BLOB)) BETWEEN 1 AND 128 AND source_ip = trim(source_ip))
    ),
    resource_type TEXT NOT NULL CHECK (resource_type IN ('GATEWAY_IDENTITY', 'TUNNEL_TOKEN', 'TUNNEL')),
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
    ),
    CHECK (
        (
            action = 'GATEWAY_KEY_ROTATE'
            AND actor_type = 'LOCAL_OPERATOR'
            AND actor_id IS NULL
            AND source_ip IS NULL
            AND resource_type = 'GATEWAY_IDENTITY'
        )
        OR (
            action IN ('CONNECTION_TOKEN_REVEAL', 'CONNECTION_TOKEN_ROTATE', 'CONNECTION_TOKEN_REVOKE')
            AND actor_type = 'ADMIN'
            AND actor_id IS NOT NULL
            AND resource_type = 'TUNNEL_TOKEN'
            AND length(resource_id) = 30
            AND substr(resource_id, 1, 4) = 'tun_'
            AND substr(resource_id, 5, 1) GLOB '[0-7]'
            AND substr(resource_id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        )
        OR (
            action = 'TUNNEL_REVOKE'
            AND actor_type = 'ADMIN'
            AND actor_id IS NOT NULL
            AND resource_type = 'TUNNEL'
            AND length(resource_id) = 30
            AND substr(resource_id, 1, 4) = 'tun_'
            AND substr(resource_id, 5, 1) GLOB '[0-7]'
            AND substr(resource_id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        )
    )
);

INSERT INTO security_audit_events (
    event_id, operation_id, event, action, actor_type, actor_id, source_ip,
    resource_type, resource_id, result, error_code, request_id, trace_id,
    before_state_digest, after_state_digest, occurred_at
)
SELECT
    event_id, operation_id, event, action, actor_type, actor_id, source_ip,
    resource_type, resource_id, result, error_code, request_id, trace_id,
    before_state_digest, after_state_digest, occurred_at
FROM security_audit_events_v3;

DROP TABLE security_audit_events_v3;

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
