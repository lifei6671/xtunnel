//go:build linux

package gateway

import (
	"fmt"
	"os"
)

// writeFileSync 在关闭前把内容持久化；轮换 Journal 和临时身份文件都依赖该顺序。
func writeFileSync(path string, data []byte, mode os.FileMode) (resultErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return fmt.Errorf("set file permissions: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return nil
}

// syncDirectory 持久化同盘 rename 对目录项的改变。
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
