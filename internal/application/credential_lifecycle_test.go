package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/token"
	"github.com/lifei6671/xtunnel/internal/repository"
	"google.golang.org/protobuf/proto"
)

const applicationTestAdminID = "adm_01J00000000000000000000000"

func TestCredentialLifecycleRevealRotateAndRevoke(t *testing.T) {
	dataDir := t.TempDir()
	store := openApplicationStore(t, dataDir)
	protector := testTokenProtector(t, 0xA1)
	tokens := NewConnectionTokenService(store, protector)
	seedApplicationTunnel(t, store)
	issued, err := tokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	lifecycle, output := newCredentialLifecycleTestService(t, store, tokens)
	auditContext := testSecurityAuditContext()

	revealed, err := lifecycle.Reveal(context.Background(), applicationTestTunnelID, auditContext)
	if err != nil {
		t.Fatalf("Reveal() error = %v", err)
	}
	if revealed != issued {
		t.Fatal("Reveal() did not return the byte-stable current Token")
	}

	rotated, err := lifecycle.Rotate(context.Background(), CredentialMutationInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: auditContext,
	})
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.TunnelVersion != 2 || rotated.Credential.TokenVersion != 2 || rotated.Credential.Token == issued.Token {
		t.Fatalf(
			"Rotate() versions = Tunnel %d Token %d, token_reused=%t",
			rotated.TunnelVersion, rotated.Credential.TokenVersion, rotated.Credential.Token == issued.Token,
		)
	}
	oldParsed, err := token.Parse(issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	newParsed, err := token.Parse(rotated.Credential.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(oldParsed.GetEndpoint(), newParsed.GetEndpoint()) || !proto.Equal(oldParsed.GetTlsTrust(), newParsed.GetTlsTrust()) {
		t.Fatal("Rotate() changed Endpoint or TLS Trust")
	}
	if _, err := tokens.Verify(context.Background(), issued.Token); !errors.Is(err, ErrConnectionTokenInactive) {
		t.Fatalf("Verify(old) error = %v, want ErrConnectionTokenInactive", err)
	}
	if _, err := tokens.Verify(context.Background(), rotated.Credential.Token); err != nil {
		t.Fatalf("Verify(rotated) error = %v", err)
	}

	revoked, err := lifecycle.Revoke(context.Background(), CredentialMutationInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 2, Audit: auditContext,
	})
	if err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if revoked.TunnelVersion != 3 || revoked.Credential.Token != "" || revoked.Credential.TokenVersion != 2 {
		t.Fatalf("Revoke() tunnel_version=%d token_version=%d token_present=%t",
			revoked.TunnelVersion, revoked.Credential.TokenVersion, revoked.Credential.Token != "")
	}
	if _, err := tokens.Verify(context.Background(), rotated.Credential.Token); !errors.Is(err, ErrConnectionTokenInactive) {
		t.Fatalf("Verify(revoked) error = %v, want ErrConnectionTokenInactive", err)
	}

	logText := output.String()
	for _, action := range []string{
		repository.SecurityAuditActionTokenReveal,
		repository.SecurityAuditActionTokenRotate,
		repository.SecurityAuditActionTokenRevoke,
	} {
		if !strings.Contains(logText, `"action":"`+action+`"`) {
			t.Fatalf("audit log does not contain action %s: %s", action, logText)
		}
	}
	for _, sensitive := range []string{issued.Token, rotated.Credential.Token, issued.TokenID, rotated.Credential.TokenID} {
		if strings.Contains(logText, sensitive) {
			t.Fatalf("audit log leaked credential material %q", sensitive)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	assertSensitiveBytesAbsent(t, dataDir, []byte(issued.Token), []byte(rotated.Credential.Token), oldParsed.GetAuthenticationSecret(), newParsed.GetAuthenticationSecret())
}

func TestCredentialLifecycleConcurrentRotateHasOneCASWinner(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA2))
	if _, err := tokens.Issue(context.Background(), testIssueInput()); err != nil {
		t.Fatal(err)
	}
	lifecycle, _ := newCredentialLifecycleTestService(t, store, tokens)

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := lifecycle.Rotate(context.Background(), CredentialMutationInput{
				TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
			})
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var success, conflict int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, repository.ErrVersionConflict):
			conflict++
		default:
			t.Fatalf("Rotate() unexpected error = %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent Rotate() success=%d conflict=%d", success, conflict)
	}
	assertTunnelAndActiveVersion(t, store, 2, 2)
}

