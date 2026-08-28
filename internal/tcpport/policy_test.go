package tcpport

import (
	"errors"
	"testing"
)

func TestPolicyExplicitAndAutomaticAllocation(t *testing.T) {
	policy, err := New(10000, 10005, []uint16{10001, 443})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	used := map[uint16]struct{}{10000: {}, 10003: {}}

	tests := []struct {
		name string
		port uint16
	}{
		{name: "below range", port: 9999},
		{name: "above range", port: 10006},
		{name: "reserved", port: 10001},
		{name: "assigned even when route may be disabled", port: 10003},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := policy.ValidateExplicit(test.port, used); !errors.Is(err, ErrPortUnavailable) {
				t.Fatalf("ValidateExplicit(%d) error = %v, want ErrPortUnavailable", test.port, err)
			}
		})
	}
	if err := policy.ValidateExplicit(10004, used); err != nil {
		t.Fatalf("ValidateExplicit(10004) error = %v", err)
	}
	allocated, err := policy.Allocate(used)
	if err != nil || allocated != 10002 {
		t.Fatalf("Allocate() = %d, %v, want 10002, nil", allocated, err)
	}
}

func TestPolicyRejectsInvalidRangeAndExhaustion(t *testing.T) {
	for _, bounds := range [][2]int{{0, 1}, {1, 65536}, {2, 1}} {
		if _, err := New(bounds[0], bounds[1], nil); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("New(%d, %d) error = %v, want ErrInvalidPolicy", bounds[0], bounds[1], err)
		}
	}
	policy, err := New(10000, 10001, []uint16{10001})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := policy.Allocate(map[uint16]struct{}{10000: {}}); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Allocate() error = %v, want ErrPoolExhausted", err)
	}
}
