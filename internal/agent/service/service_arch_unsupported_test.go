//go:build 386

package service

import (
	"context"
	"strings"
	"testing"
)

func TestServiceManagementRejectsUnsupportedArchitectureBeforePlatformAccess(t *testing.T) {
	if err := Install(context.Background(), "xta_test_secret"); err == nil || !strings.Contains(err.Error(), "architecture 386") {
		t.Fatalf("Install() error = %v, want unsupported architecture", err)
	}
	if _, err := Uninstall(context.Background()); err == nil || !strings.Contains(err.Error(), "architecture 386") {
		t.Fatalf("Uninstall() error = %v, want unsupported architecture", err)
	}
}
