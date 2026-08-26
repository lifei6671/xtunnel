package workauth

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

const (
	serverTunnelID       = "tun_01J00000000000000000000000"
	serverOtherTunnelID  = "tun_01J00000000000000000000001"
	serverConnectorID    = "con_01J00000000000000000000000"
	serverOtherConnector = "con_01J00000000000000000000001"
	serverSessionID      = "sess_01J00000000000000000000000"
	serverOtherSessionID = "sess_01J00000000000000000000001"
	serverLeaseID        = "lease_01J00000000000000000000000"
	serverOtherLeaseID   = "lease_01J00000000000000000000001"
	serverWorkID         = "work_01J00000000000000000000000"
	serverOtherWorkID    = "work_01J00000000000000000000001"
)

func TestSessionAuthenticatorAcceptsFrozenGoldenHMACAndCopiesSecret(t *testing.T) {
	clock := &fakeClock{}
	secret := filledBytes(0x11, sessionSecretSize)
	authenticator, err := New(validSession(secret), 16, clock.Now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// 构造后改写调用方切片，认证仍应使用内部复制的 0x11 Secret。
	clear(secret)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
		t.Fatalf("GrantLease() error = %v", err)
	}
	hello := goldenHello(t)
	if got := hex.EncodeToString(hello.GetMac()); got != "0ca89abb5945cb3e765eb8410606a2b2d1d7c619be45cf68c8cd837f2e239b7b" {
		t.Fatalf("golden WorkHello MAC = %s", got)
	}
	if err := authenticator.ValidateAndConsume(hello); err != nil {
		t.Fatalf("ValidateAndConsume() error = %v", err)
	}
	if authenticator.leases[serverLeaseID].remaining != 0 || len(authenticator.replayIndex) != 1 {
		t.Fatalf("post-consume state = remaining=%d replay=%d", authenticator.leases[serverLeaseID].remaining, len(authenticator.replayIndex))
	}
}

func TestNewRejectsInvalidSessionConfiguration(t *testing.T) {
	valid := validSession(filledBytes(0x11, sessionSecretSize))
	tests := []struct {
		name      string
		mutate    func(*Session)
		maxReplay int
		clock     MonotonicClock
	}{
		{name: "Tunnel ID", mutate: func(session *Session) { session.TunnelID = "tun_invalid" }, maxReplay: 1, clock: (&fakeClock{}).Now},
		{name: "Connector ID", mutate: func(session *Session) { session.ConnectorID = "con_invalid" }, maxReplay: 1, clock: (&fakeClock{}).Now},
		{name: "Session ID", mutate: func(session *Session) { session.SessionID = "sess_invalid" }, maxReplay: 1, clock: (&fakeClock{}).Now},
		{name: "zero generation", mutate: func(session *Session) { session.Generation = 0 }, maxReplay: 1, clock: (&fakeClock{}).Now},
		{name: "short secret", mutate: func(session *Session) { session.Secret = make([]byte, sessionSecretSize-1) }, maxReplay: 1, clock: (&fakeClock{}).Now},
		{name: "zero replay capacity", mutate: func(*Session) {}, maxReplay: 0, clock: (&fakeClock{}).Now},
		{name: "nil clock", mutate: func(*Session) {}, maxReplay: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := valid
			test.mutate(&session)
			if _, err := New(session, test.maxReplay, test.clock); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestGrantLeaseValidatesInputWithoutOverwritingState(t *testing.T) {
	tests := []struct {
		name    string
		leaseID string
		slots   uint32
		ttl     time.Duration
	}{
		{name: "invalid ID", leaseID: "lease_invalid", slots: 1, ttl: time.Second},
		{name: "zero slots", leaseID: serverLeaseID, ttl: time.Second},
		{name: "zero TTL", leaseID: serverLeaseID, slots: 1},
		{name: "negative TTL", leaseID: serverLeaseID, slots: 1, ttl: -time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, _ := newTestAuthenticator(t, 8)
			assertDecision(t, authenticator.GrantLease(test.leaseID, test.slots, test.ttl), ReasonProtocol, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR)
			if len(authenticator.leases) != 0 {
				t.Fatal("invalid GrantLease mutated lease state")
			}
		})
	}

	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 2, time.Second); err != nil {
		t.Fatalf("GrantLease(first) error = %v", err)
	}
	assertDecision(t, authenticator.GrantLease(serverLeaseID, 9, time.Minute), ReasonProtocol, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR)
	if got := authenticator.leases[serverLeaseID].remaining; got != 2 {
		t.Fatalf("duplicate GrantLease remaining = %d, want 2", got)
	}
}

func TestRevokeLeaseRejectsSubsequentWorkHello(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 2, time.Minute); err != nil {
		t.Fatalf("GrantLease() error = %v", err)
	}
	if err := authenticator.RevokeLease(serverLeaseID); err != nil {
		t.Fatalf("RevokeLease() error = %v", err)
	}
	if err := authenticator.RevokeLease(serverLeaseID); err != nil {
		t.Fatalf("RevokeLease(idempotent) error = %v", err)
	}
	assertDecision(t, authenticator.ValidateAndConsume(goldenHello(t)), ReasonLeaseInvalid,
		protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)
	if len(authenticator.leases) != 0 || len(authenticator.replayIndex) != 0 {
		t.Fatalf("revoked state = leases=%d replay=%d, want zero", len(authenticator.leases), len(authenticator.replayIndex))
	}
}

