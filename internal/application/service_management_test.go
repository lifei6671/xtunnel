package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/repository"
	repositorysqlite "github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

const (
	serviceManagementTunnelID    = "tun_01J00000000000000000000010"
	serviceManagementTunnelIDTwo = "tun_01J00000000000000000000011"
	serviceManagementIDOne       = "svc_01J00000000000000000000010"
	serviceManagementIDTwo       = "svc_01J00000000000000000000011"
	serviceManagementIDThree     = "svc_01J00000000000000000000012"
	serviceManagementConnector   = "con_01J00000000000000000000010"
)

func TestServiceManagementRequiresHealthBudget(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	service := NewServiceManagementService(store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, nil)
	if _, err := service.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "missing-budget"),
	); !errors.Is(err, ErrServiceManagementInput) {
		t.Fatalf("Create(without Health Budget) error = %v, want ErrServiceManagementInput", err)
	}
	assertServiceManagementState(t, store, serviceManagementTunnelID, 0, 0)
}

func TestServiceManagementHealthBudgetRejectsTunnelAndGlobalCapacity(t *testing.T) {
	for _, test := range []struct {
		name              string
		perTunnel, global uint64
		secondTunnel      string
	}{
		{name: "per tunnel", perTunnel: 1, global: 2, secondTunnel: serviceManagementTunnelID},
		{name: "global", perTunnel: 1, global: 1, secondTunnel: serviceManagementTunnelIDTwo},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openServiceManagementStore(t)
			seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
			seedServiceManagementTunnel(t, store, serviceManagementTunnelIDTwo)
			budget := newServiceManagementBudget(store, test.perTunnel, test.global)
			firstConnector, err := budget.AcquireConnector(serviceManagementTunnelID, serviceManagementConnector)
			if err != nil {
				t.Fatal(err)
			}
			defer firstConnector.Release()
			if test.secondTunnel != serviceManagementTunnelID {
				secondConnector, err := budget.AcquireConnector(test.secondTunnel, serviceManagementConnector)
				if err != nil {
					t.Fatal(err)
				}
				defer secondConnector.Release()
			}
			service := newServiceManagementTestServiceWithBudget(
				store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, budget,
				serviceManagementIDOne, serviceManagementIDTwo,
			)
			first := validCreateServiceInput(serviceManagementTunnelID, "first")
			first.Health = &ServiceHealthInput{Type: repository.HealthTypeTCP}
			if _, err := service.Create(context.Background(), first); err != nil {
				t.Fatalf("Create(first) error = %v", err)
			}
			second := validCreateServiceInput(test.secondTunnel, "second")
			second.Health = &ServiceHealthInput{Type: repository.HealthTypeTCP}
			if _, err := service.Create(context.Background(), second); !errors.Is(err, healthbudget.ErrTargetCapacity) {
				t.Fatalf("Create(over capacity) error = %v, want ErrTargetCapacity", err)
			}
			assertServiceManagementState(t, store, test.secondTunnel, map[string]int64{
				serviceManagementTunnelID: 1, serviceManagementTunnelIDTwo: 0,
			}[test.secondTunnel], map[string]int64{
				serviceManagementTunnelID: 1, serviceManagementTunnelIDTwo: 0,
			}[test.secondTunnel])
			snapshot := budget.Snapshot()
			if snapshot.Tunnels[test.secondTunnel].ReservationActive || snapshot.TargetsGlobal != 1 {
				t.Fatalf("failed budget reservation changed state: %+v", snapshot)
			}
		})
	}
}

func TestServiceManagementHealthBudgetTracksEnableDisableAndDelete(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	budget := newServiceManagementBudget(store, 10, 10)
	connector, err := budget.AcquireConnector(serviceManagementTunnelID, serviceManagementConnector)
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Release()
	service := newServiceManagementTestServiceWithBudget(
		store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, budget, serviceManagementIDOne,
	)
	input := validCreateServiceInput(serviceManagementTunnelID, "health")
	input.Health = &ServiceHealthInput{Type: repository.HealthTypeTCP}
	created, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	assertHealthBudgetTunnel(t, budget, 1, 1, 1)

	disabled := false
	updated, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHealthBudgetTunnel(t, budget, 2, 0, 0)

	enabled := true
	updated, err = service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: updated.Service.Version, Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHealthBudgetTunnel(t, budget, 3, 1, 1)
	if _, err := service.Delete(context.Background(), DeleteServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: updated.Service.Version,
	}); err != nil {
		t.Fatal(err)
	}
	assertHealthBudgetTunnel(t, budget, 4, 0, 0)
}

func TestServiceManagementHealthBudgetReleasesFailuresAndCommitsBeforeNotify(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	budget := newServiceManagementBudget(store, 10, 10)
	connector, err := budget.AcquireConnector(serviceManagementTunnelID, serviceManagementConnector)
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Release()

	gate := &recordingSnapshotGate{err: errors.New("gate failed")}
	service := newServiceManagementTestServiceWithBudget(
		store, gate, &recordingSnapshotNotifier{}, budget, serviceManagementIDOne, serviceManagementIDTwo,
	)
	input := validCreateServiceInput(serviceManagementTunnelID, "gate")
	input.Health = &ServiceHealthInput{Type: repository.HealthTypeTCP}
	if _, err := service.Create(context.Background(), input); err == nil {
		t.Fatal("Create(gate failure) succeeded")
	}
	assertHealthBudgetTunnel(t, budget, 0, 0, 0)

	gate.setError(nil)
	created, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	assertHealthBudgetTunnel(t, budget, 1, 1, 1)

	duplicateService := newServiceManagementTestServiceWithBudget(
		store, gate, &recordingSnapshotNotifier{}, budget, serviceManagementIDTwo,
	)
	duplicate := validCreateServiceInput(serviceManagementTunnelID, "duplicate")
	duplicate.Health = &ServiceHealthInput{Type: repository.HealthTypeTCP}
	if _, err := duplicateService.Create(context.Background(), duplicate); err == nil {
		t.Fatal("Create(database failure) succeeded")
	}
	assertHealthBudgetTunnel(t, budget, 1, 1, 1)

	name := "renamed"
	held, err := budget.ReserveConfiguration(serviceManagementTunnelID, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	nameOnly, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Name: &name,
	})
	if err != nil {
		t.Fatalf("Update(name only with active budget reservation) error = %v", err)
	}
	noOp, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: nameOnly.Service.Version, Name: &name,
	})
	if err != nil || noOp.Changed {
		t.Fatalf("Update(no-op with active budget reservation) = changed:%t error:%v", noOp.Changed, err)
	}
	if !held.Release() {
		t.Fatal("release held configuration reservation failed")
	}

	notifier := &recordingSnapshotNotifier{afterMark: func(_ string, _ int) {
		assertHealthBudgetTunnel(t, budget, 2, 0, 0)
	}}
	service = newServiceManagementTestServiceWithBudget(store, gate, notifier, budget)
	disabled := false
	if _, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: nameOnly.Service.Version, Enabled: &disabled,
	}); err != nil {
		t.Fatalf("Update(commit before notifier) error = %v", err)
	}
}