func TestCredentialLifecycleConcurrentRotateAndRevokeHaveOneCommit(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA6))
	if _, err := tokens.Issue(context.Background(), testIssueInput()); err != nil {
		t.Fatal(err)
	}
	lifecycle, _ := newCredentialLifecycleTestService(t, store, tokens)
	input := CredentialMutationInput{TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext()}

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := lifecycle.Rotate(context.Background(), input)
		results <- err
	}()
	go func() {
		<-start
		_, err := lifecycle.Revoke(context.Background(), input)
		results <- err
	}()
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("Rotate/Revoke errors = %v, %v; want exactly one commit", first, second)
	}
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnelRecord, err := view.Tunnels().Get(context.Background(), applicationTestTunnelID)
		if err != nil {
			return err
		}
		if tunnelRecord.Version != 2 {
			return fmt.Errorf("Tunnel version = %d, want 2", tunnelRecord.Version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialLifecycleAuditFailureRollsBackRotation(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA3))
	issued, err := tokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, _ := newCredentialLifecycleTestService(t, store, tokens)
	conflictingID := "evt_01J00000000000000000000010"
	seedEvent := applicationSecurityAuditEvent()
	seedEvent.EventID = conflictingID
	seedEvent.OperationID = "op_01J00000000000000000000010"
	if err := lifecycle.audit.Append(context.Background(), seedEvent); err != nil {
		t.Fatal(err)
	}
	lifecycle.newAuditEventID = func() (string, error) { return conflictingID, nil }
	lifecycle.newAuditOperationID = func() (string, error) { return "op_01J00000000000000000000011", nil }

	_, err = lifecycle.Rotate(context.Background(), CredentialMutationInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	})
	if !errors.Is(err, repository.ErrSecurityAuditConflict) {
		t.Fatalf("Rotate() error = %v, want ErrSecurityAuditConflict", err)
	}
	assertTunnelAndActiveVersion(t, store, 1, 1)
	current, err := tokens.Current(context.Background(), applicationTestTunnelID)
	if err != nil || current.Token != issued.Token {
		t.Fatalf(
			"Current() after rollback error = %v, Tunnel version=%d token_matches=%t",
			err, current.TokenVersion, current.Token == issued.Token,
		)
	}
}

func TestCredentialLifecycleRevealDoesNotReturnTokenWhenAuditFails(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA7))
	if _, err := tokens.Issue(context.Background(), testIssueInput()); err != nil {
		t.Fatal(err)
	}
	lifecycle, _ := newCredentialLifecycleTestService(t, store, tokens)
	conflictingID := "evt_01J00000000000000000000012"
	seedEvent := applicationSecurityAuditEvent()
	seedEvent.EventID = conflictingID
	seedEvent.OperationID = "op_01J00000000000000000000012"
	if err := lifecycle.audit.Append(context.Background(), seedEvent); err != nil {
		t.Fatal(err)
	}
	lifecycle.newAuditEventID = func() (string, error) { return conflictingID, nil }
	lifecycle.newAuditOperationID = func() (string, error) { return "op_01J00000000000000000000013", nil }

	result, err := lifecycle.Reveal(context.Background(), applicationTestTunnelID, testSecurityAuditContext())
	if !errors.Is(err, repository.ErrSecurityAuditConflict) {
		t.Fatalf("Reveal() error = %v, want ErrSecurityAuditConflict", err)
	}
	if result != (ConnectionTokenResult{}) {
		t.Fatalf(
			"Reveal() returned credential despite audit failure: tunnel_set=%t token_id_set=%t version=%d token_set=%t",
			result.TunnelID != "", result.TokenID != "", result.TokenVersion, result.Token != "",
		)
	}
}

