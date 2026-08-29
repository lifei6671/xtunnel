package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/lifei6671/xtunnel/internal/repository"
)

var ErrSecurityAuditQueryServiceInput = errors.New("security audit query service input is invalid")

// SecurityAuditQueryService 是安全审计列表的只读 Application Owner。
// opaque cursor 的签名、筛选绑定和响应编码属于 Management API 边界，不能下沉到这里。
type SecurityAuditQueryService struct {
	store repository.SecurityAuditEventQueryStore
}

// NewSecurityAuditQueryService 创建只读安全审计查询服务。
func NewSecurityAuditQueryService(store repository.SecurityAuditEventQueryStore) *SecurityAuditQueryService {
	return &SecurityAuditQueryService{store: store}
}

// Query 校验已经解码的查询条件，并读取稳定排序的一页安全审计证据。
func (service *SecurityAuditQueryService) Query(
	ctx context.Context,
	query repository.SecurityAuditEventQuery,
) (repository.SecurityAuditEventPage, error) {
	if service == nil || service.store == nil || ctx == nil {
		return repository.SecurityAuditEventPage{}, ErrSecurityAuditQueryServiceInput
	}
	if err := query.Validate(); err != nil {
		return repository.SecurityAuditEventPage{}, err
	}
	page, err := service.store.QuerySecurityAuditEvents(ctx, query)
	if err != nil {
		return repository.SecurityAuditEventPage{}, fmt.Errorf("query security audit events: %w", err)
	}
	return page, nil
}
