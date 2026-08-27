// Package origin 从已发布的 Agent Snapshot 解析并连接 Service Origin。
package origin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/originconfig"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

var (
	// ErrInvalidSnapshot 表示 Origin 字段不符合冻结的 Protocol v1 语义。
	ErrInvalidSnapshot = errors.New("agent origin snapshot is invalid")
	// ErrConfigNotObserved 表示没有任何已经 APPLIED Ack 的当前 Snapshot。
	ErrConfigNotObserved = errors.New("service configuration has not been observed by connector")
	// ErrServiceNotFound 表示当前 Snapshot 不包含目标 Service。
	ErrServiceNotFound = errors.New("service was not found in current snapshot")
	// ErrServiceDisabled 表示当前 Snapshot 已显式禁用目标 Service。
	ErrServiceDisabled = errors.New("service is disabled in current snapshot")
	// ErrResolverState 表示进程内同时出现多个 active Resolver 等内部不变量破坏。
	ErrResolverState = errors.New("agent origin resolver state is invalid")
	// ErrDial 表示 DNS 或 TCP 连接失败；公开细分类由 ErrorCode 承载。
	ErrDial = errors.New("agent origin dial failed")
	// ErrTLSHandshake 表示 TLS 协议或证书校验失败。
	ErrTLSHandshake = errors.New("agent origin TLS handshake failed")
)

// Origin 是从一代完整 Snapshot 编译出的不可变连接配置。
type Origin struct {
	Scheme         string
	Host           string
	Port           uint16
	ConnectTimeout time.Duration
	TLSVerify      bool
	TLSServerName  string
	HTTPHostHeader string
	enabled        bool
}

type runtimeState struct {
	gate    configruntime.Gate
	origins map[string]Origin
}

// Manager 同时是 Config Candidate Builder、当前 Origin Resolver 和 OPEN Dialer。
// Candidate 可以提前注册，但只有其发布门 Active 后才可被 Resolve 观察。
type Manager struct {
	mu          sync.Mutex
	states      map[*runtimeState]struct{}
	dialContext func(context.Context, string, string) (net.Conn, error)
	rootCAs     *x509.CertPool
}

// New 创建进程级 Resolver。DNS 只会在每次 DialOrigin 时交给系统 Resolver 解析。
func New() *Manager {
	dialer := &net.Dialer{}
	return &Manager{
		states:      make(map[*runtimeState]struct{}),
		dialContext: dialer.DialContext,
	}
}

// Build 校验完整 Snapshot 并构建尚未发布的一代 Origin 索引。
func (manager *Manager) Build(
	ctx context.Context,
	snapshot *protocolv1.TunnelSnapshot,
	gate configruntime.Gate,
) (configruntime.Candidate, error) {
	if manager == nil || manager.states == nil || manager.dialContext == nil || ctx == nil || snapshot == nil || gate == nil {
		return nil, ErrInvalidSnapshot
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	origins := make(map[string]Origin, len(snapshot.GetServices()))
	for index, service := range snapshot.GetServices() {
		compiled, err := compileService(service)
		if err != nil {
			return nil, fmt.Errorf("%w: %w: service index=%d", configruntime.ErrProtocolViolation, ErrInvalidSnapshot, index)
		}
		if _, exists := origins[service.GetServiceId()]; exists {
			return nil, fmt.Errorf("%w: %w: duplicate service index=%d", configruntime.ErrProtocolViolation, ErrInvalidSnapshot, index)
		}
		origins[service.GetServiceId()] = compiled
	}
	state := &runtimeState{gate: gate, origins: origins}
	return &candidate{manager: manager, state: state}, nil
}

// Resolve 只从当前已经发布且 APPLIED Ack 成功入队的一代 Snapshot 返回 Origin。
func (manager *Manager) Resolve(serviceID string) (Origin, error) {
	if manager == nil || !identity.ValidServiceID(serviceID) {
		return Origin{}, ErrServiceNotFound
	}
	manager.mu.Lock()
	active, observed := manager.activeStateLocked()
	if active == nil {
		manager.mu.Unlock()
		if observed {
			return Origin{}, ErrResolverState
		}
		return Origin{}, ErrConfigNotObserved
	}
	resolved, exists := active.origins[serviceID]
	manager.mu.Unlock()
	if !exists {
		return Origin{}, ErrServiceNotFound
	}
	if !resolved.enabled {
		return Origin{}, ErrServiceDisabled
	}
	return resolved, nil
}

// activeStateLocked 通过一次重扫消除 old→new 原子切换期间跨 Gate 读取产生的
// 瞬时双 active；持续违反单 active 不变量时保持 fail-closed。
func (manager *Manager) activeStateLocked() (*runtimeState, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		var active *runtimeState
		multiple := false
		for state := range manager.states {
			if !state.gate.Active() {
				continue
			}
			if active != nil {
				multiple = true
				break
			}
			active = state
		}
		if !multiple && (active == nil || active.gate.Active()) {
			return active, false
		}
	}
	return nil, true
}

