package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	repositorysqlite "github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	serverusage "github.com/lifei6671/xtunnel/internal/server/usage"
)

const (
	concurrentRotateTunnelID = "tun_01J00000000000000000000001"
	concurrentAdminID        = "adm_01J00000000000000000000000"
)

type concurrentSnapshotNotifier struct {
	marked chan string
}

func (notifier *concurrentSnapshotNotifier) MarkDirty(tunnelID string) error {
	select {
	case notifier.marked <- tunnelID:
		return nil
	default:
		return errors.New("snapshot reconcile notification capacity exceeded")
	}
}

// concurrentSQLiteUsageRepository 与生产 Bootstrap 适配层保持同一映射，只把 Usage
// Owner 的进程内 Delta 交给真实 SQLite Store；事务、writeGate 与 Rollup 仍由生产实现拥有。
type concurrentSQLiteUsageRepository struct {
	store *repositorysqlite.Store
}

func (adapter concurrentSQLiteUsageRepository) Flush(ctx context.Context, deltas []serverusage.Delta) error {
	persisted := make([]repository.UsageDelta, len(deltas))
	for index, delta := range deltas {
		persisted[index] = repository.UsageDelta{
			Bucket: delta.BucketTime, TunnelID: delta.TunnelID, ServiceID: delta.ServiceID,
			Connections: delta.Connections, IngressBytes: delta.IngressBytes,
			EgressBytes: delta.EgressBytes, Errors: delta.Errors,
		}
	}
	return adapter.store.Flush(ctx, persisted)
}

func (adapter concurrentSQLiteUsageRepository) Rollup(ctx context.Context, completedBefore time.Time) error {
	return adapter.store.Rollup(ctx, completedBefore)
}

