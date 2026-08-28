package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
	"gorm.io/gorm"
)

func TestServiceRepositoryProxyOptionsDefaultsApplicabilityAndRoundTrip(t *testing.T) {
	defaultOptions := (repository.ServiceProxyOptions{}).WithDefaults()
	tests := []struct {
		name        string
		scheme      repository.OriginScheme
		options     repository.ServiceProxyOptions
		want        repository.ServiceProxyOptions
		wantInvalid bool
	}{
		{
			name:    "whole zero value uses frozen defaults",
			scheme:  repository.OriginSchemeHTTP,
			options: repository.ServiceProxyOptions{},
			want:    defaultOptions,
		},
		{
			name:   "HTTP preserves explicit options and disabled keepalive",
			scheme: repository.OriginSchemeHTTPS,
			options: repository.ServiceProxyOptions{
				DisableChunkedEncoding: true, DisableHappyEyeballs: true,
				HTTPIdleConnectionTimeoutMS: 1, HTTPMaxIdleConnections: 1,
				TCPKeepAliveIntervalMS: 0,
			},
			want: repository.ServiceProxyOptions{
				DisableChunkedEncoding: true, DisableHappyEyeballs: true,
				HTTPIdleConnectionTimeoutMS: 1, HTTPMaxIdleConnections: 1,
				TCPKeepAliveIntervalMS: 0,
			},
		},
		{
			name:   "TCP preserves explicit disabled keepalive",
			scheme: repository.OriginSchemeTCP,
			options: repository.ServiceProxyOptions{
				DisableHappyEyeballs:        true,
				HTTPIdleConnectionTimeoutMS: defaultOptions.HTTPIdleConnectionTimeoutMS,
				HTTPMaxIdleConnections:      defaultOptions.HTTPMaxIdleConnections,
				TCPKeepAliveIntervalMS:      0,
			},
			want: repository.ServiceProxyOptions{
				DisableHappyEyeballs:        true,
				HTTPIdleConnectionTimeoutMS: defaultOptions.HTTPIdleConnectionTimeoutMS,
				HTTPMaxIdleConnections:      defaultOptions.HTTPMaxIdleConnections,
				TCPKeepAliveIntervalMS:      0,
			},
		},
		{
			name:   "TCP rejects chunked encoding option",
			scheme: repository.OriginSchemeTCP,
			options: repository.ServiceProxyOptions{
				DisableChunkedEncoding:      true,
				HTTPIdleConnectionTimeoutMS: defaultOptions.HTTPIdleConnectionTimeoutMS,
				HTTPMaxIdleConnections:      defaultOptions.HTTPMaxIdleConnections,
				TCPKeepAliveIntervalMS:      defaultOptions.TCPKeepAliveIntervalMS,
			},
			wantInvalid: true,
		},
		{
			name:   "TCP rejects non-default HTTP timeout",
			scheme: repository.OriginSchemeTCP,
			options: repository.ServiceProxyOptions{
				HTTPIdleConnectionTimeoutMS: defaultOptions.HTTPIdleConnectionTimeoutMS - 1,
				HTTPMaxIdleConnections:      defaultOptions.HTTPMaxIdleConnections,
				TCPKeepAliveIntervalMS:      defaultOptions.TCPKeepAliveIntervalMS,
			},
			wantInvalid: true,
		},
		{
			name:   "TCP rejects non-default HTTP connection count",
			scheme: repository.OriginSchemeTCP,
			options: repository.ServiceProxyOptions{
				HTTPIdleConnectionTimeoutMS: defaultOptions.HTTPIdleConnectionTimeoutMS,
				HTTPMaxIdleConnections:      defaultOptions.HTTPMaxIdleConnections + 1,
				TCPKeepAliveIntervalMS:      defaultOptions.TCPKeepAliveIntervalMS,
			},
			wantInvalid: true,
		},
		{
			name:   "HTTP rejects zero idle timeout in explicit structure",
			scheme: repository.OriginSchemeHTTP,
			options: repository.ServiceProxyOptions{
				HTTPMaxIdleConnections: defaultOptions.HTTPMaxIdleConnections,
			},
			wantInvalid: true,
		},
		{
			name:   "HTTP rejects zero idle connection count in explicit structure",
			scheme: repository.OriginSchemeHTTP,
			options: repository.ServiceProxyOptions{
				HTTPIdleConnectionTimeoutMS: defaultOptions.HTTPIdleConnectionTimeoutMS,
			},
			wantInvalid: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openServiceTestStore(t)
			seedServiceTestTunnel(t, store, repositoryTestTunnelID)
			service := testService(serviceTestIDOne, repositoryTestTunnelID)
			service.OriginScheme = test.scheme
			service.ProxyOptions = test.options
			services := serviceRepository{database: store.database}
			err := services.Create(context.Background(), service)
			if test.wantInvalid {
				if !errors.Is(err, repository.ErrInvalidService) {
					t.Fatalf("Create() error = %v, want ErrInvalidService", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := services.Get(context.Background(), service.TunnelID, service.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !reflect.DeepEqual(got.ProxyOptions, test.want) {
				t.Fatalf("Get().ProxyOptions = %#v, want %#v", got.ProxyOptions, test.want)
			}
		})
	}
}

func TestServiceProxyOptionsMigrationUpgradesV7AndPreservesDefaults(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:7], testNow); err != nil {
		t.Fatalf("run v7 migrations error = %v", err)
	}
	seedServiceProxyMigrationV7Data(t, database)

	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("upgrade to v8 error = %v", err)
	}
	var record serviceRecord
	if err := database.Where(ServiceColumns.ID+" = ?", repositoryRouteTestServiceID).Take(&record).Error; err != nil {
		t.Fatalf("read upgraded Service error = %v", err)
	}
	defaultOptions := (repository.ServiceProxyOptions{}).WithDefaults()
	got := repository.ServiceProxyOptions{
		DisableChunkedEncoding:      record.DisableChunkedEncoding,
		DisableHappyEyeballs:        record.DisableHappyEyeballs,
		HTTPIdleConnectionTimeoutMS: record.HTTPIdleConnectionTimeoutMS,
		HTTPMaxIdleConnections:      record.HTTPMaxIdleConnections,
		TCPKeepAliveIntervalMS:      record.TCPKeepAliveIntervalMS,
	}
	if !reflect.DeepEqual(got, defaultOptions) {
		t.Fatalf("upgraded ProxyOptions = %#v, want %#v", got, defaultOptions)
	}
	var versionCount int64
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count upgraded versions error = %v", err)
	}
	if versionCount != 8 {
		t.Fatalf("version count = %d, want 8", versionCount)
	}
}