func TestValidateRejectsShapeIdentityAndMACBeforeStateMutation(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*protocolv1.WorkHello)
		wantReason Reason
		wantCode   protocolv1.ErrorCode
	}{
		{name: "unknown field", mutate: func(hello *protocolv1.WorkHello) { hello.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01}) }, wantReason: ReasonProtocol, wantCode: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR},
		{name: "invalid Work ID", mutate: func(hello *protocolv1.WorkHello) { hello.WorkId = "work_invalid" }, wantReason: ReasonProtocol, wantCode: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR},
		{name: "Tunnel mismatch", mutate: func(hello *protocolv1.WorkHello) { hello.TunnelId = serverOtherTunnelID; resignHello(t, hello, 0x11) }, wantReason: ReasonSessionInvalid, wantCode: protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID},
		{name: "Connector mismatch", mutate: func(hello *protocolv1.WorkHello) {
			hello.ConnectorId = serverOtherConnector
			resignHello(t, hello, 0x11)
		}, wantReason: ReasonSessionInvalid, wantCode: protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID},
		{name: "Session mismatch", mutate: func(hello *protocolv1.WorkHello) { hello.SessionId = serverOtherSessionID; resignHello(t, hello, 0x11) }, wantReason: ReasonSessionInvalid, wantCode: protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID},
		{name: "short nonce", mutate: func(hello *protocolv1.WorkHello) { hello.Nonce = hello.Nonce[:31] }, wantReason: ReasonProtocol, wantCode: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR},
		{name: "short MAC", mutate: func(hello *protocolv1.WorkHello) { hello.Mac = hello.Mac[:31] }, wantReason: ReasonProtocol, wantCode: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR},
		{name: "wrong MAC", mutate: func(hello *protocolv1.WorkHello) { hello.Mac = filledBytes(0xff, sessionSecretSize) }, wantReason: ReasonSessionInvalid, wantCode: protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, _ := newTestAuthenticator(t, 8)
			if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
				t.Fatalf("GrantLease() error = %v", err)
			}
			hello := signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)
			test.mutate(hello)
			assertDecision(t, authenticator.ValidateAndConsume(hello), test.wantReason, test.wantCode)
			if got := authenticator.leases[serverLeaseID].remaining; got != 1 || len(authenticator.replayIndex) != 0 {
				t.Fatalf("failed decision mutated state: remaining=%d replay=%d", got, len(authenticator.replayIndex))
			}
		})
	}
}

func TestValidateRejectsNilHelloWithoutStateMutation(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
		t.Fatalf("GrantLease() error = %v", err)
	}
	assertDecision(t, authenticator.ValidateAndConsume(nil), ReasonProtocol, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR)
	if authenticator.leases[serverLeaseID].remaining != 1 || len(authenticator.replayIndex) != 0 {
		t.Fatal("nil WorkHello mutated lease or replay state")
	}
}

func TestValidateMapsUnknownLeaseBudgetAndReplay(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	unknownLease := signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)
	assertDecision(t, authenticator.ValidateAndConsume(unknownLease), ReasonLeaseInvalid, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)

	if err := authenticator.GrantLease(serverLeaseID, 2, time.Minute); err != nil {
		t.Fatalf("GrantLease() error = %v", err)
	}
	if err := authenticator.ValidateAndConsume(unknownLease); err != nil {
		t.Fatalf("ValidateAndConsume(first) error = %v", err)
	}
	assertDecision(t, authenticator.ValidateAndConsume(unknownLease), ReasonReplay, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)
	if got := authenticator.leases[serverLeaseID].remaining; got != 1 {
		t.Fatalf("replay consumed slot: remaining=%d, want 1", got)
	}

	second := signedHello(t, serverOtherWorkID, serverLeaseID, 0x43, 0x11)
	if err := authenticator.ValidateAndConsume(second); err != nil {
		t.Fatalf("ValidateAndConsume(second) error = %v", err)
	}
	third := signedHello(t, "work_01J00000000000000000000002", serverLeaseID, 0x44, 0x11)
	assertDecision(t, authenticator.ValidateAndConsume(third), ReasonBudgetExhausted, protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED)
}

