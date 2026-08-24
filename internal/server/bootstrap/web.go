package bootstrap

import (
	"bytes"
	"fmt"
	"io/fs"

	webui "github.com/lifei6671/xtunnel/web"
)

func validateEmbeddedWeb() error {
	return validateWebAssets(webui.Dist)
}

func validateWebAssets(assets fs.FS) error {
	index, err := fs.ReadFile(assets, "dist/index.html")
	if err != nil {
		return fmt.Errorf("read dist/index.html: %w", err)
	}
	if len(bytes.TrimSpace(index)) == 0 {
		return fmt.Errorf("dist/index.html is empty")
	}
	return nil
}