func TestConcurrentConfigWriteUsageFlushAndTokenRotate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	store, err := repositorysqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open SQLite Store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedConcurrentSQLiteState(t, ctx, store)

	snapshotBuilder, err := serversnapshot.New(serversnapshot.Config{
		ProtocolVersion:      1,
		MaxServices:          serversnapshot.MaxServicesPerTunnel,
		MaxSnapshotBytes:     serversnapshot.MaxTunnelSnapshotSize,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	if err != nil {
		t.Fatalf("create Snapshot builder: %v", err)
	}
	healthBudget, err := healthbudget.New(healthbudget.Options{
		MaxTargetsPerTunnel: 2_000,
		MaxTargetsGlobal:    50_000,
	})
	if err != nil {
		t.Fatalf("create Health Budget Manager: %v", err)
	}
	if err := healthBudget.InitializeTunnel(testTunnelID, 1, 0); err != nil {
		t.Fatalf("initialize Health Budget Manager: %v", err)
	}
	notifier := &concurrentSnapshotNotifier{marked: make(chan string, 1)}
	serviceManagement := application.NewServiceManagementService(store, snapshotBuilder, notifier, healthBudget)

	protector, err := application.NewAES256GCMTokenProtector(bytes.Repeat([]byte{0x6a}, 32))
	if err != nil {
		t.Fatalf("create Token protector: %v", err)
	}
	tokens := application.NewConnectionTokenService(store, protector)
	issued, err := tokens.Issue(ctx, application.IssueConnectionTokenInput{
		TunnelID: concurrentRotateTunnelID,
		Endpoint: &protocolv1.GatewayEndpoint{Host: "gateway.concurrent.test", Port: 443},
		TLSTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{
			PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: bytes.Repeat([]byte{0x7b}, 32)},
		}},
	})
	if err != nil {
		t.Fatalf("issue initial Connection Token: %v", err)
	}
	auditWriter := application.NewSecurityAuditWriter(
		store,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	credentials := application.NewCredentialLifecycleService(tokens, auditWriter)

	usageOwner, err := serverusage.New(serverusage.Options{
		Repository: concurrentSQLiteUsageRepository{store: store},
	})
	if err != nil {
		t.Fatalf("create Usage Owner: %v", err)
	}
	usageStartedAt := time.Now().UTC()
	if err := usageOwner.ObserveOpen(testTunnelID, testServiceID, true); err != nil {
		t.Fatalf("record successful OPEN: %v", err)
	}
	if err := usageOwner.ObserveOpen(testTunnelID, testServiceID, false); err != nil {
		t.Fatalf("record failed OPEN: %v", err)
	}
	if err := usageOwner.AddIngressBytes(testTunnelID, testServiceID, 11); err != nil {
		t.Fatalf("record ingress bytes: %v", err)
	}
	if err := usageOwner.AddEgressBytes(testTunnelID, testServiceID, 17); err != nil {
		t.Fatalf("record egress bytes: %v", err)
	}

	type configOutcome struct {
		result application.ServiceMutationResult
		err    error
	}
	type rotationOutcome struct {
		result application.CredentialMutationResult
		err    error
	}
	configResult := make(chan configOutcome, 1)
	usageResult := make(chan error, 1)
	rotationResult := make(chan rotationOutcome, 1)
	start := make(chan struct{})
	var ready sync.WaitGroup
	var workers sync.WaitGroup
	ready.Add(3)
	workers.Add(3)

	// 三个 goroutine 在同一屏障后进入各自正式入口。它们写不同业务聚合，只有 Store
	// 的统一 writeGate 可以串行化 SQLite；任一 SQLITE_BUSY 都表示该边界未生效。
	go func() {
		defer workers.Done()
		ready.Done()
		<-start
		disabled := false
		result, updateErr := serviceManagement.Update(ctx, application.UpdateServiceInput{
			TunnelID: testTunnelID, ServiceID: testServiceID,
			ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1,
			Enabled: &disabled,
		})
		configResult <- configOutcome{result: result, err: updateErr}
	}()
	go func() {
		defer workers.Done()
		ready.Done()
		<-start
		usageResult <- usageOwner.Flush(ctx)
	}()
	go func() {
		defer workers.Done()
		ready.Done()
		<-start
		result, rotateErr := credentials.Rotate(ctx, application.CredentialMutationInput{
			TunnelID: concurrentRotateTunnelID, ExpectedVersion: 1,
			Audit: application.SecurityAuditContext{ActorID: concurrentAdminID},
		})
		rotationResult <- rotationOutcome{result: result, err: rotateErr}
	}()

	ready.Wait()
	close(start)
	workers.Wait()

	config := <-configResult
	usageErr := <-usageResult
	rotation := <-rotationResult
	for _, outcome := range []struct {
		operation string
		err       error
	}{
		{operation: "Config Write", err: config.err},
		{operation: "Usage Flush", err: usageErr},
		{operation: "Token Rotate", err: rotation.err},
	} {
		operation, operationErr := outcome.operation, outcome.err
		if operationErr == nil {
			continue
		}
		message := strings.ToUpper(operationErr.Error())
		if strings.Contains(message, "SQLITE_BUSY") || strings.Contains(message, "DATABASE IS LOCKED") {
			t.Fatalf("%s returned unhandled SQLITE_BUSY: %v", operation, operationErr)
		}
		t.Fatalf("%s error: %v", operation, operationErr)
	}

	if !config.result.Changed || config.result.TunnelRevision != 2 || config.result.Service.Version != 2 ||
		config.result.Service.RequiredRevision != 2 || config.result.Service.Enabled {
		t.Fatalf("Config Write result = %+v, want disabled Service at revision/version 2", config.result)
	}
	select {
	case tunnelID := <-notifier.marked:
		if tunnelID != testTunnelID {
			t.Fatalf("Snapshot notifier Tunnel = %q, want %q", tunnelID, testTunnelID)
		}
	default:
		t.Fatal("Config Write committed without Snapshot reconcile notification")
	}
	if rotation.result.TunnelVersion != 2 || rotation.result.Credential.TokenVersion != 2 ||
		rotation.result.Credential.Token == "" || rotation.result.Credential.Token == issued.Token {
		t.Fatalf("Token Rotate result versions = Tunnel %d Token %d, token_reused=%t",
			rotation.result.TunnelVersion,
			rotation.result.Credential.TokenVersion,
			rotation.result.Credential.Token == issued.Token,
		)
	}
	if _, err := tokens.Verify(ctx, issued.Token); !errors.Is(err, application.ErrConnectionTokenInactive) {
		t.Fatalf("verify old Token error = %v, want ErrConnectionTokenInactive", err)
	}
	if _, err := tokens.Verify(ctx, rotation.result.Credential.Token); err != nil {
		t.Fatalf("verify rotated Token: %v", err)
	}

	assertConcurrentSQLiteState(t, ctx, store, usageStartedAt)
}