func TestCredentialLifecycleRevealReturnsCommittedResultOnPostCommitCleanupFailure(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	protector := testTokenProtector(t, 0xAB)
	seedTokens := NewConnectionTokenService(store, protector)
	issued, err := seedTokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("injected credential reveal cleanup failure")
	auditStore := &recordingAuditStore{Store: store}
	faultStore := &postCommitCleanupStore{Store: auditStore, err: cleanupErr}
	tokens := NewConnectionTokenService(faultStore, protector)
	lifecycle, output := newCredentialLifecycleTestService(t, faultStore, tokens)

	result, err := lifecycle.Reveal(context.Background(), applicationTestTunnelID, testSecurityAuditContext())
	if !errors.Is(err, repository.ErrPostCommitCleanup) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Reveal() error = %v, want post-commit cleanup causes", err)
	}
	if result != issued {
		t.Fatalf("Reveal() committed identity = Tunnel %q Token %q version=%d, want issued identity",
			result.TunnelID, result.TokenID, result.TokenVersion)
	}
	assertCommittedCredentialAudit(t, auditStore, output, repository.SecurityAuditActionTokenReveal, result.Token)
	current, currentErr := tokens.Current(context.Background(), applicationTestTunnelID)
	if currentErr != nil || current != issued {
		t.Fatalf("Current() after Reveal cleanup error = %v, token_stable=%t", currentErr, current == issued)
	}
}

func TestCredentialLifecycleRotateReturnsCommittedResultOnPostCommitCleanupFailure(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	protector := testTokenProtector(t, 0xAC)
	seedTokens := NewConnectionTokenService(store, protector)
	issued, err := seedTokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("injected credential rotate cleanup failure")
	auditStore := &recordingAuditStore{Store: store}
	faultStore := &postCommitCleanupStore{Store: auditStore, err: cleanupErr}
	tokens := NewConnectionTokenService(faultStore, protector)
	lifecycle, output := newCredentialLifecycleTestService(t, faultStore, tokens)

	result, err := lifecycle.Rotate(context.Background(), CredentialMutationInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	})
	if !errors.Is(err, repository.ErrPostCommitCleanup) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Rotate() error = %v, want post-commit cleanup causes", err)
	}
	if result.TunnelVersion != 2 || result.Credential.TokenVersion != 2 ||
		result.Credential.Token == "" || result.Credential.Token == issued.Token {
		t.Fatalf("Rotate() committed result = Tunnel version %d Token version %d token_present=%t token_reused=%t",
			result.TunnelVersion, result.Credential.TokenVersion, result.Credential.Token != "", result.Credential.Token == issued.Token)
	}
	assertCommittedCredentialAudit(t, auditStore, output, repository.SecurityAuditActionTokenRotate, result.Credential.Token)
	assertTunnelAndActiveVersion(t, store, 2, 2)
	if _, verifyErr := tokens.Verify(context.Background(), issued.Token); !errors.Is(verifyErr, ErrConnectionTokenInactive) {
		t.Fatalf("Verify(old) after committed Rotate error = %v, want ErrConnectionTokenInactive", verifyErr)
	}
	if _, verifyErr := tokens.Verify(context.Background(), result.Credential.Token); verifyErr != nil {
		t.Fatalf("Verify(new) after committed Rotate error = %v", verifyErr)
	}
}

