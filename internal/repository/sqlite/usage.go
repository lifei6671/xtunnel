package sqlite

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	UsageDayRetention = 7 * 24 * time.Hour
	UsageVacuumPages  = 256
)

const (
	usageMinuteTable = "usage_minutes"
	usageHourTable   = "usage_hours"
	usageDayTable    = "usage_days"
)

type usageRepository struct {
	database *gorm.DB
	readOnly bool
}

var _ repository.UsageRepository = usageRepository{}

type usageRecord struct {
	BucketTime   int64  `gorm:"column:bucket_time"`
	TunnelID     string `gorm:"column:tunnel_id"`
	ServiceID    string `gorm:"column:service_id"`
	Connections  int64  `gorm:"column:connections"`
	IngressBytes int64  `gorm:"column:ingress_bytes"`
	EgressBytes  int64  `gorm:"column:egress_bytes"`
	Errors       int64  `gorm:"column:errors"`
}

// Flush 在一个 BEGIN IMMEDIATE 事务中累加整批 minute Delta。
// 返回成功后调用方才能丢弃内存批次；任一非法行、外键或溢出都会回滚整批。
func (store *Store) Flush(ctx context.Context, deltas []repository.UsageDelta) error {
	if ctx == nil {
		return repository.ErrInvalidUsage
	}
	if len(deltas) == 0 {
		return nil
	}
	return store.WithTx(ctx, func(transaction repository.TxStore) error {
		return transaction.Usage().Add(ctx, deltas)
	})
}

// Add 将已校验的 minute Delta 累加到当前写事务。
func (store usageRepository) Add(ctx context.Context, deltas []repository.UsageDelta) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if ctx == nil {
		return repository.ErrInvalidUsage
	}
	if len(deltas) == 0 {
		return nil
	}
	for _, delta := range deltas {
		if err := delta.Validate(); err != nil {
			return err
		}
		result := store.database.WithContext(ctx).Exec(usageUpsertSQL(usageMinuteTable),
			delta.Bucket.Unix(), delta.TunnelID, delta.ServiceID,
			delta.Connections, delta.IngressBytes, delta.EgressBytes, delta.Errors,
		)
		if result.Error != nil {
			return fmt.Errorf("add usage minute: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return repository.ErrUsageOverflow
		}
	}
	return nil
}

// Today 汇总 UTC 当日的 minute/hour/day 权威行。空 Tunnel/Service 表示 Dashboard
// 总量；只给 Tunnel 表示该 Tunnel；Service 查询必须同时给出所属 Tunnel。
func (store usageRepository) Today(ctx context.Context, now time.Time, tunnelID, serviceID string) (repository.UsageTotals, error) {
	if ctx == nil || now.IsZero() || now.UTC().Unix() <= 0 ||
		(tunnelID != "" && !validate.ValidID(tunnelID, "tun_")) ||
		(serviceID != "" && (!validate.ValidID(serviceID, "svc_") || tunnelID == "")) {
		return repository.UsageTotals{}, repository.ErrInvalidUsage
	}
	start := now.UTC().Truncate(24 * time.Hour).Unix()
	end := start + int64((24*time.Hour)/time.Second)
	query := `SELECT bucket_time, tunnel_id, service_id, connections, ingress_bytes, egress_bytes, errors
		FROM (` + usageUnionSQL() + `) WHERE bucket_time >= ? AND bucket_time < ?`
	arguments := []any{start, end}
	if tunnelID != "" {
		query += " AND tunnel_id = ?"
		arguments = append(arguments, tunnelID)
	}
	if serviceID != "" {
		query += " AND service_id = ?"
		arguments = append(arguments, serviceID)
	}
	var records []usageRecord
	if err := store.database.WithContext(ctx).Raw(query, arguments...).Scan(&records).Error; err != nil {
		return repository.UsageTotals{}, fmt.Errorf("query today's usage: %w", err)
	}
	var totals repository.UsageTotals
	for _, record := range records {
		if record.Connections < 0 || record.IngressBytes < 0 || record.EgressBytes < 0 || record.Errors < 0 ||
			!addUsageCounter(&totals.Connections, uint64(record.Connections)) ||
			!addUsageCounter(&totals.IngressBytes, uint64(record.IngressBytes)) ||
			!addUsageCounter(&totals.EgressBytes, uint64(record.EgressBytes)) ||
			!addUsageCounter(&totals.Errors, uint64(record.Errors)) {
			return repository.UsageTotals{}, repository.ErrUsageOverflow
		}
	}
	return totals, nil
}

