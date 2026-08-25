package deterministic

import (
	"bytes"
	"errors"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

func TestMarshalSnapshotSortsCopyWithoutMutatingSource(t *testing.T) {
	snapshot := &protocolv1.AgentSnapshot{
		AgentId: "ag_01J00000000000000000000000",
		Bindings: []*protocolv1.TunnelBindingConfig{
			{TunnelId: "tunnel-b"},
			{TunnelId: "tunnel-a"},
		},
	}

	encoded, err := MarshalSnapshot(snapshot)
	if err != nil {
		t.Fatalf("MarshalSnapshot() error = %v", err)
	}
	if got := snapshot.GetBindings()[0].GetTunnelId(); got != "tunnel-b" {
		t.Fatalf("MarshalSnapshot() 改写了源 Binding 顺序: got %q", got)
	}

	expected := &protocolv1.AgentSnapshot{
		AgentId: "ag_01J00000000000000000000000",
		Bindings: []*protocolv1.TunnelBindingConfig{
			{TunnelId: "tunnel-a"},
			{TunnelId: "tunnel-b"},
		},
	}
	want, err := Marshal(expected)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("MarshalSnapshot() = %x, want %x", encoded, want)
	}
}

func TestWorkHelloMACIgnoresExistingMACAndRejectsInvalidSecret(t *testing.T) {
	hello := &protocolv1.WorkHello{
		AgentId:       "ag_01J00000000000000000000000",
		InstanceId:    "ai_01J00000000000000000000000",
		SessionId:     "sess_01J00000000000000000000000",
		WorkId:        "work_01J00000000000000000000000",
		BudgetLeaseId: "lease_01J00000000000000000000000",
		Nonce:         bytes.Repeat([]byte{0x42}, 32),
		Mac:           []byte("untrusted-existing-mac"),
	}
	secret := bytes.Repeat([]byte{0x11}, 32)

	first, err := ComputeWorkHelloMAC(secret, hello)
	if err != nil {
		t.Fatalf("ComputeWorkHelloMAC() error = %v", err)
	}
	hello.Mac = []byte("different-untrusted-mac")
	second, err := ComputeWorkHelloMAC(secret, hello)
	if err != nil {
		t.Fatalf("ComputeWorkHelloMAC() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("WorkHello 既有 mac 不应影响新的 HMAC 输入")
	}
	if _, err := ComputeWorkHelloMAC(secret[:31], hello); !errors.Is(err, ErrInvalidSessionSecret) {
		t.Fatalf("ComputeWorkHelloMAC() error = %v, want ErrInvalidSessionSecret", err)
	}
}

func TestComputeWorkHelloMACRejectsInvalidIDBeforeMAC(t *testing.T) {
	hello := &protocolv1.WorkHello{
		AgentId:       "ag_01J00000000000000000000000",
		InstanceId:    "ai_01J00000000000000000000000",
		SessionId:     "sess_01J00000000000000000000000",
		WorkId:        "work_invalid",
		BudgetLeaseId: "lease_01J00000000000000000000000",
	}
	if _, err := ComputeWorkHelloMAC(bytes.Repeat([]byte{0x11}, 32), hello); !errors.Is(err, validate.ErrInvalidID) {
		t.Fatalf("ComputeWorkHelloMAC() error = %v, want ErrInvalidID", err)
	}
}