// DialOrigin 每次使用系统 Resolver 重新解析 DNS，并让 DNS、TCP 与可选 TLS 握手
// 共享当前 Service 的 connect_timeout Context。
func (manager *Manager) DialOrigin(ctx context.Context, serviceID string) (net.Conn, protocolv1.ErrorCode, error) {
	if ctx == nil {
		return nil, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, ErrResolverState
	}
	resolved, err := manager.Resolve(serviceID)
	if err != nil {
		return nil, resolveErrorCode(err), err
	}
	return manager.dialResolved(ctx, resolved)
}

// dialResolved 对已经由 Snapshot 编译并固定的 Origin 执行统一的
// DNS、TCP、TLS 和 connect_timeout 策略。调用方必须先完成所属代次的可见性检查。
func (manager *Manager) dialResolved(ctx context.Context, resolved Origin) (net.Conn, protocolv1.ErrorCode, error) {
	dialContext, cancel := context.WithTimeout(ctx, resolved.ConnectTimeout)
	defer cancel()

	connection, err := manager.dialContext(dialContext, "tcp", net.JoinHostPort(resolved.Host, strconv.Itoa(int(resolved.Port))))
	if err != nil {
		return nil, connectionErrorCode(dialContext, err), sanitizedError{kind: ErrDial, cause: err}
	}
	if resolved.Scheme != "https" {
		return connection, protocolv1.ErrorCode_ERROR_CODE_OK, nil
	}

	tlsConnection := tls.Client(connection, &tls.Config{
		RootCAs:            manager.rootCAs,
		ServerName:         resolved.TLSServerName,
		InsecureSkipVerify: !resolved.TLSVerify, // Snapshot 中的显式 TLS 策略是唯一关闭验证的入口。
	})
	if err := tlsConnection.HandshakeContext(dialContext); err != nil {
		_ = connection.Close()
		code := protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TLS_ERROR
		if isTimeout(dialContext, err) {
			code = protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT
		}
		return nil, code, sanitizedError{kind: ErrTLSHandshake, cause: err}
	}
	return tlsConnection, protocolv1.ErrorCode_ERROR_CODE_OK, nil
}

func compileService(service *protocolv1.ServiceConfig) (Origin, error) {
	if service == nil || !identity.ValidServiceID(service.GetServiceId()) {
		return Origin{}, ErrInvalidSnapshot
	}
	fields := originconfig.Fields{
		Scheme: service.GetOriginScheme(), Host: service.GetOriginHost(), Port: service.GetOriginPort(),
		ConnectTimeoutMS: service.GetConnectTimeoutMs(), TLSVerify: service.GetTlsVerify(),
		TLSServerName: service.GetTlsServerName(), HTTPHostHeader: service.GetOriginHttpHost(),
	}
	if err := originconfig.Validate(fields); err != nil {
		return Origin{}, ErrInvalidSnapshot
	}

	resolved := Origin{
		Scheme: fields.Scheme, Host: fields.Host, Port: uint16(fields.Port),
		ConnectTimeout: time.Duration(fields.ConnectTimeoutMS) * time.Millisecond,
		TLSVerify:      fields.TLSVerify, TLSServerName: fields.TLSServerName,
		HTTPHostHeader: fields.HTTPHostHeader, enabled: service.GetEnabled(),
	}
	if resolved.Scheme == "https" {
		if resolved.TLSServerName == "" {
			resolved.TLSServerName = resolved.Host
		}
		if resolved.TLSVerify && resolved.TLSServerName == "" {
			return Origin{}, ErrInvalidSnapshot
		}
	}
	return resolved, nil
}

