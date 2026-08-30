package sqlite

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestUsageMigrationUpgradesV10AndEnablesIncrementalVacuum(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:10], testNow); err != nil {
		t.Fatalf("run v10 migrations error = %v", err)
	}
	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("upgrade to v11 error = %v", err)
	}
	for _, table := range []string{usageMinuteTable, usageHourTable, usageDayTable} {
		var count int64
		if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count).Error; err != nil {
			t.Fatalf("inspect table %s error = %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %s count = %d, want 1", table, count)
		}
	}
	var mode int
	if err := database.Raw("PRAGMA auto_vacuum").Scan(&mode).Error; err != nil {
		t.Fatalf("read auto_vacuum error = %v", err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (INCREMENTAL)", mode)
	}
	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("idempotent v11 rerun error = %v", err)
	}
	var versions int64
	if err := database.Table("schema_migrations").Count(&versions).Error; err != nil {
		t.Fatalf("count schema versions error = %v", err)
	}
	if versions != 11 {
		t.Fatalf("schema versions = %d, want 11", versions)
	}
}

func TestUsageFlushAddsBatchAndTodayReadsAuthority(t *testing.T) {
	store := openUsageTestStore(t)
	bucket := time.Date(2026, 8, 30, 9, 12, 0, 0, time.UTC)
	deltas := []repository.UsageDelta{
		usageTestDelta(bucket, 1, 100, 50, 0),
		usageTestDelta(bucket, 2, 20, 10, 1),
	}
	if err := store.Flush(context.Background(), deltas); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	want := repository.UsageTotals{Connections: 3, IngressBytes: 120, EgressBytes: 60, Errors: 1}
	if got := usageToday(t, store, bucket.Add(time.Hour), repositoryTestTunnelID, serviceTestIDOne); !reflect.DeepEqual(got, want) {
		t.Fatalf("Today(service) = %#v, want %#v", got, want)
	}
	if got := usageToday(t, store, bucket.Add(time.Hour), "", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("Today(dashboard) = %#v, want %#v", got, want)
	}
	var byService map[string]repository.UsageTotals
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		var err error
		byService, err = view.Usage().TodayByService(context.Background(), bucket.Add(time.Hour), repositoryTestTunnelID)
		return err
	}); err != nil {
		t.Fatalf("TodayByService() error = %v", err)
	}
	if !reflect.DeepEqual(byService, map[string]repository.UsageTotals{serviceTestIDOne: want}) {
		t.Fatalf("TodayByService() = %#v, want one service %#v", byService, want)
	}
}

func TestUsageHistorySurvivesServiceAndTunnelDeletion(t *testing.T) {
	store := openUsageTestStore(t)
	bucket := time.Date(2026, 8, 30, 9, 12, 0, 0, time.UTC)
	if err := store.Flush(context.Background(), []repository.UsageDelta{usageTestDelta(bucket, 1, 2, 3, 4)}); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Services().Delete(context.Background(), repositoryTestTunnelID, serviceTestIDOne, 1); err != nil {
			return err
		}
		return transaction.Tunnels().Delete(context.Background(), repositoryTestTunnelID, 1)
	}); err != nil {
		t.Fatalf("delete Service and Tunnel error = %v", err)
	}
	want := repository.UsageTotals{Connections: 1, IngressBytes: 2, EgressBytes: 3, Errors: 4}
	if got := usageToday(t, store, bucket, "", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("Today(after owner deletion) = %#v, want preserved %#v", got, want)
	}
}

