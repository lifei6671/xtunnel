//go:build windows

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
	"github.com/lifei6671/xtunnel/internal/server/service"
	"github.com/lifei6671/xtunnel/internal/tracing"
)

// executeService 在读取配置或启动业务资源前进入 Dispatcher。服务入口只接收
// SCM 固定 ImagePath 的配置参数，生命周期使用 SCM 的取消信号而非控制台信号。
func executeService(args, environ []string) (bool, error) {
	return service.RunIfService(func(ctx context.Context, writer io.Writer, ready func()) error {
		return runService(ctx, writer, ready, args, environ)
	})
}

// runService 在 Dispatcher 回调内装配固定 Service Profile，资源与停止路径仍由
// runConfigured 唯一拥有；隔离 CI 测试入口复用相同生命周期以报告启动失败。
func runService(ctx context.Context, writer io.Writer, ready func(), args, environ []string) error {
	configPath, err := service.FixedConfigPath()
	if err != nil {
		return err
	}
	if len(args) != 2 || args[0] != "--config" || !strings.EqualFold(args[1], configPath) {
		return errors.New("Windows Server service requires only its fixed --config path")
	}
	content, err := service.ReadConfig()
	if err != nil {
		return err
	}
	config, err := serverconfig.LoadService(baseconfig.Options{YAML: content, Environment: environ})
	if err != nil {
		return fmt.Errorf("load Server service config: %w", err)
	}
	startedAt := time.Now()
	return runConfigured(ctx, config, writer, func(ctx context.Context, dataDir string) (storage, error) {
		profile, err := pathprofile.ResolveService(dataDir)
		if err != nil {
			return nil, err
		}
		return openServerStorage(ctx, profile.DataDir, profile.RuntimeDir)
	}, func(ctx context.Context, config serverconfig.Config, resources storage, logger *slog.Logger, tracing *tracing.Runtime) (io.Closer, error) {
		return openGatewayAndBootstrapAtTracing(ctx, config, resources, logger, startedAt, tracing)
	}, ready)
}

// maintenanceOptions 只在管理员显式指定固定受管 Config 时选择 Service Profile。
// 在读取凭据或取得数据库之前检查 SCM 停止状态；随后 External Lock 处理与
// 并发启动之间的竞争。其他配置路径始终保留前台语义，不尝试服务目录兜底。
func maintenanceOptions(values *configFlagValues, environ []string) (baseconfig.Options, bool, error) {
	configPath, err := service.FixedConfigPath()
	if err != nil {
		return baseconfig.Options{}, false, err
	}
	if !filepath.IsAbs(values.path) || !strings.EqualFold(filepath.Clean(values.path), configPath) {
		options, err := values.options(environ)
		return options, false, err
	}
	if err := service.RequireStopped(); err != nil {
		return baseconfig.Options{}, true, err
	}
	content, err := service.ReadConfig()
	if err != nil {
		return baseconfig.Options{}, true, err
	}
	// CLI/Environment 仍由同一 Schema 合并；去掉路径以避免绕过安全 Handle 重读。
	flags := configFlagValues{overrides: values.overrides}
	options, err := flags.options(environ)
	options.YAML = content
	return options, true, err
}
