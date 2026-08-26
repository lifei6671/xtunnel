//go:build linux

package limits

import "testing"

func TestLinuxCurrentFDLimitIsSupported(t *testing.T) {
	limit, supported, err := currentFDLimit()
	if err != nil {
		t.Fatalf("currentFDLimit() error = %v", err)
	}
	if !supported || limit == 0 {
		t.Fatalf("currentFDLimit() = %d, %t, want non-zero supported limit", limit, supported)
	}
}