func TestUsageFlushRejectsInvalidAndRollsBackOverflow(t *testing.T) {
	store := openUsageTestStore(t)
	bucket := time.Date(2026, 8, 30, 9, 12, 0, 0, time.UTC)
	invalid := []repository.UsageDelta{
		{},
		usageTestDelta(bucket.Add(time.Second), 1, 0, 0, 0),
		usageTestDelta(bucket.In(time.FixedZone("UTC-like", 0)), 1, 0, 0, 0),
		usageTestDelta(bucket, uint64(math.MaxInt64)+1, 0, 0, 0),
	}
	for index, delta := range invalid {
		if err := store.Flush(context.Background(), []repository.UsageDelta{delta}); !errors.Is(err, repository.ErrInvalidUsage) {
			t.Fatalf("Flush(invalid %d) error = %v, want ErrInvalidUsage", index, err)
		}
	}
	maximum := usageTestDelta(bucket, math.MaxInt64, 0, 0, 0)
	if err := store.Flush(context.Background(), []repository.UsageDelta{maximum}); err != nil {
		t.Fatalf("Flush(maximum) error = %v", err)
	}
	otherBucket := bucket.Add(time.Minute)
	err := store.Flush(context.Background(), []repository.UsageDelta{
		usageTestDelta(otherBucket, 1, 0, 0, 0),
		usageTestDelta(bucket, 1, 0, 0, 0),
	})
	if !errors.Is(err, repository.ErrUsageOverflow) {
		t.Fatalf("Flush(overflow) error = %v, want ErrUsageOverflow", err)
	}
	got := usageToday(t, store, bucket, repositoryTestTunnelID, serviceTestIDOne)
	if got.Connections != math.MaxInt64 {
		t.Fatalf("Today().Connections = %d, want %d", got.Connections, uint64(math.MaxInt64))
	}
	var otherRows int64
	if err := store.database.Table(usageMinuteTable).Where("bucket_time = ?", otherBucket.Unix()).Count(&otherRows).Error; err != nil {
		t.Fatalf("count rolled-back row error = %v", err)
	}
	if otherRows != 0 {
		t.Fatalf("overflow batch preserved %d earlier row(s), want rollback", otherRows)
	}
}

func TestUsageRollupCommitsBeforeDeleteAndRerunsWithoutDuplication(t *testing.T) {
	store := openUsageTestStore(t)
	now := time.Date(2026, 8, 30, 12, 34, 30, 0, time.UTC)
	oldMinute := usageTestDelta(now.Truncate(time.Minute).Add(-time.Minute), 2, 20, 10, 1)
	currentMinute := usageTestDelta(now.Truncate(time.Minute), 3, 30, 15, 0)
	if err := store.Flush(context.Background(), []repository.UsageDelta{oldMinute, currentMinute}); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	insertUsageRecord(t, store, usageHourTable, now.Truncate(time.Hour).Add(-time.Hour), 5, 50, 25, 1)
	insertUsageRecord(t, store, usageDayTable, now.Truncate(24*time.Hour).Add(-24*time.Hour), 7, 70, 35, 1)
	insertUsageRecord(t, store, usageDayTable, now.Truncate(24*time.Hour).Add(-6*24*time.Hour), 9, 90, 45, 1)
	insertUsageRecord(t, store, usageDayTable, now.Truncate(24*time.Hour).Add(-7*24*time.Hour), 10, 100, 50, 1)
	insertUsageRecord(t, store, usageDayTable, now.Truncate(24*time.Hour).Add(-8*24*time.Hour), 11, 110, 55, 1)

	if err := store.Rollup(context.Background(), now); err != nil {
		t.Fatalf("Rollup() error = %v", err)
	}
	wantToday := repository.UsageTotals{Connections: 10, IngressBytes: 100, EgressBytes: 50, Errors: 2}
	if got := usageToday(t, store, now, repositoryTestTunnelID, serviceTestIDOne); !reflect.DeepEqual(got, wantToday) {
		t.Fatalf("Today(after rollup) = %#v, want %#v", got, wantToday)
	}
	assertUsageTableCount(t, store, usageMinuteTable, 1)
	assertUsageTableCount(t, store, usageHourTable, 1)
	assertUsageTableCount(t, store, usageDayTable, 3)

	if err := store.Rollup(context.Background(), now); err != nil {
		t.Fatalf("second Rollup() error = %v", err)
	}
	if got := usageToday(t, store, now, repositoryTestTunnelID, serviceTestIDOne); !reflect.DeepEqual(got, wantToday) {
		t.Fatalf("Today(after rerun) = %#v, want unchanged %#v", got, wantToday)
	}
}

