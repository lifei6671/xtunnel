package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lifei6671/xtunnel/tests/release/internal/secretcheck"
)

func main() {
	root := flag.String("path", "", "file or directory to scan")
	allowlistPath := flag.String("allowlist", "", "optional exact SHA-256 allowlist")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "usage: secretscan -path FILE_OR_DIRECTORY")
		os.Exit(2)
	}
	allowlist, err := loadAllowlist(*allowlistPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "release secret scan failed: %v\n", err)
		os.Exit(1)
	}
	if err := scan(*root, allowlist); err != nil {
		fmt.Fprintf(os.Stderr, "release secret scan failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("release secret scan passed: %s\n", *root)
}

func scan(root string, allowlist map[string]string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() && !rootInfo.Mode().IsRegular() {
		return fmt.Errorf("scan root %s is not a regular file or directory", root)
	}
	consumed := make(map[string]bool, len(allowlist))
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("scan target %s is not a regular file", path)
		}
		relative := entry.Name()
		if rootInfo.IsDir() {
			relative, err = filepath.Rel(root, path)
			if err != nil {
				return err
			}
		}
		relative = filepath.ToSlash(relative)
		if expectedHash, allowed := allowlist[relative]; allowed {
			actualHash, hashErr := fileSHA256(path)
			if hashErr != nil {
				return hashErr
			}
			if actualHash != expectedHash {
				return fmt.Errorf("allowlisted file %s SHA-256 = %s, want %s", relative, actualHash, expectedHash)
			}
			consumed[relative] = true
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		scanErr := secretcheck.Reader(path, file)
		closeErr := file.Close()
		if scanErr != nil {
			return scanErr
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", path, closeErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for path := range allowlist {
		if !consumed[path] {
			return fmt.Errorf("allowlisted file %s was not scanned", path)
		}
	}
	return nil
}

func loadAllowlist(path string) (map[string]string, error) {
	result := make(map[string]string)
	if path == "" {
		return result, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open allowlist: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			return nil, fmt.Errorf("invalid allowlist line %q", line)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("invalid allowlist digest %q", fields[0])
		}
		path := filepath.ToSlash(filepath.Clean(fields[1]))
		if path == "." || path == ".." || strings.HasPrefix(path, "../") || filepath.IsAbs(fields[1]) || filepath.VolumeName(fields[1]) != "" || result[path] != "" {
			return nil, fmt.Errorf("invalid or duplicate allowlist path %q", path)
		}
		result[path] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read allowlist: %w", err)
	}
	return result, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("hash %s: %w", path, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close %s: %w", path, closeErr)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
