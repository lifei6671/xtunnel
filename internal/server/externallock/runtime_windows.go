//go:build windows

package externallock

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func runtimeDirectory() (string, error) {
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("resolve Windows ProgramData known folder: %w", err)
	}
	return filepath.Join(programData, "XTunnel", "Server", "runtime"), nil
}
