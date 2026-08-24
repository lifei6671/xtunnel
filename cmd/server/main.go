package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(execute(os.Args[0], os.Args[1:], os.Environ(), os.Stderr))
}

// execute 把操作系统信号转换为 Context 取消，并统一进程退出码。
func execute(program string, args, environ []string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, program, args, environ, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "%s: %v\n", program, err)
		return 1
	}
	return 0
}
