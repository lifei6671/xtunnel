package repository

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

var (
	// ErrInvalidUsage 表示 Usage Bucket、归属或计数不符合持久化契约。
	ErrInvalidUsage = errors.New("usage is invalid")
	// ErrUsageOverflow 表示累计结果超出 SQLite 有符号 INTEGER 的安全范围。
	ErrUsageOverflow = errors.New("usage counter overflow")
)

// UsageDelta 是一次尚未持久化的 UTC minute Bucket 增量。
type UsageDelta struct {
	Bucket       time.Time
	TunnelID     string
	ServiceID    string
	Connections  uint64
	IngressBytes uint64
	EgressBytes  uint64
	Errors       uint64
}

// Validate 保证 Delta 可无损写入 SQLite INTEGER，且 Bucket 精确对齐 UTC minute。
func (delta UsageDelta) Validate() error {
	if delta.Bucket.IsZero() || delta.Bucket.Location() != time.UTC ||
		!delta.Bucket.Equal(delta.Bucket.Truncate(time.Minute)) || delta.Bucket.Unix() <= 0 ||
		!validate.ValidID(delta.TunnelID, "tun_") || !validate.ValidID(delta.ServiceID, "svc_") ||
		delta.Connections > math.MaxInt64 || delta.IngressBytes > math.MaxInt64 ||
		delta.EgressBytes > math.MaxInt64 || delta.Errors > math.MaxInt64 ||
		(delta.Connections|delta.IngressBytes|delta.EgressBytes|delta.Errors) == 0 {
		return ErrInvalidUsage
	}
	return nil
}

// UsageTotals 是指定查询范围内的权威累计值。
type UsageTotals struct {
	Connections  uint64
	IngressBytes uint64
	EgressBytes  uint64
	Errors       uint64
}

// UsageRepository 在一个 Repository View 内提供 Usage 写入与读取。
type UsageRepository interface {
	Add(context.Context, []UsageDelta) error
	Today(context.Context, time.Time, string, string) (UsageTotals, error)
	TodayByService(context.Context, time.Time, string) (map[string]UsageTotals, error)
}