func TestServiceManagementCreateAppliesDefaultsAndGatesCandidate(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne)

	result, err := service.Create(context.Background(), CreateServiceInput{
		TunnelID: serviceManagementTunnelID, ExpectedTunnelVersion: 1, Name: "console",
		Origin: ServiceOriginInput{
			Scheme: repository.OriginSchemeHTTPS, Host: "127.0.0.1", Port: 8443,
			TLSServerName: "origin.example.test", HTTPHost: "app.example.test",
		},
		Health: &ServiceHealthInput{Type: repository.HealthTypeHTTP},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	created := result.Service
	if created.ID != serviceManagementIDOne || created.TunnelID != serviceManagementTunnelID ||
		created.RequiredRevision != 1 || created.Version != 1 || created.CreatedAt != 100 || created.UpdatedAt != 100 ||
		!created.TLSVerify || !created.Enabled || created.ConnectTimeoutMS != defaultServiceConnectTimeoutMS {
		t.Fatalf("Create() Service metadata/defaults = %+v", nonSensitiveServiceState(created))
	}
	if created.Health == nil || created.Health.Type != repository.HealthTypeHTTP ||
		created.Health.Path != defaultHealthPath || created.Health.IntervalMS != defaultHealthIntervalMS ||
		created.Health.TimeoutMS != defaultHealthTimeoutMS ||
		created.Health.ExpectedStatusMin != defaultHealthStatusMin || created.Health.ExpectedStatusMax != defaultHealthStatusMax ||
		created.Health.FailureThreshold != defaultHealthFailureThreshold ||
		created.Health.SuccessThreshold != defaultHealthSuccessThreshold {
		t.Fatalf("Create() Health defaults = %+v", created.Health)
	}
	if !result.Changed || result.TunnelVersion != 1 || result.TunnelRevision != 1 {
		t.Fatalf("Create() result = %+v", nonSensitiveMutationState(result))
	}

	calls := gate.snapshot()
	if len(calls) != 1 || calls[0].tunnelID != serviceManagementTunnelID || calls[0].revision != 1 ||
		len(calls[0].services) != 1 || !reflect.DeepEqual(calls[0].services[0], created) {
		t.Fatalf("Snapshot Gate calls = %+v", gateCallStates(calls))
	}
	assertStoredService(t, store, created)
}

func TestServiceManagementRejectsOriginThatAgentCannotCompileBeforeCommit(t *testing.T) {
	t.Run("Create", func(t *testing.T) {
		store := openServiceManagementStore(t)
		seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
		gate := &recordingSnapshotGate{}
		service := newServiceManagementTestService(store, gate, serviceManagementIDOne)
		input := validCreateServiceInput(serviceManagementTunnelID, "invalid-origin")
		input.Origin.Host = "Origin.Example"

		if _, err := service.Create(context.Background(), input); !errors.Is(err, ErrServiceManagementInput) {
			t.Fatalf("Create() error = %v, want ErrServiceManagementInput", err)
		}
		assertServiceManagementState(t, store, serviceManagementTunnelID, 0, 0)
		if len(gate.snapshot()) != 0 {
			t.Fatal("invalid Create reached Snapshot Gate")
		}
	})

	t.Run("Update", func(t *testing.T) {
		store := openServiceManagementStore(t)
		seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
		gate := &recordingSnapshotGate{}
		service := newServiceManagementTestService(store, gate, serviceManagementIDOne)
		created, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "valid"))
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		invalid := validServiceOriginInput()
		invalid.Host = "origin.example:8080"

		if _, err := service.Update(context.Background(), UpdateServiceInput{
			TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
			ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Origin: &invalid,
		}); !errors.Is(err, ErrServiceManagementInput) {
			t.Fatalf("Update() error = %v, want ErrServiceManagementInput", err)
		}
		assertServiceManagementState(t, store, serviceManagementTunnelID, 1, 1)
		stored := readServiceManagementService(t, store, serviceManagementTunnelID, created.Service.ID)
		if stored.Version != 1 || stored.OriginHost != created.Service.OriginHost {
			t.Fatalf("invalid Update changed Service = %+v", nonSensitiveServiceState(stored))
		}
		if len(gate.snapshot()) != 1 {
			t.Fatalf("invalid Update Gate calls = %d, want only initial Create", len(gate.snapshot()))
		}
	})
}

