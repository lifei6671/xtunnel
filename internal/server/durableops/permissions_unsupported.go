//go:build !linux

package durableops

import "os"

// platformSupported 在非 Linux 构建中关闭依赖 openat2/statx 的操作入口。
func platformSupported() bool {
	return false
}

// sourceModeValid 仅让平台无关代码通过编译；非 Linux 入口会在调用前失败。
func sourceModeValid(os.FileMode, uint32) bool {
	// 非 Linux 入口会先快速失败；这个实现只用于保持纯 Manifest 代码可编译。
	return true
}

// openRegularNoFollow 在非 Linux 平台明确返回不支持，不提供弱化的安全实现。
func openRegularNoFollow(string) (*os.File, error) {
	return nil, ErrUnsupported
}

// openRegularBeneath 在非 Linux 平台明确返回不支持。
func openRegularBeneath(string, string) (*os.File, error) {
	return nil, ErrUnsupported
}

// openAbsoluteRegularNoSymlinks 在非 Linux 平台明确返回不支持。
func openAbsoluteRegularNoSymlinks(string) (*os.File, error) {
	return nil, ErrUnsupported
}

// createExclusiveOutput 在非 Linux 平台明确返回不支持。
func createExclusiveOutput(string) (*os.File, *os.File, error) {
	return nil, nil, ErrUnsupported
}

// removeExclusiveOutput 在非 Linux 平台明确返回不支持。
func removeExclusiveOutput(*os.File, string) error {
	return ErrUnsupported
}
