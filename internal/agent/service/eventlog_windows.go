//go:build windows

package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

const (
	windowsEventLogSource         = windowsServiceName
	windowsEventLogMarkerValue    = "XTunnelManaged"
	windowsEventLogMessageFile    = `%SystemRoot%\System32\EventCreate.exe`
	windowsEventLogRegistryRoot   = `SYSTEM\CurrentControlSet\Services\EventLog\Application`
	windowsEventLogInformationID  = 1
	windowsEventLogWarningID      = 2
	windowsEventLogErrorID        = 3
	windowsEventLogTypesSupported = eventlog.Info | eventlog.Warning | eventlog.Error
)

type windowsEventSourceStore interface {
	inspect(string) (exists bool, managed bool, err error)
	install(string) error
	remove(string) error
}

// registryWindowsEventSourceStore 将 Event Source 视为安装器拥有的系统资源。
// 除 x/sys 创建的标准值外，私有 marker 用于在重复安装、异常中断恢复和卸载时
// 区分 XTunnel 产物与管理员或其他程序创建的同名 Source。
type registryWindowsEventSourceStore struct{}

type windowsEventSourceRegistryKey interface {
	SetDWordValue(string, uint32) error
	SetExpandStringValue(string, string) error
	SetStringValue(string, string) error
	Close() error
}