func seedConcurrentSQLiteState(t *testing.T, ctx context.Context, store *repositorysqlite.Store) {
	t.Helper()
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		for _, tunnel := range []repository.Tunnel{
			{ID: testTunnelID, Name: "config-write", Version: 1, DesiredRevision: 1, CreatedAt: 1, UpdatedAt: 1},
			{ID: concurrentRotateTunnelID, Name: "token-rotate", Version: 1, CreatedAt: 1, UpdatedAt: 1},
		} {
			if err := transaction.Tunnels().Create(ctx, tunnel); err != nil {
				return err
			}
		}
		return transaction.Services().Create(ctx, repository.Service{
			ID: testServiceID, TunnelID: testTunnelID, Name: "concurrent-service",
			RequiredRevision: 1, OriginScheme: repository.OriginSchemeTCP,
			OriginHost: "127.0.0.1", OriginPort: 8080, TLSVerify: true,
			ConnectTimeoutMS: 2_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("seed concurrent SQLite state: %v", err)
	}
}

func assertConcurrentSQLiteState(
	t *testing.T,
	ctx context.Context,
	store *repositorysqlite.Store,
	usageStartedAt time.Time,
) {
	t.Helper()
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		configTunnel, err := view.Tunnels().Get(ctx, testTunnelID)
		if err != nil {
			return err
		}
		service, err := view.Services().Get(ctx, testTunnelID, testServiceID)
		if err != nil {
			return err
		}
		rotateTunnel, err := view.Tunnels().Get(ctx, concurrentRotateTunnelID)
		if err != nil {
			return err
		}
		activeToken, err := view.TunnelTokens().GetActiveByTunnel(ctx, concurrentRotateTunnelID)
		if err != nil {
			return err
		}
		usageCompletedAt := time.Now().UTC()
		usage, err := view.Usage().Today(ctx, usageStartedAt, testTunnelID, testServiceID)
		if err != nil {
			return err
		}
		if usageStartedAt.Year() != usageCompletedAt.Year() || usageStartedAt.YearDay() != usageCompletedAt.YearDay() {
			nextDayUsage, err := view.Usage().Today(ctx, usageCompletedAt, testTunnelID, testServiceID)
			if err != nil {
				return err
			}
			usage.Connections += nextDayUsage.Connections
			usage.IngressBytes += nextDayUsage.IngressBytes
			usage.EgressBytes += nextDayUsage.EgressBytes
			usage.Errors += nextDayUsage.Errors
		}
		if configTunnel.Version != 1 || configTunnel.DesiredRevision != 2 || service.Version != 2 ||
			service.RequiredRevision != 2 || service.Enabled {
			return fmt.Errorf(
				"Config Write state = Tunnel(version=%d revision=%d) Service(version=%d revision=%d enabled=%t)",
				configTunnel.Version, configTunnel.DesiredRevision,
				service.Version, service.RequiredRevision, service.Enabled,
			)
		}
		if rotateTunnel.Version != 2 || activeToken.Version != 2 || activeToken.Status != repository.TunnelTokenStatusActive {
			return fmt.Errorf(
				"Token Rotate state = Tunnel version %d, active Token(version=%d status=%s)",
				rotateTunnel.Version, activeToken.Version, activeToken.Status,
			)
		}
		wantUsage := (repository.UsageTotals{Connections: 1, IngressBytes: 11, EgressBytes: 17, Errors: 1})
		if usage != wantUsage {
			return fmt.Errorf("Usage Flush totals = %+v, want %+v", usage, wantUsage)
		}
		return nil
	}); err != nil {
		t.Fatalf("read concurrent SQLite state: %v", err)
	}
}
