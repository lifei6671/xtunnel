//go:build linux

package pathprofile

// resolve 保留 Linux 现有显式 DataDir 与固定 /run Runtime 契约。
func resolveForeground(dataDir string) (Profile, error) {
	if dataDir == AutomaticDataDir {
		dataDir = "/var/lib/xtunnel/data"
	}
	return Profile{DataDir: dataDir, RuntimeDir: "/run/xtunnel"}, nil
}

func resolveService(dataDir string) (Profile, error) {
	return resolveForeground(dataDir)
}
