//go:build windows

package config

import (
	"strings"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
)

func TestServiceLoadKeepsProfilesSeparate(t *testing.T) {
	foreground, err := pathprofile.Resolve("auto")
	if err != nil {
		t.Fatal(err)
	}
	service, err := pathprofile.ResolveService("auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(foreground.DataDir, service.DataDir) {
		t.Fatal("profiles share data")
	}
	for _, test := range []struct {
		name, path string
		load       func(baseconfig.Options) (Config, error)
		want       string
		reject     bool
	}{
		{"service_auto", "auto", LoadService, service.DataDir, false},
		{"service_fixed", service.DataDir, LoadService, service.DataDir, false},
		{"foreground_auto", "auto", Load, foreground.DataDir, false},
		{"service_rejects_foreground", foreground.DataDir, LoadService, "", true},
		{"foreground_rejects_service", service.DataDir, Load, "", true},
		{"service_rejects_custom", `C:\custom-data`, LoadService, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := test.load(baseconfig.Options{YAML: []byte("management:\n  public_url: https://admin.example.test\nagent_gateway:\n  public_hostname: gateway.example.test\n"), CLI: map[string]string{"server.data_dir": test.path}})
			if test.reject {
				if err == nil {
					t.Fatal("accepted wrong profile")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.Server.DataDir != test.want {
				t.Fatalf("data = %q, want %q", config.Server.DataDir, test.want)
			}
		})
	}
}

func TestServiceLoadRetainsStrictSchemaAndCrossFieldValidation(t *testing.T) {
	for _, extra := range []string{"unknown: true\n", "tcp_ingress:\n  min_port: 40000\n  max_port: 30000\n", "server:\n  data_dir: auto\n  data_dir: auto\n"} {
		_, err := LoadService(baseconfig.Options{YAML: []byte("management:\n  public_url: https://admin.example.test\nagent_gateway:\n  public_hostname: gateway.example.test\n" + extra), CLI: map[string]string{"server.data_dir": "auto"}})
		if err == nil {
			t.Fatalf("accepted invalid configuration %q", extra)
		}
	}
}
