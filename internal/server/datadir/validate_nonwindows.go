//go:build !windows

package datadir

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func validateTarget(target Target) error {
	if target.Parent == "" || target.Leaf == "" || target.Path == "" || target.Hash == "" {
		return errors.New("stable data target is incomplete")
	}
	canonicalParent, err := filepath.EvalSymlinks(target.Parent)
	if err != nil {
		return fmt.Errorf("resolve stable data parent: %w", err)
	}
	canonicalParent, err = filepath.Abs(canonicalParent)
	if err != nil {
		return fmt.Errorf("make stable data parent absolute: %w", err)
	}
	if filepath.Clean(canonicalParent) != filepath.Clean(target.Parent) {
		return errors.New("stable data parent is not canonical")
	}
	parentInfo, err := os.Lstat(canonicalParent)
	if err != nil {
		return fmt.Errorf("inspect stable data parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return errors.New("stable data parent must be a non-symbolic-link directory")
	}
	if filepath.Base(target.Leaf) != target.Leaf || target.Leaf == "." || target.Leaf == ".." {
		return errors.New("stable data target leaf is invalid")
	}
	wantTarget := filepath.Join(canonicalParent, target.Leaf)
	if filepath.Clean(target.Path) != wantTarget {
		return errors.New("stable data target is not a direct child of its parent")
	}
	digest := sha256.Sum256([]byte(target.Path))
	if target.Hash != fmt.Sprintf("%x", digest) {
		return errors.New("stable data target hash is invalid")
	}
	return nil
}

func validateCanonical(target Target) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	info, err := os.Lstat(target.Path)
	if err != nil {
		return fmt.Errorf("inspect server data directory %q: %w", target.Path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("server data directory %q must not be a symbolic link", target.Path)
	}
	if !info.IsDir() {
		return fmt.Errorf("server data path %q is not a directory", target.Path)
	}

	canonicalPath, err := filepath.EvalSymlinks(target.Path)
	if err != nil {
		return fmt.Errorf("resolve server data directory %q: %w", target.Path, err)
	}
	canonicalPath, err = filepath.Abs(canonicalPath)
	if err != nil {
		return fmt.Errorf("make server data directory absolute: %w", err)
	}
	if filepath.Clean(canonicalPath) != filepath.Clean(target.Path) {
		return fmt.Errorf("server data directory resolved to %q, want stable target %q", canonicalPath, target.Path)
	}
	return nil
}

func pinParent(target Target) (*ParentGuard, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	return &ParentGuard{
		validateCanonical: func() error { return validateCanonical(target) },
		close:             func() error { return nil },
	}, nil
}
