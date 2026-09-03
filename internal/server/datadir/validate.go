package datadir

// ParentGuard 把 Stable Target 的父目录身份固定到一段完整的持久化资源
// 生命周期。Windows 依靠不共享删除的目录 Handle 阻止 rename/replace；其他
// 平台保留既有 realpath 校验语义。
type ParentGuard struct {
	validateCanonical func() error
	close             func() error
}

// PinParent 重新验证 Stable Target 并固定父目录，调用方必须在释放 External
// Lock 前关闭返回的 Guard。只固定父目录而不长期持有 leaf，Restore 仍可按协议
// 原子替换正式数据目录。
func PinParent(target Target) (*ParentGuard, error) {
	return pinParent(target)
}

// ValidateCanonical 在已固定的父目录下校验正式数据目录 leaf。
func (guard *ParentGuard) ValidateCanonical() error {
	return guard.validateCanonical()
}

// Close 释放父目录身份 Guard。
func (guard *ParentGuard) Close() error {
	return guard.close()
}

// ValidateTarget 重新验证 Stable Target 的父目录对象、leaf 与 Hash 绑定关系，
// 但不要求正式 leaf 已存在。Restore 路径派生和获锁后的正式目录校验必须复用
// 这个入口，避免各自维护第二套 Hash 规则。
func ValidateTarget(target Target) error {
	return validateTarget(target)
}

// ValidateCanonical 在取得 External Lock 后校验正式数据目录。
func ValidateCanonical(target Target) error {
	return validateCanonical(target)
}