func TestUsageRollupFailurePreservesSourceRows(t *testing.T) {
	store := openUsageTestStore(t)
	now := time.Date(2026, 8, 30, 12, 34, 30, 0, time.UTC)
	if err := store.Flush(context.Background(), []repository.UsageDelta{
		usageTestDelta(now.Truncate(time.Minute).Add(-time.Minute), 1, 1, 1, 0),
	}); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := store.database.Exec(`CREATE TRIGGER reject_usage_hour BEFORE INSERT ON usage_hours
		BEGIN SELECT RAISE(ABORT, 'rollup rejected'); END`).Error; err != nil {
		t.Fatalf("create failure trigger error = %v", err)
	}
	if err := store.Rollup(context.Background(), now); err == nil {
		t.Fatal("Rollup() error = nil, want injected failure")
	}
	assertUsageTableCount(t, store, usageMinuteTable, 1)
	assertUsageTableCount(t, store, usageHourTable, 0)
	if err := store.database.Exec("DROP TRIGGER reject_usage_hour").Error; err != nil {
		t.Fatalf("drop failure trigger error = %v", err)
	}
	if err := store.Rollup(context.Background(), now); err != nil {
		t.Fatalf("Rollup(retry) error = %v", err)
	}
	assertUsageTableCount(t, store, usageMinuteTable, 0)
	assertUsageTableCount(t, store, usageHourTable, 1)
}

func TestUsageIncrementalVacuumReclaimsFreelistPages(t *testing.T) {
	store := openUsageTestStore(t)
	records := make([]usageRecord, 0, 8_000)
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for index := range 8_000 {
		records = append(records, usageRecord{
			BucketTime: start.Add(time.Duration(index) * 24 * time.Hour).Unix(),
			TunnelID:   repositoryTestTunnelID, ServiceID: serviceTestIDOne,
			Connections: 1, IngressBytes: 1, EgressBytes: 1,
		})
	}
	if err := store.database.Table(usageDayTable).CreateInBatches(&records, 500).Error; err != nil {
		t.Fatalf("seed vacuum rows error = %v", err)
	}
	if err := store.database.Exec("DELETE FROM " + usageDayTable).Error; err != nil {
		t.Fatalf("delete vacuum rows error = %v", err)
	}
	before := usageFreelistPages(t, store)
	if before == 0 {
		t.Fatal("freelist_count before vacuum = 0, want reclaimable pages")
	}
	if err := store.incrementalVacuum(context.Background()); err != nil {
		t.Fatalf("incrementalVacuum() error = %v", err)
	}
	after := usageFreelistPages(t, store)
	if after >= before {
		t.Fatalf("freelist_count after vacuum = %d, want less than %d", after, before)
	}
}

func openUsageTestStore(t *testing.T) *Store {
	t.Helper()
	store := openServiceTestStore(t)
	seedServiceTestTunnel(t, store, repositoryTestTunnelID)
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Services().Create(context.Background(), testService(serviceTestIDOne, repositoryTestTunnelID))
	}); err != nil {
		t.Fatalf("seed Usage Service error = %v", err)
	}
	return store
}

func usageTestDelta(bucket time.Time, connections, ingress, egress, failures uint64) repository.UsageDelta {
	return repository.UsageDelta{
		Bucket: bucket, TunnelID: repositoryTestTunnelID, ServiceID: serviceTestIDOne,
		Connections: connections, IngressBytes: ingress, EgressBytes: egress, Errors: failures,
	}
}

func usageToday(t *testing.T, store *Store, now time.Time, tunnelID, serviceID string) repository.UsageTotals {
	t.Helper()
	var totals repository.UsageTotals
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		var err error
		totals, err = view.Usage().Today(context.Background(), now, tunnelID, serviceID)
		return err
	}); err != nil {
		t.Fatalf("Today() error = %v", err)
	}
	return totals
}

func insertUsageRecord(t *testing.T, store *Store, table string, bucket time.Time, connections, ingress, egress, failures int64) {
	t.Helper()
	if err := store.database.Exec(usageUpsertSQL(table), bucket.Unix(), repositoryTestTunnelID, serviceTestIDOne,
		connections, ingress, egress, failures).Error; err != nil {
		t.Fatalf("insert %s record error = %v", table, err)
	}
}

func assertUsageTableCount(t *testing.T, store *Store, table string, want int64) {
	t.Helper()
	var got int64
	if err := store.database.Table(table).Count(&got).Error; err != nil {
		t.Fatalf("count %s error = %v", table, err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}

func usageFreelistPages(t *testing.T, store *Store) int64 {
	t.Helper()
	var pages int64
	if err := store.database.Raw("PRAGMA freelist_count").Scan(&pages).Error; err != nil {
		t.Fatalf("read freelist_count error = %v", err)
	}
	return pages
}