func TestReplayIndexRejectsSameWorkAcrossActiveLeases(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := authenticator.GrantLease(serverOtherLeaseID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := authenticator.ValidateAndConsume(signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)); err != nil {
		t.Fatal(err)
	}
	replayed := signedHello(t, serverWorkID, serverOtherLeaseID, 0x43, 0x11)
	assertDecision(t, authenticator.ValidateAndConsume(replayed), ReasonReplay, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)
	if authenticator.leases[serverOtherLeaseID].remaining != 1 {
		t.Fatal("cross-lease replay consumed a slot")
	}
}

func TestLeaseExpiryUsesOnlyInjectedMonotonicClockAndCleansReplay(t *testing.T) {
	authenticator, clock := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 2, time.Second); err != nil {
		t.Fatalf("GrantLease() error = %v", err)
	}
	hello := signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)
	if err := authenticator.ValidateAndConsume(hello); err != nil {
		t.Fatalf("ValidateAndConsume() error = %v", err)
	}
	clock.Advance(time.Second)
	assertDecision(t, authenticator.ValidateAndConsume(hello), ReasonLeaseExpired, protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED)
	if len(authenticator.leases) != 0 || len(authenticator.replayIndex) != 0 {
		t.Fatalf("expired state = leases=%d replay=%d, want zero", len(authenticator.leases), len(authenticator.replayIndex))
	}
}

func TestReplayCapacityFailureIsAtomicAndExpiresByLeaseBucket(t *testing.T) {
	authenticator, clock := newTestAuthenticator(t, 1)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := authenticator.GrantLease(serverOtherLeaseID, 1, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := authenticator.ValidateAndConsume(signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)); err != nil {
		t.Fatal(err)
	}
	second := signedHello(t, serverOtherWorkID, serverOtherLeaseID, 0x43, 0x11)
	assertDecision(t, authenticator.ValidateAndConsume(second), ReasonReplayCapacity, protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED)
	if authenticator.leases[serverOtherLeaseID].remaining != 1 {
		t.Fatal("capacity failure consumed target lease slot")
	}
	clock.Advance(2 * time.Second)
	if err := authenticator.ValidateAndConsume(second); err != nil {
		t.Fatalf("ValidateAndConsume(after expiry cleanup) error = %v", err)
	}
	if len(authenticator.replayIndex) != 1 {
		t.Fatalf("replay count after bucket cleanup = %d, want 1", len(authenticator.replayIndex))
	}
}

func TestConcurrentSameHelloHasSingleLinearizedSuccess(t *testing.T) {
	const attempts = 64
	authenticator, _ := newTestAuthenticator(t, attempts)
	if err := authenticator.GrantLease(serverLeaseID, attempts, time.Minute); err != nil {
		t.Fatal(err)
	}
	hello := signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)
	results := make(chan error, attempts)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- authenticator.ValidateAndConsume(hello)
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		assertDecision(t, err, ReasonReplay, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)
	}
	if successes != 1 || authenticator.leases[serverLeaseID].remaining != attempts-1 || len(authenticator.replayIndex) != 1 {
		t.Fatalf("concurrent same hello = success=%d remaining=%d replay=%d", successes, authenticator.leases[serverLeaseID].remaining, len(authenticator.replayIndex))
	}
}

func TestConcurrentDifferentWorkNeverExceedsLeaseSlots(t *testing.T) {
	const (
		attempts = 100
		slots    = 10
	)
	authenticator, _ := newTestAuthenticator(t, attempts)
	if err := authenticator.GrantLease(serverLeaseID, slots, time.Minute); err != nil {
		t.Fatal(err)
	}
	hellos := make([]*protocolv1.WorkHello, 0, attempts)
	for index := range attempts {
		workID := fmt.Sprintf("work_%026d", index)
		hellos = append(hellos, signedHello(t, workID, serverLeaseID, byte(index), 0x11))
	}
	results := make(chan error, attempts)
	var wait sync.WaitGroup
	for _, hello := range hellos {
		wait.Add(1)
		go func(hello *protocolv1.WorkHello) {
			defer wait.Done()
			results <- authenticator.ValidateAndConsume(hello)
		}(hello)
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		assertDecision(t, err, ReasonBudgetExhausted, protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED)
	}
	if successes != slots || authenticator.leases[serverLeaseID].remaining != 0 || len(authenticator.replayIndex) != slots {
		t.Fatalf("concurrent different work = success=%d remaining=%d replay=%d", successes, authenticator.leases[serverLeaseID].remaining, len(authenticator.replayIndex))
	}
}