func TestServiceManagementHealthDefaultsAndRejectsTCPHTTPFieldMixing(t *testing.T) {
	interval := uint32(30_000)
	httpHealth, err := serviceHealth(&ServiceHealthInput{
		Type:       repository.HealthTypeHTTP,
		IntervalMS: &interval,
	})
	if err != nil {
		t.Fatalf("serviceHealth(HTTP partial) error = %v", err)
	}
	if httpHealth.Path != "/health" || httpHealth.IntervalMS != interval ||
		httpHealth.TimeoutMS != defaultHealthTimeoutMS ||
		httpHealth.ExpectedStatusMin != defaultHealthStatusMin ||
		httpHealth.ExpectedStatusMax != defaultHealthStatusMax ||
		httpHealth.FailureThreshold != defaultHealthFailureThreshold ||
		httpHealth.SuccessThreshold != defaultHealthSuccessThreshold {
		t.Fatalf("serviceHealth(HTTP partial) = %+v", httpHealth)
	}

	tcpHealth, err := serviceHealth(&ServiceHealthInput{Type: repository.HealthTypeTCP})
	if err != nil {
		t.Fatalf("serviceHealth(TCP partial) error = %v", err)
	}
	if tcpHealth.Path != "" || tcpHealth.IntervalMS != defaultHealthIntervalMS ||
		tcpHealth.TimeoutMS != defaultHealthTimeoutMS || tcpHealth.ExpectedStatusMin != 0 ||
		tcpHealth.ExpectedStatusMax != 0 || tcpHealth.FailureThreshold != defaultHealthFailureThreshold ||
		tcpHealth.SuccessThreshold != defaultHealthSuccessThreshold {
		t.Fatalf("serviceHealth(TCP partial) = %+v", tcpHealth)
	}

	path := "/health"
	status := uint32(200)
	for _, test := range []struct {
		name   string
		health ServiceHealthInput
	}{
		{name: "TCP path", health: ServiceHealthInput{Type: repository.HealthTypeTCP, Path: &path}},
		{name: "TCP status minimum", health: ServiceHealthInput{Type: repository.HealthTypeTCP, ExpectedStatusMin: &status}},
		{name: "TCP status maximum", health: ServiceHealthInput{Type: repository.HealthTypeTCP, ExpectedStatusMax: &status}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := serviceHealth(&test.health); !errors.Is(err, ErrServiceManagementInput) {
				t.Fatalf("serviceHealth() error = %v, want ErrServiceManagementInput", err)
			}
		})
	}
}

func TestServiceManagementGateFailureRollsBackAndPreservesCapacityError(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	capacityErr := errors.New("TUNNEL_SERVICE_LIMIT")
	gate := &recordingSnapshotGate{err: capacityErr}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne)

	result, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "blocked"))
	if !errors.Is(err, capacityErr) {
		t.Fatalf("Create() error = %v, want capacity error", err)
	}
	if result != (ServiceMutationResult{}) {
		t.Fatalf("Create() result after rollback = %+v", nonSensitiveMutationState(result))
	}
	assertServiceManagementState(t, store, serviceManagementTunnelID, 0, 0)
	if len(gate.snapshot()) != 1 {
		t.Fatalf("Snapshot Gate call count = %d, want 1", len(gate.snapshot()))
	}
}

func TestServiceManagementNotifiesOnlyCommittedSnapshotChanges(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	wantRevisions := []int64{1, 2, 3}
	wantServiceCounts := []int64{1, 1, 0}
	notifier := &recordingSnapshotNotifier{afterMark: func(tunnelID string, call int) {
		assertServiceManagementState(t, store, tunnelID, wantRevisions[call-1], wantServiceCounts[call-1])
	}}
	service := newServiceManagementTestServiceWithNotifier(
		store, gate, notifier, serviceManagementIDOne,
	)

	created, err := service.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "before"),
	)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	name := "after"
	nameOnly, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Name: &name,
	})
	if err != nil {
		t.Fatalf("Update(name only) error = %v", err)
	}
	if _, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: nameOnly.Service.Version, Name: &name,
	}); err != nil {
		t.Fatalf("Update(no-op) error = %v", err)
	}
	if calls := notifier.snapshot(); !reflect.DeepEqual(calls, []string{serviceManagementTunnelID}) {
		t.Fatalf("dirty calls after name-only/no-op Updates = %v, want Create only", calls)
	}

	disabled := false
	gateErr := errors.New("TUNNEL_SERVICE_LIMIT")
	gate.setError(gateErr)
	if _, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: nameOnly.Service.Version, Enabled: &disabled,
	}); !errors.Is(err, gateErr) {
		t.Fatalf("Update(gate failure) error = %v, want Gate error", err)
	}
	if calls := notifier.snapshot(); len(calls) != 1 {
		t.Fatalf("dirty calls after rolled-back Update = %v, want Create only", calls)
	}

	gate.setError(nil)
	updated, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: nameOnly.Service.Version, Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("Update(snapshot fields) error = %v", err)
	}
	if _, err := service.Delete(context.Background(), DeleteServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: updated.Service.Version,
	}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if calls := notifier.snapshot(); !reflect.DeepEqual(calls, []string{
		serviceManagementTunnelID, serviceManagementTunnelID, serviceManagementTunnelID,
	}) {
		t.Fatalf("committed Snapshot dirty calls = %v", calls)
	}
}

func TestServiceManagementNotifierFailureReturnsCommittedResult(t *testing.T) {
	signalErr := errors.New("injected reconcile signal failure")
	for _, operation := range []string{"Create", "Update", "Delete"} {
		t.Run(operation, func(t *testing.T) {
			store := openServiceManagementStore(t)
			seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
			gate := &recordingSnapshotGate{}
			var created ServiceMutationResult
			if operation != "Create" {
				var err error
				created, err = newServiceManagementTestService(store, gate, serviceManagementIDOne).Create(
					context.Background(), validCreateServiceInput(serviceManagementTunnelID, "before"),
				)
				if err != nil {
					t.Fatalf("seed Create() error = %v", err)
				}
			}

			notifier := &recordingSnapshotNotifier{err: signalErr}
			service := newServiceManagementTestServiceWithNotifier(
				store, gate, notifier, serviceManagementIDOne,
			)
			var result ServiceMutationResult
			var err error
			switch operation {
			case "Create":
				result, err = service.Create(
					context.Background(), validCreateServiceInput(serviceManagementTunnelID, "created"),
				)
			case "Update":
				disabled := false
				result, err = service.Update(context.Background(), UpdateServiceInput{
					TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
					ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
				})
			case "Delete":
				result, err = service.Delete(context.Background(), DeleteServiceInput{
					TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
					ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1,
				})
			}

			if !errors.Is(err, ErrServiceRuntimeConvergence) || !errors.Is(err, signalErr) {
				t.Fatalf("%s() error = %v, want convergence and signal causes", operation, err)
			}
			if !result.Changed || result.Service.ID == "" {
				t.Fatalf("%s() committed result = %+v", operation, nonSensitiveMutationState(result))
			}
			if calls := notifier.snapshot(); !reflect.DeepEqual(calls, []string{serviceManagementTunnelID}) {
				t.Fatalf("%s() dirty calls = %v", operation, calls)
			}
			switch operation {
			case "Create":
				assertStoredService(t, store, result.Service)
				assertServiceManagementState(t, store, serviceManagementTunnelID, 1, 1)
			case "Update":
				assertStoredService(t, store, result.Service)
				assertServiceManagementState(t, store, serviceManagementTunnelID, 2, 1)
			case "Delete":
				assertServiceManagementState(t, store, serviceManagementTunnelID, 2, 0)
			}
		})
	}
}