func (registryWindowsEventSourceStore) inspect(source string) (bool, bool, error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		windowsEventLogRegistryRoot+`\`+source,
		registry.QUERY_VALUE,
	)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("open Windows Event Log Source: %w", err)
	}

	marker, markerType, markerErr := key.GetStringValue(windowsEventLogMarkerValue)
	customSource, customSourceType, customSourceErr := key.GetIntegerValue("CustomSource")
	messageFile, messageFileType, messageFileErr := key.GetStringValue("EventMessageFile")
	typesSupported, typesSupportedType, typesSupportedErr := key.GetIntegerValue("TypesSupported")
	closeErr := key.Close()
	for _, valueErr := range []error{markerErr, customSourceErr, messageFileErr, typesSupportedErr} {
		if valueErr != nil && !errors.Is(valueErr, windows.ERROR_FILE_NOT_FOUND) {
			return true, false, errors.Join(
				fmt.Errorf("inspect Windows Event Log Source values: %w", valueErr),
				closeErr,
			)
		}
	}
	if closeErr != nil {
		return true, false, fmt.Errorf("close Windows Event Log Source registry key: %w", closeErr)
	}

	managed := markerErr == nil && markerType == registry.SZ && marker == windowsManagedMarker &&
		customSourceErr == nil && customSourceType == registry.DWORD && customSource == 1 &&
		messageFileErr == nil && messageFileType == registry.EXPAND_SZ &&
		strings.EqualFold(messageFile, windowsEventLogMessageFile) &&
		typesSupportedErr == nil && typesSupportedType == registry.DWORD &&
		typesSupported == uint64(windowsEventLogTypesSupported)
	return true, managed, nil
}

func (registryWindowsEventSourceStore) install(source string) error {
	return installWindowsEventSource(source, createWindowsEventSourceRegistryKey, removeWindowsEventSource)
}

func (registryWindowsEventSourceStore) remove(source string) error {
	return removeWindowsEventSource(source)
}

func removeWindowsEventSource(source string) error {
	if err := eventlog.Remove(source); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return fmt.Errorf("remove Windows Event Log Source: %w", err)
	}
	return nil
}

func createWindowsEventSourceRegistryKey(source string) (windowsEventSourceRegistryKey, bool, error) {
	applicationKey, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		windowsEventLogRegistryRoot,
		registry.CREATE_SUB_KEY,
	)
	if err != nil {
		return nil, false, fmt.Errorf("open Windows Application Event Log registry key: %w", err)
	}
	key, alreadyExists, createErr := registry.CreateKey(applicationKey, source, registry.SET_VALUE)
	closeErr := applicationKey.Close()
	if createErr != nil || closeErr != nil {
		if createErr == nil {
			closeErr = errors.Join(closeErr, key.Close())
			if !alreadyExists {
				closeErr = errors.Join(closeErr, removeWindowsEventSource(source))
			}
		}
		if createErr != nil {
			createErr = fmt.Errorf("create Windows Event Log Source: %w", createErr)
		}
		return nil, false, errors.Join(createErr, closeErr)
	}
	return &key, alreadyExists, nil
}

// installWindowsEventSource 只清理本次明确创建的 Source。标准值或 marker 任一
// 写入失败都会关闭并回滚该 Key；并发出现的外来同名 Key 则拒绝接管或删除。
func installWindowsEventSource(
	source string,
	create func(string) (windowsEventSourceRegistryKey, bool, error),
	remove func(string) error,
) (resultErr error) {
	key, alreadyExists, err := create(source)
	if err != nil {
		return err
	}
	if alreadyExists {
		return errors.Join(
			errors.New("Windows Event Log Source appeared during installation; refusing to overwrite it"),
			key.Close(),
		)
	}
	defer func() {
		resultErr = errors.Join(resultErr, key.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, remove(source))
		}
	}()

	if err := key.SetDWordValue("CustomSource", 1); err != nil {
		return fmt.Errorf("set Windows Event Log CustomSource: %w", err)
	}
	if err := key.SetExpandStringValue("EventMessageFile", windowsEventLogMessageFile); err != nil {
		return fmt.Errorf("set Windows Event Log message file: %w", err)
	}
	if err := key.SetDWordValue("TypesSupported", windowsEventLogTypesSupported); err != nil {
		return fmt.Errorf("set Windows Event Log event types: %w", err)
	}
	if err := key.SetStringValue(windowsEventLogMarkerValue, windowsManagedMarker); err != nil {
		return fmt.Errorf("mark managed Windows Event Log Source: %w", err)
	}
	return nil
}

func ensureWindowsEventSource(store windowsEventSourceStore) (created bool, err error) {
	exists, managed, err := store.inspect(windowsEventLogSource)
	if err != nil {
		return false, err
	}
	if exists {
		if !managed {
			return false, errors.New("refusing to overwrite an unmanaged or modified XTunnelAgent Event Log Source")
		}
		return false, nil
	}
	if err := store.install(windowsEventLogSource); err != nil {
		return false, err
	}
	return true, nil
}

func removeManagedWindowsEventSource(store windowsEventSourceStore) error {
	exists, managed, err := store.inspect(windowsEventLogSource)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !managed {
		return errors.New("refusing to remove an unmanaged or modified XTunnelAgent Event Log Source")
	}
	return store.remove(windowsEventLogSource)
}

func requireManagedWindowsEventSource(store windowsEventSourceStore) error {
	exists, managed, err := store.inspect(windowsEventLogSource)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("managed XTunnelAgent Event Log Source is missing")
	}
	if !managed {
		return errors.New("XTunnelAgent Event Log Source is unmanaged or modified")
	}
	return nil
}

type windowsEventLogger interface {
	Info(uint32, string) error
	Warning(uint32, string) error
	Error(uint32, string) error
	Close() error
}

type windowsEventLogOpen func(string) (windowsEventLogger, error)

// windowsEventLogWriter 保留 logging.JSONHandler 生成的完整 JSON，只把 level
// 映射到 Windows 原生事件类型。它不重新解释或重建属性，避免形成第二套脱敏和字段
// 规范；Event Log 写入失败原样返回，SCM 路径不得静默退回 stderr。
type windowsEventLogWriter struct {
	mu          sync.Mutex
	log         windowsEventLogger
	closed      bool
	failureOnce sync.Once
	failures    chan error
}

func openWindowsEventLogWriter(source string, open windowsEventLogOpen) (*windowsEventLogWriter, error) {
	log, err := open(source)
	if err != nil {
		return nil, fmt.Errorf("open Windows Event Log Source: %w", err)
	}
	return &windowsEventLogWriter{log: log, failures: make(chan error, 1)}, nil
}

// Failures 返回运行期首个 Event Log 写失败。slog.Logger 不向调用方返回
// Handler 错误，SCM owner 必须单独监听该信号并终止服务，不能静默丢日志。
func (writer *windowsEventLogWriter) Failures() <-chan error {
	return writer.failures
}

func (writer *windowsEventLogWriter) reportFailure(err error) error {
	if err != nil {
		writer.failureOnce.Do(func() { writer.failures <- err })
	}
	return err
}

func (writer *windowsEventLogWriter) Write(payload []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed {
		return 0, writer.reportFailure(errors.New("Windows Event Log writer is closed"))
	}

	message := bytes.TrimSuffix(payload, []byte("\n"))
	if len(message) == 0 || bytes.Contains(message, []byte("\n")) {
		return 0, writer.reportFailure(errors.New("Windows Event Log write requires exactly one JSON record"))
	}
	var envelope struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return 0, writer.reportFailure(fmt.Errorf("decode Windows Event Log JSON record: %w", err))
	}

	var err error
	switch envelope.Level {
	case "debug", "info":
		err = writer.log.Info(windowsEventLogInformationID, string(message))
	case "warn":
		err = writer.log.Warning(windowsEventLogWarningID, string(message))
	case "error":
		err = writer.log.Error(windowsEventLogErrorID, string(message))
	default:
		return 0, writer.reportFailure(fmt.Errorf("unsupported Windows Event Log level %q", envelope.Level))
	}
	if err != nil {
		return 0, writer.reportFailure(fmt.Errorf("write Windows Event Log record: %w", err))
	}
	return len(payload), nil
}

func (writer *windowsEventLogWriter) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed {
		return nil
	}
	writer.closed = true
	if err := writer.log.Close(); err != nil {
		return fmt.Errorf("close Windows Event Log Source: %w", err)
	}
	return nil
}

var _ io.WriteCloser = (*windowsEventLogWriter)(nil)
