//go:build !windows

package tokenkey

func writeKeyAtomicallyPlatform(directoryPath, keyPath string, key []byte) error {
	return writeKeyAtomicallyPOSIX(directoryPath, keyPath, key)
}

func createCredentialDirectoryPlatform(dataDir, directoryPath string) error {
	return createCredentialDirectoryPOSIX(dataDir, directoryPath)
}

func loadExistingPlatform(directoryPath, keyPath string) (Key, bool, error) {
	return loadExistingPOSIX(directoryPath, keyPath)
}