func TestServiceManagementPostCommitCleanupStillCommitsBudgetAndNotifies(t *testing.T) {
	cleanupErr := errors.New("injected service transaction cleanup failure")
	signalErr := errors.New("injected service reconcile signal failure")
	for _, operation := range []string{"Create", "Update", "Delete"} {
		t.Run(operation, func(t *testing.T) {
			store := openServiceManagementStore(t)
			seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
			gate := &recordingSnapshotGate{}
			budget := newServiceManagementBudget(store, 10, 10)

			var created ServiceMutationResult
			if operation != "Create" {
				seed := validCreateServiceInput(serviceManagementTunnelID, "before")
				if operation == "Delete" {
					seed.Health = &ServiceHealthInput{Type: repository.HealthTypeTCP}
				}
				var err error
				created, err = newServiceManagementTestServiceWithBudget(
					store, gate, &recordingSnapshotNotifier{}, budget, serviceManagementIDOne,
				).Create(context.Background(), seed)
				if err != nil {
					t.Fatalf("seed Create() error = %v", err)
				}
			}
			connector, err := budget.AcquireConnector(serviceManagementTunnelID, serviceManagementConnector)
			if err != nil {
				t.Fatal(err)
			}
			defer connector.Release()

			faultStore := &serviceManagementPostCommitCleanupStore{Store: store, err: cleanupErr}
			notifier := &recordingSnapshotNotifier{err: signalErr}
			identifiers := []string(nil)
			if operation == "Create" {
				identifiers = []string{serviceManagementIDOne}
			}
			service := newServiceManagementTestServiceWithBudget(
				faultStore, gate, notifier, budget, identifiers...,
			)
			var result ServiceMutationResult
			switch operation {
			case "Create":
				input := validCreateServiceInput(serviceManagementTunnelID, "created")
				input.Health = &ServiceHealthInput{Type: repository.HealthTypeTCP}
				result, err = service.Create(context.Background(), input)
			case "Update":
				result, err = service.Update(context.Background(), UpdateServiceInput{
					TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
					ExpectedTunnelVersion: 1, ExpectedServiceVersion: created.Service.Version,
					Health: &ServiceHealthInput{Type: repository.HealthTypeTCP},
				})
			case "Delete":
				result, err = service.Delete(context.Background(), DeleteServiceInput{
					TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
					ExpectedTunnelVersion: 1, ExpectedServiceVersion: created.Service.Version,
				})
			}

			if !errors.Is(err, repository.ErrPostCommitCleanup) || !errors.Is(err, cleanupErr) ||
				!errors.Is(err, ErrServiceRuntimeConvergence) || !errors.Is(err, signalErr) {
				t.Fatalf("%s() error = %v, want cleanup and convergence causes", operation, err)
			}
			if !result.Changed || result.Service.ID == "" {
				t.Fatalf("%s() committed result = %+v", operation, nonSensitiveMutationState(result))
			}
			if calls := notifier.snapshot(); !reflect.DeepEqual(calls, []string{serviceManagementTunnelID}) {
				t.Fatalf("%s() dirty calls = %v", operation, calls)
			}
			switch operation {
			case "Create":
				assertServiceManagementState(t, store, serviceManagementTunnelID, 1, 1)
				assertHealthBudgetTunnel(t, budget, 1, 1, 1)
			case "Update":
				assertServiceManagementState(t, store, serviceManagementTunnelID, 2, 1)
				assertHealthBudgetTunnel(t, budget, 2, 1, 1)
			case "Delete":
				assertServiceManagementState(t, store, serviceManagementTunnelID, 2, 0)
				assertHealthBudgetTunnel(t, budget, 2, 0, 0)
			}
		})
	}
}

