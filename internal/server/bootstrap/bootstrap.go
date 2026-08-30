// Package bootstrap 负责装配并运行 Server 进程。
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lifei6671/xtunnel/internal/buildinfo"
	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/logging"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/tracing"
)

// Execute 把操作系统输入和信号接入 Server 生命周期，并返回进程退出码。
func Execute(program string, args, environ []string, stderr io.Writer) int {
	startedAt := time.Now()
	return executeWithRun(program, args, environ, stderr, func(ctx context.Context, options baseconfig.Options, stderr io.Writer) error {
		return runWithStorageAndBootstrapOptions(ctx, options, stderr, func(ctx context.Context, dataDir string) (storage, error) {
			return openServerStorage(ctx, dataDir, externallock.RuntimeDirectory)
		}, func(ctx context.Context, config serverconfig.Config, resources storage, logger *slog.Logger, traceRuntime *tracing.Runtime) (io.Closer, error) {
			return openGatewayAndBootstrapAtTracing(ctx, config, resources, logger, startedAt, traceRuntime)
		})
	})
}

// executeWithRun 是进程命令分发与信号 Context 的唯一入口。管理命令、维护命令和
// 常驻 Server 共用相同退出码和单行错误输出，测试只替换常驻 runner；具体错误
// 是否包含敏感信息由各业务边界负责保证。
func executeWithRun(
	program string,
	args, environ []string,
	stderr io.Writer,
	runner func(context.Context, baseconfig.Options, io.Writer) error,
) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := newServerCommand(program, args, environ, stderr, runner)
	err := command.Run(ctx, append([]string{program}, args...))
	if errors.Is(err, errServerCLIHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", program, err)
		return 1
	}
	return 0
}

// run 完成 Server 的配置、日志、Web 资源、External Lock 和 SQLite 初始化，并保持前台运行直到收到退出信号。
// 后续任务会在 SQLite 已就绪且 External Lock 仍被持有时继续接入 PKI 和 Listener。
func run(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	startedAt := time.Now()
	return runWithStorageAndBootstrap(ctx, program, args, environ, stderr, func(ctx context.Context, dataDir string) (storage, error) {
		return openServerStorage(ctx, dataDir, externallock.RuntimeDirectory)
	}, func(ctx context.Context, config serverconfig.Config, resources storage, logger *slog.Logger, traceRuntime *tracing.Runtime) (io.Closer, error) {
		return openGatewayAndBootstrapAtTracing(ctx, config, resources, logger, startedAt, traceRuntime)
	})
}

// runWithStorage 保留给只验证存储生命周期的测试，不启动任何 Listener。
func runWithStorage(ctx context.Context, program string, args, environ []string, stderr io.Writer, openStorage func(context.Context, string) (storage, error)) error {
	return runWithStorageAndBootstrap(ctx, program, args, environ, stderr, openStorage, nil)
}

// runWithStorageAndBootstrap 按配置、日志、Web、存储、运行时的固定顺序启动，并在
// 退出时先关闭所有 Listener/Session，再关闭 SQLite 和 External Lock。任一阶段失败
// 都逆序释放已经取得的资源，运行时异步错误则优先于普通信号退出返回。
func runWithStorageAndBootstrap(
	ctx context.Context,
	program string,
	args, environ []string,
	stderr io.Writer,
	openStorage func(context.Context, string) (storage, error),
	openBootstrap func(context.Context, serverconfig.Config, storage, *slog.Logger, *tracing.Runtime) (io.Closer, error),
) error {
	options, err := parseConfigOptions(program, args, environ, stderr)
	if err != nil {
		return err
	}
	return runWithStorageAndBootstrapOptions(ctx, options, stderr, openStorage, openBootstrap)
}

