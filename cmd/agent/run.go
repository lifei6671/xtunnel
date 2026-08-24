package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	agentconfig "github.com/lifei6671/xtunnel/internal/agent/config"
	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/logging"
)

// run 完成 Agent 进程当前阶段的配置和日志初始化，并保持前台运行直到收到退出信号。
// 后续任务会在等待 Context 取消前依次接入身份、Control Session 和 WorkPool。
func run(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	options, err := parseConfigOptions(program, args, environ, stderr)
	if err != nil {
		return err
	}
	config, err := agentconfig.Load(options)
	if err != nil {
		return fmt.Errorf("load agent config: %w", err)
	}
	logger, err := logging.New(stderr, logging.Options{
		Level:     config.Logging.Level,
		Format:    config.Logging.Format,
		Component: "agent",
	})
	if err != nil {
		return fmt.Errorf("initialize agent logging: %w", err)
	}

	logger.InfoContext(ctx, "process_started")
	<-ctx.Done()
	logger.Info("process_stopped")
	return nil
}

func parseConfigOptions(program string, args, environ []string, stderr io.Writer) (baseconfig.Options, error) {
	flags := flag.NewFlagSet(program, flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	overrides := make(configOverrides)
	flags.StringVar(&configPath, "config", "", "YAML configuration file")
	flags.Var(overrides, "set", "override one Schema path with path=value; may be repeated")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s [--config path] [--set path=value]...\n", program)
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return baseconfig.Options{}, fmt.Errorf("parse command line: %w", err)
	}
	if flags.NArg() != 0 {
		return baseconfig.Options{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	var yamlData []byte
	if configPath != "" {
		var err error
		yamlData, err = os.ReadFile(configPath)
		if err != nil {
			return baseconfig.Options{}, fmt.Errorf("read config file %q: %w", configPath, err)
		}
	}
	return baseconfig.Options{YAML: yamlData, Environment: environ, CLI: overrides}, nil
}

type configOverrides map[string]string

func (configOverrides) String() string {
	return ""
}

func (values configOverrides) Set(raw string) error {
	path, value, ok := strings.Cut(raw, "=")
	if !ok || path == "" {
		return errors.New("expected path=value")
	}
	values[path] = value
	return nil
}
