//go:build !linux && !windows

package service

import (
	"context"
	"errors"
	"testing"
)

func TestServiceManagementIsUnsupported(t *testing.T) {
	if err := Install(context.Background(), "xta_test_secret"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Install() error = %v, want ErrUnsupported", err)
	}
	if _, err := Uninstall(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Uninstall() error = %v, want ErrUnsupported", err)
	}
}