func TestServiceProxyOptionsMigrationRollsBackAtomically(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:7], testNow); err != nil {
		t.Fatalf("run v7 migrations error = %v", err)
	}
	seedServiceProxyMigrationV7Data(t, database)

	failed := append([]migration{}, productionMigrations[:7]...)
	statements := append([]string{}, productionMigrations[7].statements...)
	statements = append(statements, "THIS IS NOT VALID SQL")
	failed = append(failed, migration{version: 8, statements: statements})
	if err := runMigrations(context.Background(), database, failed, testNow); err == nil {
		t.Fatal("failed v8 migration error = nil")
	}
	var columnCount int64
	if err := database.Raw(
		"SELECT COUNT(*) FROM pragma_table_info('services') WHERE name = ?",
		ServiceColumns.DisableChunkedEncoding,
	).Scan(&columnCount).Error; err != nil {
		t.Fatalf("inspect rolled-back column error = %v", err)
	}
	var serviceCount, versionCount int64
	if err := database.Table(ServiceTable).Count(&serviceCount).Error; err != nil {
		t.Fatalf("count preserved Services error = %v", err)
	}
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count versions after failed v8 error = %v", err)
	}
	if columnCount != 0 || serviceCount != 1 || versionCount != 7 {
		t.Fatalf("failed v8 state = columns:%d services:%d versions:%d, want 0/1/7", columnCount, serviceCount, versionCount)
	}
}

func TestServiceProxyOptionsMigrationEnforcesColumnConstraints(t *testing.T) {
	store := openServiceTestStore(t)
	seedServiceTestTunnel(t, store, repositoryTestTunnelID)
	services := serviceRepository{database: store.database}
	if err := services.Create(context.Background(), testService(serviceTestIDOne, repositoryTestTunnelID)); err != nil {
		t.Fatalf("seed Service error = %v", err)
	}

	tests := []struct {
		name string
		sql  string
	}{
		{name: "chunked boolean", sql: "UPDATE services SET disable_chunked_encoding = 2"},
		{name: "happy eyeballs boolean", sql: "UPDATE services SET disable_happy_eyeballs = 2"},
		{name: "zero HTTP idle timeout", sql: "UPDATE services SET http_idle_connection_timeout_ms = 0"},
		{name: "zero HTTP connection count", sql: "UPDATE services SET http_max_idle_connections = 0"},
		{name: "negative TCP keepalive", sql: "UPDATE services SET tcp_keepalive_interval_ms = -1"},
		{name: "HTTP timeout above uint32", sql: "UPDATE services SET http_idle_connection_timeout_ms = 4294967296"},
		{name: "HTTP connection count above uint32", sql: "UPDATE services SET http_max_idle_connections = 4294967296"},
		{name: "TCP keepalive above uint32", sql: "UPDATE services SET tcp_keepalive_interval_ms = 4294967296"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.database.Exec(test.sql).Error; err == nil {
				t.Fatal("invalid proxy option was accepted")
			}
		})
	}

	if err := store.database.Exec("UPDATE services SET origin_scheme = 'tcp'").Error; err != nil {
		t.Fatalf("switch test Service to TCP error = %v", err)
	}
	for _, test := range []struct {
		name string
		sql  string
	}{
		{name: "TCP chunked option", sql: "UPDATE services SET disable_chunked_encoding = 1"},
		{name: "TCP non-default HTTP timeout", sql: "UPDATE services SET http_idle_connection_timeout_ms = 89999"},
		{name: "TCP non-default HTTP connection count", sql: "UPDATE services SET http_max_idle_connections = 99"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := store.database.Exec(test.sql).Error; err == nil {
				t.Fatal("TCP Service accepted a non-default HTTP option")
			}
		})
	}
	if err := store.database.Exec("UPDATE services SET tcp_keepalive_interval_ms = 0").Error; err != nil {
		t.Fatalf("disable TCP keepalive error = %v", err)
	}
}

func seedServiceProxyMigrationV7Data(t *testing.T, database *gorm.DB) {
	t.Helper()
	if err := database.Exec(
		"INSERT INTO tunnels(id, name, version, desired_revision, created_at, updated_at) VALUES (?, 'office', 1, 0, 1, 1)",
		repositoryTestTunnelID,
	).Error; err != nil {
		t.Fatalf("seed v7 Tunnel error = %v", err)
	}
	if err := database.Exec(
		`INSERT INTO services(id, tunnel_id, name, origin_scheme, origin_host, origin_port, created_at, updated_at)
		 VALUES (?, ?, 'api', 'http', '127.0.0.1', 8080, 1, 1)`,
		repositoryRouteTestServiceID, repositoryTestTunnelID,
	).Error; err != nil {
		t.Fatalf("seed v7 Service error = %v", err)
	}
}