func runWithStorageAndBootstrapOptions(
	ctx context.Context,
	options baseconfig.Options,
	stderr io.Writer,
	openStorage func(context.Context, string) (storage, error),
	openBootstrap func(context.Context, serverconfig.Config, storage, *slog.Logger, *tracing.Runtime) (io.Closer, error),
) (resultErr error) {
	config, err := serverconfig.Load(options)
	if err != nil {
		return fmt.Errorf("load server config: %w", err)
	}
	logger, err := logging.New(stderr, logging.Options{
		Level:     config.Logging.Level,
		Format:    config.Logging.Format,
		Component: "server",
	})
	if err != nil {
		return fmt.Errorf("initialize server logging: %w", err)
	}
	var traceRuntime *tracing.Runtime
	if openBootstrap != nil {
		traceRuntime, err = tracing.New(ctx, tracing.Config{
			ServiceName:    "xtunnel-server",
			ServiceVersion: buildinfo.Version(),
			ReportExportFailure: func() {
				logger.Warn("tracing_export_failed", logging.ErrorCodeKey, "EXPORT_FAILED")
			},
		})
		if err != nil {
			return fmt.Errorf("initialize server tracing: %w", err)
		}
		// 数据面 owner 的 Close/Wait 先于本函数返回；最后再用不继承进程取消的
		// 有界 Context Flush，避免丢失收敛阶段 Span，也不让 Collector 阻塞退出。
		defer func() {
			resultErr = errors.Join(resultErr, traceRuntime.Shutdown(context.WithoutCancel(ctx)))
		}()
	}
	if err := validateEmbeddedWeb(); err != nil {
		return fmt.Errorf("initialize embedded web: %w", err)
	}
	resources, err := openStorage(ctx, config.Server.DataDir)
	if err != nil {
		return fmt.Errorf("initialize server storage: %w", err)
	}
	if serverResources, ok := resources.(*serverStorage); ok {
		if _, err := reconcileGatewayRotationAudit(ctx, serverResources.dataDir, serverResources.database, logger); err != nil {
			return errors.Join(fmt.Errorf("reconcile gateway rotation security audit before startup: %w", err), resources.Close())
		}
	}
	var bootstrapSocket io.Closer
	if openBootstrap != nil {
		bootstrapSocket, err = openBootstrap(ctx, config, resources, logger, traceRuntime)
		if err != nil {
			return errors.Join(fmt.Errorf("initialize admin bootstrap socket: %w", err), resources.Close())
		}
	}

	logger.InfoContext(ctx, "process_started")
	var runtimeErr error
	if source, ok := bootstrapSocket.(interface{ RuntimeErrors() <-chan error }); ok {
		select {
		case runtimeErr = <-source.RuntimeErrors():
		case <-ctx.Done():
			// 信号退出与运行时失败可能同时发生；已排队的具体错误优先返回。
			select {
			case runtimeErr = <-source.RuntimeErrors():
			default:
			}
		}
	} else {
		<-ctx.Done()
	}
	if bootstrapSocket != nil {
		if err := bootstrapSocket.Close(); err != nil {
			return errors.Join(runtimeErr, fmt.Errorf("close admin bootstrap socket: %w", err), resources.Close())
		}
	}
	closeErr := resources.Close()
	logger.Info("process_stopped")
	if runtimeErr != nil {
		if closeErr != nil {
			return errors.Join(runtimeErr, fmt.Errorf("close server storage: %w", closeErr))
		}
		return runtimeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close server storage: %w", closeErr)
	}
	return nil
}

// parseConfigOptions 只收集 YAML、环境变量和显式 CLI override；字段解释、默认值与
// 类型校验仍由配置 Schema 驱动的 Load 统一完成。
func parseConfigOptions(program string, args, environ []string, stderr io.Writer) (baseconfig.Options, error) {
	return parseServerConfigOptions(program, args, environ, stderr)
}

// configOverrides 实现可重复的 --set path=value flag，并保留最后一次显式赋值。
type configOverrides map[string]string

// Set 严格拆分第一个等号，允许值本身继续包含等号。
func (values configOverrides) Set(raw string) error {
	path, value, ok := strings.Cut(raw, "=")
	if !ok || path == "" {
		return errors.New("expected path=value")
	}
	values[path] = value
	return nil
}