func TestCredentialLifecycleRevokeReturnsCommittedResultOnPostCommitCleanupFailure(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	protector := testTokenProtector(t, 0xAD)
	seedTokens := NewConnectionTokenService(store, protector)
	issued, err := seedTokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("injected credential revoke cleanup failure")
	auditStore := &recordingAuditStore{Store: store}
	faultStore := &postCommitCleanupStore{Store: auditStore, err: cleanupErr}
	tokens := NewConnectionTokenService(faultStore, protector)
	lifecycle, output := newCredentialLifecycleTestService(t, faultStore, tokens)

	result, err := lifecycle.Revoke(context.Background(), CredentialMutationInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	})
	if !errors.Is(err, repository.ErrPostCommitCleanup) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Revoke() error = %v, want post-commit cleanup causes", err)
	}
	if result.TunnelVersion != 2 || result.Credential.TokenID != issued.TokenID ||
		result.Credential.TokenVersion != issued.TokenVersion || result.Credential.Token != "" {
		t.Fatalf("Revoke() committed result = Tunnel version %d Token %q version %d token_present=%t",
			result.TunnelVersion, result.Credential.TokenID, result.Credential.TokenVersion, result.Credential.Token != "")
	}
	assertCommittedCredentialAudit(t, auditStore, output, repository.SecurityAuditActionTokenRevoke, issued.Token)
	if _, verifyErr := tokens.Verify(context.Background(), issued.Token); !errors.Is(verifyErr, ErrConnectionTokenInactive) {
		t.Fatalf("Verify() after committed Revoke error = %v, want ErrConnectionTokenInactive", verifyErr)
	}
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnelRecord, readErr := view.Tunnels().Get(context.Background(), applicationTestTunnelID)
		if readErr != nil {
			return readErr
		}
		if tunnelRecord.Version != 2 {
			return fmt.Errorf("Tunnel version = %d, want 2", tunnelRecord.Version)
		}
		_, readErr = view.TunnelTokens().GetActiveByTunnel(context.Background(), applicationTestTunnelID)
		if !errors.Is(readErr, repository.ErrNotFound) {
			return fmt.Errorf("active Token after Revoke error = %v, want ErrNotFound", readErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTunnelLifecycleRevokeIsIdempotentAndConvergesRuntime(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA4))
	issued, err := tokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatal(err)
	}
	_, auditWriter := newTestAuditWriter(t, store)
	runtime := &recordingTunnelRevoker{}
	service := NewTunnelLifecycleService(store, auditWriter, runtime)
	input := TunnelRevokeInput{TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext()}

	first, err := service.Revoke(context.Background(), input)
	if err != nil {
		t.Fatalf("first Revoke() error = %v", err)
	}
	if first.TunnelVersion != 2 || first.AlreadyRevoked || runtime.calls.Load() != 1 {
		t.Fatalf("first Revoke() = %#v calls=%d", first, runtime.calls.Load())
	}
	if _, err := tokens.Verify(context.Background(), issued.Token); !errors.Is(err, ErrConnectionTokenTunnelRevoked) {
		t.Fatalf("Verify() after Tunnel revoke error = %v", err)
	}

	input.ExpectedVersion = 2
	second, err := service.Revoke(context.Background(), input)
	if err != nil {
		t.Fatalf("second Revoke() error = %v", err)
	}
	if second.TunnelVersion != 2 || !second.AlreadyRevoked || runtime.calls.Load() != 2 {
		t.Fatalf("second Revoke() = %#v calls=%d", second, runtime.calls.Load())
	}

	input.ExpectedVersion = 1
	if _, err := service.Revoke(context.Background(), input); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("stale Revoke() error = %v", err)
	}
	if runtime.calls.Load() != 2 {
		t.Fatalf("stale Revoke() runtime calls = %d, want 2", runtime.calls.Load())
	}
}

