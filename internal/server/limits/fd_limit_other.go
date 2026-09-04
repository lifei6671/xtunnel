//go:build !linux

package limits

func currentFDLimit() (limit uint64, supported bool, err error) {
	// RLIMIT_NOFILE 预算仅适用于 Linux。Windows Server Preview 使用不同的
	// 句柄语义，此处不读取或推定其上限，明确返回未执行平台 FD 限额校验。
	return 0, false, nil
}