func TestServiceManagementUpdateHandlesNameNoopAndZeroValueSnapshotFields(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne)
	created, err := service.Create(context.Background(), CreateServiceInput{
		TunnelID: serviceManagementTunnelID, ExpectedTunnelVersion: 1, Name: "before",
		Origin: ServiceOriginInput{
			Scheme: repository.OriginSchemeHTTPS, Host: "127.0.0.1", Port: 8443,
			TLSServerName: "origin.example.test", HTTPHost: "app.example.test",
		},
		Health: &ServiceHealthInput{Type: repository.HealthTypeHTTP},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	name := "after"
	nameOnly, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Name: &name,
	})
	if err != nil {
		t.Fatalf("Update(name only) error = %v", err)
	}
	if nameOnly.Service.Version != 2 || nameOnly.Service.RequiredRevision != 1 ||
		nameOnly.TunnelRevision != 1 || len(gate.snapshot()) != 1 {
		t.Fatalf("Update(name only) state = %+v, Gate calls = %d", nonSensitiveMutationState(nameOnly), len(gate.snapshot()))
	}

	enabled := true
	noOp, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 2,
		Name: &name,
		Origin: &ServiceOriginInput{
			Scheme: repository.OriginSchemeHTTPS, Host: "127.0.0.1", Port: 8443,
			TLSServerName: "origin.example.test", HTTPHost: "app.example.test",
		},
		Health:  &ServiceHealthInput{Type: repository.HealthTypeHTTP},
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("Update(no-op) error = %v", err)
	}
	if noOp.Changed || noOp.Service.Version != 2 || noOp.TunnelRevision != 1 || len(gate.snapshot()) != 1 {
		t.Fatalf("Update(no-op) state = %+v, Gate calls = %d", nonSensitiveMutationState(noOp), len(gate.snapshot()))
	}

	disabled := false
	connectTimeout := uint32(7_000)
	snapshotUpdate, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 2,
		Origin: &ServiceOriginInput{
			Scheme: repository.OriginSchemeHTTP, Host: "127.0.0.2", Port: 8080,
			TLSVerify: &disabled, ConnectTimeoutMS: &connectTimeout,
		},
		DisableHealth: true,
		Enabled:       &disabled,
	})
	if err != nil {
		t.Fatalf("Update(snapshot fields) error = %v", err)
	}
	updated := snapshotUpdate.Service
	if updated.Version != 3 || updated.RequiredRevision != 2 || updated.Health != nil || updated.Enabled ||
		updated.TLSVerify || updated.TLSServerName != "" || updated.OriginHTTPHost != "" ||
		updated.ConnectTimeoutMS != connectTimeout || snapshotUpdate.TunnelRevision != 2 || len(gate.snapshot()) != 2 {
		t.Fatalf("Update(snapshot fields) state = %+v, Gate calls = %d", nonSensitiveMutationState(snapshotUpdate), len(gate.snapshot()))
	}
	assertStoredService(t, store, updated)
}

func TestServiceManagementUpdateGateFailureRollsBackServiceAndRevision(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne)
	created, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "before"))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	gateErr := errors.New("TUNNEL_SERVICE_LIMIT")
	gate.setError(gateErr)
	disabled := false
	if _, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
	}); !errors.Is(err, gateErr) {
		t.Fatalf("Update(gate failure) error = %v, want Gate error", err)
	}
	assertStoredService(t, store, created.Service)
	assertServiceManagementState(t, store, serviceManagementTunnelID, 1, 1)
	calls := gate.snapshot()
	if len(calls) != 2 || calls[1].revision != 2 || len(calls[1].services) != 1 || calls[1].services[0].Enabled {
		t.Fatalf("Update(gate failure) Candidate calls = %+v", gateCallStates(calls))
	}
}

func TestServiceManagementDeleteGateFailureRollsBackThenCommits(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne)
	created, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "delete-me"))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	gateErr := errors.New("SNAPSHOT_TOO_LARGE")
	gate.setError(gateErr)
	input := DeleteServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1,
	}
	if _, err := service.Delete(context.Background(), input); !errors.Is(err, gateErr) {
		t.Fatalf("Delete(gate failure) error = %v, want Gate error", err)
	}
	assertStoredService(t, store, created.Service)
	assertServiceManagementState(t, store, serviceManagementTunnelID, 1, 1)

	gate.setError(nil)
	deleted, err := service.Delete(context.Background(), input)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !deleted.Changed || deleted.Service.ID != created.Service.ID || deleted.TunnelRevision != 2 {
		t.Fatalf("Delete() result = %+v", nonSensitiveMutationState(deleted))
	}
	assertServiceManagementState(t, store, serviceManagementTunnelID, 2, 0)
	last := gate.snapshot()[2]
	if last.revision != 2 || len(last.services) != 0 {
		t.Fatalf("Delete() Gate candidate = %+v", gateCallStates([]snapshotGateCall{last}))
	}
}

func TestServiceManagementFencesTunnelServiceOwnerVersionsAndRevoke(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelIDTwo)
	gate := &recordingSnapshotGate{}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne, serviceManagementIDTwo)
	created, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "owned"))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	tests := []struct {
		name  string
		input UpdateServiceInput
		want  error
	}{
		{
			name: "Tunnel Version CAS",
			input: UpdateServiceInput{TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
				ExpectedTunnelVersion: 2, ExpectedServiceVersion: 1},
			want: repository.ErrVersionConflict,
		},
		{
			name: "Service Version CAS",
			input: UpdateServiceInput{TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
				ExpectedTunnelVersion: 1, ExpectedServiceVersion: 2},
			want: repository.ErrVersionConflict,
		},
		{
			name: "Cross Tunnel owner",
			input: UpdateServiceInput{TunnelID: serviceManagementTunnelIDTwo, ServiceID: created.Service.ID,
				ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1},
			want: ErrServiceManagementUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.Update(context.Background(), test.input); !errors.Is(err, test.want) {
				t.Fatalf("Update() error = %v, want %v", err, test.want)
			}
		})
	}

	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		_, err := transaction.Tunnels().Revoke(context.Background(), serviceManagementTunnelID, 1, 200)
		return err
	}); err != nil {
		t.Fatalf("revoke Tunnel error = %v", err)
	}
	if _, err := service.Update(context.Background(), UpdateServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 2, ExpectedServiceVersion: 1,
	}); !errors.Is(err, ErrServiceManagementTunnelRevoked) {
		t.Fatalf("Update(revoked) error = %v, want ErrServiceManagementTunnelRevoked", err)
	}
	if _, err := service.Create(context.Background(), CreateServiceInput{
		TunnelID: serviceManagementTunnelID, ExpectedTunnelVersion: 2, Name: "after-revoke",
		Origin: validServiceOriginInput(),
	}); !errors.Is(err, ErrServiceManagementTunnelRevoked) {
		t.Fatalf("Create(revoked) error = %v, want ErrServiceManagementTunnelRevoked", err)
	}
	if len(gate.snapshot()) != 1 {
		t.Fatalf("failed mutations reached Gate: calls = %d, want 1", len(gate.snapshot()))
	}
}