func TestCredentialLifecycleRejectsRevealAndRotateAfterTunnelRevoke(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	protector := testTokenProtector(t, 0xAE)
	tokens := NewConnectionTokenService(store, protector)
	if _, err := tokens.Issue(context.Background(), testIssueInput()); err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	auditStore := &recordingAuditStore{Store: store}
	_, auditWriter := newTestAuditWriter(t, auditStore)
	tunnelLifecycle := NewTunnelLifecycleService(auditStore, auditWriter, &recordingTunnelRevoker{})
	if _, err := tunnelLifecycle.Revoke(context.Background(), TunnelRevokeInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	}); err != nil {
		t.Fatalf("Tunnel Revoke() error = %v", err)
	}
	credentialLifecycle := NewCredentialLifecycleService(tokens, auditWriter)

	if _, err := credentialLifecycle.Reveal(
		context.Background(), applicationTestTunnelID, testSecurityAuditContext(),
	); !errors.Is(err, ErrConnectionTokenTunnelRevoked) {
		t.Fatalf("Reveal() error = %v, want ErrConnectionTokenTunnelRevoked", err)
	}
	if _, err := credentialLifecycle.Rotate(context.Background(), CredentialMutationInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 2, Audit: testSecurityAuditContext(),
	}); !errors.Is(err, ErrConnectionTokenTunnelRevoked) {
		t.Fatalf("Rotate() error = %v, want ErrConnectionTokenTunnelRevoked", err)
	}
	if events := auditStore.snapshot(); len(events) != 1 || events[0].Action != repository.SecurityAuditActionTunnelRevoke {
		t.Fatalf("audit events after rejected Reveal/Rotate = %#v, want only Tunnel Revoke", events)
	}
}

func TestTunnelLifecycleRuntimeFailureHappensAfterDurableCommit(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA5))
	issued, err := tokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatal(err)
	}
	auditStore := &recordingAuditStore{Store: store}
	_, auditWriter := newTestAuditWriter(t, auditStore)
	runtime := &recordingTunnelRevoker{err: errors.New("injected runtime close failure")}
	service := NewTunnelLifecycleService(auditStore, auditWriter, runtime)
	result, err := service.Revoke(context.Background(), TunnelRevokeInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	})
	if !errors.Is(err, ErrTunnelRuntimeConvergence) || result.TunnelVersion != 2 {
		t.Fatalf("Revoke() = %#v, %v", result, err)
	}
	if _, err := tokens.Verify(context.Background(), issued.Token); !errors.Is(err, ErrConnectionTokenTunnelRevoked) {
		t.Fatalf("durable revoke was rolled back after runtime error: %v", err)
	}
	events := auditStore.snapshot()
	if len(events) != 2 {
		t.Fatalf("persisted audit event count = %d, want 2", len(events))
	}
	if events[0].Result != repository.SecurityAuditResultSucceeded || events[0].ErrorCode != "" ||
		events[1].Result != repository.SecurityAuditResultFailed || events[1].ErrorCode != tunnelRuntimeConvergenceErrorCode {
		t.Fatalf(
			"audit results = first(%s,%s) second(%s,%s)",
			events[0].Result, events[0].ErrorCode, events[1].Result, events[1].ErrorCode,
		)
	}
	if events[0].EventID == events[1].EventID || events[0].OperationID == events[1].OperationID ||
		events[0].RequestID != events[1].RequestID || events[0].TraceID != events[1].TraceID {
		t.Fatal("runtime failure audit did not use distinct IDs with the same request/trace correlation")
	}
}

func TestTunnelLifecyclePostCommitCleanupStillCallsRuntime(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA9))
	issued, err := tokens.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("injected post-commit cleanup failure")
	faultStore := &postCommitCleanupStore{Store: store, err: cleanupErr}
	_, auditWriter := newTestAuditWriter(t, store)
	runtimeErr := errors.New("injected runtime close failure")
	runtime := &recordingTunnelRevoker{err: runtimeErr}
	service := NewTunnelLifecycleService(faultStore, auditWriter, runtime)

	result, err := service.Revoke(context.Background(), TunnelRevokeInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	})
	if result.TunnelVersion != 2 || !errors.Is(err, repository.ErrPostCommitCleanup) ||
		!errors.Is(err, cleanupErr) || !errors.Is(err, ErrTunnelRuntimeConvergence) || !errors.Is(err, runtimeErr) {
		t.Fatalf("Revoke() version=%d error=%v, want committed cleanup and runtime convergence causes", result.TunnelVersion, err)
	}
	if runtime.calls.Load() != 1 {
		t.Fatalf("runtime calls after committed cleanup failure = %d, want 1", runtime.calls.Load())
	}
	if _, err := tokens.Verify(context.Background(), issued.Token); !errors.Is(err, ErrConnectionTokenTunnelRevoked) {
		t.Fatalf("durable revoke missing after cleanup failure: %v", err)
	}
}

