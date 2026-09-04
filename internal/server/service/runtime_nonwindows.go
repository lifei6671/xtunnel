//go:build !windows

package service

import (
	"context"
	"io"
)

// RunIfService reports that this platform uses its foreground lifecycle.
func RunIfService(func(context.Context, io.Writer, func()) error) (bool, error) { return false, nil }

// FixedConfigPath returns the native service configuration location.
func FixedConfigPath() (string, error) { return ConfigPath, nil }

// ReadConfig is reserved for the Windows managed configuration reader.
func ReadConfig() ([]byte, error) { return nil, ErrUnsupported }

// RequireStopped is reserved for Windows SCM offline maintenance.
func RequireStopped() error { return ErrUnsupported }