// TodayByService 用一次分组查询返回 UTC 当日每个 Service 的累计值；空 Tunnel
// 表示全部 Tunnel。它供 Service 列表批量投影，避免逐 Service 查询形成 N+1。
func (store usageRepository) TodayByService(ctx context.Context, now time.Time, tunnelID string) (map[string]repository.UsageTotals, error) {
	if ctx == nil || now.IsZero() || now.UTC().Unix() <= 0 ||
		(tunnelID != "" && !validate.ValidID(tunnelID, "tun_")) {
		return nil, repository.ErrInvalidUsage
	}
	start := now.UTC().Truncate(24 * time.Hour).Unix()
	end := start + int64((24*time.Hour)/time.Second)
	query := `SELECT service_id, SUM(connections) AS connections,
		SUM(ingress_bytes) AS ingress_bytes, SUM(egress_bytes) AS egress_bytes,
		SUM(errors) AS errors FROM (` + usageUnionSQL() + `)
		WHERE bucket_time >= ? AND bucket_time < ?`
	arguments := []any{start, end}
	if tunnelID != "" {
		query += " AND tunnel_id = ?"
		arguments = append(arguments, tunnelID)
	}
	query += " GROUP BY service_id"
	var records []usageRecord
	if err := store.database.WithContext(ctx).Raw(query, arguments...).Scan(&records).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "integer overflow") {
			return nil, repository.ErrUsageOverflow
		}
		return nil, fmt.Errorf("query today's usage by service: %w", err)
	}
	totals := make(map[string]repository.UsageTotals, len(records))
	for _, record := range records {
		if record.Connections < 0 || record.IngressBytes < 0 || record.EgressBytes < 0 || record.Errors < 0 {
			return nil, repository.ErrUsageOverflow
		}
		totals[record.ServiceID] = repository.UsageTotals{
			Connections: uint64(record.Connections), IngressBytes: uint64(record.IngressBytes),
			EgressBytes: uint64(record.EgressBytes), Errors: uint64(record.Errors),
		}
	}
	return totals, nil
}

