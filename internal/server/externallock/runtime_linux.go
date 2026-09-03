//go:build linux

package externallock

func runtimeDirectory() (string, error) {
	return "/run/xtunnel", nil
}
