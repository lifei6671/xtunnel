package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/tracing"
)

type serviceLifecycleCloser struct{ close func() error }

func (closer serviceLifecycleCloser) Close() error { return closer.close() }

func TestServiceReadinessAndCleanupFollowResourceOwnership(t *testing.T) {
	for _, failStage := range []string{"", "storage", "runtime"} {
		t.Run("failure_"+failStage, func(t *testing.T) {
			config, err := serverconfig.Load(baseconfig.Options{YAML: []byte("server:\n  data_dir: auto\nmanagement:\n  public_url: https://admin.example.test\nagent_gateway:\n  public_hostname: gateway.example.test\n")})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("fixture initialization failure")
			var sequence []string
			err = runConfigured(ctx, config, io.Discard,
				func(context.Context, string) (storage, error) {
					sequence = append(sequence, "storage_open")
					if failStage == "storage" {
						return nil, failure
					}
					return serviceLifecycleCloser{func() error { sequence = append(sequence, "storage_close"); return nil }}, nil
				},
				func(context.Context, serverconfig.Config, storage, *slog.Logger, *tracing.Runtime) (io.Closer, error) {
					sequence = append(sequence, "runtime_open")
					if failStage == "runtime" {
						return nil, failure
					}
					return serviceLifecycleCloser{func() error { sequence = append(sequence, "runtime_close"); return nil }}, nil
				},
				func() { sequence = append(sequence, "ready"); cancel() },
			)
			want := []string{"storage_open", "runtime_open", "ready", "runtime_close", "storage_close"}
			if failStage == "storage" {
				want = []string{"storage_open"}
			}
			if failStage == "runtime" {
				want = []string{"storage_open", "runtime_open", "storage_close"}
			}
			if !reflect.DeepEqual(sequence, want) {
				t.Fatalf("sequence=%v want=%v", sequence, want)
			}
			if failStage == "" && err != nil {
				t.Fatal(err)
			}
			if failStage != "" && !errors.Is(err, failure) {
				t.Fatalf("lost error: %v", err)
			}
		})
	}
}
