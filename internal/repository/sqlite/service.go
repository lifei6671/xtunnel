package sqlite

import (
	"context"
	"errors"
	"fmt"
	"math"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

// ServiceTable 是 services 的固定表名。
const ServiceTable = "services"

// ServiceColumns 集中定义 services 的列名，避免 CRUD 条件和更新字段漂移。
var ServiceColumns = struct {
	ID                      string
	TunnelID                string
	Name                    string
	RequiredRevision        string
	OriginScheme            string
	OriginHost              string
	OriginPort              string
	TLSVerify               string
	TLSServerName           string
	OriginHTTPHost          string
	ConnectTimeoutMS        string
	HealthType              string
	HealthPath              string
	HealthIntervalMS        string
	HealthTimeoutMS         string
	HealthExpectedStatusMin string
	HealthExpectedStatusMax string
	HealthFailureThreshold  string
	HealthSuccessThreshold  string
	Enabled                 string
	Version                 string
	CreatedAt               string
	UpdatedAt               string
}{
	ID:                      "id",
	TunnelID:                "tunnel_id",
	Name:                    "name",
	RequiredRevision:        "required_revision",
	OriginScheme:            "origin_scheme",
	OriginHost:              "origin_host",
	OriginPort:              "origin_port",
	TLSVerify:               "tls_verify",
	TLSServerName:           "tls_server_name",
	OriginHTTPHost:          "origin_http_host",
	ConnectTimeoutMS:        "connect_timeout_ms",
	HealthType:              "health_type",
	HealthPath:              "health_path",
	HealthIntervalMS:        "health_interval_ms",
	HealthTimeoutMS:         "health_timeout_ms",
	HealthExpectedStatusMin: "health_expected_status_min",
	HealthExpectedStatusMax: "health_expected_status_max",
	HealthFailureThreshold:  "health_failure_threshold",
	HealthSuccessThreshold:  "health_success_threshold",
	Enabled:                 "enabled",
	Version:                 "version",
	CreatedAt:               "created_at",
	UpdatedAt:               "updated_at",
}

type serviceRecord struct {
	ID                      string  `gorm:"column:id;primaryKey"`
	TunnelID                string  `gorm:"column:tunnel_id"`
	Name                    string  `gorm:"column:name"`
	RequiredRevision        int64   `gorm:"column:required_revision"`
	OriginScheme            string  `gorm:"column:origin_scheme"`
	OriginHost              string  `gorm:"column:origin_host"`
	OriginPort              uint32  `gorm:"column:origin_port"`
	TLSVerify               bool    `gorm:"column:tls_verify"`
	TLSServerName           *string `gorm:"column:tls_server_name"`
	OriginHTTPHost          *string `gorm:"column:origin_http_host"`
	ConnectTimeoutMS        uint32  `gorm:"column:connect_timeout_ms"`
	HealthType              *string `gorm:"column:health_type"`
	HealthPath              *string `gorm:"column:health_path"`
	HealthIntervalMS        *uint32 `gorm:"column:health_interval_ms"`
	HealthTimeoutMS         *uint32 `gorm:"column:health_timeout_ms"`
	HealthExpectedStatusMin *uint32 `gorm:"column:health_expected_status_min"`
	HealthExpectedStatusMax *uint32 `gorm:"column:health_expected_status_max"`
	HealthFailureThreshold  *uint32 `gorm:"column:health_failure_threshold"`
	HealthSuccessThreshold  *uint32 `gorm:"column:health_success_threshold"`
	Enabled                 bool    `gorm:"column:enabled"`
	Version                 int64   `gorm:"column:version"`
	CreatedAt               int64   `gorm:"column:created_at"`
	UpdatedAt               int64   `gorm:"column:updated_at"`
}

func (serviceRecord) TableName() string { return ServiceTable }

type serviceRepository struct{ database *gorm.DB }

var _ repository.ServiceRepository = serviceRepository{}

func (store serviceRepository) Create(ctx context.Context, service repository.Service) error {
	if err := service.Validate(); err != nil {
		return err
	}
	if err := store.database.WithContext(ctx).Create(serviceRecordFromDomain(service)).Error; err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

func (store serviceRepository) Get(ctx context.Context, tunnelID, serviceID string) (repository.Service, error) {
	if !validate.ValidID(tunnelID, "tun_") || !validate.ValidID(serviceID, "svc_") {
		return repository.Service{}, repository.ErrInvalidService
	}
	var record serviceRecord
	if err := store.database.WithContext(ctx).
		Where(ServiceColumns.TunnelID+" = ?", tunnelID).
		Where(ServiceColumns.ID+" = ?", serviceID).
		Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.Service{}, repository.ErrNotFound
		}
		return repository.Service{}, fmt.Errorf("get service: %w", err)
	}
	return serviceDomainFromRecord(record)
}

