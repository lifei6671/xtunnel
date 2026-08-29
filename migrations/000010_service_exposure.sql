-- M5-05：一个 Service 最多拥有一个公网 Exposure，且类型只能是 HTTP 或 TCP。
-- 单表唯一索引约束同类型重复；双向触发器约束跨表重复。Application 切换类型时
-- 必须在同一事务中先删除旧 Exposure，再插入新 Exposure。
CREATE UNIQUE INDEX http_routes_unique_service_exposure
ON http_routes(service_id);

CREATE UNIQUE INDEX tcp_routes_unique_service_exposure
ON tcp_routes(service_id);

CREATE TRIGGER http_routes_reject_tcp_exposure_insert
BEFORE INSERT ON http_routes
WHEN EXISTS (SELECT 1 FROM tcp_routes WHERE service_id = NEW.service_id)
BEGIN
    SELECT RAISE(ABORT, 'service already has TCP exposure');
END;

CREATE TRIGGER http_routes_reject_tcp_exposure_update
BEFORE UPDATE OF service_id ON http_routes
WHEN EXISTS (SELECT 1 FROM tcp_routes WHERE service_id = NEW.service_id)
BEGIN
    SELECT RAISE(ABORT, 'service already has TCP exposure');
END;

CREATE TRIGGER tcp_routes_reject_http_exposure_insert
BEFORE INSERT ON tcp_routes
WHEN EXISTS (SELECT 1 FROM http_routes WHERE service_id = NEW.service_id)
BEGIN
    SELECT RAISE(ABORT, 'service already has HTTP exposure');
END;

CREATE TRIGGER tcp_routes_reject_http_exposure_update
BEFORE UPDATE OF service_id ON tcp_routes
WHEN EXISTS (SELECT 1 FROM http_routes WHERE service_id = NEW.service_id)
BEGIN
    SELECT RAISE(ABORT, 'service already has HTTP exposure');
END;