// Rollup 将超过固定保留期的 minute/hour 行依次汇总，并在同一事务中删除已覆盖
// 明细；Day Retention 也在该事务末尾执行。COMMIT 后再做有界 Incremental Vacuum。
func (store *Store) Rollup(ctx context.Context, now time.Time) error {
	if ctx == nil || now.IsZero() || now.UTC().Unix() <= 0 {
		return repository.ErrInvalidUsage
	}
	now = now.UTC()
	minuteCutoff := now.Truncate(time.Minute).Unix()
	hourCutoff := now.Truncate(time.Hour).Unix()
	// 7 日口径包含当前 UTC 日，因此只向前回看 6 个完整日界线。
	dayCutoff := now.Truncate(24 * time.Hour).Add(-(UsageDayRetention - 24*time.Hour)).Unix()
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		usage := transaction.Usage().(usageRepository)
		if err := usage.rollup(ctx, usageMinuteTable, usageHourTable, minuteCutoff, 3600); err != nil {
			return err
		}
		if err := usage.rollup(ctx, usageHourTable, usageDayTable, hourCutoff, 86400); err != nil {
			return err
		}
		if err := usage.database.WithContext(ctx).Exec("DELETE FROM "+usageDayTable+" WHERE bucket_time < ?", dayCutoff).Error; err != nil {
			return fmt.Errorf("delete expired usage days: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := store.incrementalVacuum(ctx); err != nil {
		return fmt.Errorf("%w: %w", repository.ErrPostCommitCleanup, err)
	}
	return nil
}

func (store usageRepository) rollup(ctx context.Context, sourceTable, targetTable string, cutoff, targetSeconds int64) error {
	var aggregates []usageRecord
	query := `SELECT (bucket_time / ?) * ? AS bucket_time, tunnel_id, service_id,
		SUM(connections) AS connections, SUM(ingress_bytes) AS ingress_bytes,
		SUM(egress_bytes) AS egress_bytes, SUM(errors) AS errors
		FROM ` + sourceTable + ` WHERE bucket_time < ?
		GROUP BY (bucket_time / ?) * ?, tunnel_id, service_id`
	if err := store.database.WithContext(ctx).Raw(query, targetSeconds, targetSeconds, cutoff, targetSeconds, targetSeconds).Scan(&aggregates).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "integer overflow") {
			return repository.ErrUsageOverflow
		}
		return fmt.Errorf("aggregate %s: %w", sourceTable, err)
	}
	for _, aggregate := range aggregates {
		if aggregate.Connections < 0 || aggregate.IngressBytes < 0 || aggregate.EgressBytes < 0 || aggregate.Errors < 0 {
			return repository.ErrUsageOverflow
		}
		result := store.database.WithContext(ctx).Exec(usageUpsertSQL(targetTable),
			aggregate.BucketTime, aggregate.TunnelID, aggregate.ServiceID,
			aggregate.Connections, aggregate.IngressBytes, aggregate.EgressBytes, aggregate.Errors,
		)
		if result.Error != nil {
			return fmt.Errorf("write %s rollup: %w", targetTable, result.Error)
		}
		if result.RowsAffected != 1 {
			return repository.ErrUsageOverflow
		}
	}
	if err := store.database.WithContext(ctx).Exec("DELETE FROM "+sourceTable+" WHERE bucket_time < ?", cutoff).Error; err != nil {
		return fmt.Errorf("delete rolled up %s: %w", sourceTable, err)
	}
	return nil
}

func (store *Store) incrementalVacuum(ctx context.Context) error {
	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire usage vacuum write lease: %w", err)
	}
	defer lease.Release()
	if err := store.database.WithContext(ctx).Exec(fmt.Sprintf("PRAGMA incremental_vacuum(%d)", UsageVacuumPages)).Error; err != nil {
		return fmt.Errorf("incremental vacuum usage database: %w", err)
	}
	return nil
}

func usageUpsertSQL(table string) string {
	return `INSERT INTO ` + table + `
		(bucket_time, tunnel_id, service_id, connections, ingress_bytes, egress_bytes, errors)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_time, tunnel_id, service_id) DO UPDATE SET
		connections = connections + excluded.connections,
		ingress_bytes = ingress_bytes + excluded.ingress_bytes,
		egress_bytes = egress_bytes + excluded.egress_bytes,
		errors = errors + excluded.errors
		WHERE connections <= ` + fmt.Sprint(math.MaxInt64) + ` - excluded.connections
		AND ingress_bytes <= ` + fmt.Sprint(math.MaxInt64) + ` - excluded.ingress_bytes
		AND egress_bytes <= ` + fmt.Sprint(math.MaxInt64) + ` - excluded.egress_bytes
		AND errors <= ` + fmt.Sprint(math.MaxInt64) + ` - excluded.errors`
}

func usageUnionSQL() string {
	columns := "bucket_time, tunnel_id, service_id, connections, ingress_bytes, egress_bytes, errors"
	return "SELECT " + columns + " FROM " + usageMinuteTable +
		" UNION ALL SELECT " + columns + " FROM " + usageHourTable +
		" UNION ALL SELECT " + columns + " FROM " + usageDayTable
}

func addUsageCounter(target *uint64, delta uint64) bool {
	if math.MaxUint64-*target < delta {
		return false
	}
	*target += delta
	return true
}
