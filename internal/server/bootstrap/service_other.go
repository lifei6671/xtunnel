//go:build !windows

package bootstrap

import baseconfig "github.com/lifei6671/xtunnel/internal/config"

func executeService([]string, []string) (bool, error) { return false, nil }

func maintenanceOptions(values *configFlagValues, environ []string) (baseconfig.Options, bool, error) {
	options, err := values.options(environ)
	return options, false, err
}
