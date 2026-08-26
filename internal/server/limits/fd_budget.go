package limits

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrFDBudgetExceeded 表示稳定态与握手峰值所需 FD 超过进程软限制。
	ErrFDBudgetExceeded = errors.New("server file descriptor budget exceeds process limit")
	// ErrFDBudgetInvalid 表示预算分项为零或求和发生 uint64 溢出。
	ErrFDBudgetInvalid = errors.New("server file descriptor budget is invalid")
)

// FDBudget 明确列出 Server 启动前必须预留的 FD 类别。
// Work、公网 ACTIVE 与 Pending OPEN 分别各占一条 Server 侧 socket；Control、
// TLS/AUTH 峰值、Listener、SQLite、Management、Metrics 和安全余量均独立计入，
// 便于错误直接定位。
type FDBudget struct {
	WorkConnections         uint64
	PublicActiveConnections uint64
	PendingOpenConnections  uint64
	ConnectorControls       uint64
	PendingTLSHandshakes    uint64
	PendingAuth             uint64
	Listeners               uint64
	SQLite                  uint64
	Management              uint64
	Metrics                 uint64
	SafetyMargin            uint64
}

// FDBudgetError 保留软限制、所需总量和完整分项，启动日志无需重新推导。
type FDBudgetError struct {
	Limit    uint64
	Required uint64
	Budget   FDBudget
}

func (failure *FDBudgetError) Error() string {
	return fmt.Sprintf(
		"%v: limit=%d required=%d work=%d public_active=%d pending_open=%d connector_control=%d pending_tls=%d pending_auth=%d listeners=%d sqlite=%d management=%d metrics=%d safety_margin=%d",
		ErrFDBudgetExceeded, failure.Limit, failure.Required,
		failure.Budget.WorkConnections, failure.Budget.PublicActiveConnections,
		failure.Budget.PendingOpenConnections,
		failure.Budget.ConnectorControls, failure.Budget.PendingTLSHandshakes,
		failure.Budget.PendingAuth, failure.Budget.Listeners, failure.Budget.SQLite,
		failure.Budget.Management, failure.Budget.Metrics, failure.Budget.SafetyMargin,
	)
}

// Unwrap 允许调用方用 errors.Is 分类启动失败。
func (*FDBudgetError) Unwrap() error { return ErrFDBudgetExceeded }

// CheckFDBudget 在支持的部署平台读取 RLIMIT_NOFILE 并执行启动前校验。
// 非 Linux 不属于 V0.1 Server 部署范围，平台实现明确采用 no-op，而不是猜测一个
// 不可靠的 FD 上限；各分项的结构与溢出校验仍会执行。
func CheckFDBudget(budget FDBudget) error {
	required, err := requiredFDs(budget)
	if err != nil {
		return err
	}
	limit, supported, err := currentFDLimit()
	if err != nil {
		return fmt.Errorf("read process file descriptor limit: %w", err)
	}
	if !supported {
		return nil
	}
	return checkFDBudgetAgainstLimit(budget, required, limit)
}

func requiredFDs(budget FDBudget) (uint64, error) {
	parts := [...]uint64{
		budget.WorkConnections, budget.PublicActiveConnections, budget.PendingOpenConnections, budget.ConnectorControls,
		budget.PendingTLSHandshakes, budget.PendingAuth, budget.Listeners, budget.SQLite,
		budget.Management, budget.Metrics, budget.SafetyMargin,
	}
	var total uint64
	for _, part := range parts {
		if part == 0 || total > math.MaxUint64-part {
			return 0, ErrFDBudgetInvalid
		}
		total += part
	}
	return total, nil
}

func checkFDBudgetAgainstLimit(budget FDBudget, required, limit uint64) error {
	if required > limit {
		return &FDBudgetError{Limit: limit, Required: required, Budget: budget}
	}
	return nil
}
