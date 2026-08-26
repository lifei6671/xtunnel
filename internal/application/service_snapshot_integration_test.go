package application

import (
	"context"
	"errors"
	"testing"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
)

func TestServiceManagementRealSnapshotGateRollsBackServiceLimit(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate, err := serversnapshot.New(serversnapshot.Config{
		ProtocolVersion:      1,
		MaxServices:          1,
		MaxSnapshotBytes:     serversnapshot.MaxTunnelSnapshotSize,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	if err != nil {
		t.Fatalf("snapshot.New() error = %v", err)
	}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne, serviceManagementIDTwo)

	first, err := service.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "first"),
	)
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	if first.TunnelRevision != 1 || first.Service.RequiredRevision != 1 {
		t.Fatalf("Create(first) result = %+v", nonSensitiveMutationState(first))
	}

	if _, err := service.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "second"),
	); !errors.Is(err, serversnapshot.ErrServiceLimit) {
		t.Fatalf("Create(second) error = %v, want ErrServiceLimit", err)
	}
	assertServiceManagementState(t, store, serviceManagementTunnelID, 1, 1)
}