func TestServiceManagementRejectsInvalidStoredRevisionThroughCreatePath(t *testing.T) {
	tests := []struct {
		name     string
		revision int64
	}{
		{name: "negative", revision: -1},
		{name: "maximum", revision: int64(^uint64(0) >> 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openServiceManagementStore(t)
			seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
			gate := &recordingSnapshotGate{}
			faultStore := revisionOverrideStore{Store: store, revision: test.revision}
			service := newServiceManagementTestService(faultStore, gate, serviceManagementIDOne)

			if _, err := service.Create(
				context.Background(), validCreateServiceInput(serviceManagementTunnelID, "blocked"),
			); !errors.Is(err, ErrServiceManagementRevisionExhausted) {
				t.Fatalf("Create() error = %v, want ErrServiceManagementRevisionExhausted", err)
			}
			assertServiceManagementState(t, store, serviceManagementTunnelID, 0, 0)
			if len(gate.snapshot()) != 0 {
				t.Fatalf("invalid stored revision reached Gate: calls = %d", len(gate.snapshot()))
			}
		})
	}
}

func TestServiceManagementConcurrentSameServiceHasSingleCASWinner(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne)
	created, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "before"))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	start := make(chan struct{})
	errorsByAttempt := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			disabled := false
			_, err := service.Update(context.Background(), UpdateServiceInput{
				TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
				ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
			})
			errorsByAttempt <- err
		}()
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		err := <-errorsByAttempt
		switch {
		case err == nil:
			successes++
		case errors.Is(err, repository.ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent Update() unexpected error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent Update() outcomes = success:%d conflict:%d", successes, conflicts)
	}
	stored := readServiceManagementService(t, store, serviceManagementTunnelID, created.Service.ID)
	if stored.Version != 2 || stored.RequiredRevision != 2 || stored.Enabled {
		t.Fatalf("concurrent Update() stored state = %+v", nonSensitiveServiceState(stored))
	}
	assertHealthBudgetTunnel(t, service.budget, 2, 0, 0)
}

func TestServiceManagementConcurrentDifferentServicesAdvanceDistinctRevisions(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	service := newServiceManagementTestService(store, gate, serviceManagementIDOne, serviceManagementIDTwo)
	first, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "first"))
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, err := service.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "second"))
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}

	start := make(chan struct{})
	results := make(chan ServiceMutationResult, 2)
	errorsByAttempt := make(chan error, 2)
	for _, serviceID := range []string{first.Service.ID, second.Service.ID} {
		serviceID := serviceID
		go func() {
			<-start
			disabled := false
			result, err := service.Update(context.Background(), UpdateServiceInput{
				TunnelID: serviceManagementTunnelID, ServiceID: serviceID,
				ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
			})
			results <- result
			errorsByAttempt <- err
		}()
	}
	close(start)
	revisions := make([]int, 0, 2)
	for range 2 {
		result, err := <-results, <-errorsByAttempt
		if err != nil {
			t.Fatalf("concurrent different-Service Update() error = %v", err)
		}
		revisions = append(revisions, int(result.TunnelRevision))
	}
	sort.Ints(revisions)
	if !reflect.DeepEqual(revisions, []int{3, 4}) {
		t.Fatalf("concurrent Tunnel revisions = %v, want [3 4]", revisions)
	}
	storedFirst := readServiceManagementService(t, store, serviceManagementTunnelID, first.Service.ID)
	storedSecond := readServiceManagementService(t, store, serviceManagementTunnelID, second.Service.ID)
	serviceRevisions := []int{int(storedFirst.RequiredRevision), int(storedSecond.RequiredRevision)}
	sort.Ints(serviceRevisions)
	if storedFirst.Version != 2 || storedSecond.Version != 2 || !reflect.DeepEqual(serviceRevisions, []int{3, 4}) {
		t.Fatalf("different-Service stored states = first:%+v second:%+v",
			nonSensitiveServiceState(storedFirst), nonSensitiveServiceState(storedSecond))
	}
	assertServiceManagementState(t, store, serviceManagementTunnelID, 4, 2)
}