func (store serviceRepository) ListByTunnel(ctx context.Context, tunnelID string) ([]repository.Service, error) {
	if !validate.ValidID(tunnelID, "tun_") {
		return nil, repository.ErrInvalidService
	}
	var records []serviceRecord
	if err := store.database.WithContext(ctx).
		Where(ServiceColumns.TunnelID+" = ?", tunnelID).
		Order(ServiceColumns.ID + " ASC").
		Find(&records).Error; err != nil {
		return nil, fmt.Errorf("list services by tunnel: %w", err)
	}
	services := make([]repository.Service, 0, len(records))
	for _, record := range records {
		service, err := serviceDomainFromRecord(record)
		if err != nil {
			return nil, err
		}
		services = append(services, service)
	}
	return services, nil
}

func (store serviceRepository) CountByTunnel(ctx context.Context, tunnelID string) (int64, error) {
	if !validate.ValidID(tunnelID, "tun_") {
		return 0, repository.ErrInvalidService
	}
	var count int64
	if err := store.database.WithContext(ctx).Model(&serviceRecord{}).
		Where(ServiceColumns.TunnelID+" = ?", tunnelID).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count services by tunnel: %w", err)
	}
	return count, nil
}

func (store serviceRepository) Update(ctx context.Context, service repository.Service, expectedVersion int64) (repository.Service, error) {
	if err := service.Validate(); err != nil || expectedVersion < 1 || expectedVersion == math.MaxInt64 || service.Version != expectedVersion {
		return repository.Service{}, repository.ErrInvalidService
	}
	record := serviceRecordFromDomain(service)
	updates := serviceUpdates(record, expectedVersion+1)
	result := store.database.WithContext(ctx).Model(&serviceRecord{}).
		Where(ServiceColumns.TunnelID+" = ?", service.TunnelID).
		Where(ServiceColumns.ID+" = ?", service.ID).
		Where(ServiceColumns.Version+" = ?", expectedVersion).
		Updates(updates)
	if result.Error != nil {
		return repository.Service{}, fmt.Errorf("update service: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.Get(ctx, service.TunnelID, service.ID); err != nil {
			return repository.Service{}, err
		}
		return repository.Service{}, repository.ErrVersionConflict
	}
	return store.Get(ctx, service.TunnelID, service.ID)
}

func (store serviceRepository) Delete(ctx context.Context, tunnelID, serviceID string, expectedVersion int64) error {
	if !validate.ValidID(tunnelID, "tun_") || !validate.ValidID(serviceID, "svc_") || expectedVersion < 1 {
		return repository.ErrInvalidService
	}
	result := store.database.WithContext(ctx).
		Where(ServiceColumns.TunnelID+" = ?", tunnelID).
		Where(ServiceColumns.ID+" = ?", serviceID).
		Where(ServiceColumns.Version+" = ?", expectedVersion).
		Delete(&serviceRecord{})
	if result.Error != nil {
		return fmt.Errorf("delete service: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.Get(ctx, tunnelID, serviceID); err != nil {
			return err
		}
		return repository.ErrVersionConflict
	}
	return nil
}

