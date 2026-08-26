//go:build !linux

package limits

import "testing"

func TestNonLinuxFDLimitUsesExplicitNoopPolicy(t *testing.T) {
	limit, supported, err := currentFDLimit()
	if err != nil || supported || limit != 0 {
		t.Fatalf("currentFDLimit() = %d, %t, %v, want 0, false, nil", limit, supported, err)
	}
}
