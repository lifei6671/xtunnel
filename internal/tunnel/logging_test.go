package tunnel

import (
	"io"
	"log/slog"

	"github.com/lifei6671/xtunnel/internal/logging"
)

func testTunnelLogger() *slog.Logger {
	return testTunnelLoggerTo(io.Discard)
}

func testTunnelLoggerTo(writer io.Writer) *slog.Logger {
	logger, err := logging.New(writer, logging.Options{
		Level: logging.LevelDebug, Format: "json", Component: "server",
	})
	if err != nil {
		panic(err)
	}
	return logger
}