func TestCloseClearsSecretAndStateAndIsIdempotent(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	hello := signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)
	if err := authenticator.ValidateAndConsume(hello); err != nil {
		t.Fatal(err)
	}
	authenticator.Close()
	authenticator.Close()
	if !authenticator.closed || authenticator.leases != nil || authenticator.replayIndex != nil ||
		!bytes.Equal(authenticator.secret[:], make([]byte, sessionSecretSize)) {
		t.Fatalf("Close() did not clear state: closed=%t leases=%v replay=%v secret=%x", authenticator.closed, authenticator.leases, authenticator.replayIndex, authenticator.secret)
	}
	assertDecision(t, authenticator.ValidateAndConsume(hello), ReasonClosed, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)
	assertDecision(t, authenticator.GrantLease(serverOtherLeaseID, 1, time.Minute), ReasonClosed, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)
}

func TestConcurrentValidateAndCloseDoesNotRaceSecretOrState(t *testing.T) {
	const attempts = 64
	authenticator, _ := newTestAuthenticator(t, attempts)
	if err := authenticator.GrantLease(serverLeaseID, attempts, time.Minute); err != nil {
		t.Fatal(err)
	}
	hellos := make([]*protocolv1.WorkHello, 0, attempts)
	for index := range attempts {
		hellos = append(hellos, signedHello(t, fmt.Sprintf("work_%026d", index), serverLeaseID, byte(index), 0x11))
	}
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wait sync.WaitGroup
	for _, hello := range hellos {
		wait.Add(1)
		go func(hello *protocolv1.WorkHello) {
			defer wait.Done()
			<-start
			results <- authenticator.ValidateAndConsume(hello)
		}(hello)
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		authenticator.Close()
	}()
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err == nil {
			continue
		}
		assertDecision(t, err, ReasonClosed, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID)
	}
	if !authenticator.closed || !bytes.Equal(authenticator.secret[:], make([]byte, sessionSecretSize)) {
		t.Fatal("concurrent Close did not leave a closed, cleared authenticator")
	}
}

func TestDecisionErrorDoesNotLeakAuthenticationMaterial(t *testing.T) {
	decision := decisionError(ReasonSessionInvalid)
	want := "server work auth rejected: reason=session_invalid code=ERROR_CODE_SESSION_INVALID"
	if decision.Error() != want {
		t.Fatalf("DecisionError.Error() = %q, want %q", decision.Error(), want)
	}
}

func newTestAuthenticator(t *testing.T, maxReplay int) (*SessionAuthenticator, *fakeClock) {
	t.Helper()
	clock := &fakeClock{}
	authenticator, err := New(validSession(filledBytes(0x11, sessionSecretSize)), maxReplay, clock.Now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return authenticator, clock
}

func validSession(secret []byte) Session {
	return Session{
		TunnelID: serverTunnelID, ConnectorID: serverConnectorID,
		SessionID: serverSessionID, Generation: 1, Secret: secret,
	}
}

func goldenHello(t *testing.T) *protocolv1.WorkHello {
	t.Helper()
	return signedHello(t, serverWorkID, serverLeaseID, 0x42, 0x11)
}

func signedHello(t *testing.T, workID, leaseID string, nonceByte, secretByte byte) *protocolv1.WorkHello {
	t.Helper()
	hello := &protocolv1.WorkHello{
		TunnelId: serverTunnelID, ConnectorId: serverConnectorID, SessionId: serverSessionID,
		WorkId: workID, BudgetLeaseId: leaseID, Nonce: filledBytes(nonceByte, sessionSecretSize),
	}
	resignHello(t, hello, secretByte)
	return hello
}

func resignHello(t *testing.T, hello *protocolv1.WorkHello, secretByte byte) {
	t.Helper()
	hello.Mac = nil
	mac, err := deterministic.ComputeWorkHelloMAC(filledBytes(secretByte, sessionSecretSize), hello)
	if err != nil {
		t.Fatalf("ComputeWorkHelloMAC() error = %v", err)
	}
	hello.Mac = mac
}

func filledBytes(value byte, size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = value
	}
	return result
}

func assertDecision(t *testing.T, err error, wantReason Reason, wantCode protocolv1.ErrorCode) {
	t.Helper()
	var decision *DecisionError
	if !errors.As(err, &decision) {
		t.Fatalf("error = %v, want *DecisionError", err)
	}
	if decision.Reason != wantReason || decision.Code != wantCode {
		t.Fatalf("DecisionError = %#v, want reason=%s code=%s", decision, wantReason.String(), wantCode.String())
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Duration
}

func (clock *fakeClock) Now() time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(delta time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now += delta
}