func TestServiceManagementMutationOwnerSerializesSameTunnelWithoutBlockingOthers(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelIDTwo)
	gate := &recordingSnapshotGate{}
	setup := newServiceManagementTestService(
		store, gate, serviceManagementIDOne, serviceManagementIDTwo, serviceManagementIDThree,
	)
	first, err := setup.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "first"))
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, err := setup.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelID, "second"))
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	other, err := setup.Create(context.Background(), validCreateServiceInput(serviceManagementTunnelIDTwo, "other"))
	if err != nil {
		t.Fatalf("Create(other Tunnel) error = %v", err)
	}

	blockingStore := &serviceManagementCommitBlockingStore{
		Store: store, blockTag: "first", committed: make(chan struct{}), release: make(chan struct{}),
	}
	service := NewServiceManagementService(blockingStore, gate, &recordingSnapshotNotifier{}, setup.budget)
	service.now = func() time.Time { return time.Unix(200, 0) }
	type mutationOutcome struct {
		result ServiceMutationResult
		err    error
	}
	firstOutcome := make(chan mutationOutcome, 1)
	go func() {
		disabled := false
		result, updateErr := service.Update(
			context.WithValue(context.Background(), serviceManagementMutationContextKey{}, "first"),
			UpdateServiceInput{
				TunnelID: serviceManagementTunnelID, ServiceID: first.Service.ID,
				ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
			},
		)
		firstOutcome <- mutationOutcome{result: result, err: updateErr}
	}()
	select {
	case <-blockingStore.committed:
	case <-time.After(time.Second):
		t.Fatal("first Tunnel mutation did not reach the post-COMMIT budget window")
	}
	released := false
	defer func() {
		if !released {
			close(blockingStore.release)
		}
	}()

	secondOutcome := make(chan mutationOutcome, 1)
	go func() {
		disabled := false
		result, updateErr := service.Update(context.Background(), UpdateServiceInput{
			TunnelID: serviceManagementTunnelID, ServiceID: second.Service.ID,
			ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
		})
		secondOutcome <- mutationOutcome{result: result, err: updateErr}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		service.mutationOwnersMu.Lock()
		owner := service.mutationOwners[serviceManagementTunnelID]
		waiting := owner != nil && owner.references == 2
		service.mutationOwnersMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second same-Tunnel mutation did not wait on the existing owner")
		}
		time.Sleep(time.Millisecond)
	}

	otherOutcome := make(chan mutationOutcome, 1)
	go func() {
		disabled := false
		result, updateErr := service.Update(context.Background(), UpdateServiceInput{
			TunnelID: serviceManagementTunnelIDTwo, ServiceID: other.Service.ID,
			ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
		})
		otherOutcome <- mutationOutcome{result: result, err: updateErr}
	}()
	select {
	case outcome := <-otherOutcome:
		if outcome.err != nil || outcome.result.TunnelRevision != 2 {
			t.Fatalf("different-Tunnel Update() = (%+v, %v), want revision 2 while first Tunnel is blocked", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("different-Tunnel mutation was globally blocked")
	}
	select {
	case outcome := <-secondOutcome:
		t.Fatalf("same-Tunnel mutation completed before prior Budget Commit: (%+v, %v)", outcome.result, outcome.err)
	default:
	}

	close(blockingStore.release)
	released = true
	firstResult := <-firstOutcome
	secondResult := <-secondOutcome
	if firstResult.err != nil || firstResult.result.TunnelRevision != 3 {
		t.Fatalf("first same-Tunnel Update() = (%+v, %v), want revision 3", firstResult.result, firstResult.err)
	}
	if secondResult.err != nil || secondResult.result.TunnelRevision != 4 {
		t.Fatalf("second same-Tunnel Update() = (%+v, %v), want revision 4", secondResult.result, secondResult.err)
	}
	service.mutationOwnersMu.Lock()
	owners := len(service.mutationOwners)
	service.mutationOwnersMu.Unlock()
	if owners != 0 {
		t.Fatalf("mutation owner entries after completion = %d, want 0", owners)
	}
}

type snapshotGateCall struct {
	tunnelID string
	revision int64
	services []repository.Service
}

type recordingSnapshotGate struct {
	mu    sync.Mutex
	err   error
	calls []snapshotGateCall
}

type recordingSnapshotNotifier struct {
	mu        sync.Mutex
	err       error
	calls     []string
	afterMark func(string, int)
}

type revisionOverrideStore struct {
	repository.Store
	revision int64
}

type serviceManagementMutationContextKey struct{}

type serviceManagementCommitBlockingStore struct {
	repository.Store
	blockTag  string
	committed chan struct{}
	release   chan struct{}
}

type serviceManagementPostCommitCleanupStore struct {
	repository.Store
	err error
}

func (store *serviceManagementCommitBlockingStore) WithTx(
	ctx context.Context,
	fn func(repository.TxStore) error,
) error {
	err := store.Store.WithTx(ctx, fn)
	if err == nil && ctx.Value(serviceManagementMutationContextKey{}) == store.blockTag {
		close(store.committed)
		<-store.release
	}
	return err
}

func (store *serviceManagementPostCommitCleanupStore) WithTx(
	ctx context.Context,
	fn func(repository.TxStore) error,
) error {
	if err := store.Store.WithTx(ctx, fn); err != nil {
		return err
	}
	return errors.Join(repository.ErrPostCommitCleanup, store.err)
}

func (store revisionOverrideStore) WithTx(ctx context.Context, fn func(repository.TxStore) error) error {
	return store.Store.WithTx(ctx, func(transaction repository.TxStore) error {
		return fn(revisionOverrideTxStore{TxStore: transaction, revision: store.revision})
	})
}

type revisionOverrideTxStore struct {
	repository.TxStore
	revision int64
}

func (store revisionOverrideTxStore) Tunnels() repository.TunnelRepository {
	return revisionOverrideTunnelRepository{
		TunnelRepository: store.TxStore.Tunnels(),
		revision:         store.revision,
	}
}

type revisionOverrideTunnelRepository struct {
	repository.TunnelRepository
	revision int64
}

func (store revisionOverrideTunnelRepository) Get(ctx context.Context, tunnelID string) (repository.Tunnel, error) {
	tunnel, err := store.TunnelRepository.Get(ctx, tunnelID)
	if err != nil {
		return repository.Tunnel{}, err
	}
	tunnel.DesiredRevision = store.revision
	return tunnel, nil
}

func (gate *recordingSnapshotGate) Validate(tunnelID string, revision int64, services []repository.Service) error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	candidate := make([]repository.Service, len(services))
	copy(candidate, services)
	gate.calls = append(gate.calls, snapshotGateCall{tunnelID: tunnelID, revision: revision, services: candidate})
	return gate.err
}

func (gate *recordingSnapshotGate) setError(err error) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.err = err
}

func (gate *recordingSnapshotGate) snapshot() []snapshotGateCall {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	calls := make([]snapshotGateCall, len(gate.calls))
	copy(calls, gate.calls)
	return calls
}

func (notifier *recordingSnapshotNotifier) MarkDirty(tunnelID string) error {
	notifier.mu.Lock()
	notifier.calls = append(notifier.calls, tunnelID)
	call := len(notifier.calls)
	err := notifier.err
	afterMark := notifier.afterMark
	notifier.mu.Unlock()
	if afterMark != nil {
		afterMark(tunnelID, call)
	}
	return err
}

func (notifier *recordingSnapshotNotifier) snapshot() []string {
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	return append([]string(nil), notifier.calls...)
}

func newServiceManagementTestService(
	store repository.Store,
	gate TunnelSnapshotGate,
	identifiers ...string,
) *ServiceManagementService {
	return newServiceManagementTestServiceWithNotifier(
		store, gate, &recordingSnapshotNotifier{}, identifiers...,
	)
}

func newServiceManagementTestServiceWithNotifier(
	store repository.Store,
	gate TunnelSnapshotGate,
	notifier SnapshotReconcileNotifier,
	identifiers ...string,
) *ServiceManagementService {
	return newServiceManagementTestServiceWithBudget(
		store, gate, notifier, newServiceManagementBudget(store, 1_000_000, 1_000_000), identifiers...,
	)
}