func TestTunnelLifecycleJoinsRuntimeFailureAuditError(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xAA))
	if _, err := tokens.Issue(context.Background(), testIssueInput()); err != nil {
		t.Fatal(err)
	}
	auditErr := errors.New("injected runtime failure audit error")
	auditStore := &recordingAuditStore{Store: store, failFailedEvent: auditErr}
	_, auditWriter := newTestAuditWriter(t, auditStore)
	runtime := &recordingTunnelRevoker{err: errors.New("injected runtime close failure")}
	service := NewTunnelLifecycleService(auditStore, auditWriter, runtime)

	result, err := service.Revoke(context.Background(), TunnelRevokeInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	})
	if result.TunnelVersion != 2 || !errors.Is(err, ErrTunnelRuntimeConvergence) || !errors.Is(err, auditErr) {
		t.Fatalf("Revoke() version=%d error=%v, want convergence and audit causes", result.TunnelVersion, err)
	}
	events := auditStore.snapshot()
	if len(events) != 1 || events[0].Result != repository.SecurityAuditResultSucceeded {
		t.Fatalf("persisted audit results after failure injection = %d, want only original success", len(events))
	}
}

func TestTunnelLifecycleAuditFailureRollsBackAndSkipsRuntime(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	tokens := NewConnectionTokenService(store, testTokenProtector(t, 0xA8))
	if _, err := tokens.Issue(context.Background(), testIssueInput()); err != nil {
		t.Fatal(err)
	}
	_, auditWriter := newTestAuditWriter(t, store)
	conflictingID := "evt_01J00000000000000000000014"
	seedEvent := applicationSecurityAuditEvent()
	seedEvent.EventID = conflictingID
	seedEvent.OperationID = "op_01J00000000000000000000014"
	if err := auditWriter.Append(context.Background(), seedEvent); err != nil {
		t.Fatal(err)
	}
	runtime := &recordingTunnelRevoker{}
	service := NewTunnelLifecycleService(store, auditWriter, runtime)
	service.newAuditEventID = func() (string, error) { return conflictingID, nil }
	service.newAuditOperationID = func() (string, error) { return "op_01J00000000000000000000015", nil }

	_, err := service.Revoke(context.Background(), TunnelRevokeInput{
		TunnelID: applicationTestTunnelID, ExpectedVersion: 1, Audit: testSecurityAuditContext(),
	})
	if !errors.Is(err, repository.ErrSecurityAuditConflict) {
		t.Fatalf("Revoke() error = %v, want ErrSecurityAuditConflict", err)
	}
	if runtime.calls.Load() != 0 {
		t.Fatalf("runtime calls after rolled-back revoke = %d", runtime.calls.Load())
	}
	assertTunnelAndActiveVersion(t, store, 1, 1)
}

type recordingTunnelRevoker struct {
	calls atomic.Int32
	err   error
}

type postCommitCleanupStore struct {
	repository.Store
	err error
}

func (store *postCommitCleanupStore) WithDurableTx(
	ctx context.Context,
	fn func(repository.TxStore) error,
) error {
	if err := store.Store.WithDurableTx(ctx, fn); err != nil {
		return err
	}
	return errors.Join(repository.ErrPostCommitCleanup, store.err)
}

type recordingAuditStore struct {
	repository.Store
	mu              sync.Mutex
	events          []repository.SecurityAuditEvent
	failFailedEvent error
}

