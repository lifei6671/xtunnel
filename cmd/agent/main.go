package main

import (
	"os"

	"github.com/lifei6671/xtunnel/internal/agent/bootstrap"
)

func main() {
	os.Exit(bootstrap.Execute(os.Args[0], os.Args[1:], os.Environ(), os.Stderr))
}
