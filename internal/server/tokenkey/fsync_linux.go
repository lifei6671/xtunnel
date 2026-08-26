//go:build linux

package tokenkey

import "os"

// syncDirectory 持久化目录创建和同目录 rename 产生的目录项变更。
func syncDirectory(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if err := directory.Close(); resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	return directory.Sync()
}