func (store *recordingAuditStore) WithDurableTx(
	ctx context.Context,
	fn func(repository.TxStore) error,
) error {
	return store.Store.WithDurableTx(ctx, func(transaction repository.TxStore) error {
		return fn(&recordingAuditTx{TxStore: transaction, store: store})
	})
}

func (store *recordingAuditStore) snapshot() []repository.SecurityAuditEvent {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]repository.SecurityAuditEvent(nil), store.events...)
}

type recordingAuditTx struct {
	repository.TxStore
	store *recordingAuditStore
}

func (transaction *recordingAuditTx) SecurityAuditEvents() repository.SecurityAuditEventRepository {
	return &recordingAuditRepository{
		SecurityAuditEventRepository: transaction.TxStore.SecurityAuditEvents(),
		store:                        transaction.store,
	}
}

type recordingAuditRepository struct {
	repository.SecurityAuditEventRepository
	store *recordingAuditStore
}

func (audit *recordingAuditRepository) Append(ctx context.Context, event repository.SecurityAuditEvent) error {
	if event.Result == repository.SecurityAuditResultFailed && audit.store.failFailedEvent != nil {
		return audit.store.failFailedEvent
	}
	if err := audit.SecurityAuditEventRepository.Append(ctx, event); err != nil {
		return err
	}
	audit.store.mu.Lock()
	audit.store.events = append(audit.store.events, event)
	audit.store.mu.Unlock()
	return nil
}

func (revoker *recordingTunnelRevoker) RevokeTunnel(tunnelID string) error {
	if tunnelID != applicationTestTunnelID {
		return fmt.Errorf("unexpected Tunnel %s", tunnelID)
	}
	revoker.calls.Add(1)
	return revoker.err
}

func newCredentialLifecycleTestService(
	t *testing.T,
	store repository.Store,
	tokens *ConnectionTokenService,
) (*CredentialLifecycleService, *bytes.Buffer) {
	t.Helper()
	output, writer := newTestAuditWriter(t, store)
	return NewCredentialLifecycleService(tokens, writer), output
}

func newTestAuditWriter(t *testing.T, store repository.Store) (*bytes.Buffer, *SecurityAuditWriter) {
	t.Helper()
	var output bytes.Buffer
	logger, err := logging.New(&output, logging.Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	return &output, NewSecurityAuditWriter(store, logger)
}

func assertCommittedCredentialAudit(
	t *testing.T,
	store *recordingAuditStore,
	output *bytes.Buffer,
	action string,
	sensitiveToken string,
) {
	t.Helper()
	events := store.snapshot()
	if len(events) != 1 || events[0].Action != action || events[0].Result != repository.SecurityAuditResultSucceeded {
		t.Fatalf("committed audit events = %d, want one SUCCEEDED %s event", len(events), action)
	}
	logText := output.String()
	if !strings.Contains(logText, `"action":"`+action+`"`) ||
		!strings.Contains(logText, `"result":"`+repository.SecurityAuditResultSucceeded+`"`) {
		t.Fatalf("committed audit log missing action/result: %s", logText)
	}
	if sensitiveToken != "" && strings.Contains(logText, sensitiveToken) {
		t.Fatal("committed audit log leaked Connection Token")
	}
}

func testSecurityAuditContext() SecurityAuditContext {
	return SecurityAuditContext{
		ActorID: applicationTestAdminID, SourceIP: "127.0.0.1", RequestID: "req-test", TraceID: "trace-test",
	}
}

func assertTunnelAndActiveVersion(t *testing.T, store repository.Store, tunnelVersion, tokenVersion int64) {
	t.Helper()
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnelRecord, err := view.Tunnels().Get(context.Background(), applicationTestTunnelID)
		if err != nil {
			return err
		}
		active, err := view.TunnelTokens().GetActiveByTunnel(context.Background(), applicationTestTunnelID)
		if err != nil {
			return err
		}
		if tunnelRecord.Version != tunnelVersion || active.Version != tokenVersion {
			return fmt.Errorf("versions = Tunnel %d Token %d", tunnelRecord.Version, active.Version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
