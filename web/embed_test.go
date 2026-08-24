package webui

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestDistContainsProductionAssets(t *testing.T) {
	index, err := Dist.ReadFile("dist/index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	if len(bytes.TrimSpace(index)) == 0 {
		t.Fatal("embedded index is empty")
	}

	foundJavaScript := false
	err = fs.WalkDir(Dist, "dist/assets", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		asset, err := Dist.ReadFile(path)
		if err != nil {
			return err
		}
		if len(asset) > 0 {
			foundJavaScript = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded assets: %v", err)
	}
	if !foundJavaScript {
		t.Fatal("embedded assets do not contain a non-empty JavaScript bundle")
	}
}