func newServiceManagementTestServiceWithBudget(
	store repository.Store,
	gate TunnelSnapshotGate,
	notifier SnapshotReconcileNotifier,
	budget *healthbudget.Manager,
	identifiers ...string,
) *ServiceManagementService {
	service := NewServiceManagementService(store, gate, notifier, budget)
	var mutex sync.Mutex
	next := 0
	service.newServiceID = func() (string, error) {
		mutex.Lock()
		defer mutex.Unlock()
		identifier := identifiers[next]
		next++
		return identifier, nil
	}
	service.now = func() time.Time { return time.Unix(100, 0) }
	return service
}

func assertHealthBudgetTunnel(
	t *testing.T,
	budget *healthbudget.Manager,
	revision, enabled, targets uint64,
) {
	t.Helper()
	snapshot := budget.Snapshot().Tunnels[serviceManagementTunnelID]
	if snapshot.Revision != revision || snapshot.EnabledCount != enabled || snapshot.Targets != targets ||
		snapshot.ReservationActive {
		t.Fatalf("Health Budget Tunnel = %+v, want revision:%d enabled:%d targets:%d no reservation",
			snapshot, revision, enabled, targets)
	}
}

func newServiceManagementBudget(store repository.Store, perTunnel, global uint64) *healthbudget.Manager {
	manager, err := healthbudget.New(healthbudget.Options{
		MaxTargetsPerTunnel: perTunnel, MaxTargetsGlobal: global,
	})
	if err != nil {
		panic(fmt.Sprintf("create Health Budget Manager: %v", err))
	}
	for _, tunnelID := range []string{serviceManagementTunnelID, serviceManagementTunnelIDTwo} {
		var tunnel repository.Tunnel
		var services []repository.Service
		err := store.Read(context.Background(), func(view repository.RepositoryView) error {
			var err error
			tunnel, err = view.Tunnels().Get(context.Background(), tunnelID)
			if err != nil {
				return err
			}
			services, err = view.Services().ListByTunnel(context.Background(), tunnelID)
			return err
		})
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			panic(fmt.Sprintf("read Health Budget baseline: %v", err))
		}
		if err := manager.InitializeTunnel(
			tunnelID, uint64(tunnel.DesiredRevision), healthEnabledServiceCount(services),
		); err != nil {
			panic(fmt.Sprintf("initialize Health Budget baseline: %v", err))
		}
	}
	return manager
}

func validCreateServiceInput(tunnelID, name string) CreateServiceInput {
	return CreateServiceInput{
		TunnelID: tunnelID, ExpectedTunnelVersion: 1, Name: name,
		Origin: validServiceOriginInput(),
	}
}

func validServiceOriginInput() ServiceOriginInput {
	return ServiceOriginInput{Scheme: repository.OriginSchemeHTTP, Host: "127.0.0.1", Port: 8080}
}

func openServiceManagementStore(t *testing.T) *repositorysqlite.Store {
	t.Helper()
	store, err := repositorysqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open Service Management store error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close Service Management store error = %v", err)
		}
	})
	return store
}

func seedServiceManagementTunnel(t *testing.T, store repository.Store, tunnelID string) {
	t.Helper()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), repository.Tunnel{
			ID: tunnelID, Name: tunnelID, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("seed Service Management Tunnel error = %v", err)
	}
}

func assertStoredService(t *testing.T, store repository.Store, want repository.Service) {
	t.Helper()
	got := readServiceManagementService(t, store, want.TunnelID, want.ID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored Service = %+v, want %+v", nonSensitiveServiceState(got), nonSensitiveServiceState(want))
	}
}

func readServiceManagementService(t *testing.T, store repository.Store, tunnelID, serviceID string) repository.Service {
	t.Helper()
	var result repository.Service
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		var err error
		result, err = view.Services().Get(context.Background(), tunnelID, serviceID)
		return err
	}); err != nil {
		t.Fatalf("read Service error = %v", err)
	}
	return result
}

func assertServiceManagementState(
	t *testing.T,
	store repository.Store,
	tunnelID string,
	wantRevision, wantServices int64,
) {
	t.Helper()
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnel, err := view.Tunnels().Get(context.Background(), tunnelID)
		if err != nil {
			return err
		}
		count, err := view.Services().CountByTunnel(context.Background(), tunnelID)
		if err != nil {
			return err
		}
		if tunnel.DesiredRevision != wantRevision || count != wantServices {
			t.Fatalf("stored Tunnel state = revision:%d services:%d, want %d/%d",
				tunnel.DesiredRevision, count, wantRevision, wantServices)
		}
		return nil
	}); err != nil {
		t.Fatalf("read Service Management state error = %v", err)
	}
}

type serviceStateForTest struct {
	ID, TunnelID              string
	RequiredRevision, Version int64
	Enabled, TLSVerify        bool
	HasHealth                 bool
	ConnectTimeoutMS          uint32
}

func nonSensitiveServiceState(service repository.Service) serviceStateForTest {
	return serviceStateForTest{
		ID: service.ID, TunnelID: service.TunnelID, RequiredRevision: service.RequiredRevision,
		Version: service.Version, Enabled: service.Enabled, TLSVerify: service.TLSVerify,
		HasHealth: service.Health != nil, ConnectTimeoutMS: service.ConnectTimeoutMS,
	}
}

type mutationStateForTest struct {
	ServiceID                     string
	ServiceVersion                int64
	ServiceRevision               int64
	TunnelVersion, TunnelRevision int64
	Changed                       bool
}

func nonSensitiveMutationState(result ServiceMutationResult) mutationStateForTest {
	return mutationStateForTest{
		ServiceID: result.Service.ID, ServiceVersion: result.Service.Version,
		ServiceRevision: result.Service.RequiredRevision, TunnelVersion: result.TunnelVersion,
		TunnelRevision: result.TunnelRevision, Changed: result.Changed,
	}
}

type gateCallStateForTest struct {
	TunnelID string
	Revision int64
	Count    int
}

func gateCallStates(calls []snapshotGateCall) []gateCallStateForTest {
	states := make([]gateCallStateForTest, 0, len(calls))
	for _, call := range calls {
		states = append(states, gateCallStateForTest{TunnelID: call.tunnelID, Revision: call.revision, Count: len(call.services)})
	}
	return states
}