func serviceRecordFromDomain(service repository.Service) serviceRecord {
	record := serviceRecord{
		ID: service.ID, TunnelID: service.TunnelID, Name: service.Name,
		RequiredRevision: service.RequiredRevision, OriginScheme: string(service.OriginScheme),
		OriginHost: service.OriginHost, OriginPort: service.OriginPort, TLSVerify: service.TLSVerify,
		TLSServerName: optionalString(service.TLSServerName), OriginHTTPHost: optionalString(service.OriginHTTPHost),
		ConnectTimeoutMS: service.ConnectTimeoutMS, Enabled: service.Enabled, Version: service.Version,
		CreatedAt: service.CreatedAt, UpdatedAt: service.UpdatedAt,
	}
	if service.Health == nil {
		return record
	}
	healthType := string(service.Health.Type)
	record.HealthType = &healthType
	record.HealthIntervalMS = new(service.Health.IntervalMS)
	record.HealthTimeoutMS = new(service.Health.TimeoutMS)
	record.HealthFailureThreshold = new(service.Health.FailureThreshold)
	record.HealthSuccessThreshold = new(service.Health.SuccessThreshold)
	if service.Health.Type == repository.HealthTypeHTTP {
		record.HealthPath = optionalString(service.Health.Path)
		record.HealthExpectedStatusMin = new(service.Health.ExpectedStatusMin)
		record.HealthExpectedStatusMax = new(service.Health.ExpectedStatusMax)
	}
	return record
}

func serviceDomainFromRecord(record serviceRecord) (repository.Service, error) {
	service := repository.Service{
		ID: record.ID, TunnelID: record.TunnelID, Name: record.Name,
		RequiredRevision: record.RequiredRevision, OriginScheme: repository.OriginScheme(record.OriginScheme),
		OriginHost: record.OriginHost, OriginPort: record.OriginPort, TLSVerify: record.TLSVerify,
		TLSServerName: stringValue(record.TLSServerName), OriginHTTPHost: stringValue(record.OriginHTTPHost),
		ConnectTimeoutMS: record.ConnectTimeoutMS, Enabled: record.Enabled, Version: record.Version,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if record.HealthType != nil {
		service.Health = &repository.HealthCheck{
			Type:              repository.HealthType(*record.HealthType),
			Path:              stringValue(record.HealthPath),
			IntervalMS:        uint32Value(record.HealthIntervalMS),
			TimeoutMS:         uint32Value(record.HealthTimeoutMS),
			ExpectedStatusMin: uint32Value(record.HealthExpectedStatusMin),
			ExpectedStatusMax: uint32Value(record.HealthExpectedStatusMax),
			FailureThreshold:  uint32Value(record.HealthFailureThreshold),
			SuccessThreshold:  uint32Value(record.HealthSuccessThreshold),
		}
	}
	if err := service.Validate(); err != nil {
		return repository.Service{}, fmt.Errorf("stored service is invalid: %w", err)
	}
	return service, nil
}

func serviceUpdates(record serviceRecord, nextVersion int64) map[string]any {
	return map[string]any{
		ServiceColumns.Name:                    record.Name,
		ServiceColumns.RequiredRevision:        record.RequiredRevision,
		ServiceColumns.OriginScheme:            record.OriginScheme,
		ServiceColumns.OriginHost:              record.OriginHost,
		ServiceColumns.OriginPort:              record.OriginPort,
		ServiceColumns.TLSVerify:               record.TLSVerify,
		ServiceColumns.TLSServerName:           record.TLSServerName,
		ServiceColumns.OriginHTTPHost:          record.OriginHTTPHost,
		ServiceColumns.ConnectTimeoutMS:        record.ConnectTimeoutMS,
		ServiceColumns.HealthType:              record.HealthType,
		ServiceColumns.HealthPath:              record.HealthPath,
		ServiceColumns.HealthIntervalMS:        record.HealthIntervalMS,
		ServiceColumns.HealthTimeoutMS:         record.HealthTimeoutMS,
		ServiceColumns.HealthExpectedStatusMin: record.HealthExpectedStatusMin,
		ServiceColumns.HealthExpectedStatusMax: record.HealthExpectedStatusMax,
		ServiceColumns.HealthFailureThreshold:  record.HealthFailureThreshold,
		ServiceColumns.HealthSuccessThreshold:  record.HealthSuccessThreshold,
		ServiceColumns.Enabled:                 record.Enabled,
		ServiceColumns.Version:                 nextVersion,
		ServiceColumns.UpdatedAt:               record.UpdatedAt,
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func uint32Value(value *uint32) uint32 {
	if value == nil {
		return 0
	}
	return *value
}