func resolveErrorCode(err error) protocolv1.ErrorCode {
	switch {
	case errors.Is(err, ErrConfigNotObserved):
		return protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED
	case errors.Is(err, ErrServiceDisabled):
		return protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED
	case errors.Is(err, ErrServiceNotFound):
		return protocolv1.ErrorCode_ERROR_CODE_SERVICE_NOT_FOUND
	case errors.Is(err, ErrResolverState):
		return protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR
	default:
		return protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE
	}
}

func connectionErrorCode(ctx context.Context, err error) protocolv1.ErrorCode {
	if isTimeout(ctx, err) {
		return protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED
	}
	return protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE
}

func isTimeout(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

type sanitizedError struct {
	kind  error
	cause error
}

func (err sanitizedError) Error() string { return err.kind.Error() }
func (err sanitizedError) Unwrap() []error {
	return []error{err.kind, err.cause}
}

type candidate struct {
	manager  *Manager
	state    *runtimeState
	mu       sync.Mutex
	started  bool
	terminal bool
	remove   sync.Once
}

// DialOrigin 只使用本 Candidate 在 Build 时固定的 Origin Plan。Health Checker
// 通过该代次作用域拨号，避免 Snapshot 切换后把旧 service_revision 的检查误拨到
// 新 Origin；未注册、未 Ack 或已 Retire 的 Candidate 始终 fail-closed。
func (candidate *candidate) DialOrigin(ctx context.Context, serviceID string) (net.Conn, protocolv1.ErrorCode, error) {
	if ctx == nil || candidate == nil || candidate.manager == nil || candidate.state == nil || !identity.ValidServiceID(serviceID) {
		return nil, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, ErrResolverState
	}
	candidate.manager.mu.Lock()
	_, registered := candidate.manager.states[candidate.state]
	active := registered && candidate.state.gate.Active()
	resolved, exists := candidate.state.origins[serviceID]
	candidate.manager.mu.Unlock()
	if !active {
		return nil, protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED, ErrConfigNotObserved
	}
	if !exists {
		return nil, protocolv1.ErrorCode_ERROR_CODE_SERVICE_NOT_FOUND, ErrServiceNotFound
	}
	if !resolved.enabled {
		return nil, protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED, ErrServiceDisabled
	}
	return candidate.manager.dialResolved(ctx, resolved)
}

func (candidate *candidate) Start(ctx context.Context) error {
	if ctx == nil {
		return ErrResolverState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if candidate.started || candidate.terminal || candidate.manager == nil || candidate.state == nil {
		return ErrResolverState
	}
	candidate.manager.mu.Lock()
	candidate.manager.states[candidate.state] = struct{}{}
	candidate.manager.mu.Unlock()
	candidate.started = true
	return nil
}

func (candidate *candidate) Abort(context.Context) error {
	candidate.mu.Lock()
	candidate.terminal = true
	candidate.mu.Unlock()
	candidate.unregister()
	return nil
}

func (candidate *candidate) Runtime() configruntime.Resources {
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if !candidate.started {
		return nil
	}
	return &resources{candidate: candidate}
}

func (candidate *candidate) unregister() {
	candidate.remove.Do(func() {
		if candidate.manager == nil || candidate.state == nil {
			return
		}
		candidate.manager.mu.Lock()
		delete(candidate.manager.states, candidate.state)
		candidate.manager.mu.Unlock()
	})
}

type resources struct {
	candidate *candidate
}

func (resources *resources) Retire(context.Context) error {
	if resources == nil || resources.candidate == nil {
		return ErrResolverState
	}
	resources.candidate.unregister()
	return nil
}

var _ configruntime.Builder = (*Manager)(nil)
