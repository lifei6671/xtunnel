package limits

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestFDBudgetReportsEveryCategory(t *testing.T) {
	budget := FDBudget{
		WorkConnections: 10, PublicActiveConnections: 11, PendingOpenConnections: 15, ConnectorControls: 12,
		PendingTLSHandshakes: 13, PendingAuth: 14, Listeners: 2, SQLite: 4,
		Management: 1, Metrics: 1, SafetyMargin: 32,
	}
	required, err := requiredFDs(budget)
	if err != nil {
		t.Fatalf("requiredFDs() error = %v", err)
	}
	if required != 115 {
		t.Fatalf("requiredFDs() = %d, want 115", required)
	}
	err = checkFDBudgetAgainstLimit(budget, required, 114)
	if !errors.Is(err, ErrFDBudgetExceeded) {
		t.Fatalf("checkFDBudgetAgainstLimit() error = %v, want ErrFDBudgetExceeded", err)
	}
	for _, fragment := range []string{
		"limit=114", "required=115", "work=10", "public_active=11", "pending_open=15", "connector_control=12",
		"pending_tls=13", "pending_auth=14", "listeners=2", "sqlite=4",
		"management=1", "metrics=1", "safety_margin=32",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("FDBudgetError %q does not contain %q", err, fragment)
		}
	}
}

func TestFDBudgetRejectsZeroCategoryAndOverflow(t *testing.T) {
	valid := FDBudget{
		WorkConnections: 1, PublicActiveConnections: 1, PendingOpenConnections: 1, ConnectorControls: 1,
		PendingTLSHandshakes: 1, PendingAuth: 1, Listeners: 1, SQLite: 1,
		Management: 1, Metrics: 1, SafetyMargin: 1,
	}
	tests := []struct {
		name   string
		mutate func(*FDBudget)
	}{
		{name: "zero category", mutate: func(budget *FDBudget) { budget.Metrics = 0 }},
		{name: "overflow", mutate: func(budget *FDBudget) { budget.WorkConnections = math.MaxUint64 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := valid
			test.mutate(&budget)
			if _, err := requiredFDs(budget); !errors.Is(err, ErrFDBudgetInvalid) {
				t.Fatalf("requiredFDs() error = %v, want ErrFDBudgetInvalid", err)
			}
		})
	}
}
