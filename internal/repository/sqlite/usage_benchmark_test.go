package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
)

// BenchmarkUsageDayRetention7Days 固定生产保留口径：20,000 个活跃 Service、
// 7 天、140,000 条 day 行，并测量 Dashboard 当日聚合读取。
func BenchmarkUsageDayRetention7Days(b *testing.B) {
	benchmarkUsageDayRetention(b, 7)
}

// BenchmarkUsageDayRetention31DaysComparison 保留 31 日、620,000 条 day 行的
// 容量对照，不代表生产 Retention。
func BenchmarkUsageDayRetention31DaysComparison(b *testing.B) {
	benchmarkUsageDayRetention(b, 31)
}

func benchmarkUsageDayRetention(b *testing.B, retentionDays int) {
	dataDir := b.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("Store.Close() error = %v", err)
		}
	})
	const services = 20_000
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	seedUsageCapacity(b, store, now, services, retentionDays)

	if err := store.database.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		b.Fatalf("checkpoint benchmark database error = %v", err)
	}
	databasePath := filepath.Join(dataDir, databaseFilename)
	var bytesPerRow float64
	if info, err := os.Stat(databasePath); err == nil {
		bytesPerRow = float64(info.Size()) / float64(services*retentionDays)
	}
	b.ResetTimer()
	for range b.N {
		if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
			_, err := view.Usage().Today(context.Background(), now, "", "")
			return err
		}); err != nil {
			b.Fatalf("Today() error = %v", err)
		}
	}
	if bytesPerRow != 0 {
		b.ReportMetric(bytesPerRow, "bytes/day-row")
	}
}

func seedUsageCapacity(b *testing.B, store *Store, now time.Time, serviceCount, retentionDays int) {
	b.Helper()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		database := transaction.(*transactionStore).database
		for day := 0; day < retentionDays; day++ {
			bucket := now.Truncate(24 * time.Hour).Add(-time.Duration(day) * 24 * time.Hour)
			for start := 0; start < serviceCount; start += 500 {
				end := min(start+500, serviceCount)
				records := make([]usageRecord, 0, end-start)
				for index := start; index < end; index++ {
					records = append(records, usageRecord{
						BucketTime: bucket.Unix(), TunnelID: repositoryTestTunnelID,
						ServiceID: usageBenchmarkServiceID(index), Connections: 1,
					})
				}
				if err := database.Table(usageDayTable).CreateInBatches(&records, 500).Error; err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed usage days error = %v", err)
	}
}

func usageBenchmarkServiceID(index int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	id := []byte("svc_01J00000000000000000000000")
	id[len(id)-3] = alphabet[(index/(32*32))%32]
	id[len(id)-2] = alphabet[(index/32)%32]
	id[len(id)-1] = alphabet[index%32]
	return string(id)
}
